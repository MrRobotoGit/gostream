package main

import (
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/MrRobotoGit/gostream/internal/gostorm/torr"
)

// CleanupManager provides periodic cleanup of various in-memory structures
// to prevent memory leaks on long-running instances
type CleanupManager struct {
	// Deleted torrent hashes (24h TTL)
	deletedHashes map[string]time.Time
	deletedMu     sync.RWMutex

	// File read offsets (1h TTL)
	fileOffsets map[string]*offsetEntry
	offsetsMu   sync.RWMutex

	// File activity timestamps (1h TTL)
	fileActivities map[string]time.Time
	activitiesMu   sync.RWMutex

	// External components to clean (V238 Audit 1.A)
	peerPreloader *PeerPreloader
	metaCache     *LRUCache
	nativeBridge  *NativeClient

	// Configuration
	deletedHashTTL  time.Duration
	offsetTTL       time.Duration
	activityTTL     time.Duration
	cleanupInterval time.Duration

	// Control
	mu      sync.Mutex
	stopped bool
	stopCh  chan struct{}
	logger  *log.Logger
}

// offsetEntry tracks read position and timestamp for sequential detection
type offsetEntry struct {
	offset    int64
	length    int
	timestamp time.Time
}

// NewCleanupManager creates a new cleanup manager with references to components
func NewCleanupManager(logger *log.Logger, pp *PeerPreloader, mc *LRUCache, nb *NativeClient) *CleanupManager {
	return &CleanupManager{
		deletedHashes:   make(map[string]time.Time),
		fileOffsets:     make(map[string]*offsetEntry),
		fileActivities:  make(map[string]time.Time),
		peerPreloader:   pp,
		metaCache:       mc,
		nativeBridge:    nb,
		deletedHashTTL:  24 * time.Hour,
		offsetTTL:       1 * time.Hour,
		activityTTL:     1 * time.Hour,
		cleanupInterval: 5 * time.Minute,
		stopCh:          make(chan struct{}),
		logger:          logger,
	}
}

// Start begins the periodic cleanup loop
func (cm *CleanupManager) Start() {
	go cm.cleanupLoop()
	cm.logger.Printf("Cleanup manager started (interval: %v)", cm.cleanupInterval)
}

// Stop stops the cleanup manager
func (cm *CleanupManager) Stop() {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if !cm.stopped {
		cm.stopped = true
		close(cm.stopCh)
		cm.logger.Printf("Cleanup manager stopped")
	}
}

// cleanupLoop runs periodic cleanup
func (cm *CleanupManager) cleanupLoop() {
	ticker := time.NewTicker(cm.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			cm.mu.Lock()
			isStopped := cm.stopped
			cm.mu.Unlock()
			if isStopped {
				return
			}
			cm.runCleanup()
		case <-cm.stopCh:
			return
		}
	}
}

// runCleanup performs cleanup of all tracked structures
func (cm *CleanupManager) runCleanup() {
	now := time.Now()
	stats := CleanupStats{}

	// Cleanup deleted hashes (24h TTL)
	cm.deletedMu.Lock()
	for hash, deletedAt := range cm.deletedHashes {
		if now.Sub(deletedAt) > cm.deletedHashTTL {
			delete(cm.deletedHashes, hash)
			stats.DeletedHashesRemoved++
		}
	}
	stats.DeletedHashesTotal = len(cm.deletedHashes)
	cm.deletedMu.Unlock()

	// Cleanup file offsets (1h TTL)
	cm.offsetsMu.Lock()
	for path, entry := range cm.fileOffsets {
		if now.Sub(entry.timestamp) > cm.offsetTTL {
			delete(cm.fileOffsets, path)
			stats.OffsetsRemoved++
		}
	}
	stats.OffsetsTotal = len(cm.fileOffsets)
	cm.offsetsMu.Unlock()

	// Cleanup file activities (1h TTL)
	cm.activitiesMu.Lock()
	for path, lastActivity := range cm.fileActivities {
		if now.Sub(lastActivity) > cm.activityTTL {
			delete(cm.fileActivities, path)
			stats.ActivitiesRemoved++
		}
	}
	stats.ActivitiesTotal = len(cm.fileActivities)
	cm.activitiesMu.Unlock()

	// 4. Cleanup PeerPreloader strategy cache
	if cm.peerPreloader != nil {
		cm.peerPreloader.Cleanup()
	}

	// V238 Audit 1.A: Cleanup Expired Metadata
	if cm.metaCache != nil {
		stats.MetadataPruned = cm.metaCache.CleanupExpired()
	}

	// V238 Audit 1.A: Cleanup Native Bridge hashes
	if cm.nativeBridge != nil {
		stats.HashesRemoved = cm.nativeBridge.CleanupHashes()
	}

	// 5. V170-LeakFix: Cleanup InodeMap (Garbage Collection)
	// V302-Fix: WalkDir is the primary source of truth. Physical files AND directories
	// are always preserved. Deleted MKV entries are pruned when gone from disk.
	// Active torrent virtual paths are added on top.
	// Safety: skip pruning if validFiles is empty (mount unavailable).
	if globalInodeMap != nil {
		validFiles := make(map[string]bool)

		// Primary: all physical files AND directories on disk (including virtual MKV stubs).
		// Directories must be included — their inodes are in the InodeMap too, and pruning
		// them causes FUSE to regenerate them on every Plex/Samba directory traversal.
		if physicalSourcePath != "" {
			_ = filepath.WalkDir(physicalSourcePath, func(path string, d os.DirEntry, err error) error {
				if err == nil {
					validFiles[path] = true
				}
				return nil
			})
		}

		// Secondary: virtual paths for active torrents (served by FUSE, not on disk)
		torrents := torr.ListTorrent()
		for _, t := range torrents {
			if t == nil {
				continue
			}
			for _, f := range t.Files() {
				validFiles[filepath.Join(physicalSourcePath, f.Path())] = true
			}
		}

		// Prune ghost entries. Safety: skip if validFiles is empty (mount failure).
		if len(validFiles) > 0 {
			pruned := globalInodeMap.PruneMissing(validFiles)
			if pruned > 0 {
				stats.InodeMapPruned = pruned
			}
		}
	}

	// 6. V170-LeakFix: Cleanup PlaybackRegistry
	// key: path(string), value: *PlaybackState

	// Prepare allow-list of active paths from activeHandles
	activePaths := make(map[string]bool)
	activeHandles.Range(func(key, value interface{}) bool {
		h := key.(*MkvHandle)
		activePaths[h.path] = true
		return true
	})

	playbackRegistry.Range(func(key, value interface{}) bool {
		path := key.(string)
		ps, ok := value.(*PlaybackState)
		if !ok {
			playbackRegistry.Delete(key)
			stats.PlaybackRegistryPruned++
			return true
		}

		// V182: Remove if no longer active (with grace period logic if needed, but for now strict)
		// We trust activeHandles as the ground truth for "currently open file"
		// If not in activePaths, it means FUSE Release() was called.
		if !activePaths[path] {
			// Orphaned entry (playback stopped/paused)
			// V216: Allow extended grace period for resume (15 mins) to keep Priority capability active.
			// Determine the most recent sign of life:
			// 1. OpenedAt (initial open)
			// 2. ConfirmedAt (Plex Webhook)
			// 3. File Activity (Read operations)

			lastActivity := ps.OpenedAt

			if !ps.ConfirmedAt.IsZero() && ps.ConfirmedAt.After(lastActivity) {
				lastActivity = ps.ConfirmedAt
			}

			// Check file read activity (for non-Plex players like VLC)
			if act, ok := cm.GetLastActivity(path); ok && act.After(lastActivity) {
				lastActivity = act
			}

			if now.Sub(lastActivity) > 15*time.Minute {
				// V238 Audit 1.B: Ensure Priority is OFF before deleting zombie registry entry
				// V273: PeekTorrent instead of GetTorrent — cleanup is read-only monitoring,
				// must NOT reactivate dormant torrents (same pattern as cache.go fix).
				if ps.Hash != "" {
					if t := torr.PeekTorrent(ps.Hash); t != nil && t.Torrent != nil && t.IsPriority {
						t.IsPriority = false
						t.SetAggressiveMode(false, 0)
						cm.logger.Printf("[V273] Force Priority OFF for zombie torrent: %s", ps.Hash[:8])
					}
				}
				playbackRegistry.Delete(key)
				stats.PlaybackRegistryPruned++
			}
			return true
		}

		// Remove entries older than 24h (just in case)
		if now.Sub(ps.OpenedAt) > 24*time.Hour {
			playbackRegistry.Delete(key)
			stats.PlaybackRegistryPruned++
		}
		return true
	})

	// Log only if something was cleaned
	if stats.DeletedHashesRemoved > 0 || stats.OffsetsRemoved > 0 || stats.ActivitiesRemoved > 0 ||
		stats.InodeMapPruned > 0 || stats.PlaybackRegistryPruned > 0 || stats.MetadataPruned > 0 || stats.HashesRemoved > 0 {
		cm.logger.Printf("Cleanup: hashes=%d offsets=%d acts=%d inodes=%d registry=%d meta=%d",
			stats.DeletedHashesRemoved, stats.OffsetsRemoved, stats.ActivitiesRemoved,
			stats.InodeMapPruned, stats.PlaybackRegistryPruned, stats.MetadataPruned)
	}
}

// --- Deleted Hashes Management ---

func (cm *CleanupManager) AddDeletedHash(hash string) {
	cm.deletedMu.Lock()
	cm.deletedHashes[hash] = time.Now()
	cm.deletedMu.Unlock()
}

func (cm *CleanupManager) RemoveDeletedHash(hash string) {
	cm.deletedMu.Lock()
	delete(cm.deletedHashes, hash)
	cm.deletedMu.Unlock()
}

func (cm *CleanupManager) IsDeleted(hash string) bool {
	cm.deletedMu.RLock()
	_, exists := cm.deletedHashes[hash]
	cm.deletedMu.RUnlock()
	return exists
}

// --- File Offset Management ---

// UpdateOffset records the last read position for a file
func (cm *CleanupManager) UpdateOffset(path string, offset int64, length int) {
	cm.offsetsMu.Lock()
	cm.fileOffsets[path] = &offsetEntry{
		offset:    offset,
		length:    length,
		timestamp: time.Now(),
	}
	cm.offsetsMu.Unlock()
}

func (cm *CleanupManager) GetOffset(path string) (int64, int, bool) {
	cm.offsetsMu.RLock()
	entry, exists := cm.fileOffsets[path]
	cm.offsetsMu.RUnlock()
	if !exists {
		return 0, 0, false
	}
	return entry.offset, entry.length, true
}

// --- File Activity Management ---

// UpdateActivity records activity for a file
func (cm *CleanupManager) UpdateActivity(path string) {
	cm.activitiesMu.Lock()
	cm.fileActivities[path] = time.Now()
	cm.activitiesMu.Unlock()
}

func (cm *CleanupManager) GetLastActivity(path string) (time.Time, bool) {
	cm.activitiesMu.RLock()
	t, exists := cm.fileActivities[path]
	cm.activitiesMu.RUnlock()
	return t, exists
}

func (cm *CleanupManager) GetIdleDuration(path string) time.Duration {
	cm.activitiesMu.RLock()
	last, exists := cm.fileActivities[path]
	cm.activitiesMu.RUnlock()
	if !exists {
		return 0
	}
	return time.Since(last)
}

// Clear removes all data (for testing)
func (cm *CleanupManager) Clear() {
	cm.deletedMu.Lock()
	cm.deletedHashes = make(map[string]time.Time)
	cm.deletedMu.Unlock()

	cm.offsetsMu.Lock()
	cm.fileOffsets = make(map[string]*offsetEntry)
	cm.offsetsMu.Unlock()

	cm.activitiesMu.Lock()
	cm.fileActivities = make(map[string]time.Time)
	cm.activitiesMu.Unlock()
}

// Statistics

// CleanupStats represents cleanup statistics
type CleanupStats struct {
	DeletedHashesTotal     int
	DeletedHashesRemoved   int
	OffsetsTotal           int
	OffsetsRemoved         int
	ActivitiesTotal        int
	ActivitiesRemoved      int
	InodeMapPruned         int // V170
	PlaybackRegistryPruned int // V170
	MetadataPruned         int // V238
	HashesRemoved          int // V238
}

// Stats returns current cleanup manager statistics
func (cm *CleanupManager) Stats() CleanupStats {
	cm.deletedMu.RLock()
	deletedHashesTotal := len(cm.deletedHashes)
	cm.deletedMu.RUnlock()

	cm.offsetsMu.RLock()
	offsetsTotal := len(cm.fileOffsets)
	cm.offsetsMu.RUnlock()

	cm.activitiesMu.RLock()
	activitiesTotal := len(cm.fileActivities)
	cm.activitiesMu.RUnlock()

	return CleanupStats{
		DeletedHashesTotal: deletedHashesTotal,
		OffsetsTotal:       offsetsTotal,
		ActivitiesTotal:    activitiesTotal,
	}
}
