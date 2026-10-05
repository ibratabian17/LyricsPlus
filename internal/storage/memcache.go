package storage

import (
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"

	"lyricsplus/backend/internal/config"
)

type CacheEntry struct {
	Body     []byte
	Header   map[string][]string
	Status   int
	StoredAt time.Time
	TTL      time.Duration
}

type MemoryCache struct {
	mu       sync.Mutex
	entries  *lru.Cache[string, *CacheEntry]
	bytes    int64
	maxKeys  int
	maxBytes int64
	maxBody  int64
}

func NewMemoryCache(cfg config.Cache) *MemoryCache {
	e, _ := lru.New[string, *CacheEntry](cfg.MaxEntries)
	return &MemoryCache{
		entries:  e,
		maxKeys:  cfg.MaxEntries,
		maxBytes: cfg.MaxBytes,
		maxBody:  cfg.MaxBodyBytes,
	}
}

func (c *MemoryCache) Get(key string) (*CacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries.Get(key)
	if !ok {
		return nil, false
	}
	if e.TTL > 0 && time.Since(e.StoredAt) > e.TTL {
		c.bytes -= int64(len(e.Body))
		c.entries.Remove(key)
		return nil, false
	}
	return e, true
}

func (c *MemoryCache) Set(key string, entry *CacheEntry) {
	if int64(len(entry.Body)) > c.maxBody {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if prev, ok := c.entries.Get(key); ok {
		c.bytes -= int64(len(prev.Body))
		c.entries.Remove(key)
	}
	for c.bytes+int64(len(entry.Body)) > c.maxBytes && c.entries.Len() > 0 {
		if _, v, ok := c.entries.RemoveOldest(); ok {
			c.bytes -= int64(len(v.Body))
		}
	}
	c.entries.Add(key, entry)
	c.bytes += int64(len(entry.Body))
}

func (c *MemoryCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries.Len()
}

func (c *MemoryCache) Shed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	toRemove := c.entries.Len() / 2
	if toRemove == 0 {
		toRemove = c.entries.Len()
	}
	for i := 0; i < toRemove; i++ {
		if _, v, ok := c.entries.RemoveOldest(); ok && v != nil {
			c.bytes -= int64(len(v.Body))
		}
	}
	if c.bytes < 0 {
		c.bytes = 0
	}
}
