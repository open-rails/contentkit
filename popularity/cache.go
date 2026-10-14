package popularity

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Cache memoizes global rankings. Optional: without one every read hits the
// signal plane. Keys carry the tenant, policy name, content kind, window and
// bounds, so two policies sharing one cache never read each other's entries.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration)
}

// DefaultMemoryCacheBytes is the budget of a MemoryCache built with none.
const DefaultMemoryCacheBytes = 32 << 20

// MemoryCache is a process-local Cache with per-entry expiry and a byte
// budget (keys plus values). Past the budget the least recently used entries
// are evicted, so callers choosing keys can never grow it without bound.
type MemoryCache struct {
	mu       sync.Mutex
	maxBytes int
	bytes    int
	lru      *list.List // front is most recently used
	entries  map[string]*list.Element
}

type memoryEntry struct {
	key     string
	value   []byte
	expires time.Time
}

// NewMemoryCache returns an empty MemoryCache holding at most maxBytes;
// maxBytes <= 0 means DefaultMemoryCacheBytes.
func NewMemoryCache(maxBytes int) *MemoryCache {
	if maxBytes <= 0 {
		maxBytes = DefaultMemoryCacheBytes
	}
	return &MemoryCache{maxBytes: maxBytes, lru: list.New(), entries: map[string]*list.Element{}}
}

func (c *MemoryCache) Get(_ context.Context, key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*memoryEntry)
	if !e.expires.IsZero() && !time.Now().Before(e.expires) {
		c.remove(el)
		return nil, false
	}
	c.lru.MoveToFront(el)
	return e.value, true
}

// Set stores value; ttl <= 0 never expires. A value larger than the whole
// budget is not stored.
func (c *MemoryCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		c.remove(el)
	}
	size := len(key) + len(value)
	if size > c.maxBytes {
		return
	}
	e := &memoryEntry{key: key, value: value}
	if ttl > 0 {
		e.expires = time.Now().Add(ttl)
	}
	c.entries[key] = c.lru.PushFront(e)
	c.bytes += size
	for c.bytes > c.maxBytes {
		c.remove(c.lru.Back())
	}
}

// Len is the number of stored entries, expired ones included until touched.
func (c *MemoryCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// Bytes is the stored key and value bytes; never more than the budget.
func (c *MemoryCache) Bytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

func (c *MemoryCache) remove(el *list.Element) {
	e := c.lru.Remove(el).(*memoryEntry)
	delete(c.entries, e.key)
	c.bytes -= len(e.key) + len(e.value)
}
