package record

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"testing"
)

// helper function's
// randBytes returns between 0 and max random bytes.
func randBytes(rng *rand.Rand, max int) []byte {
	b := make([]byte, rng.IntN(max+1))
	for i := range b {
		b[i] = byte(rng.IntN(256))
	}
	return b
}

// randomRecord builds a random put, or one time in five a delete.
func randomRecord(rng *rand.Rand) Record {
	r := Record{Seq: rng.Uint64(), Key: randBytes(rng, 40)}
	if rng.IntN(5) == 0 {
		r.Kind = KindDelete
	} else {
		r.Value = randBytes(rng, 100)
	}
	return r
}

// equal compares two records by content.
func equal(a, b Record) bool {
	return a.Seq == b.Seq && a.Kind == b.Kind &&
		bytes.Equal(a.Key, b.Key) && bytes.Equal(a.Value, b.Value)
}

// round trip test 10,000 random records , include empty key, empty value
func TestRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) //used to reproduce same rand every time
	for i := 0; i < 10_000; i++ {
		want := randomRecord(rng)
		buf := Encode(nil, want)
		got, n, err := Decode(buf)
		if err != nil || n != len(buf) || !equal(got, want) {
			t.Fatalf("record %d: got %+v, %d, %v; want %+v", i, got, n, err, want)
		}
	}
}

// Flip every bit and and test currouption detection works or not
func TestEveryBitFlipIsCause(t *testing.T) {
	buf := Encode(nil, Record{Seq: 7, Key: []byte("user:7"), Value: []byte("ann")})
	for i := range buf {
		for bit := 0; bit < 8; bit++ {
			bad := bytes.Clone(buf)
			bad[i] ^= 1 << bit
			_, _, err := Decode(bad)
			if err == nil {
				t.Fatalf("flip byte %d bit %d : decoded without error", i, bit)
			}
		}
	}
}

// Truncation of bit is detected
func TestEveryTruncationIsShort(t *testing.T) {
	buf := Encode(nil, Record{Seq: 7, Key: []byte("user:7"), Value: []byte("ann")})
	for n := 0; n < len(buf); n++ {
		_, _, err := Decode(buf[:n])
		if !errors.Is(err, ErrShortBuffer) {
			t.Fatalf("truncated to %d:got %v, want ErrShorBuffer", n, err)
		}
	}
}

// formate checker if the encoder and decoder aggries on disk saved formate
func TestGoldenVytes(t *testing.T) {
	got := Encode(nil, Record{Seq: 1, Kind: KindPut, Key: []byte("a"), Value: []byte("b")})
	want, _ := hex.DecodeString("e26d0d3b" + "0100000000000000" + "00" + "01000000" +
		"01000000" + "61" + "62")
	if !bytes.Equal(got, want) {
		t.Fatalf("the on-disk formate changed! \n got %x \n want %x", got, want)
	}
}

// Fuzz Decoder never panic,accepth thing and encode beck to same byte
func FuzzDecode(f *testing.F) {
	f.Add(Encode(nil, Record{Seq: 1, Key: []byte("k"), Value: []byte("v")}))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		r, n, err := Decode(data)
		if err != nil {
			return
		}
		again := Encode(nil, r)
		if !bytes.Equal(again, data[:n]) {
			t.Fatalf("decode then encode changes the byte")
		}
	})
}
