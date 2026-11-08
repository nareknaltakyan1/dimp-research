package cache

import (
	"sync"
	"time"
)

// Entry is one cached HTTP response.
type Entry struct {
	Body    []byte        `json:"body"`
	Status  int           `json:"status"`
	TTLSecs time.Duration `json:"ttl"`
	Created time.Time
	Hits    int64 `json:"hits"`
}

// Cache is the process-wide response cache.
type Cache struct {
	mu    sync.RWMutex
	items map[string]*Entry
}

var Default = &Cache{items: map[string]*Entry{}}

func (c *Cache) Get(k string) (*Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.items[k]
	if !ok || time.Since(e.Created) > e.TTLSecs {
		return nil, false
	}
	e.Hits++
	return e, true
}
