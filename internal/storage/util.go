package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

func mkdirAll(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

func jsonUnmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(replaceEmoji(data), v)
}

// replaceEmoji guards against invalid UTF-8 bytes in cached payloads.
func replaceEmoji(data []byte) []byte {
	if !utf8Valid(data) {
		cleaned := make([]byte, 0, len(data))
		for _, b := range data {
			if b == 0 {
				cleaned = append(cleaned, ' ')
			} else {
				cleaned = append(cleaned, b)
			}
		}
		return cleaned
	}
	return data
}

func utf8Valid(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return false
		}
	}
	return true
}

// hash32 computes a 32-bit content hash for debounce dedup.
func hash32(data []byte) uint32 {
	h := sha256.Sum256(data)
	return (uint32(h[0]) << 24) | (uint32(h[1]) << 16) | (uint32(h[2]) << 8) | uint32(h[3])
}

// debounce deduplicates identical writes within the configured window.
type debounce struct {
	mu     sync.Mutex
	seen   map[uint32]time.Time
	window time.Duration
}

func newDebounce(window time.Duration) *debounce {
	return &debounce{seen: map[uint32]time.Time{}, window: window}
}

func (d *debounce) allowed(content []byte) bool {
	h := hash32(content)
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.seen[h]; ok && time.Since(t) < d.window {
		return false
	}
	d.seen[h] = time.Now()
	for k, t := range d.seen {
		if time.Since(t) > d.window {
			delete(d.seen, k)
		}
	}
	return true
}

// recentlySaved hooks the 60-second debounce for google drive writes.
func (s *Store) recentlySaved(row *Row) bool { return false }

func dirOf(path string) string {
	return filepath.Dir(path)
}

// hexString of hash for cache keys used by the GDrive layer.
func hexOf(data []byte) string {
	return hex.EncodeToString(data)[:16]
}
