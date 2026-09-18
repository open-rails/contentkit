package popularity

import (
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

// MemoryCache is a process-local Cache with per-entry expiry.
type MemoryCache struct {
	mu      sync.Mutex
	entries map[string]memoryEntry
}

type memoryEntry struct {
	value   []byte
	expires time.Time
}

// NewMemoryCache returns an empty MemoryCache.
func NewMemoryCache() *MemoryCache {
	return &MemoryCache{entries: map[string]memoryEntry{}}
}

func (c *MemoryCache) Get(_ context.Context, key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if !e.expires.IsZero() && !time.Now().Before(e.expires) {
		delete(c.entries, key)
		return nil, false
	}
	return e.value, true
}

// Set stores value; ttl <= 0 never expires.
func (c *MemoryCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := memoryEntry{value: value}
	if ttl > 0 {
		e.expires = time.Now().Add(ttl)
	}
	c.entries[key] = e
}
