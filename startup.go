package main

import (
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// StartupCacheBuilder pre-populates metadata cache at startup
// This dramatically improves Plex scan performance (30s -> 10s for 1000+ files)
// Runs in background to avoid delaying mount operation
type StartupCacheBuilder struct {
	sourcePath string
	cache      *LRUCache
	logger     *log.Logger

	// Statistics
	mu             sync.Mutex
	filesProcessed int
	filesSkipped   int
	errors         int
	startTime      time.Time
	foundFiles     map[string]bool // V142: Track files for GC
}

// NewStartupCacheBuilder creates a new startup cache builder
func NewStartupCacheBuilder(sourcePath string, cache *LRUCache, logger *log.Logger) *StartupCacheBuilder {
	return &StartupCacheBuilder{
		sourcePath: sourcePath,
		cache:      cache,
		logger:     logger,
		startTime:  time.Now(),
		foundFiles: make(map[string]bool),
	}
}

// Start begins background cache population
// Returns immediately, processing happens in goroutine
func (b *StartupCacheBuilder) Start() {
	b.logger.Printf("Starting cache pre-population from %s", b.sourcePath)

	go func() {
		// Process movies directory
		moviesPath := filepath.Join(b.sourcePath, "movies")
		if _, err := os.Stat(moviesPath); err == nil {
			b.processDirectory(moviesPath, false) // Not recursive
		}

		// Process TV directory (recursive for seasons)
		tvPath := filepath.Join(b.sourcePath, "tv")
		if _, err := os.Stat(tvPath); err == nil {
			b.processDirectory(tvPath, true) // Recursive
		}

		// Log final statistics
		duration := time.Since(b.startTime)
		b.mu.Lock()
		processed := b.filesProcessed
		skipped := b.filesSkipped
		errors := b.errors
		b.mu.Unlock()

		b.logger.Printf("Cache pre-population complete: %d processed, %d skipped, %d errors in %v",
			processed, skipped, errors, duration)

		// Log cache statistics
		stats := b.cache.Stats()
		b.logger.Printf("Cache after startup: %d entries, %.2f MB used of %.2f MB capacity",
			stats.Entries, float64(stats.Size)/(1024*1024), float64(stats.Capacity)/(1024*1024))

		// V133: Save inode map after startup scan completes
		// This ensures the map is persisted even if no background save triggered
		if globalInodeMap != nil && globalInodeMap.IsDirty() {
			if err := globalInodeMap.SaveToDisk(); err != nil {
				b.logger.Printf("InodeMap: Post-startup save error: %v", err)
			} else {
				files, dirs, _, _ := GetInodeMapStats()
				b.logger.Printf("InodeMap: Saved after startup scan (%d files, %d dirs)", files, dirs)
			}
		}

		// V142: Inode GC - Remove ghost entries (files that no longer exist)
		if globalInodeMap != nil {
			b.mu.Lock()
			// Make a copy or pass reference? Pass reference since we are done modifying it
			// However, PruneMissing only reads from it, so it's safe if we don't modify it anymore.
			// We hold b.mu just to be safe or we can just pass it as we are in the final serial block.
			foundSet := b.foundFiles
			b.mu.Unlock()

			pruned := globalInodeMap.PruneMissing(foundSet)
			if pruned > 0 {
				b.logger.Printf("Startup GC: Pruned %d ghost inodes", pruned)
			}
		}
	}()
}

// processDirectory walks directory and processes all .mkv files
// If recursive is true, also processes subdirectories
func (b *StartupCacheBuilder) processDirectory(dirPath string, recursive bool) {
	// Semaphore to limit concurrent file operations
	// Too many concurrent reads can overwhelm the filesystem
	sem := make(chan struct{}, 10) // Max 10 concurrent
	var wg sync.WaitGroup

	// Walk directory
	err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			b.logger.Printf("Startup cache: error accessing %s: %v", path, err)
			b.incrementErrors()
			return nil // Continue walking
		}

		// Skip directories if not recursive
		if info.IsDir() {
			if !recursive && path != dirPath {
				return filepath.SkipDir
			}
			// V133: Register directory in inode map
			// Compute relative path from source for consistent inode generation
			relPath, _ := filepath.Rel(b.sourcePath, path)
			if relPath != "." && relPath != "" {
				getDirInodeFromMap("/" + relPath)
			}
			return nil
		}

		// Process only .mkv files
		if filepath.Ext(path) != ".mkv" {
			return nil
		}

		// Check if already in cache (fast path)
		if _, ok := b.cache.Get(path); ok {
			b.incrementSkipped()
			return nil
		}

		// Acquire semaphore
		wg.Add(1)
		sem <- struct{}{}

		// Process file in goroutine
		go func(filePath string) {
			defer wg.Done()
			defer func() { <-sem }()

			b.processFile(filePath)
		}(path)

		return nil
	})

	if err != nil {
		b.logger.Printf("Startup cache: error walking %s: %v", dirPath, err)
		b.incrementErrors()
	}

	// Wait for all goroutines to complete
	wg.Wait()
}

// processFile reads metadata for a single file and adds to cache
func (b *StartupCacheBuilder) processFile(path string) {
	// Read metadata from file
	fileMeta, err := ReadMetadataFromFile(path)
	if err != nil {
		b.logger.Printf("Startup cache: error reading metadata for %s: %v", path, err)
		b.incrementErrors()
		return
	}

	// Convert to Metadata format
	meta := &Metadata{
		URL:    fileMeta.URL,
		Size:   fileMeta.Size,
		Mtime:  fileMeta.Mtime,
		Path:   fileMeta.Path,
		ImdbID: fileMeta.ImdbID,
	}

	// Calculate approximate size
	size := approximateMetadataSize(meta)

	// Add to cache
	b.cache.Put(path, meta, size)

	// V133: Add to inode map for deterministic inode generation
	// This ensures Plex sees the same inode after restarts
	// BUG FIX: Pass full path instead of just filename to avoid collisions
	addFileToInodeMap(path, fileMeta.URL)

	b.incrementProcessed(path)
}

// Statistics helpers (thread-safe)
func (b *StartupCacheBuilder) incrementProcessed(path string) {
	b.mu.Lock()
	b.filesProcessed++
	b.foundFiles[path] = true // V142: Mark as found
	b.mu.Unlock()
}

func (b *StartupCacheBuilder) incrementSkipped() {
	b.mu.Lock()
	b.filesSkipped++
	b.mu.Unlock()
}

func (b *StartupCacheBuilder) incrementErrors() {
	b.mu.Lock()
	b.errors++
	b.mu.Unlock()
}
