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

	"gostream/internal/metadb"
)

// SyncCacheManager manages synchronization caches.
// V83: In-memory cache to eliminate disk I/O on every access.
// V1.7.1: SQLite persistence replaces JSON temp+rename writes.
type SyncCacheManager struct {
	stateDir string
	mu       sync.RWMutex
	logger   *log.Logger
	db       *metadb.DB

	// V83: In-memory caches (loaded at startup)
	negativeCache map[string]NegativeCacheEntry
	fullpackCache map[string]FullpackCacheEntry
	dirty         bool
}

// NewSyncCacheManager creates a new cache synchronization manager.
func NewSyncCacheManager(stateDir string, logger *log.Logger) *SyncCacheManager {
	return &SyncCacheManager{
		stateDir:      stateDir,
		logger:        logger,
		negativeCache: make(map[string]NegativeCacheEntry),
		fullpackCache: make(map[string]FullpackCacheEntry),
		dirty:         false,
	}
}

// SetDB enables SQLite persistence. Call after NewSyncCacheManager and before any operations.
func (s *SyncCacheManager) SetDB(db *metadb.DB) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db = db
}

// LoadCachesFromDisk loads cache entries into memory (one-time at startup).
// If SQLite is available, loads from DB; otherwise falls back to JSON files.
func (s *SyncCacheManager) LoadCachesFromDisk() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.db != nil {
		neg, full, err := s.db.LoadAllCaches()
		if err != nil {
			s.logger.Printf("SyncCache: Warning - failed to load from DB: %v", err)
			// Fallback to JSON
			return s.loadFromJSONLocked()
		}
		s.negativeCache = make(map[string]NegativeCacheEntry, len(neg))
		for hash, entry := range neg {
			ts, _ := time.Parse(time.RFC3339, entry.Timestamp)
			s.negativeCache[hash] = NegativeCacheEntry{Hash: hash, Timestamp: ts}
		}
		s.fullpackCache = make(map[string]FullpackCacheEntry, len(full))
		for hash, entry := range full {
			ts, _ := time.Parse(time.RFC3339, entry.Timestamp)
			s.fullpackCache[hash] = FullpackCacheEntry{Hash: hash, Title: entry.Title, ProcessedAt: ts}
		}
		s.logger.Printf("SyncCache: Loaded %d negative + %d fullpack entries from StateDB",
			len(s.negativeCache), len(s.fullpackCache))
		return nil
	}

	return s.loadFromJSONLocked()
}

func (s *SyncCacheManager) loadFromJSONLocked() error {
	// Legacy JSON loading (fallback when StateDB is disabled)
	negPath := GetStateDir() + "/no_mkv_hashes.json"
	fullPath := GetStateDir() + "/tv_fullpacks.json"

	if data, err := readFileSafe(negPath); err == nil {
		if err := unmarshalJSON(data, &s.negativeCache); err != nil {
			s.logger.Printf("SyncCache: Warning - failed to parse negative cache: %v", err)
			s.negativeCache = make(map[string]NegativeCacheEntry)
		}
	} else {
		s.negativeCache = make(map[string]NegativeCacheEntry)
	}

	if data, err := readFileSafe(fullPath); err == nil {
		if err := unmarshalJSON(data, &s.fullpackCache); err != nil {
			s.logger.Printf("SyncCache: Warning - failed to parse fullpack cache: %v", err)
			s.fullpackCache = make(map[string]FullpackCacheEntry)
		}
	} else {
		s.fullpackCache = make(map[string]FullpackCacheEntry)
	}

	s.logger.Printf("SyncCache: Loaded %d negative + %d fullpack entries from disk",
		len(s.negativeCache), len(s.fullpackCache))
	return nil
}

// SyncToDisk writes in-memory caches to persistence if dirty flag is set.
// With StateDB: writes a single transaction. Without: writes JSON via temp+rename.
func (s *SyncCacheManager) SyncToDisk() error {
	s.mu.Lock()

	if !s.dirty {
		s.mu.Unlock()
		return nil
	}

	negCopy := make(map[string]metadb.NegativeCacheEntry, len(s.negativeCache))
	for k, v := range s.negativeCache {
		negCopy[k] = metadb.NegativeCacheEntry{
			Hash:      k,
			Timestamp: v.Timestamp.UTC().Format(time.RFC3339),
		}
	}

	fullCopy := make(map[string]metadb.FullpackCacheEntry, len(s.fullpackCache))
	for k, v := range s.fullpackCache {
		fullCopy[k] = metadb.FullpackCacheEntry{
			Hash:      k,
			Title:     v.Title,
			Timestamp: v.ProcessedAt.UTC().Format(time.RFC3339),
		}
	}

	s.dirty = false
	s.mu.Unlock()

	if s.db != nil {
		if err := s.db.SaveAllCaches(negCopy, fullCopy); err != nil {
			s.logger.Printf("SyncCache: Warning - failed to save to DB: %v", err)
			return fmt.Errorf("sync caches to DB: %w", err)
		}
		s.logger.Printf("SyncCache: Synced %d negative + %d fullpack entries to StateDB",
			len(negCopy), len(fullCopy))
		return nil
	}

	// Fallback: legacy JSON write
	return s.syncJSONLocked(negCopy, fullCopy)
}

// ClearNegativeCache removes a hash from the negative cache.
func (s *SyncCacheManager) ClearNegativeCache(hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.negativeCache[hash]; exists {
		delete(s.negativeCache, hash)
		s.dirty = true
		s.logger.Printf("SyncCache: Cleared negative cache for hash %s", hash[:8])
	}
	return nil
}

// ClearFullpackCache removes a hash from the fullpack cache.
func (s *SyncCacheManager) ClearFullpackCache(hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.fullpackCache[hash]; exists {
		delete(s.fullpackCache, hash)
		s.dirty = true
		s.logger.Printf("SyncCache: Cleared fullpack cache for hash %s", hash[:8])
	}
	return nil
}

// CleanupStaleEntries removes expired entries from all caches.
func (s *SyncCacheManager) CleanupStaleEntries(negativeTTL, fullpackTTL time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	removed := 0

	for hash, entry := range s.negativeCache {
		if now.Sub(entry.Timestamp) > negativeTTL {
			delete(s.negativeCache, hash)
			removed++
		}
	}

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

// Stats returns cache statistics.
func (s *SyncCacheManager) Stats() SyncCacheStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return SyncCacheStats{
		NegativeCacheEntries: len(s.negativeCache),
		FullpackCacheEntries: len(s.fullpackCache),
	}
}

// SyncCacheStats holds cache statistics.
type SyncCacheStats struct {
	NegativeCacheEntries int
	FullpackCacheEntries int
}

// NegativeCacheEntry represents a torrent hash without valid mkv files.
type NegativeCacheEntry struct {
	Hash      string    `json:"hash"`
	Timestamp time.Time `json:"timestamp"`
}

// AddedAt returns the timestamp when the entry was added.
func (e *NegativeCacheEntry) AddedAt() time.Time {
	return e.Timestamp
}

// FullpackCacheEntry represents a processed TV fullpack torrent.
type FullpackCacheEntry struct {
	Hash        string    `json:"hash"`
	Title       string    `json:"title"`
	ProcessedAt time.Time `json:"processed_at"`
}

// syncJSONLocked writes caches to JSON files using temp+rename (legacy fallback).
func (s *SyncCacheManager) syncJSONLocked(
	negCopy map[string]metadb.NegativeCacheEntry,
	fullCopy map[string]metadb.FullpackCacheEntry,
) error {
	negPath := filepath.Join(s.stateDir, "no_mkv_hashes.json")
	if err := atomicWriteJSON(negPath, negCopy); err != nil {
		return fmt.Errorf("sync negative cache: %w", err)
	}

	fullPath := filepath.Join(s.stateDir, "tv_fullpacks.json")
	if err := atomicWriteJSON(fullPath, fullCopy); err != nil {
		return fmt.Errorf("sync fullpack cache: %w", err)
	}

	s.logger.Printf("SyncCache: Synced %d negative + %d fullpack entries to disk",
		len(negCopy), len(fullCopy))
	return nil
}

// readFileSafe reads a file, returning error if not found or unreadable.
func readFileSafe(path string) ([]byte, error) {
	return ioutil.ReadFile(path)
}

// unmarshalJSON unmarshals JSON data into a target.
func unmarshalJSON(data []byte, target interface{}) error {
	return json.Unmarshal(data, target)
}

// atomicWriteJSON writes data to a JSON file atomically using temp file + rename.
func atomicWriteJSON(path string, data interface{}) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}

	dir := filepath.Dir(path)
	tempFile, err := ioutil.TempFile(dir, ".tmp-cache-*.json")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)

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

	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}

	return nil
}
