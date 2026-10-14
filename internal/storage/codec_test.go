package storage

import (
	"bytes"
	"testing"
)

func TestCompressDecompressRoundtrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"tiny", []byte("hi")},
		{"json", bytes.Repeat([]byte(`{"title":"test","artist":"artist","lines":["a","b","c"]}`), 500)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enc := CompressContent(tc.in)
			dec := DecompressContent(enc)
			if !bytes.Equal(dec, tc.in) {
				t.Fatalf("roundtrip mismatch: got %d bytes, want %d", len(dec), len(tc.in))
			}
			if len(tc.in) > 64 && len(enc) >= len(tc.in) {
				t.Fatalf("repetitive payload did not compress: %d -> %d bytes", len(tc.in), len(enc))
			}
		})
	}
}

func TestDecompressLegacyRaw(t *testing.T) {
	raw := []byte(`{"title":"old"}`)
	if got := DecompressContent(raw); !bytes.Equal(got, raw) {
		t.Fatalf("legacy raw passthrough failed: %q", got)
	}
}
