# kvgo design

Status: draft v1, written before any Go code. Every "decision" below has a reason;
anything marked **OPEN** is a question I have not answered yet.

## 1. Goals and non-goals

**Goals**

- **Embeddable.** A Go library you import, not a server. One directory, one process.
- **Crash-safe.** Anything acknowledged is recoverable after the crashes listed in section 3.
  A damaged byte is reported or cut off, never returned as data.
- **Measured.** Every performance claim in the README comes from `cmd/bench`, with the
  machine, disk, SyncMode and key/value sizes printed next to it.

**Non-goals**

- Network server, replication, clustering.
- Transactions or multi-key atomicity. Each call is atomic on its own; nothing more.
- TTLs, compression, encryption.
- Multiple processes on one directory. The second `Open` fails (section 8).

## 2. API and semantics

`kv.DB` is the contract for both engines (Bitcask first, LSM later). The engine **copies in and
copies out**: it never keeps a slice the caller passed, and every slice it returns belongs to
the caller. Reason: aliasing bugs show up as random corruption far from the cause.

| Method | Returns | Errors it can return | Copies? |
|---|---|---|---|
| `Put(key, value)` | `nil` once the write is acknowledged (section 3) | `ErrTooLarge` (key > `MaxKeySize` or value > `MaxValueSize`, checked first, before any work), `ErrClosed`, the sticky I/O error after a failed write (below) | Copies both into its write buffer before returning |
| `Get(key)` | A fresh copy of the newest value | `ErrNotFound` (never written, or deleted), `ErrCorrupt` (CRC mismatch or key mismatch on read), `ErrClosed`, I/O error | Copies the value out; key not retained |
| `Delete(key)` | `nil`, **including when the key does not exist** | `ErrTooLarge` (key), `ErrClosed`, sticky I/O error | Key not retained |
| `Scan(start, end)` | Iterator over live keys in `[start, end)`, ascending, snapshot as of the call | `ErrUnsupported` (Bitcask: keys are unordered), `ErrClosed` | `Key()`/`Value()` valid only until the next `Next()`; caller copies to keep |
| `Compact()` | `nil` when finished | `ErrClosed`, I/O error. Calling it while an automatic compaction runs waits for that one rather than starting a second | n/a |
| `Sync()` | `nil` once every acknowledged write is on stable storage, whatever the SyncMode | `ErrClosed`, I/O error | n/a |
| `Stats()` | A point-in-time `Stats` value; cheap, takes no I/O | none | n/a |
| `Close()` | `nil` after a final sync and releasing the directory lock | I/O error from the final sync. Later calls on the DB return `ErrClosed`; a second `Close` returns `ErrClosed` | n/a |

`Open(dir, Options)` additionally returns `ErrCorrupt` (with file name and offset) for damage
outside the torn tail, an "already open" error if the directory lock is held, and any I/O error.

**Decisions on edge cases**

- **Empty value is not a delete.** `Put(k, []byte{})` then `Get(k)` returns an empty slice and
  `nil`. A deleted key returns `ErrNotFound`. The record's `kind` byte exists to keep these apart.
- **Empty key is allowed.** It is the smallest key. Tested deliberately.
- **`nil` and `[]byte{}` are equivalent** as arguments (both length 0). `Get` returns a non-nil
  empty slice for an empty value.
- **Delete of a missing key writes a tombstone anyway.** One code path; the LSM needs it.
  Revisit only with a measurement.
- **After an I/O error or full disk**, that call returns the error and the DB is *poisoned*:
  every later write returns the same error until reopened. Reads of already-indexed data keep
  working. Reason: after a failed append, I don't know what is at the file tail.
- **Visibility.** A `Get` sees every `Put`/`Delete` that returned before the `Get` started.
  Concurrent calls are linearizable per key. `Scan` is a snapshot at call time.
- **Errors are sentinels**, wrapped with `%w` and context (file, offset); callers use `errors.Is`.

## 3. What "acknowledged" means

**Acknowledged = `Put` (or `Delete`) returned `nil`.** What that guarantees depends on
`SyncMode`. `SyncAlways` is the zero value, so `Options{}` is the safe configuration.

| SyncMode | When fsync happens | Survives `kill -9` / panic | Survives OS crash / power cut | Loss window |
|---|---|---|---|---|
| `SyncAlways` (default) | Before `Put` returns, under the write lock | Yes | Yes | **None.** A write that returned is on stable storage |
| `SyncBatch` | A background goroutine every `BatchInterval` (default 10 ms) | Yes (data is in the OS page cache) | **No.** Last interval lost | Up to `BatchInterval` + one fsync duration, of acknowledged writes |
| `SyncNever` | Never, except `Sync()`, rotation, `Close()` | Yes | **No.** | Whatever the OS had not flushed: tens of seconds, unbounded by us |

Rules that hold in *every* mode:

- **Order is preserved.** After any crash the surviving writes are a *prefix* of the acknowledged
  sequence (point-in-time recovery). We never keep write N+1 and lose write N.
- **No garbage.** A torn or flipped record is detected by CRC and never returned.
- **`Sync()` always works**, so a caller on `SyncNever` can add their own durability points.
- **Visibility is after the append** (and after the fsync in `SyncAlways`). In `SyncBatch` a
  reader may see a value a power cut will later erase. That is part of the trade-off.
- "Power cut" assumes the disk honours fsync. A drive that lies about flushing is out of scope.
- Directory entries are fsynced when files are created, renamed or deleted (segment rotation,
  compaction), because on Linux a fsynced file can still vanish if its directory was not.

**Measured on this machine** (Windows 11, spike, 100-byte append + fsync, 1,000 iterations):
p50 ≈ 0.7 ms, p99 ≈ 73 ms, max ≈ 143 ms; 100,000 un-synced 100-byte appends took ≈ 0.4 s.
So `SyncAlways` is bounded at roughly 1,400 writes/s here, and its tail latency is dominated by
the disk, not the engine. `cmd/fsyncbench` replaces this spike with proper numbers.

**Crash test contract** (`cmd/crashtest`): `kill -9` verifies the logic in all modes. It does
*not* test the fsync calls; those are only tested by reasoning plus the ordering rules above.

## 4. Record format

Fixed 21-byte header, then key, then value. **All integers little-endian.**

| Offset | Size | Field | Notes |
|---|---|---|---|
| 0 | 4 | `crc` | CRC-32C (Castagnoli) of bytes `[4 : 21+keyLen+valLen)` |
| 4 | 8 | `seq` | uint64, strictly increasing, never reused across restarts (recovered as max+1) |
| 12 | 1 | `kind` | `0` = put, `1` = delete (tombstone, `valLen` must be 0) |
| 13 | 4 | `keyLen` | uint32, ≤ `MaxKeySize` |
| 17 | 4 | `valLen` | uint32, ≤ `MaxValueSize` |
| 21 | keyLen | `key` | raw bytes |
| 21+keyLen | valLen | `value` | raw bytes |

- **The CRC covers everything after itself, lengths included.** If it skipped the lengths, a
  flipped length bit could make the reader allocate or read gigabytes before the check ran.
- **`PeekLen` validates `keyLen`/`valLen` against the limits before allocating anything.** The
  header is untrusted until the CRC passes.
- **Why these choices:** CRC-32C (hardware-accelerated in Go's stdlib, strong on burst errors);
  fixed-width lengths (simple to parse and fuzz; varints would save ~6 B/record); CRC at the
  front (a torn record is cut at the end, so it cannot have a valid trailer by accident);
  version once per file, not per record.

**Segment file header** (8 bytes, offset 0 of each segment; records start at offset 8):

| Offset | Size | Field | Notes |
|---|---|---|---|
| 0 | 4 | magic | `KVGO` |
| 4 | 1 | version | `1` |
| 5 | 1 | kind | `0` = live (appended record by record, may have a torn tail), `1` = compacted (written whole, fsynced, renamed; never torn) |
| 6 | 2 | reserved | zero |

**Hex dump: `Put("a", "b")` at `seq = 1`** (23 bytes; produced and checked by a throwaway program):

```
e2 6d 0d 3b   01 00 00 00 00 00 00 00   00     01 00 00 00   01 00 00 00   61    62
└── crc32c ┘  └──────── seq = 1 ──────┘  kind   keyLen = 1    valLen = 1    "a"   "b"
              (little-endian)            = put
```

**Hex dump: `Delete("a")` at `seq = 2`** (22 bytes; `kind = 01`, `valLen = 0`):

```
fe 74 32 7b   02 00 00 00 00 00 00 00   01     01 00 00 00   00 00 00 00   61
```

Both become golden tests: if one byte of the format changes, the test fails.

## 5. Index (keydir)

Bitcask keeps **every live key in RAM**, in a `map[string]Pos`:

```go
type Pos struct {
    Off int64  // offset of the record header in its segment
    Seq uint64 // sequence number, so recovery can compare any file order
    Seg uint32 // segment id
    Len uint32 // header + key + value, so one ReadAt fetches the whole record
}
```

(24 bytes.) A `Get` is: read-lock, copy `Pos`, unlock, one `ReadAt`, verify CRC, check the
key matches, copy the value out.

**Memory per key: ≈ 117 bytes, measured** (Go map, 1,000,000 keys of 15 bytes → 24-byte `Pos`:
116.6 B/key by `runtime.MemStats` after GC; the guide's earlier measurement agreed at ≈117).
Longer keys cost their length more. I will re-measure on the real keydir in week 3.

**RAM limit this implies:** index RAM ≈ `keys × (≈100 B + key length)`. 10 million 15-byte keys
need ≈ 1.1 GiB for the index alone, before any caching and before Go's GC headroom. 100 million
keys are out of reach on a normal machine. **Values are never in RAM**, so the data can be far
larger than memory; only the key count is bounded. Stage 2 (the LSM) removes this limit at the
cost of read amplification and range-scan complexity.

Decision: a plain hash map, because O(1) point reads and simplest recovery. It costs us ordered
scans and keys-in-RAM. Accepted for stage 1.

## 6. Compaction trigger

- **When:** on `Compact()`, or automatically when **dead bytes exceed 50% of the bytes in sealed
  segments**. Dead bytes per segment = segment size − live bytes, maintained as a counter that
  is decremented whenever a `Put`/`Delete` supersedes a record. This bounds space amplification
  at about 2×.
- **What it never touches:** the active (live) segment. Compaction first rotates under the write
  lock so every existing segment is sealed, then takes **all sealed segments at once**. All at
  once, not the worst one, because dropping a tombstone from one segment while an older `Put`
  survives in another would **resurrect a deleted key** on the next restart.
- **How it avoids racing writers:**
  1. Under the write lock: rotate, record the ids to compact. Release.
  2. With no lock held: stream those segments, keeping a record only if the keydir still points
     at exactly that `(segment, offset)`.
  3. Write survivors to new *compacted*-kind segments as `.tmp`, fsync, rename, fsync directory;
     same for hint files.
  4. For each copied key, **compare-and-swap** the keydir entry from the old position to the new
     one under the keydir lock. If a `Put` landed meanwhile, the CAS fails and the new write wins.
  5. Remove old segments from the reader map; the last in-flight reader deletes the file
     (readers pin segments with a refcount). fsync the directory.
- A crash at any step leaves either the old segments or the new ones valid; leftover `.tmp`
  files are deleted on `Open`. Recovery picks winners by `seq`, so file order is irrelevant.
- At most one compaction runs at a time.

## 7. Recovery

`Open(dir)` does, in order:

1. Create the directory if missing. Take an exclusive lock file; fail immediately if held.
2. Delete leftover `*.tmp` files (an unfinished compaction or hint write).
3. List segments by id. For each, validate the 8-byte header (magic, version, kind).
4. Scan every segment's records (using a hint file instead where one exists and validates),
   verifying each CRC. For each key keep the record with the **highest `seq`**; keep
   tombstones until the end, then drop keys whose winner is a tombstone. The rest is the keydir.
5. Repair the **torn tail** (below) of the newest *live* segment only.
6. Set `seq` to `max(seq seen) + 1`. Rebuild per-segment live-byte counters.
7. **Start a fresh live segment.** The repaired old tail is sealed forever, so a segment is only
   ever appended to by the process that created it.
8. Start the background goroutine if `SyncBatch`.

**Torn tail vs corruption**

| Where the bad record is | Meaning | Action |
|---|---|---|
| End of the newest live segment (short header, short body, or CRC mismatch on the last record) | Crash mid-append | Truncate the file to the last good record; log how many bytes were dropped |
| Newest live segment with no complete 8-byte header (0–7 bytes) | Crash while creating the segment | Delete the file; it never held a record |
| Anywhere in a sealed or compacted segment, or *before* the last record of a live one | Real corruption: bad disk or a bug | `Open` fails with `ErrCorrupt` plus file name and offset. **Never skip it**: cutting data from the middle silently loses acknowledged writes |

Why the segment `kind` byte: after compaction, the highest id is not necessarily the newest
live file, so "highest id may be torn" stops being true. The header says it explicitly.

## 8. Concurrency

| Lock / goroutine | Protects | Notes |
|---|---|---|
| `writeMu` (`sync.Mutex`) | Serialises `Put`, `Delete`, segment rotation, and the rotate/swap steps of compaction; the sequence counter; the active segment's append and fsync | `SyncAlways` fsyncs *while holding it* |
| `mu` (`sync.RWMutex`) | The keydir map and the reader map | Never held across disk I/O |
| Lock ordering | `writeMu` first, then `mu` | Always this order; rules out deadlock |
| Segment refcounts (atomics) | Lifetime of a segment file while readers use it | The last reader out deletes a compacted-away file |
| Directory lock file | One process per directory | Second `Open` fails |
| Background goroutine: **sync ticker** | Calls fsync every `BatchInterval` | Only in `SyncBatch`. Stopped by `Close` |
| Background goroutine: **compactor** | Runs compaction when the 50% trigger fires | One at a time; stopped and awaited by `Close` |

Rules: the keydir is updated only after the append succeeds (and after fsync in
`SyncAlways`). `Get` copies the `Pos` out under `RLock`, unlocks, then does the `ReadAt`.
`go test -race` is part of "done". `Close` stops both goroutines, waits for them, syncs,
then releases the directory lock.

## 9. Limits

| Limit | Value | Enforced |
|---|---|---|
| Max key size | 64 KiB (`MaxKeySize`) | `Put`/`Delete` return `ErrTooLarge` before any work; also checked by `PeekLen` on read |
| Max value size | 16 MiB (`MaxValueSize`) | Same |
| Min key / value | 0 bytes each; empty key and empty value are valid | |
| Keys in RAM | ≈ 117 B per key (15-byte keys): 10 M keys ≈ 1.1 GiB | Not enforced; documented. Out of memory is the failure mode |
| Dataset size | Bounded by disk, not RAM (values stay on disk) | |
| Processes per directory | **One.** A second `Open` fails immediately | Lock file |
| Segment size | 64 MiB default (`SegmentSize`); a record is never split across segments, so a single 16 MiB value fits | |
| Ordered scans | Not supported by Bitcask (`ErrUnsupported`) | |
| Platform | Developed on Windows; Linux behaviour (directory fsync) is a design assumption until tested; durability claims assume the OS honours fsync | |

## 10. Benchmarks I intend to run

Every result is printed with CPU, disk, OS, Go version, SyncMode, key/value sizes, key count,
and the commit hash. Fixed random seeds; the seed is printed.

1. **fsync modes** (`cmd/fsyncbench`, then `cmd/bench`): Put throughput and latency under
   `SyncAlways`, `SyncBatch` (10 ms), `SyncNever`, at 1, 8 and 64 writer goroutines.
2. **Get / Put latency percentiles:** p50, p95, p99, p99.9 (not averages), with 100 B and 4 KiB
   values, hot and cold page cache, Get during compaction vs idle.
3. **Startup time:** `Open` on 1 M and 10 M keys, with and without hint files.
4. **Space amplification:** `DiskBytes / LiveBytes` over a long overwrite/delete workload, with
   and without compaction; must stay near or below 2×.
5. **Write amplification:** `WrittenBytes / UserBytes`, compaction included.
6. **Keydir memory per key:** measured on the real structure, several key lengths.
7. **Crash test** is a correctness check, not a benchmark, but its pass count (rounds, kills) is
   reported next to the numbers.

Noise rules: run each benchmark at least 5 times, report the median and the spread, discard
a run if another heavy process was active.

## 11. Alternatives considered, and open questions

**Alternatives**

| Decision | Alternative | Why not |
|---|---|---|
| Hash-map keydir | B-tree / skiplist in RAM | Costs memory and complexity for ordered scans stage 1 does not offer. The LSM in stage 2 is that answer |
| CRC-32C | CRC-32 IEEE, xxHash64 | CRC-32C is hardware-accelerated in the stdlib, strong on burst errors, and what LevelDB/RocksDB/Pebble use |
| Fixed-width lengths | Varints | ~6 B/record saving, but a harder parser to fuzz |
| `SyncAlways` as default | Default to batch (the plan's decision 4) | The Go zero value is the default; the default should be the one that cannot lose data. README recommends `SyncBatch` for most uses (bbolt does the same, RocksDB the opposite) |
| `writeMu` + `RWMutex` | One mutex; `sync.Map`; sharded map | Readers do not block each other and it is easy to reason about; sharding only after measuring contention |
| Recovery by `seq`, not file order | "Later file wins" | Stays correct after compaction reorders file ids |
| Compact all sealed segments | Compact only the dirtiest | Avoids tombstone-drop resurrection |
| Truncate torn tail | Skip bad records and continue | Skipping loses acknowledged writes silently |
| Group commit | `SyncBatch` | Group commit makes every ack real at the price of up to one interval of latency. Deferred to a later step, not in v1 |

**Open questions** (decide by the date shown; each has a way to answer it)

1. **Does `SyncBatch` need a way to learn about a failed background fsync?** Proposal: poison
   the DB, and the next write returns the error. Decide in week 2 with `fsyncbench`.
2. **Do hint files pay for themselves?** Measure startup at 10 M keys without them first
   (week 4); add only if the number is bad.
3. **Is the 50% compaction trigger right?** Check space amplification and write amplification on
   the week 5 workload; the threshold is a constant, so it is cheap to move.
4. **Windows directory fsync:** Windows has no direct equivalent of fsyncing a directory handle.
   Decide in week 2 what the portable behaviour is and document the weaker guarantee if needed.
5. **Fsync tail latency on this machine:** p99 ≈ 73 ms looks like OS or antivirus interference,
   not the disk. Re-measure with `fsyncbench` before putting any number in the README.

**Pre-mortem: the three most likely reasons this project fails**

1. *Compaction loses or resurrects data.* Mitigation: CAS on the keydir, compact all sealed
   segments together, and the crash test runs compaction under load.
2. *Benchmarks are noise.* Mitigation: section 10's repeat-and-report rules; conditions printed
   with every number.
3. *Run out of time in the LSM.* Mitigation: stage 1 ships and is tagged v0.1 on its own;
   stage 2 reuses `internal/record`, `internal/segment` and `kvtest`.
