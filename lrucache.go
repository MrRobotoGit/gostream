package main

import (
	"container/list"
	"hash/fnv"
	"sync"
	"time"
)

// ShardedLRUCache Wrapper per ridurre la contesa sui lock (V231-Fix)
// Splits the cache into 32 shards to minimize mutex contention during parallel accesses.
type LRUCache struct {
	shards    []*simpleLRUCache
	shardMask uint64
}

const numShards = 32

func NewLRUCache(capacity int64, ttl time.Duration) *LRUCache {
	shards := make([]*simpleLRUCache, numShards)
	shardCap := capacity / numShards
	if shardCap == 0 {
		shardCap = 1024 * 1024 // min 1MB per shard
	}

	for i := 0; i < numShards; i++ {
		shards[i] = newSimpleLRUCache(shardCap, ttl)
	}

	return &LRUCache{
		shards:    shards,
		shardMask: numShards - 1,
	}
}

func (c *LRUCache) getShard(key string) *simpleLRUCache {
	h := fnv.New64a()
	h.Write([]byte(key))
	return c.shards[h.Sum64()&c.shardMask]
}

func (c *LRUCache) Get(key string) (*Metadata, bool) {
	return c.getShard(key).Get(key)
}

func (c *LRUCache) Put(key string, value *Metadata, size int64) {
	c.getShard(key).Put(key, value, size)
}

func (c *LRUCache) Delete(key string) {
	c.getShard(key).Delete(key)
}

func (c *LRUCache) Len() int {
	total := 0
	for _, s := range c.shards {
		total += s.Len()
	}
	return total
}

func (c *LRUCache) Clear() {
	for _, s := range c.shards {
		s.Clear()
	}
}

func (c *LRUCache) CleanupExpired() int {
	total := 0
	for _, s := range c.shards {
		total += s.CleanupExpired()
	}
	return total
}

// Stats returns aggregated stats
func (c *LRUCache) Stats() CacheStats {
	var totalSize, totalCap int64
	var totalEntries int
	for _, s := range c.shards {
		st := s.Stats()
		totalSize += st.Size
		totalCap += st.Capacity
		totalEntries += st.Entries
	}
	return CacheStats{
		Size:     totalSize,
		Capacity: totalCap,
		Entries:  totalEntries,
	}
}

// simpleLRUCache implements the actual LRU logic (formerly LRUCache)
type simpleLRUCache struct {
	capacity    int64
	currentSize int64
	ttl         time.Duration
	items       map[string]*list.Element
	order       *list.List
	mu          sync.RWMutex
}

// newSimpleLRUCache creates a new simple LRU cache
func newSimpleLRUCache(capacity int64, ttl time.Duration) *simpleLRUCache {
	if capacity == 0 {
		capacity = 500 * 1024 * 1024
	}
	if ttl == 0 {
		ttl = 24 * time.Hour
	}

	return &simpleLRUCache{
		capacity: capacity,
		ttl:      ttl,
		items:    make(map[string]*list.Element),
		order:    list.New(),
	}
}

// Get retrieves a value from the cache and marks it as recently used
func (c *simpleLRUCache) Get(key string) (*Metadata, bool) {
	// Optimize: Use Read Lock for most accesses
	c.mu.RLock()
	elem, exists := c.items[key]
	if !exists {
		c.mu.RUnlock()
		return nil, false
	}

	entry := elem.Value.(*cacheEntry)

	// Check if entry has expired
	if time.Now().After(entry.expiresAt) {
		c.mu.RUnlock()
		// Determine removal requires Write Lock
		c.mu.Lock()
		// Double check
		if elem, exists := c.items[key]; exists {
			c.removeElement(elem)
		}
		c.mu.Unlock()
		return nil, false
	}

	// Lazy Promotion: Only promote if >10s have passed since last promotion
	// This avoids "thundering herd" on Write Lock for hot items
	if time.Since(entry.lastPromoted) > 10*time.Second {
		c.mu.RUnlock()
		c.mu.Lock()
		// Double check existence
		if elem, exists := c.items[key]; exists {
			c.order.MoveToFront(elem)
			elem.Value.(*cacheEntry).lastPromoted = time.Now()
		}
		c.mu.Unlock()
	} else {
		c.mu.RUnlock()
	}

	return entry.value, true
}

func (c *simpleLRUCache) Put(key string, value *Metadata, size int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, exists := c.items[key]; exists {
		entry := elem.Value.(*cacheEntry)
		c.currentSize -= entry.size
		c.currentSize += size
		entry.value = value
		entry.size = size
		entry.expiresAt = time.Now().Add(c.ttl)
		entry.lastPromoted = time.Now()
		c.order.MoveToFront(elem)
		return
	}

	for c.currentSize+size > c.capacity && c.order.Len() > 0 {
		oldest := c.order.Back()
		if oldest != nil {
			c.removeElement(oldest)
		}
	}

	entry := &cacheEntry{
		key:          key,
		value:        value,
		size:         size,
		expiresAt:    time.Now().Add(c.ttl),
		lastPromoted: time.Now(),
	}

	elem := c.order.PushFront(entry)
	c.items[key] = elem
	c.currentSize += size
}

func (c *simpleLRUCache) removeElement(elem *list.Element) {
	if elem == nil {
		return
	}
	entry := elem.Value.(*cacheEntry)
	delete(c.items, entry.key)
	c.order.Remove(elem)
	c.currentSize -= entry.size
}

func (c *simpleLRUCache) CleanupExpired() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	removed := 0
	var toRemove []*list.Element

	for elem := c.order.Front(); elem != nil; elem = elem.Next() {
		entry := elem.Value.(*cacheEntry)
		if now.After(entry.expiresAt) {
			toRemove = append(toRemove, elem)
		}
	}

	for _, elem := range toRemove {
		c.removeElement(elem)
		removed++
	}

	return removed
}

func (c *simpleLRUCache) Stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return CacheStats{
		Size:     c.currentSize,
		Capacity: c.capacity,
		Entries:  c.order.Len(),
	}
}

func (c *simpleLRUCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.order.Len()
}

func (c *simpleLRUCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, exists := c.items[key]; exists {
		c.removeElement(elem)
	}
}

func (c *simpleLRUCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[string]*list.Element)
	c.order.Init()
	c.currentSize = 0
}

// cacheEntry represents a single cache entry with metadata and TTL
type cacheEntry struct {
	key          string
	value        *Metadata
	size         int64     // Approximate size in bytes
	expiresAt    time.Time // TTL expiration time
	lastPromoted time.Time // V239-Optimization: Lazy Promotion timestamp
}

// CacheStats holds aggregated cache statistics
type CacheStats struct {
	Size     int64
	Capacity int64
	Entries  int
}
