package main

import (
	"context"
	"log"
	"os"
	"sync"
	"time"
)

// PreloadStrategy defines the preload size based on torrent download speed
type PreloadStrategy int

const (
	StrategyLow    PreloadStrategy = iota // 6 MB - slow downloads
	StrategyMedium                        // 10 MB - moderate downloads
	StrategyHigh                          // 16 MB - fast downloads
)

// Strategy size mappings
const (
	PreloadSizeLow    = 6 * 1024 * 1024  // 6 MB
	PreloadSizeMedium = 10 * 1024 * 1024 // 10 MB
	PreloadSizeHigh   = 16 * 1024 * 1024 // 16 MB
)

// Speed thresholds (bytes per second)
const (
	SpeedThresholdHigh   = 15 * 1024 * 1024 // 15 MiB/s
	SpeedThresholdMedium = 4 * 1024 * 1024  // 4 MiB/s
)

// TorrentStats represents torrent statistics from GoStorm API
type TorrentStats struct {
	Hash          string  `json:"hash"`
	Title         string  `json:"title"`
	DownloadSpeed float64 `json:"download_speed"` // bytes/sec
	TotalPeers    int     `json:"total_peers"`
	ActivePeers   int     `json:"active_peers"`
	Downloaded    int64   `json:"downloaded"`
}

// PeerPreloader manages adaptive preload based on peer speed
// PeerPreloader manages adaptive preload based on peer speed
type PeerPreloader struct {
	nativeBridge  *NativeClient // V160: Native Bridge
	strategyCache map[string]*StrategyEntry
	mu            sync.RWMutex
	cacheTTL      time.Duration
	logger        *log.Logger
}

// StrategyEntry caches strategy decisions with TTL
type StrategyEntry struct {
	Strategy  PreloadStrategy
	UpdatedAt time.Time
	Speed     float64 // Last observed speed (bytes/sec)
}

// NewPeerPreloader creates a new peer-based preloader
func NewPeerPreloader(nativeBridge *NativeClient) *PeerPreloader {
	return &PeerPreloader{
		nativeBridge:  nativeBridge,
		strategyCache: make(map[string]*StrategyEntry),
		cacheTTL:      5 * time.Second, // V162: Reduced to 5s thanks to StatHighFreq optimization
		logger:        log.New(os.Stdout, "[PeerPreload] ", log.LstdFlags),
	}
}

// GetStrategy determines preload strategy for a torrent hash
// Queries GoStorm API (Native Bridge) for download speed and returns appropriate strategy
func (pp *PeerPreloader) GetStrategy(ctx context.Context, hash string) (PreloadStrategy, int) {
	// Check cache first (fast path)
	pp.mu.RLock()
	entry, exists := pp.strategyCache[hash]
	if exists && time.Since(entry.UpdatedAt) < pp.cacheTTL {
		strategy, size := entry.Strategy, pp.strategyToSize(entry.Strategy)
		pp.mu.RUnlock()
		return strategy, size
	}
	pp.mu.RUnlock()

	// Query GoStorm for torrent stats via Native Bridge
	stats, err := pp.nativeBridge.GetTorrent(hash)
	if err != nil {
		// Fallback to medium strategy on error (e.g. torrent not found yet)
		// pp.logger.Printf("Failed to get stats for %s: %v (using medium)", hash[:8], err)
		return StrategyMedium, PreloadSizeMedium
	}

	// Determine strategy based on download speed
	strategy := pp.calculateStrategy(stats.DownloadSpeed)
	size := pp.strategyToSize(strategy)

	// Update cache with double-check
	pp.mu.Lock()
	// Check again if someone else updated it while we were fetching stats
	if entry, exists := pp.strategyCache[hash]; exists && time.Since(entry.UpdatedAt) < pp.cacheTTL {
		strategy, size = entry.Strategy, pp.strategyToSize(entry.Strategy)
		pp.mu.Unlock()
		return strategy, size
	}

	pp.strategyCache[hash] = &StrategyEntry{
		Strategy:  strategy,
		UpdatedAt: time.Now(),
		Speed:     stats.DownloadSpeed,
	}
	pp.mu.Unlock()

	pp.logger.Printf("Hash %s speed=%.2f MiB/s strategy=%s size=%d MB peers=%d/%d (NATIVE)",
		hash, stats.DownloadSpeed/1024/1024, pp.strategyName(strategy),
		size/1024/1024, stats.ActivePeers, stats.TotalPeers)

	return strategy, size
}

// Cleanup removes stale entries from the strategy cache to prevent memory leaks
func (pp *PeerPreloader) Cleanup() {
	pp.mu.Lock()
	defer pp.mu.Unlock()

	now := time.Now()
	// Use 1 hour as retention for strategy history
	threshold := 1 * time.Hour
	removed := 0

	for hash, entry := range pp.strategyCache {
		if now.Sub(entry.UpdatedAt) > threshold {
			delete(pp.strategyCache, hash)
			removed++
		}
	}

	if removed > 0 {
		pp.logger.Printf("Cleanup: removed %d stale strategy entries", removed)
	}
}

// calculateStrategy determines strategy based on download speed
func (pp *PeerPreloader) calculateStrategy(speed float64) PreloadStrategy {
	if speed >= SpeedThresholdHigh {
		return StrategyHigh // >= 15 MiB/s
	}
	if speed >= SpeedThresholdMedium {
		return StrategyMedium // >= 4 MiB/s
	}
	return StrategyLow // < 4 MiB/s
}

// strategyToSize converts strategy to preload size in bytes
func (pp *PeerPreloader) strategyToSize(strategy PreloadStrategy) int {
	switch strategy {
	case StrategyHigh:
		return PreloadSizeHigh
	case StrategyMedium:
		return PreloadSizeMedium
	case StrategyLow:
		return PreloadSizeLow
	default:
		return PreloadSizeMedium
	}
}

// strategyName returns human-readable strategy name
func (pp *PeerPreloader) strategyName(strategy PreloadStrategy) string {
	switch strategy {
	case StrategyHigh:
		return "HIGH"
	case StrategyMedium:
		return "MEDIUM"
	case StrategyLow:
		return "LOW"
	default:
		return "UNKNOWN"
	}
}
