// Package kv defines the contract every kvgo storage engine implements.
//
// The engines (bitcask, and later lsm) live in their own packages and each
// provide an Open function that returns a DB. Code that uses kvgo depends
// only on this package, so it can switch engines without changing.
package kv

import (
	"errors"
	"time"
)

// DB is an embedded, crash-safe key-value store. It is safe for concurrent
// use by multiple goroutines.
//
// DB copies in and copies out: it never keeps a slice the caller passed in,
// and every slice it returns belongs to the caller. This rules out aliasing
// bugs, where a caller reusing a buffer silently changes stored data.
type DB interface {
	// Put stores value under key, replacing any existing value. It returns
	// nil once the write is acknowledged; what that guarantees depends on
	// Options.SyncMode. It returns ErrTooLarge before doing any work if key
	// or value is over the limit.
	Put(key, value []byte) error

	// Get returns a copy of the newest value for key, or ErrNotFound if the
	// key was never written or was deleted. An empty value is not a delete:
	// it returns an empty, non-nil slice and a nil error.
	Get(key []byte) ([]byte, error)

	// Delete removes key. Deleting a key that does not exist is not an
	// error.
	Delete(key []byte) error

	// Scan iterates the live keys in [start, end) in ascending order, as of
	// the moment Scan is called. A nil start means the first key; a nil end
	// means past the last. Engines without ordered keys (Bitcask) return
	// ErrUnsupported.
	Scan(start, end []byte) (Iterator, error)

	// Compact reclaims disk space now, instead of waiting for the automatic
	// trigger. If an automatic compaction is already running, Compact waits
	// for it rather than starting a second one.
	Compact() error

	// Sync makes every acknowledged write durable, whatever the SyncMode.
	// It always does a real fsync, so a caller on SyncNever can choose its
	// own durability points. It can still fail with an I/O error.
	Sync() error

	// Stats reports sizes and latencies. It does no I/O, so it is cheap
	// enough to call often.
	Stats() Stats

	// Close does a final sync and releases the directory lock. Every later
	// call on the DB, including a second Close, returns ErrClosed.
	Close() error
}

// Iterator walks keys in ascending order. The usual loop is:
//
//	it, err := db.Scan(nil, nil)
//	if err != nil { ... }
//	defer it.Close()
//	for it.Next() {
//		use(it.Key(), it.Value())
//	}
//	if err := it.Err(); err != nil { ... }
//
// Key and Value are valid only until the next call to Next: copy them to
// keep them.
type Iterator interface {
	// Next moves to the next key. It returns false when there are no more
	// keys or an error occurred; check Err to tell which.
	Next() bool

	// Key returns the current key.
	Key() []byte

	// Value returns the current value.
	Value() []byte

	// Err returns the error that stopped iteration, or nil if it simply
	// reached the end.
	Err() error

	// Close releases the files the iterator holds open. Always call it,
	// even after Next returns false.
	Close() error
}

// SyncMode decides when writes are forced to disk (fsync), and so which
// crashes an acknowledged write survives.
type SyncMode int

const (
	// SyncAlways fsyncs before Put returns: an acknowledged write survives a
	// power cut. It is the zero value, so Options{} is the safe default.
	SyncAlways SyncMode = iota

	// SyncBatch fsyncs every BatchInterval in the background. A power cut
	// can lose up to one interval of acknowledged writes. A process crash
	// loses nothing, because the data is already in the OS page cache.
	SyncBatch

	// SyncNever leaves flushing to the OS. A power cut can lose tens of
	// seconds of acknowledged writes. A process crash loses nothing.
	SyncNever
)

// Options configures an engine. The zero value is valid: every zero field
// is replaced by its default (see WithDefaults). Each engine ignores the
// fields that belong to the other one.
type Options struct {
	SyncMode        SyncMode
	BatchInterval   time.Duration // SyncBatch only; default 10ms
	SegmentSize     int64         // bitcask: start a new log file at this size; default 64 MiB
	MemtableSize    int           // lsm: flush the memtable at this size; default 4 MiB
	BlockSize       int           // lsm: size of one SSTable data block; default 4 KiB
	BloomBitsPerKey int           // lsm: bloom filter size per key; default 10
}

// WithDefaults returns a copy of o with every zero field set to its
// default. o itself is not changed, because the receiver is a value.
func (o Options) WithDefaults() Options {
	if o.BatchInterval == 0 {
		o.BatchInterval = 10 * time.Millisecond
	}
	if o.SegmentSize == 0 {
		o.SegmentSize = 64 << 20 // 64 MiB
	}
	if o.MemtableSize == 0 {
		o.MemtableSize = 4 << 20 // 4 MiB
	}
	if o.BlockSize == 0 {
		o.BlockSize = 4 << 10 // 4 KiB
	}
	if o.BloomBitsPerKey == 0 {
		o.BloomBitsPerKey = 10
	}
	return o
}

// Size limits, checked by Put and Delete before any work is done. They keep
// one bad call from making the engine allocate gigabytes.
const (
	MaxKeySize   = 64 << 10 // 64 KiB
	MaxValueSize = 16 << 20 // 16 MiB
)

// Stats is a point-in-time report on an engine, used by cmd/bench and by
// anyone tuning an engine in production.
type Stats struct {
	LiveBytes    int64 // sum of len(key)+len(value) over live keys
	DiskBytes    int64 // bytes in all data files
	Files        int   // number of segments (bitcask) or SSTables (lsm)
	UserBytes    int64 // bytes callers passed to Put
	WrittenBytes int64 // bytes the engine wrote to files, compaction included

	Get, Put, Delete Latency // how long each kind of call took
}

// SpaceAmp is space amplification: bytes on disk per byte of live data.
// 1.0 is perfect; 3.0 means two thirds of the disk is old versions and
// tombstones waiting for compaction.
func (s Stats) SpaceAmp() float64 { return ratio(s.DiskBytes, s.LiveBytes) }

// WriteAmp is write amplification: bytes written to disk per byte the
// caller asked to store. Compaction rewrites data, so this is above 1.0.
func (s Stats) WriteAmp() float64 { return ratio(s.WrittenBytes, s.UserBytes) }

// Latency summarises how long one kind of call took. P99 = 2ms means 99% of
// calls finished within 2ms and the slowest 1% took longer.
type Latency struct {
	Count         int64 // number of calls measured
	P50, P95, P99 time.Duration
}

// ratio returns a/b, or 0 when b is 0 so an empty engine does not report
// NaN or +Inf.
func ratio(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// Sentinel errors. Engines wrap them with context (file, offset) using %w,
// so callers must compare with errors.Is, not ==.
var (
	ErrNotFound    = errors.New("kv: not found")
	ErrCorrupt     = errors.New("kv: checksum mismatch")
	ErrUnsupported = errors.New("kv: not supported by this engine")
	ErrClosed      = errors.New("kv: closed")
	ErrTooLarge    = errors.New("kv: key or value over limit")
)
