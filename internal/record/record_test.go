package record

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"testing"
)

// randBytes returns between 0 and max random bytes, so empty keys and empty
// values show up often.
func randBytes(rng *rand.Rand, max int) []byte {
	b := make([]byte, rng.IntN(max+1))
	for i := range b {
		b[i] = byte(rng.IntN(256))
	}
	return b
}

// randomRecord builds a random put (or, one time in five, a tombstone).
func randomRecord(rng *rand.Rand) Record {
	r := Record{Seq: rng.Uint64(), Key: randBytes(rng, 40)}
	if rng.IntN(5) == 0 {
		r.Kind = KindDelete // a tombstone has no value
	} else {
		r.Value = randBytes(rng, 100)
	}
	return r
}

// equal compares records by content: an empty slice equals a nil slice.
func equal(a, b Record) bool {
	return a.Seq == b.Seq && a.Kind == b.Kind &&
		bytes.Equal(a.Key, b.Key) && bytes.Equal(a.Value, b.Value)
}

// 1. Round trip: 10,000 random records, including empty keys, empty values
// and tombstones.
func TestRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) // fixed seed: failures reproduce
	for i := 0; i < 10_000; i++ {
		want := randomRecord(rng)
		buf := Encode(nil, want)
		got, n, err := Decode(buf)
		if err != nil || n != len(buf) || !equal(got, want) {
			t.Fatalf("record %d: got %+v, %d, %v; want %+v", i, got, n, err, want)
		}
	}
}

// 2. Flip every bit of an encoded record, one at a time. None may decode.
func TestEveryBitFlipIsCaught(t *testing.T) {
	buf := Encode(nil, Record{Seq: 7, Key: []byte("user:7"), Value: []byte("ann")})
	for i := range buf {
		for bit := 0; bit < 8; bit++ {
			bad := bytes.Clone(buf)
			bad[i] ^= 1 << bit
			if _, _, err := Decode(bad); err == nil {
				t.Fatalf("flip byte %d bit %d: decoded without error", i, bit)
			}
		}
	}
}

// 3. Every truncation is ErrShortBuffer, never a record and never a panic.
func TestEveryTruncationIsShort(t *testing.T) {
	buf := Encode(nil, Record{Seq: 7, Key: []byte("user:7"), Value: []byte("ann")})
	for n := 0; n < len(buf); n++ {
		if _, _, err := Decode(buf[:n]); !errors.Is(err, ErrShortBuffer) {
			t.Fatalf("truncated to %d: got %v, want ErrShortBuffer", n, err)
		}
	}
}

// 4. The format is pinned: these bytes are the hex dump from the design doc.
func TestGoldenBytes(t *testing.T) {
	got := Encode(nil, Record{Seq: 1, Kind: KindPut, Key: []byte("a"), Value: []byte("b")})
	want, _ := hex.DecodeString("e26d0d3b" + "0100000000000000" + "00" + "01000000" + "01000000" + "61" + "62")
	if !bytes.Equal(got, want) {
		t.Fatalf("the on-disk format changed!\n got %x\nwant %x", got, want)
	}
}

// 4b. The tombstone dump from the design doc: Delete("a") at seq 2.
func TestGoldenTombstone(t *testing.T) {
	got := Encode(nil, Record{Seq: 2, Kind: KindDelete, Key: []byte("a")})
	want, _ := hex.DecodeString("fe74327b" + "0200000000000000" + "01" + "01000000" + "00000000" + "61")
	if !bytes.Equal(got, want) {
		t.Fatalf("the tombstone format changed!\n got %x\nwant %x", got, want)
	}
}

// 5. Fuzz: Decode must never panic, and anything it accepts must re-encode
// to the same bytes.
func FuzzDecode(f *testing.F) {
	f.Add(Encode(nil, Record{Seq: 1, Key: []byte("k"), Value: []byte("v")}))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		r, n, err := Decode(data)
		if err != nil {
			return
		}
		if again := Encode(nil, r); !bytes.Equal(again, data[:n]) {
			t.Fatalf("decode then encode changed the bytes")
		}
	})
}

// 6. Decode copies the key and value out: the caller may reuse buf.
func TestDecodeCopiesOut(t *testing.T) {
	buf := Encode(nil, Record{Seq: 1, Key: []byte("key"), Value: []byte("value")})
	r, _, err := Decode(buf)
	if err != nil {
		t.Fatal(err)
	}
	for i := range buf {
		buf[i] = 'X' // the caller reuses its buffer
	}
	if string(r.Key) != "key" || string(r.Value) != "value" {
		t.Fatalf("Decode returned windows onto buf: key %q, value %q", r.Key, r.Value)
	}
}

// 7. Some file systems leave a run of zeros after a crash. 64 zero bytes
// must never decode as a record.
func TestZeroBufferIsNotARecord(t *testing.T) {
	if _, _, err := Decode(make([]byte, 64)); err == nil {
		t.Fatal("64 zero bytes decoded as a record")
	}
}

// 8. Encode appends: a second record lands after the first without touching
// it, and Decode walks them one after the other.
func TestEncodeAppendsAndDecodeWalks(t *testing.T) {
	first := Record{Seq: 1, Key: []byte("a"), Value: []byte("b")}
	second := Record{Seq: 2, Kind: KindDelete, Key: []byte("a")}
	buf := Encode(nil, first)
	firstLen := len(buf)
	buf = Encode(buf, second)

	got1, n1, err := Decode(buf)
	if err != nil || n1 != firstLen || !equal(got1, first) {
		t.Fatalf("first record: got %+v, %d, %v", got1, n1, err)
	}
	got2, n2, err := Decode(buf[n1:])
	if err != nil || n1+n2 != len(buf) || !equal(got2, second) {
		t.Fatalf("second record: got %+v, %d, %v", got2, n2, err)
	}
}

// 9. PeekLen rejects impossible sizes before anything is allocated.
func TestPeekLenRejectsHugeLengths(t *testing.T) {
	hdr := Encode(nil, Record{Seq: 1, Key: []byte("k"), Value: []byte("v")})[:HeaderSize]
	hdr[20] = 0xFF // valLen becomes about 4 GB
	if _, err := PeekLen(hdr); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}
