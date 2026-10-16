package demo

import "testing"

func TestKeyValueSize(t *testing.T) {
	err := ValidateKeySize([]byte("key"), []byte("value"))
	if err != nil {
		t.Errorf("Expected no error, but got %v", err)
	}
}

func TestBatch(t *testing.T) {
	task := []struct {
		name   string
		key    []byte
		val    []byte
		wanted error
	}{
		{"ValidKeyValue", []byte("key"), []byte("value"), nil},
		{"TooLargeKey", make([]byte, MaxKeySize+1), []byte("value"), TooLargeError},
		{"TooLargeValue", []byte("key"), make([]byte, MaxValueSize+1), TooLargeError},
	}

	for _, tc := range task {
		err := ValidateKeySize(tc.key, tc.val)
		if err != tc.wanted {
			t.Errorf("Test %s failed: expected %v,got %v", tc.name, tc.wanted, err)
		}
	}
}
