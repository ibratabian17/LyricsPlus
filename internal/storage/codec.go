package storage

import (
	"bytes"
	"compress/gzip"
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

var gzipReaders = sync.Pool{}

func isRawZlibHeader(data []byte) bool {
	if len(data) < 2 || data[0] != 0x78 {
		return false
	}
	return data[1] == 0x01 || data[1] == 0x5e || data[1] == 0x9c || data[1] == 0xda
}

func isGzipHeader(data []byte) bool {
	return len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b
}

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
	switch {
	case data[0] == contentCompressed:
		return decompressZlibPayload(data[1:], data)
	case data[0] == contentRaw:
		return data[1:]
	case isGzipHeader(data):
		return decompressGzipPayload(data)
	case isRawZlibHeader(data):
		return decompressZlibPayload(data, data)
	default:
		return data
	}
}

func decompressZlibPayload(compressed, fallback []byte) []byte {
	br := bytes.NewReader(compressed)
	var zr io.ReadCloser
	var err error
	if pooled := decompressReaders.Get(); pooled != nil {
		zr = pooled.(io.ReadCloser)
		err = zr.(zlib.Resetter).Reset(br, nil)
	} else {
		zr, err = zlib.NewReader(br)
	}
	if err != nil {
		return fallback
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
		return fallback
	}
	return out
}

func decompressGzipPayload(data []byte) []byte {
	br := bytes.NewReader(data)
	var gr *gzip.Reader
	var err error
	if pooled := gzipReaders.Get(); pooled != nil {
		gr = pooled.(*gzip.Reader)
		err = gr.Reset(br)
	} else {
		gr, err = gzip.NewReader(br)
	}
	if err != nil {
		return data
	}
	scratch, _ := decompressBufs.Get().(*bytes.Buffer)
	if scratch == nil {
		scratch = new(bytes.Buffer)
	}
	scratch.Reset()
	_, readErr := io.Copy(scratch, gr)

	out := make([]byte, scratch.Len())
	copy(out, scratch.Bytes())

	scratch.Reset()
	decompressBufs.Put(scratch)
	_ = gr.Close()
	gzipReaders.Put(gr)

	if readErr != nil && len(out) == 0 {
		return data
	}
	return out
}

// DecompressContentPrefix decompresses up to len(dst) bytes of data into dst
// without heap allocations for the output slice. It returns the populated slice
// of dst and a boolean indicating whether the entire decompressed payload fit in dst.
func DecompressContentPrefix(data []byte, dst []byte) ([]byte, bool) {
	if len(data) == 0 || len(dst) == 0 {
		return nil, true
	}
	switch {
	case data[0] == contentCompressed:
		return readZlibPrefix(data[1:], data, dst)
	case data[0] == contentRaw:
		raw := data[1:]
		if len(raw) <= len(dst) {
			return raw, true
		}
		return raw[:len(dst)], false
	case isGzipHeader(data):
		return readGzipPrefix(data, dst)
	case isRawZlibHeader(data):
		return readZlibPrefix(data, data, dst)
	default:
		if len(data) <= len(dst) {
			return data, true
		}
		return data[:len(dst)], false
	}
}

func readZlibPrefix(compressed, fallback, dst []byte) ([]byte, bool) {
	br := bytes.NewReader(compressed)
	var zr io.ReadCloser
	var err error
	if pooled := decompressReaders.Get(); pooled != nil {
		zr = pooled.(io.ReadCloser)
		err = zr.(zlib.Resetter).Reset(br, nil)
	} else {
		zr, err = zlib.NewReader(br)
	}
	if err != nil {
		if len(fallback) <= len(dst) {
			return fallback, true
		}
		return fallback[:len(dst)], false
	}
	prefix, complete, readErr := readPrefixFromReader(zr, dst)
	_ = zr.Close()
	decompressReaders.Put(zr)
	if readErr != nil && len(prefix) == 0 {
		if len(fallback) <= len(dst) {
			return fallback, true
		}
		return fallback[:len(dst)], false
	}
	return prefix, complete
}

func readGzipPrefix(data, dst []byte) ([]byte, bool) {
	br := bytes.NewReader(data)
	var gr *gzip.Reader
	var err error
	if pooled := gzipReaders.Get(); pooled != nil {
		gr = pooled.(*gzip.Reader)
		err = gr.Reset(br)
	} else {
		gr, err = gzip.NewReader(br)
	}
	if err != nil {
		if len(data) <= len(dst) {
			return data, true
		}
		return data[:len(dst)], false
	}
	prefix, complete, readErr := readPrefixFromReader(gr, dst)
	_ = gr.Close()
	gzipReaders.Put(gr)
	if readErr != nil && len(prefix) == 0 {
		if len(data) <= len(dst) {
			return data, true
		}
		return data[:len(dst)], false
	}
	return prefix, complete
}

func readPrefixFromReader(r io.Reader, dst []byte) ([]byte, bool, error) {
	total := 0
	for total < len(dst) {
		n, err := r.Read(dst[total:])
		total += n
		if err != nil {
			if err == io.EOF {
				return dst[:total], true, nil
			}
			return dst[:total], false, err
		}
	}
	var one [1]byte
	n, err := r.Read(one[:])
	if n == 0 && err == io.EOF {
		return dst[:total], true, nil
	}
	return dst[:total], false, nil
}
