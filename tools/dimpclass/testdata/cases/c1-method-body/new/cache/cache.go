package cache

import (
	"context"
	"sync"
	"time"
)

type entry struct {
	val     []byte
	expires time.Time
}

// Cache is a TTL cache shared by all handlers.
type Cache struct {
	mu    sync.Mutex
	items map[string]entry
	ttl   time.Duration
}

func New(ttl time.Duration) *Cache { return &Cache{items: map[string]entry{}, ttl: ttl} }

// Get returns a live entry.
func (c *Cache) Get(ctx context.Context, k string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[k]
	if !ok || time.Now().After(e.expires) {
		delete(c.items, k)
		return nil, false
	}
	return e.val, true
}

func (c *Cache) Put(ctx context.Context, k string, v []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[k] = entry{val: v, expires: time.Now().Add(c.ttl)}
}
