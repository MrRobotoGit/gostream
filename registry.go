package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"gostream/internal/metadb"
)

// EpisodeEntry matches the Python registry format.
type EpisodeEntry struct {
	QualityScore int    `json:"quality_score"`
	Hash         string `json:"hash"`
	FilePath     string `json:"file_path"`
	Source       string `json:"source"`
	Created      int64  `json:"created"`
}

var registryMutex sync.Mutex
var registryDB *metadb.DB

// SetRegistryDB enables SQLite persistence for the episode registry.
func SetRegistryDB(db *metadb.DB) {
	registryMutex.Lock()
	defer registryMutex.Unlock()
	registryDB = db
}

// GetRegistryPath returns the path to tv_episode_registry.json.
func GetRegistryPath() string {
	return filepath.Join(GetStateDir(), "tv_episode_registry.json")
}

// StartRegistryWatchdog runs the self-healing check at startup and then every 24 hours.
func StartRegistryWatchdog(stopChan chan struct{}) {
	SyncRegistryWithDisk()

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	logger.Printf("[Registry] Watchdog active (interval: 24h)")

	for {
		select {
		case <-ticker.C:
			SyncRegistryWithDisk()
		case <-stopChan:
			logger.Printf("[Registry] Watchdog stopping")
			return
		}
	}
}

// SyncRegistryWithDisk cleans up orphaned entries.
// With StateDB: deletes entries whose file_path no longer exists via SQL.
// Without StateDB: falls back to JSON read-filter-write cycle.
func SyncRegistryWithDisk() {
	registryMutex.Lock()
	defer registryMutex.Unlock()

	if registryDB != nil {
		syncRegistryWithDB()
		return
	}
	syncRegistryWithJSON()
}

func syncRegistryWithDB() {
	path := GetRegistryPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return
	}

	logger.Printf("[Registry] Starting self-healing check (StateDB): %s", path)

	entries, err := registryDB.AllEpisodes()
	if err != nil {
		logger.Printf("[Registry] ERROR: could not read episodes from DB: %v", err)
		return
	}

	removed := 0
	for _, entry := range entries {
		if _, err := os.Stat(entry.FilePath); err != nil {
			if err := registryDB.DeleteEpisode(entry.EpisodeKey); err != nil {
				logger.Printf("[Registry] ERROR: failed to delete episode %s: %v", entry.EpisodeKey, err)
			} else {
				removed++
			}
		}
	}

	if removed == 0 {
		logger.Printf("[Registry] Audit complete: 0 ghost entries found (Total: %d)", len(entries))
	} else {
		logger.Printf("[Registry] Self-Healing (StateDB): Purged %d ghost entries. Remaining: %d", removed, len(entries)-removed)
	}
}

func syncRegistryWithJSON() {
	path := GetRegistryPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return
	}

	logger.Printf("[Registry] Starting self-healing check: %s", path)

	data, err := os.ReadFile(path)
	if err != nil {
		logger.Printf("[Registry] ERROR: could not read registry: %v", err)
		return
	}

	var registry map[string]EpisodeEntry
	if err := unmarshalJSON(data, &registry); err != nil {
		logger.Printf("[Registry] ERROR: could not parse registry: %v", err)
		return
	}

	initialCount := len(registry)
	newRegistry := make(map[string]EpisodeEntry)
	removed := 0

	for key, entry := range registry {
		if _, err := os.Stat(entry.FilePath); err == nil {
			newRegistry[key] = entry
		} else {
			removed++
		}
	}

	if removed == 0 {
		logger.Printf("[Registry] Audit complete: 0 ghost entries found (Total: %d)", initialCount)
		return
	}

	if err := saveRegistryLocked(path, newRegistry); err != nil {
		logger.Printf("[Registry] ERROR: could not save cleaned registry: %v", err)
	} else {
		logger.Printf("[Registry] Self-Healing: Purged %d ghost entries. Remaining: %d", removed, len(newRegistry))
	}
}

// RemoveFromRegistry removes a specific file path from the registry.
func RemoveFromRegistry(filePath string) {
	registryMutex.Lock()
	defer registryMutex.Unlock()

	if registryDB != nil {
		removeFromRegistryDB(filePath)
		return
	}
	removeFromRegistryJSON(filePath)
}

func removeFromRegistryDB(filePath string) {
	entries, err := registryDB.EpisodesByFilePath(filePath)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if err := registryDB.DeleteEpisode(entry.EpisodeKey); err != nil {
			logger.Printf("[Registry] ERROR: failed to delete episode %s: %v", entry.EpisodeKey, err)
		} else {
			logger.Printf("[Registry] Real-time (StateDB): Removed deleted file from registry: %s", filepath.Base(filePath))
		}
	}
}

func removeFromRegistryJSON(filePath string) {
	path := GetRegistryPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	var registry map[string]EpisodeEntry
	if err := unmarshalJSON(data, &registry); err != nil {
		return
	}

	removed := false
	for key, entry := range registry {
		if entry.FilePath == filePath {
			delete(registry, key)
			removed = true
			logger.Printf("[Registry] Real-time: Removed deleted file from registry: %s", filepath.Base(filePath))
			break
		}
	}

	if removed {
		saveRegistryLocked(path, registry)
	}
}

// saveRegistryLocked saves the registry using syscall.Flock (JSON fallback).
func saveRegistryLocked(path string, registry map[string]EpisodeEntry) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	data, err := json.Marshal(registry)
	if err != nil {
		return err
	}

	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	_, err = f.Write(data)
	return err
}
