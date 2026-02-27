package main

import (
	"sync"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// DirCacheEntry holds cached directory entries
type DirCacheEntry struct {
	Entries   []fuse.DirEntry
	ExpiresAt time.Time
}

// DirCache provides a thread-safe cache for directory listings
type DirCache struct {
	cache map[string]DirCacheEntry
	mu    sync.RWMutex
	ttl   time.Duration
}

// NewDirCache creates a new directory cache
func NewDirCache(ttl time.Duration) *DirCache {
	return &DirCache{
		cache: make(map[string]DirCacheEntry),
		ttl:   ttl,
	}
}

// Get retrieves entries for a path if they exist and aren't expired
func (dc *DirCache) Get(path string) ([]fuse.DirEntry, bool) {
	dc.mu.RLock()
	entry, exists := dc.cache[path]
	dc.mu.RUnlock()

	if !exists {
		return nil, false
	}

	if time.Now().After(entry.ExpiresAt) {
		// Lazy cleanup
		dc.Delete(path)
		return nil, false
	}

	return entry.Entries, true
}

// Put stores entries for a path
func (dc *DirCache) Put(path string, entries []fuse.DirEntry) {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	// Make a copy to prevent mutation issues (though DirEntry is value type, slice is ref)
	entriesCopy := make([]fuse.DirEntry, len(entries))
	copy(entriesCopy, entries)

	dc.cache[path] = DirCacheEntry{
		Entries:   entriesCopy,
		ExpiresAt: time.Now().Add(dc.ttl),
	}
}

// Delete removes an entry (used for invalidation)
func (dc *DirCache) Delete(path string) {
	dc.mu.Lock()
	delete(dc.cache, path)
	dc.mu.Unlock()
}

// CleanupExpired purges expired entries (can be called periodically)
func (dc *DirCache) CleanupExpired() {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	now := time.Now()
	for path, entry := range dc.cache {
		if now.After(entry.ExpiresAt) {
			delete(dc.cache, path)
		}
	}
}

// Global instance
var globalDirCache *DirCache
