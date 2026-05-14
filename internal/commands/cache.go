package commands

import (
	"container/list"
	"sync"
)

// CachedResult is what a duplicate commandId replays: the dispatcher re-emits
// the saved status/output/errorMessage instead of re-executing the handler.
//
// This is the agent half of the idempotency contract. Backend dedupes by
// commandId on the receiving side too — both ends drop duplicates.
type CachedResult struct {
	Status       string
	Output       any
	ErrorMessage string
	DurationMs   int64
}

// ResultCache is a bounded LRU keyed by commandId. The capacity (256) is
// enough to cover a reconnect storm where the backend re-flushes a queue;
// commands beyond that fall out and would re-execute on a duplicate (which
// is a remote scenario we accept).
type ResultCache struct {
	mu       sync.Mutex
	capacity int
	order    *list.List // front = most recent
	entries  map[string]*list.Element
}

type cacheEntry struct {
	id     string
	result CachedResult
}

// NewResultCache returns an empty cache. capacity must be > 0.
func NewResultCache(capacity int) *ResultCache {
	if capacity <= 0 {
		capacity = 1
	}
	return &ResultCache{
		capacity: capacity,
		order:    list.New(),
		entries:  map[string]*list.Element{},
	}
}

// Get returns the cached result if present, promoting it to MRU.
func (c *ResultCache) Get(commandID string) (CachedResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[commandID]
	if !ok {
		return CachedResult{}, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*cacheEntry).result, true
}

// Put stores result under commandID, evicting the least-recently-used entry
// if we're already at capacity.
func (c *ResultCache) Put(commandID string, result CachedResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[commandID]; ok {
		el.Value.(*cacheEntry).result = result
		c.order.MoveToFront(el)
		return
	}
	if c.order.Len() >= c.capacity {
		oldest := c.order.Back()
		if oldest != nil {
			c.order.Remove(oldest)
			delete(c.entries, oldest.Value.(*cacheEntry).id)
		}
	}
	el := c.order.PushFront(&cacheEntry{id: commandID, result: result})
	c.entries[commandID] = el
}

// Len returns the current number of cached entries (useful in tests).
func (c *ResultCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
