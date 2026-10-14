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

var compressWriters = sync.Pool{
	New: func() any {
		zw, _ := zlib.NewWriterLevel(io.Discard, zlib.BestSpeed)
		return zw
	},
}

var decompressReaders = sync.Pool{}

func CompressContent(data []byte) []byte {
	if len(data) < 64 {
		out := make([]byte, len(data)+1)
		out[0] = contentRaw
		copy(out[1:], data)
		return out
	}
	scratch, _ := decompressBufs.Get().(*bytes.Buffer)
	if scratch == nil {
		scratch = new(bytes.Buffer)
	}
	scratch.Reset()
	scratch.Grow(len(data)/2 + 32)
	scratch.WriteByte(contentCompressed)

	zw, _ := compressWriters.Get().(*zlib.Writer)
	if zw == nil {
		zw, _ = zlib.NewWriterLevel(scratch, zlib.BestSpeed)
	} else {
		zw.Reset(scratch)
	}
	_, _ = zw.Write(data)
	_ = zw.Close()
	compressWriters.Put(zw)

	if scratch.Len() >= len(data)+2 {
		scratch.Reset()
		decompressBufs.Put(scratch)
		out := make([]byte, len(data)+1)
		out[0] = contentRaw
		copy(out[1:], data)
		return out
	}
	out := make([]byte, scratch.Len())
	copy(out, scratch.Bytes())
	scratch.Reset()
	decompressBufs.Put(scratch)
	return out
}

func DecompressContent(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	switch data[0] {
	case contentCompressed:
		br := bytes.NewReader(data[1:])
		var zr io.ReadCloser
		var err error
		if pooled := decompressReaders.Get(); pooled != nil {
			zr = pooled.(io.ReadCloser)
			err = zr.(zlib.Resetter).Reset(br, nil)
		} else {
			zr, err = zlib.NewReader(br)
		}
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
		decompressReaders.Put(zr)

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
