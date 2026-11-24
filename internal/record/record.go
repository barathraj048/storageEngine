// Package record encodes and decodes the single unit every kvgo file is
// made of: one checksummed key/value record.
//
// Layout, all integers little-endian:
//
//	crc32c(4) | seq(8) | kind(1) | keyLen(4) | valLen(4) | key | value
//
// The CRC covers every byte after itself, lengths included.
package record

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

// HeaderSize is crc(4) + seq(8) + kind(1) + keyLen(4) + valLen(4).
const HeaderSize = 21

// Size limits. PeekLen enforces them before anything is allocated or read.
const (
	MaxKeySize   = 64 << 10 // 64 KiB
	MaxValueSize = 16 << 20 // 16 MiB
)

// Kind says whether a record is a put or a delete (a tombstone).
type Kind uint8

const (
	KindPut Kind = iota
	KindDelete
)

// Record is one decoded entry. A tombstone has an empty Value.
type Record struct {
	Seq   uint64
	Kind  Kind
	Key   []byte
	Value []byte
}

var (
	ErrShortBuffer = errors.New("record: short buffer")
	ErrChecksum    = errors.New("record: checksum mismatch")
	ErrTooLarge    = errors.New("record: key or value over limit")
)

// castagnoli is built once: MakeTable is not free, and this table is what
// lets the CPU's CRC32 instruction do the work.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Encode appends the encoded record to dst and returns the extended slice.
// Callers must use the returned slice: append may have moved it.
func Encode(dst []byte, r Record) []byte {
	start := len(dst)
	var hdr [HeaderSize]byte // crc left as zero for now
	binary.LittleEndian.PutUint64(hdr[4:12], r.Seq)
	hdr[12] = byte(r.Kind)
	binary.LittleEndian.PutUint32(hdr[13:17], uint32(len(r.Key)))
	binary.LittleEndian.PutUint32(hdr[17:21], uint32(len(r.Value)))
	dst = append(dst, hdr[:]...)
	dst = append(dst, r.Key...)
	dst = append(dst, r.Value...)
	sum := crc32.Checksum(dst[start+4:], castagnoli) // index from start, not 0
	binary.LittleEndian.PutUint32(dst[start:start+4], sum)
	return dst
}

// PeekLen returns the total record length a header announces, after
// checking the lengths against the limits. The header is untrusted until
// the CRC passes, so this runs before anything is allocated or read.
func PeekLen(hdr []byte) (int, error) {
	if len(hdr) < HeaderSize {
		return 0, ErrShortBuffer
	}
	if hdr[12] > byte(KindDelete) {
		return 0, ErrChecksum
	}
	kl := binary.LittleEndian.Uint32(hdr[13:17])
	vl := binary.LittleEndian.Uint32(hdr[17:21])
	if kl > MaxKeySize || vl > MaxValueSize {
		return 0, ErrTooLarge
	}
	return HeaderSize + int(kl) + int(vl), nil
}

// Decode reads one record from the front of buf. It returns the record and
// the number of bytes it used. Key and Value are copies: the caller may
// reuse buf.
func Decode(buf []byte) (Record, int, error) {
	n, err := PeekLen(buf)
	if err != nil {
		return Record{}, 0, err
	}
	if len(buf) < n {
		return Record{}, 0, ErrShortBuffer
	}
	want := binary.LittleEndian.Uint32(buf[0:4])
	if crc32.Checksum(buf[4:n], castagnoli) != want {
		return Record{}, 0, ErrChecksum
	}
	kl := int(binary.LittleEndian.Uint32(buf[13:17]))
	r := Record{
		Seq:   binary.LittleEndian.Uint64(buf[4:12]),
		Kind:  Kind(buf[12]),
		Key:   append([]byte(nil), buf[HeaderSize:HeaderSize+kl]...),
		Value: append([]byte(nil), buf[HeaderSize+kl:n]...),
	}
	return r, n, nil
}
