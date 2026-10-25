package storage

import (
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"

	"lyricsplus/backend/internal/config"
)

const defaultCacheEntryTTL = 30 * time.Minute

type CacheEntry struct {
	Body     []byte
	Parsed   any
	Header   map[string][]string
	Status   int
	Size     int64
	StoredAt time.Time
	TTL      time.Duration
}

func entrySize(e *CacheEntry) int64 {
	if e == nil {
		return 0
	}
	if e.Size > 0 {
		return e.Size
	}
	return int64(len(e.Body))
}

type MemoryCache struct {
	mu       sync.RWMutex
	entries  *lru.Cache[string, *CacheEntry]
	bytes    int64
	maxKeys  int
	maxBytes int64
	maxBody  int64
}

func NewMemoryCache(cfg config.Cache) *MemoryCache {
	maxKeys := cfg.MaxEntries
	if maxKeys <= 0 {
		maxKeys = 4096
	}
	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 64 << 20
	}
	maxBody := cfg.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = 1 << 20 * 3 / 2
	}
	e, _ := lru.New[string, *CacheEntry](maxKeys)
	return &MemoryCache{
		entries:  e,
		maxKeys:  maxKeys,
		maxBytes: maxBytes,
		maxBody:  maxBody,
	}
}

func (c *MemoryCache) Get(key string) (*CacheEntry, bool) {
	if c == nil || c.entries == nil {
		return nil, false
	}
	c.mu.Lock()
	e, ok := c.entries.Get(key)
	if !ok || e == nil {
		c.mu.Unlock()
		return nil, false
	}
	ttl := e.TTL
	if ttl <= 0 {
		ttl = defaultCacheEntryTTL
	}
	if !e.StoredAt.IsZero() && time.Since(e.StoredAt) > ttl {
		c.bytes -= entrySize(e)
		if c.bytes < 0 {
			c.bytes = 0
		}
		c.entries.Remove(key)
		c.mu.Unlock()
		return nil, false
	}
	c.mu.Unlock()
	return e, true
}

func (c *MemoryCache) Set(key string, entry *CacheEntry) {
	if c == nil || c.entries == nil || entry == nil {
		return
	}
	bodyLen := entrySize(entry)
	if c.maxBody > 0 && bodyLen > c.maxBody {
		return
	}
	if entry.StoredAt.IsZero() {
		entry.StoredAt = time.Now()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if prev, ok := c.entries.Peek(key); ok && prev != nil {
		c.bytes -= entrySize(prev)
		c.entries.Remove(key)
	}
	if c.maxBytes > 0 {
		for c.bytes+bodyLen > c.maxBytes && c.entries.Len() > 0 {
			if _, v, ok := c.entries.RemoveOldest(); ok && v != nil {
				c.bytes -= entrySize(v)
			}
		}
	}
	if c.entries.Len() >= c.maxKeys && c.entries.Len() > 0 {
		if _, v, ok := c.entries.RemoveOldest(); ok && v != nil {
			c.bytes -= entrySize(v)
		}
	}
	if c.bytes < 0 {
		c.bytes = 0
	}
	c.entries.Add(key, entry)
	c.bytes += bodyLen
}

func (c *MemoryCache) Len() int {
	if c == nil || c.entries == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.entries.Len()
}

func (c *MemoryCache) Shed() {
	if c == nil || c.entries == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for _, k := range c.entries.Keys() {
		if v, ok := c.entries.Peek(k); ok && v != nil {
			ttl := v.TTL
			if ttl <= 0 {
				ttl = defaultCacheEntryTTL
			}
			if !v.StoredAt.IsZero() && now.Sub(v.StoredAt) > ttl {
				c.bytes -= entrySize(v)
				c.entries.Remove(k)
			}
		}
	}
	toRemove := c.entries.Len() / 2
	if toRemove == 0 {
		toRemove = c.entries.Len()
	}
	for i := 0; i < toRemove; i++ {
		if _, v, ok := c.entries.RemoveOldest(); ok && v != nil {
			c.bytes -= entrySize(v)
		}
	}
	if c.bytes < 0 {
		c.bytes = 0
	}
}
