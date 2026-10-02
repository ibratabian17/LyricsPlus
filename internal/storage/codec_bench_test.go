package storage

import "testing"

// realisticPayload approximates a stored LyricsResponse: mostly repetitive
// lyric text, which is the compression-friendly shape of real content.
func realisticPayload() []byte {
	buf := make([]byte, 0, 96<<10)
	for i := 0; len(buf) < 96<<10; i++ {
		buf = append(buf, `{"time":`...)
		buf = append(buf, []byte("1234567890123")[:4+i%9]...)
		buf = append(buf, []byte(`,"text":"Is this the real life? Is this just fantasy"}`)...)
	}
	return buf
}

func BenchmarkCompressContent(b *testing.B) {
	data := realisticPayload()
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = CompressContent(data)
	}
}

func BenchmarkDecompressContent(b *testing.B) {
	blob := CompressContent(realisticPayload())
	b.SetBytes(int64(len(blob)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = DecompressContent(blob)
	}
}
