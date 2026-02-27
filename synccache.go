package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SyncCacheManager manages synchronization with Python sync script caches
// Provides atomic JSON file operations to prevent corruption
// V83: In-memory cache to eliminate disk I/O on every access
type SyncCacheManager struct {
	stateDir string
	mu       sync.RWMutex
	logger   *log.Logger

	// V83: In-memory caches (loaded at startup)
	negativeCache map[string]NegativeCacheEntry // Loaded from no_mkv_hashes.json
	fullpackCache map[string]FullpackCacheEntry // Loaded from tv_fullpacks.json
	dirty         bool                           // Track if disk sync needed
}

// NewSyncCacheManager creates a new cache synchronization manager
// V83: Initializes empty in-memory caches (LoadCachesFromDisk() must be called after)
func NewSyncCacheManager(stateDir string, logger *log.Logger) *SyncCacheManager {
	return &SyncCacheManager{
		stateDir:      stateDir,
		logger:        logger,
		negativeCache: make(map[string]NegativeCacheEntry),
		fullpackCache: make(map[string]FullpackCacheEntry),
		dirty:         false,
	}
}

// NegativeCacheEntry represents a torrent hash without valid mkv files
type NegativeCacheEntry struct {
	Hash      string    `json:"hash"`
	Timestamp time.Time `json:"timestamp"`
}

// FullpackCacheEntry represents a processed TV fullpack torrent
type FullpackCacheEntry struct {
	Hash        string    `json:"hash"`
	Title       string    `json:"title"`
	ProcessedAt time.Time `json:"processed_at"`
}

// AddedAt returns the timestamp when the entry was added
// Needed for compatibility with old NegativeCacheEntry format
func (e *NegativeCacheEntry) AddedAt() time.Time {
	return e.Timestamp
}

// LoadCachesFromDisk loads cache files from disk into memory (one-time at startup)
// V83: Eliminates disk I/O on every cache access (~3000 reads/hour → 0)
func (s *SyncCacheManager) LoadCachesFromDisk() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Load negative cache (no_mkv_hashes.json)
	negPath := filepath.Join(s.stateDir, "no_mkv_hashes.json")
	if data, err := ioutil.ReadFile(negPath); err == nil {
		if err := json.Unmarshal(data, &s.negativeCache); err != nil {
			s.logger.Printf("SyncCache: Warning - failed to parse negative cache: %v", err)
			s.negativeCache = make(map[string]NegativeCacheEntry)
		}
	} else if os.IsNotExist(err) {
		// File doesn't exist yet - that's OK
		s.negativeCache = make(map[string]NegativeCacheEntry)
	} else {
		s.logger.Printf("SyncCache: Warning - failed to read negative cache: %v", err)
		s.negativeCache = make(map[string]NegativeCacheEntry)
	}

	// Load fullpack cache (tv_fullpacks.json)
	fullPath := filepath.Join(s.stateDir, "tv_fullpacks.json")
	if data, err := ioutil.ReadFile(fullPath); err == nil {
		if err := json.Unmarshal(data, &s.fullpackCache); err != nil {
			s.logger.Printf("SyncCache: Warning - failed to parse fullpack cache: %v", err)
			s.fullpackCache = make(map[string]FullpackCacheEntry)
		}
	} else if os.IsNotExist(err) {
		// File doesn't exist yet - that's OK
		s.fullpackCache = make(map[string]FullpackCacheEntry)
	} else {
		s.logger.Printf("SyncCache: Warning - failed to read fullpack cache: %v", err)
		s.fullpackCache = make(map[string]FullpackCacheEntry)
	}

	s.logger.Printf("SyncCache: Loaded %d negative + %d fullpack entries from disk",
		len(s.negativeCache), len(s.fullpackCache))

	return nil
}

// SyncToDisk writes in-memory caches to disk if dirty flag is set
// V83: Called periodically (every 30s) or on shutdown to persist changes
func (s *SyncCacheManager) SyncToDisk() error {
	s.mu.Lock()

	if !s.dirty {
		s.mu.Unlock()
		return nil // No changes to sync
	}

	// Copy caches to release lock quickly (don't hold lock during disk I/O)
	negCopy := make(map[string]NegativeCacheEntry, len(s.negativeCache))
	for k, v := range s.negativeCache {
		negCopy[k] = v
	}

	fullCopy := make(map[string]FullpackCacheEntry, len(s.fullpackCache))
	for k, v := range s.fullpackCache {
		fullCopy[k] = v
	}

	s.dirty = false
	s.mu.Unlock()

	// Write to disk outside of lock (atomic writes)
	negPath := filepath.Join(s.stateDir, "no_mkv_hashes.json")
	if err := s.atomicWriteJSON(negPath, negCopy); err != nil {
		return fmt.Errorf("sync negative cache: %w", err)
	}

	fullPath := filepath.Join(s.stateDir, "tv_fullpacks.json")
	if err := s.atomicWriteJSON(fullPath, fullCopy); err != nil {
		return fmt.Errorf("sync fullpack cache: %w", err)
	}

	s.logger.Printf("SyncCache: Synced %d negative + %d fullpack entries to disk",
		len(negCopy), len(fullCopy))

	return nil
}

// ClearNegativeCache removes a hash from the negative cache (no_mkv_hashes.json)
// V83: Operates on RAM cache, marks dirty for eventual disk sync
func (s *SyncCacheManager) ClearNegativeCache(hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Remove from in-memory cache
	if _, exists := s.negativeCache[hash]; exists {
		delete(s.negativeCache, hash)
		s.dirty = true
		s.logger.Printf("SyncCache: Cleared negative cache for hash %s", hash[:8])
	}

	return nil
}

// ClearFullpackCache removes a hash from the fullpack cache (tv_fullpacks.json)
// V83: Operates on RAM cache, marks dirty for eventual disk sync
func (s *SyncCacheManager) ClearFullpackCache(hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Remove from in-memory cache
	if _, exists := s.fullpackCache[hash]; exists {
		delete(s.fullpackCache, hash)
		s.dirty = true
		s.logger.Printf("SyncCache: Cleared fullpack cache for hash %s", hash[:8])
	}

	return nil
}

// CleanupStaleEntries removes expired entries from all caches
// V83: Operates on RAM cache, marks dirty for eventual disk sync
func (s *SyncCacheManager) CleanupStaleEntries(negativeTTL, fullpackTTL time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	removed := 0

	// Cleanup negative cache (in-memory)
	for hash, entry := range s.negativeCache {
		if now.Sub(entry.Timestamp) > negativeTTL {
			delete(s.negativeCache, hash)
			removed++
		}
	}

	// Cleanup fullpack cache (in-memory)
	fullRemoved := 0
	for hash, entry := range s.fullpackCache {
		if now.Sub(entry.ProcessedAt) > fullpackTTL {
			delete(s.fullpackCache, hash)
			fullRemoved++
		}
	}

	removed += fullRemoved

	if removed > 0 {
		s.dirty = true
		s.logger.Printf("SyncCache: Cleaned up %d stale entries", removed)
	}

	return nil
}

// atomicWriteJSON writes data to a JSON file atomically using temp file + rename
// This prevents corruption if process is killed during write
func (s *SyncCacheManager) atomicWriteJSON(path string, data interface{}) error {
	// Marshal to JSON (V84: Compact format to save SD card I/O)
	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}

	// Create temp file in same directory (ensures same filesystem for rename)
	dir := filepath.Dir(path)
	tempFile, err := ioutil.TempFile(dir, ".tmp-cache-*.json")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath) // Cleanup on error

	// Write to temp file
	if _, err := tempFile.Write(jsonData); err != nil {
		tempFile.Close()
		return fmt.Errorf("write temp file: %w", err)
	}

	if err := tempFile.Sync(); err != nil {
		tempFile.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}

	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// Atomic rename (POSIX guarantees atomicity)
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}

	return nil
}

// Stats returns cache statistics
// V83: Read from RAM cache (instantaneous)
func (s *SyncCacheManager) Stats() SyncCacheStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := SyncCacheStats{
		NegativeCacheEntries: len(s.negativeCache),
		FullpackCacheEntries: len(s.fullpackCache),
	}

	return stats
}

// SyncCacheStats holds cache statistics
type SyncCacheStats struct {
	NegativeCacheEntries int
	FullpackCacheEntries int
}
