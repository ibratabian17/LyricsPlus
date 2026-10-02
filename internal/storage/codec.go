package storage

import (
	"bytes"
	"compress/zlib"
	"io"
	"sync"
)

const (
	contentRaw        byte = 0x00
	contentCompressed byte = 0x01
)

var decompressBufs = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

func CompressContent(data []byte) []byte {
	if len(data) < 64 {
		return append([]byte{contentRaw}, data...)
	}
	var buf bytes.Buffer
	buf.Grow(len(data)/2 + 32)
	buf.WriteByte(contentCompressed)
	zw, _ := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
	_, _ = zw.Write(data)
	_ = zw.Close()
	if buf.Len() >= len(data)+2 {
		return append([]byte{contentRaw}, data...)
	}
	return buf.Bytes()
}

func DecompressContent(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	switch data[0] {
	case contentCompressed:
		zr, err := zlib.NewReader(bytes.NewReader(data[1:]))
		if err != nil {
			return data
		}
		scratch, _ := decompressBufs.Get().(*bytes.Buffer)
		if scratch == nil {
			scratch = new(bytes.Buffer)
		}
		scratch.Reset()
		_, readErr := io.Copy(scratch, zr)

		out := make([]byte, scratch.Len())
		copy(out, scratch.Bytes())

		scratch.Reset()
		decompressBufs.Put(scratch)
		_ = zr.Close()

		if readErr != nil && len(out) == 0 {
			return data
		}
		return out
	case contentRaw:
		return data[1:]
	default:
		return data
	}
}
