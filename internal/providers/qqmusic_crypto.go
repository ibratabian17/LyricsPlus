package providers

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/des"
	"encoding/hex"
	"fmt"
	"io"
)

// qqKey is the 24-byte 3DES key used for QRC decryption.
var qqKey = []byte("!@#)(*$%123ZXC!@!@#)(NHL")

// DecryptQRC decodes the hex payload, decrypts 3DES-EDE3/ECB, and zlib-deflates it.
func DecryptQRC(hexString string) (string, error) {
	cipherBytes, err := hex.DecodeString(hexString)
	if err != nil {
		return "", fmt.Errorf("hex decode error: %w", err)
	}
	block, err := des.NewTripleDESCipher(qqKey)
	if err != nil {
		return "", fmt.Errorf("3des init error: %w", err)
	}
	blockSize := block.BlockSize()
	if len(cipherBytes)%blockSize != 0 {
		return "", fmt.Errorf("invalid ciphertext length: %d", len(cipherBytes))
	}

	plainBytes := make([]byte, len(cipherBytes))
	for i := 0; i < len(cipherBytes); i += blockSize {
		block.Decrypt(plainBytes[i:i+blockSize], cipherBytes[i:i+blockSize])
	}

	// Standard zlib with raw-deflate fallback.
	if zr, err := zlib.NewReader(bytes.NewReader(plainBytes)); err == nil {
		defer func() { _ = zr.Close() }()
		if data, err := io.ReadAll(zr); err == nil {
			return string(data), nil
		}
	}
	fr := flate.NewReader(bytes.NewReader(plainBytes))
	defer func() { _ = fr.Close() }()
	data, err := io.ReadAll(fr)
	if err != nil {
		return "", fmt.Errorf("decompression error: %w", err)
	}
	return string(data), nil
}
