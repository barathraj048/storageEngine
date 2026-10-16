package demo

import "errors"

var TooLargeError = errors.New("data too large")

const (
	MaxKeySize   = 32 << 10 //32 KB
	MaxValueSize = 16 << 20 //16 MB
)

func ValidateKeySize(key, val []byte) error {
	if len(key) > MaxKeySize || len(val) > MaxValueSize {
		return TooLargeError
	}
	return nil
}
