package main

import (
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	server "github.com/MrRobotoGit/gostream/internal/gostorm"
	"github.com/MrRobotoGit/gostream/internal/gostorm/settings"
	tsutils "github.com/MrRobotoGit/gostream/internal/gostorm/utils"
	"github.com/MrRobotoGit/gostream/internal/gostorm/web"
	"hash/fnv"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// --- CONFIGURAZIONE (V71-Zero-Latency) ---
// Note: Configuration is now handled entirely by config.go via globalConfig
// Constants removed to ensure single source of truth

var logger = log.New(os.Stdout, "[GoProxy] ", log.LstdFlags)

// V79: httpClient now created in main() after config loading to use globalConfig values
var httpClient *http.Client

// V226: Unified Master Semaphore for all data operations (Native, HTTP, Prefetch)
// Provides strict protection for GoStorm internal connections (limit: 25)
var masterDataSemaphore chan struct{}

var startTime = time.Now()
var metaCache *LRUCache           // Replaced sync.Map with LRU cache (FASE 3.1)
var raCache = newReadAheadCache() // V238: Sharded Cache (Audit 2.A)

// Global rate limiter and lock manager (FASE 1 - Stabilità Critica)
// V82: Initialized in main() from globalConfig
var globalRateLimiter *RateLimiter
var globalLockManager *LockManager

// Global peer-based preloader (FASE 2 - Performance)
// V79: Now initialized in main() after httpClient creation
var peerPreloader *PeerPreloader
var nativeBridge *NativeClient // V160: Native Bridge

// Global cleanup manager (FASE 3.3 - Memory Cleanup)
var globalCleanupManager *CleanupManager

// Global torrent remover (FASE 4.2 - Auto-Remove Torrents)
var globalTorrentRemover *TorrentRemover

// Global configuration (FASE 4.16 - Env Configuration)
var globalConfig Config

// PlaybackState traccia lo stato di una sessione di visione reale
type PlaybackState struct {
	mu          sync.RWMutex
	Path        string
	Hash        string // V185: Store InfoHash to link with GoStorm priority
	ImdbID      string // V281: IMDB ID from MKV line 4 — used for webhook matching
	OpenedAt    time.Time
	ConfirmedAt time.Time // Quando arriva il webhook di Plex
	IsHealthy   bool      // Conferma definitiva da Plex
	IsStopped   bool      // V272: Explicit media.stop received
}

func (ps *PlaybackState) SetHealthy(healthy bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.IsHealthy = healthy
	ps.ConfirmedAt = time.Now()
}

func (ps *PlaybackState) GetStatus() bool {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.IsHealthy
}

var playbackRegistry sync.Map // path -> *PlaybackState

// Global sync cache manager (FASE 4.13 - Sync Script Caches)
var globalSyncCacheManager *SyncCacheManager

// V138: Global paths for physical source and virtual mount
var physicalSourcePath string
var virtualMountPath string

// V138: Global stop channel for background goroutines
var backgroundStopChan = make(chan struct{})
var backgroundStopOnce sync.Once // V227: Prevent double-close panic

// V83: Global buffer pool for Read operations (eliminates ~1000 alloc/min)
// V143-Fix: Initialized in main() to match Config.ReadAheadBase (dynamic sizing)
var readBufferPool *sync.Pool

// V281: Regex to extract IMDB ID from Plex webhook raw payload.
// Matches "imdb://tt1234567" inside the Guid array (capital G) without touching
// the lowercase "guid" string field — avoids json UnmarshalTypeError.
var reImdbID = regexp.MustCompile(`"imdb://(tt\d+)"`)

// V226: Track active handles for Idle Reader Cleanup (Unified)
var activeHandles sync.Map // key: *MkvHandle, value: bool

// V227: Deduplicate prefetch goroutines
var inFlightPrefetches sync.Map // key: "path:offset", value: bool

// V258: Global Pump Registry - One pump per file path
var activePumps sync.Map // Map[string]*NativePumpState

// V259: Global Mutex for pump creation synchronization
var pumpCreationMu sync.Mutex

// V258: NativePumpState tracks a shared pump across multiple handles
type NativePumpState struct {
	cancel   context.CancelFunc
	reader   *NativeReader
	path     string
	refCount int32 // V260: Track how many handles are using this pump
}

// V226: Helper to create NativeReader via NativeBridge (Zero-Network Data Path)
// resolveTargetFile finds the torrent hash and file index for a given URL and size
func resolveTargetFile(url string, targetSize int64, physicalPath string) (string, int, error) {
	if nativeBridge == nil {
		return "", 0, fmt.Errorf("nativeBridge is nil")
	}
	if strings.Contains(url, "link=") {
		start := strings.Index(url, "link=") + 5
		end := strings.Index(url[start:], "&")
		if end == -1 {
			end = len(url) - start
		}
		hashStr := url[start : start+end]
		hash := metainfo.NewHashFromHex(hashStr)

		// V1.4.0-Optimization: Removed 500ms retry loop. Wake() will handle registration.
		t := web.BTS.GetTorrent(hash)

		if t != nil {
			// V215: Auto-Priority Disabled. Only Webhook can grant it now.
			// t.IsPriority = true (Removed)

			files := t.Files()
			sort.Slice(files, func(i, j int) bool {
				return tsutils.CompareStrings(files[i].Path(), files[j].Path())
			})

			// V208: Size Match Candidates
			var sizeMatchIndex int
			var matchesBySize int

			// V206/V207: Smart Matching - Normalize names and strip hash
			cleanPhys := strings.ToLower(filepath.Base(physicalPath))
			if len(hashStr) >= 8 {
				// Strip full hash
				cleanPhys = strings.ReplaceAll(cleanPhys, "_"+strings.ToLower(hashStr), "")
				cleanPhys = strings.ReplaceAll(cleanPhys, "."+strings.ToLower(hashStr), "")
				// Strip short hash (first 8 chars) - common in GoStream naming
				shortHash := strings.ToLower(hashStr[:8])
				cleanPhys = strings.ReplaceAll(cleanPhys, "_"+shortHash, "")
				cleanPhys = strings.ReplaceAll(cleanPhys, "."+shortHash, "")
			}
			cleanPhys = strings.ReplaceAll(cleanPhys, "_", ".")
			cleanPhys = strings.ReplaceAll(cleanPhys, " ", ".")

			for i, f := range files {
				if f.Length() == targetSize {
					matchesBySize++
					sizeMatchIndex = i + 1

					cleanTorr := strings.ToLower(f.Path())
					cleanTorr = strings.ReplaceAll(cleanTorr, "_", ".")

					// Check for suffix match or base name match after normalization
					if strings.HasSuffix(cleanPhys, cleanTorr) || strings.HasSuffix(cleanTorr, cleanPhys) || cleanTorr == cleanPhys {
						return hashStr, i + 1, nil
					}
				}
			}

			// V208: Fallback - If only one file matches the exact size, trust it.
			// This handles cases where Plex renaming makes name matching impossible.
			if matchesBySize == 1 {
				return hashStr, sizeMatchIndex, nil
			}
		}

		// Fallback (V1.4.0): Extract index from URL if torrent not in RAM or match failed.
		// Wake() will perform full discovery later.
		urlFileIdx := 0
		if strings.Contains(url, "index=") {
			iStart := strings.Index(url, "index=") + 6
			iEnd := strings.Index(url[iStart:], "&")
			if iEnd == -1 {
				iEnd = len(url) - iStart
			}
			if idx, err := strconv.Atoi(url[iStart : iStart+iEnd]); err == nil {
				urlFileIdx = idx
			}
		}
		return hashStr, urlFileIdx, nil
	}
	return "", 0, fmt.Errorf("file not found in torrent")
}

// V86-Gold: Fast deterministic Inode generation from filename
// Uses FNV-1a hash to avoid syscalls in Readdir (eliminates "stat storm")
// FNV-1a is extremely fast (non-crypto) and provides excellent distribution
//
// V86-Gold CRITICAL BUG FIX:
// V85 regression: Used e.Type() directly which returns Go's internal bits
//   - os.ModeDir = 0x80000000 (Go internal representation)
//   - FUSE/Samba/Kernel expect POSIX bits: syscall.S_IFDIR = 0x4000
//
// V86 fix: Use IsDir() + explicit POSIX mode bits (syscall.S_IFDIR/S_IFREG)
//   - IsDir() is still cached from ReadDir (no syscall)
//   - POSIX bits ensure proper FUSE/Samba/Linux kernel compatibility
func hashFilenameToInode(name string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(name))
	return h.Sum64()
}

type Metadata struct {
	URL, Path, ImdbID string
	Size              int64
	Mtime             time.Time
}

// V79-Profiling: Detailed timing metrics for Read operations
type ReadTiming struct {
	StartTime          time.Time
	MetadataLookupTime time.Duration
	HTTPFetchTime      time.Duration
	CacheHitTime       time.Duration
	TotalTime          time.Duration
	BytesRead          int
	IsStreaming        bool
	UsedCache          bool
}

// Global profiling statistics
type ProfilingStats struct {
	mu                 sync.RWMutex
	TotalReads         int64
	CacheHits          int64
	HTTPFetches        int64
	AvgHTTPLatency     time.Duration
	AvgCacheLatency    time.Duration
	AvgMetadataLatency time.Duration
	StreamingReads     int64
	NonStreamingReads  int64
}

var globalProfilingStats = &ProfilingStats{}

// RecordRead updates global profiling statistics
func (ps *ProfilingStats) RecordRead(t *ReadTiming) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	ps.TotalReads++

	if t.UsedCache {
		oldCount := ps.CacheHits
		ps.CacheHits++
		// Update average cache latency: (avg * old + new) / new
		ps.AvgCacheLatency = time.Duration((int64(ps.AvgCacheLatency)*oldCount + int64(t.CacheHitTime)) / ps.CacheHits)
	} else {
		oldCount := ps.HTTPFetches
		ps.HTTPFetches++
		// Update average HTTP latency: (avg * old + new) / new
		ps.AvgHTTPLatency = time.Duration((int64(ps.AvgHTTPLatency)*oldCount + int64(t.HTTPFetchTime)) / ps.HTTPFetches)
	}

	if t.IsStreaming {
		ps.StreamingReads++
	} else {
		ps.NonStreamingReads++
	}
}

// Stats returns current profiling statistics
func (ps *ProfilingStats) Stats() (totalReads, cacheHits, httpFetches, streamingReads int64, avgHTTPLatency, avgCacheLatency time.Duration, cacheHitRate float64) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()

	cacheHitRate = 0.0
	if ps.TotalReads > 0 {
		cacheHitRate = float64(ps.CacheHits) / float64(ps.TotalReads) * 100.0
	}

	return ps.TotalReads, ps.CacheHits, ps.HTTPFetches, ps.StreamingReads,
		ps.AvgHTTPLatency, ps.AvgCacheLatency, cacheHitRate
}

// sanitizeTime ensures a time is never "zero" (1970), falling back to current time
func sanitizeTime(t time.Time) uint64 {
	if t.IsZero() || t.Unix() <= 0 {
		return uint64(time.Now().Unix())
	}
	return uint64(t.Unix())
}

// fillAttrFromStat populates FUSE attributes from a standard syscall.Stat_t
// V138-POSIX: Centralized but preserving all critical fields for Samba/throughput
func fillAttrFromStat(st *syscall.Stat_t, out *fuse.Attr) {
	out.Ino = st.Ino
	out.Nlink = uint32(st.Nlink)
	out.Mode = uint32(st.Mode)
	out.Uid = st.Uid
	out.Gid = st.Gid
	out.Rdev = uint32(st.Rdev)
	// out.Blksize = uint32(st.Blksize) // CRITICAL: Samba uses for buffer sizing
	out.Blksize = uint32(globalConfig.FuseBlockSize) // Configurable block size (default 1MB)
	out.Blocks = uint64(st.Blocks)                   // CRITICAL: Samba uses for throughput calc
	out.Size = uint64(st.Size)

	// Sanitizzazione dei timestamp (V138-POSIX)
	// V196: Using time.Now() as cross-platform baseline for virtualized FUSE attributes
	// prevents build errors between Mac (Darwin) and Raspberry Pi (Linux)
	now := time.Now()
	out.Mtime = sanitizeTime(now)
	out.Atime = sanitizeTime(now)
	out.Ctime = sanitizeTime(now)
}

// fillAttrFromMetadata populates FUSE attributes from our internal Metadata
// V138-POSIX: Ensures standard fields are populated even for virtual files
func fillAttrFromMetadata(m *Metadata, out *fuse.Attr) {
	out.Size = uint64(m.Size)
	out.Mode = syscall.S_IFREG | 0644
	out.Uid, out.Gid = globalConfig.UID, globalConfig.GID
	out.Nlink = 1
	// out.Blksize = 4096                                 // Standard block size
	out.Blksize = uint32(globalConfig.FuseBlockSize) // Configurable block size (default 1MB)
	out.Blocks = (uint64(m.Size) + 511) / 512        // Estimate blocks based on size

	// Sanitizzazione dei timestamp (V138-POSIX)
	ts := sanitizeTime(m.Mtime)
	out.Mtime = ts
	out.Atime = ts
	out.Ctime = ts
}

// VirtualMkvRoot - nodo radice per file virtuali .mkv
type VirtualMkvRoot struct {
	fs.Inode
	sourcePath string
}

// Compile-time interface checks - verificano che implementiamo correttamente le interfacce
var _ = (fs.InodeEmbedder)((*VirtualMkvRoot)(nil))
var _ = (fs.NodeLookuper)((*VirtualMkvRoot)(nil))
var _ = (fs.NodeReaddirer)((*VirtualMkvRoot)(nil))
var _ = (fs.NodeGetattrer)((*VirtualMkvRoot)(nil))
var _ = (fs.NodeStatfser)((*VirtualMkvRoot)(nil))

func (r *VirtualMkvRoot) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	st := syscall.Stat_t{}
	if err := syscall.Stat(r.sourcePath, &st); err != nil {
		logger.Printf("ROOT GETATTR ERROR: stat failed for %s: %v", r.sourcePath, err)
		return ToErrno(err)
	}

	// Use centralized helper for attributes (V138-POSIX)
	fillAttrFromStat(&st, &out.Attr)

	// V134 Fix: Override Ino con la costante Root (fondamentale per Plex)
	out.Ino = InodeRoot
	out.Mode = syscall.S_IFDIR | 0755
	out.Size = 4096

	return 0
}

func (r *VirtualMkvRoot) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	fullPath := filepath.Join(r.sourcePath, name)

	// V138-Gold: Optimized Lookup - use memory metadata when possible
	if strings.HasSuffix(name, ".mkv") {
		// V138-Gold: Protect backend from request storms
		// V145-Priority: BYPASS limiter if this is an active, healthy stream
		// V170-DeadlockFix: Removed Priority logic as RateLimiter is bypassed
		// isPriority := false
		// if val, ok := playbackRegistry.Load(fullPath); ok {
		// 	if ps, ok := val.(*PlaybackState); ok && ps.GetStatus() {
		// 		isPriority = true
		// 	}
		// }

		// V170-DeadlockFix: Removed RateLimiter from Lookup to prevent FUSE freeze
		// if !isPriority {
		// 	if err := globalRateLimiter.Acquire(ctx); err != nil {
		// 		return nil, syscall.EAGAIN
		// 	}
		// }

		meta, err := getOrReadMeta(fullPath)
		if err == nil {
			addFileToInodeMap(fullPath, meta.URL)
			ino := getFileInodeFromMap(fullPath)
			node := &VirtualMkvNode{vMeta: meta}
			stable := fs.StableAttr{
				Mode: syscall.S_IFREG,
				Ino:  ino,
				Gen:  1,
			}
			child := r.NewInode(ctx, node, stable)
			fillAttrFromMetadata(meta, &out.Attr)
			out.Ino = ino
			return child, 0
		}
	}

	// Fallback for directories or other files
	st := syscall.Stat_t{}
	if err := syscall.Lstat(fullPath, &st); err != nil {
		return nil, syscall.ENOENT
	}

	if (st.Mode & syscall.S_IFMT) == syscall.S_IFDIR {
		node := &VirtualDirNode{physicalPath: fullPath}
		dirIno := getDirInodeFromMap(fullPath)
		stable := fs.StableAttr{
			Mode: syscall.S_IFDIR,
			Ino:  dirIno,
			Gen:  1,
		}
		child := r.NewInode(ctx, node, stable)
		fillAttrFromStat(&st, &out.Attr)
		out.Ino = dirIno
		return child, 0
	}

	node := &fs.LoopbackNode{RootData: &fs.LoopbackRoot{Path: r.sourcePath}}
	stable := fs.StableAttr{Mode: uint32(st.Mode & syscall.S_IFMT)}
	child := r.NewInode(ctx, node, stable)
	return child, 0
}

type nfsDirStream struct {
	entries []fuse.DirEntry
	index   int
}

func (s *nfsDirStream) HasNext() bool {
	return s.index < len(s.entries)
}

func (s *nfsDirStream) Next() (fuse.DirEntry, syscall.Errno) {
	e := s.entries[s.index]
	s.index++
	return e, 0
}

func (s *nfsDirStream) Seekdir(ctx context.Context, off uint64) syscall.Errno {
	if off == 0 {
		s.index = 0
		return 0
	}
	for i, e := range s.entries {
		if e.Off == off {
			s.index = i + 1
			return 0
		}
	}
	return 0
}

func (s *nfsDirStream) Close() {}

func (r *VirtualMkvRoot) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	// V140: Check cache first
	if entries, found := globalDirCache.Get(r.sourcePath); found {
		return &nfsDirStream{entries: entries}, 0
	}

	entries, err := os.ReadDir(r.sourcePath)
	if err != nil {
		logger.Printf("READDIR ERROR: %v", err)
		return nil, ToErrno(err)
	}

	result := make([]fuse.DirEntry, 0, len(entries))
	for i, e := range entries {
		mode := uint32(syscall.S_IFREG)
		var ino uint64
		fullPath := filepath.Join(r.sourcePath, e.Name())
		if e.IsDir() {
			mode = syscall.S_IFDIR
			ino = getDirInodeFromMap(fullPath)
		} else {
			ino = getFileInodeFromMap(fullPath)
		}

		result = append(result, fuse.DirEntry{
			Name: e.Name(),
			Mode: mode,
			Ino:  ino,
			Off:  uint64(i + 1),
		})
	}

	// V140: Update cache
	globalDirCache.Put(r.sourcePath, result)

	return &nfsDirStream{entries: result}, 0
}

// Statfs implements fs.NodeStatfser to provide filesystem statistics for Samba compatibility
func (r *VirtualMkvRoot) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	logger.Printf("=== STATFS === path=%s", r.sourcePath)

	// Calculate realistic values based on virtual files
	// Block size: standard 4KB
	out.Bsize = 4096

	// Total blocks: ~1TB virtual filesystem (arbitrary but realistic)
	out.Blocks = 250 * 1024 * 1024 // 1TB / 4KB blocks

	// Free blocks: ~500GB available (half of total, arbitrary)
	out.Bfree = 125 * 1024 * 1024
	out.Bavail = 125 * 1024 * 1024 // Available to non-root users

	// File counts: based on actual cache size
	// FASE 3.1: Use LRU cache Len() instead of Range()
	totalFiles := uint64(metaCache.Len())
	if totalFiles == 0 {
		totalFiles = 1000 // Fallback estimate if cache not populated
	}
	out.Files = totalFiles
	out.Ffree = 500 // Arbitrary free inodes

	// Namemax: maximum filename length
	out.NameLen = 255

	logger.Printf("STATFS: blocks=%d/%d files=%d/%d bsize=%d",
		out.Blocks, out.Bfree, out.Files, out.Ffree, out.Bsize)

	return 0
}

// VirtualDirNode - nodo per directory (movies, tv) con file .mkv virtuali
type VirtualDirNode struct {
	fs.Inode
	physicalPath string // Path fisico della directory (es. /mnt/torrserver/movies)
}

// Compile-time interface checks for VirtualDirNode
var _ fs.NodeReaddirer = (*VirtualDirNode)(nil)
var _ fs.NodeLookuper = (*VirtualDirNode)(nil)
var _ fs.NodeGetattrer = (*VirtualDirNode)(nil)
var _ fs.NodeUnlinker = (*VirtualDirNode)(nil) // FASE 4.2: Auto-remove support

func (d *VirtualDirNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	// V140: Check cache first
	if entries, found := globalDirCache.Get(d.physicalPath); found {
		return &nfsDirStream{entries: entries}, 0
	}

	entries, err := os.ReadDir(d.physicalPath)
	if err != nil {
		logger.Printf("READDIR DIR ERROR: %v", err)
		return nil, ToErrno(err)
	}

	result := make([]fuse.DirEntry, 0, len(entries))
	for i, e := range entries {
		fullPath := filepath.Join(d.physicalPath, e.Name())
		if e.IsDir() {
			ino := getDirInodeFromMap(fullPath)
			result = append(result, fuse.DirEntry{
				Name: e.Name(),
				Mode: syscall.S_IFDIR,
				Ino:  ino,
				Off:  uint64(i + 1),
			})
		} else if strings.HasSuffix(e.Name(), ".mkv") {
			ino := getFileInodeFromMap(fullPath)
			result = append(result, fuse.DirEntry{
				Name: e.Name(),
				Mode: syscall.S_IFREG,
				Ino:  ino,
				Off:  uint64(i + 1),
			})
		}
	}

	// V140: Update cache
	globalDirCache.Put(d.physicalPath, result)

	return &nfsDirStream{entries: result}, 0
}

func (d *VirtualDirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	fullPath := filepath.Join(d.physicalPath, name)

	// V138-Gold: Optimized Lookup - use memory metadata when possible
	if strings.HasSuffix(name, ".mkv") {
		// V138-Gold: Protect backend from request storms
		// V145-Priority: BYPASS limiter if this is an active, healthy stream
		// V170-DeadlockFix: Removed Priority logic as RateLimiter is bypassed
		// isPriority := false
		// // Note: d.Lookup fullPath construction matches storage key
		// if val, ok := playbackRegistry.Load(fullPath); ok {
		// 	if ps, ok := val.(*PlaybackState); ok && ps.GetStatus() {
		// 		isPriority = true
		// 	}
		// }

		// V170-DeadlockFix: Removed RateLimiter from Lookup to prevent FUSE freeze
		// if !isPriority {
		// 	if err := globalRateLimiter.Acquire(ctx); err != nil {
		// 		return nil, syscall.EAGAIN
		// 	}
		// }

		meta, err := getOrReadMeta(fullPath)
		if err == nil {
			addFileToInodeMap(fullPath, meta.URL)
			ino := getFileInodeFromMap(fullPath)
			node := &VirtualMkvNode{vMeta: meta}
			stable := fs.StableAttr{
				Mode: syscall.S_IFREG,
				Ino:  ino,
				Gen:  1,
			}
			child := d.NewInode(ctx, node, stable)
			fillAttrFromMetadata(meta, &out.Attr)
			out.Ino = ino
			return child, 0
		}
	}

	// Fallback for directories
	st := syscall.Stat_t{}
	if err := syscall.Stat(fullPath, &st); err != nil {
		return nil, syscall.ENOENT
	}

	if (st.Mode & syscall.S_IFMT) == syscall.S_IFDIR {
		node := &VirtualDirNode{physicalPath: fullPath}
		dirIno := getDirInodeFromMap(fullPath)
		stable := fs.StableAttr{
			Mode: syscall.S_IFDIR,
			Ino:  dirIno,
			Gen:  1,
		}
		child := d.NewInode(ctx, node, stable)
		fillAttrFromStat(&st, &out.Attr)
		out.Ino = dirIno
		return child, 0
	}

	return nil, syscall.ENOENT
}

// Getattr returns directory attributes
func (d *VirtualDirNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	// V86-Gold: Removed hot path logging

	// Critical fix for Samba: Copy ALL metadata from physical directory
	st := syscall.Stat_t{}
	if err := syscall.Stat(d.physicalPath, &st); err != nil {
		logger.Printf("GETATTR DIR ERROR: %v", err)
		return ToErrno(err)
	}

	// Use centralized helper for attributes (V138-POSIX)
	fillAttrFromStat(&st, &out.Attr)

	// V134-fix: Override Ino con hash del path completo per evitare collisioni (Season.01)
	out.Ino = getDirInodeFromMap(d.physicalPath)

	// Override ONLY Mode and Size to ensure directory permissions and Samba compliance
	out.Mode = syscall.S_IFDIR | 0755
	out.Size = 4096

	return 0
}

// Unlink handles file deletion and triggers torrent auto-remove (FASE 4.2)
func (d *VirtualDirNode) Unlink(ctx context.Context, name string) syscall.Errno {
	logger.Printf("=== UNLINK === dir=%s file=%s", d.physicalPath, name)

	// Only handle .mkv files
	if !strings.HasSuffix(name, ".mkv") {
		logger.Printf("UNLINK: not an mkv file, skipping auto-remove")
		return syscall.EPERM // Not permitted for non-mkv files
	}

	fullPath := filepath.Join(d.physicalPath, name)

	// V271: Force-close active pump and handles BEFORE removing torrent.
	// Without this, smbd D-states when trying to delete a file with an active
	// read (e.g., torrent with no seeders blocks FUSE read indefinitely).
	if val, ok := activePumps.Load(fullPath); ok {
		ps := val.(*NativePumpState)
		if ps.cancel != nil {
			ps.cancel()
		}
		activePumps.Delete(fullPath)
		logger.Printf("UNLINK: force-terminated active pump for %s", name)
	}
	// Close all handles referencing this file
	activeHandles.Range(func(key, value interface{}) bool {
		h := key.(*MkvHandle)
		if h.path == fullPath {
			if h.nativeReader != nil {
				h.nativeReader.Close()
			}
			activeHandles.Delete(h)
			logger.Printf("UNLINK: force-closed handle for %s", name)
		}
		return true
	})

	// Extract hash and remove torrent from GoStorm
	success, err := globalTorrentRemover.RemoveTorrentFromFile(fullPath)
	if err != nil {
		logger.Printf("UNLINK ERROR: failed to remove torrent: %v", err)
		// Continue with file deletion even if torrent removal fails
	} else if success {
		logger.Printf("UNLINK: torrent successfully removed from GoStorm")
	} else {
		logger.Printf("UNLINK: no matching torrent found (already removed?), but hash was blacklisted")
	}

	// Delete physical .mkv file
	if err := os.Remove(fullPath); err != nil {
		logger.Printf("UNLINK ERROR: failed to delete file: %v", err)
		return ToErrno(err)
	}

	// V140: Invalidate directory cache
	globalDirCache.Delete(d.physicalPath)

	logger.Printf("UNLINK COMPLETE: file deleted successfully")
	return 0
}

// VirtualMkvNode - nodo per singolo file .mkv virtuale
type VirtualMkvNode struct {
	fs.Inode
	vMeta *Metadata
}

// Compile-time interface checks
var _ fs.NodeGetattrer = (*VirtualMkvNode)(nil)
var _ fs.NodeOpener = (*VirtualMkvNode)(nil)

func (n *VirtualMkvNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	// V138-Gold: Protect backend from request storms during heavy navigation
	// V145-Priority: BYPASS limiter if this is an active, healthy stream
	// V170-DeadlockFix: Removed Priority logic as RateLimiter is bypassed
	// isPriority := false
	// if val, ok := playbackRegistry.Load(n.vMeta.Path); ok {
	// 	if ps, ok := val.(*PlaybackState); ok && ps.GetStatus() {
	// 		isPriority = true
	// 	}
	// }

	// V170-DeadlockFix: Removed RateLimiter from Getattr to prevent FUSE freeze
	// if !isPriority {
	// 	if err := globalRateLimiter.Acquire(ctx); err != nil {
	// 		return syscall.EAGAIN
	// 	}
	// }

	// V138-Gold: RAM-ONLY metadata to prevent NFS timeouts
	fillAttrFromMetadata(n.vMeta, &out.Attr)
	out.Ino = getFileInodeFromMap(n.vMeta.Path)
	return 0
}

// V246: Proactive Context Switch on File Open
// When the OS attempts to open an MKV, we signal the cache to switch context immediately.
// This increments the SessionID, invalidating all old data BEFORE the first Read() happens.
func (n *VirtualMkvNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if globalConfig.LogLevel == "DEBUG" {
		logger.Printf("=== OPEN VIRTUAL === path=%s", n.vMeta.Path)
	}

	// PROACTIVE CLEANUP TRIGGER (V246): must be sync before any Read() can arrive.
	raCache.SwitchContext(n.vMeta.Path)

	// V185: Extract Hash for Priority management
	hashStr, urlFileIdx := ExtractHashAndIndex(n.vMeta.URL)

	// V265: hasWarmup — async Wake for instant Open only if BOTH head and tail are ready.
	hasWarmup := false
	if diskWarmup != nil && hashStr != "" {
		headReady := diskWarmup.GetAvailableRange(hashStr, urlFileIdx) > 0
		tailReady := diskWarmup.GetTailRange(hashStr, urlFileIdx) > 0
		if headReady && tailReady {
			hasWarmup = true
		}
	}

	// V237-Fix: Robust Synchronous Native Wake in Open phase.
	magnetCandidate := n.vMeta.URL
	if hashStr != "" && (strings.HasPrefix(n.vMeta.URL, "http://") || strings.HasPrefix(n.vMeta.URL, "https://")) {
		magnetCandidate = "magnet:?xt=urn:btih:" + hashStr
	}

	// V265: Dual-state Wake (Async with Warmup / Sync without).
	if nativeBridge != nil && magnetCandidate != "" {
		if hasWarmup {
			safeGo(func() {
				_ = nativeBridge.Wake(magnetCandidate, urlFileIdx)
			})
		} else {
			_ = nativeBridge.Wake(magnetCandidate, urlFileIdx)
		}
	}

	// V148-Fix: Track both movies and TV shows for priority
	if val, exists := playbackRegistry.Load(n.vMeta.Path); !exists {
		playbackRegistry.Store(n.vMeta.Path, &PlaybackState{
			Path:     n.vMeta.Path,
			Hash:     hashStr,
			ImdbID:   n.vMeta.ImdbID,
			OpenedAt: time.Now(),
		})
	} else {
		// V216-Enhancement: Silent Re-Open Priority Restoration
		if val != nil {
			state := val.(*PlaybackState)
			state.mu.Lock()
			state.OpenedAt = time.Now()
			state.IsStopped = false // V272: Reset stop flag on re-open

			// V1.4.0-Fix: Only restore priority if there was a RECENT webhook confirmation (<30m).
			// This prevents stale Healthy flags from metadata scans causing zombie torrents.
			recentlyConfirmed := !state.ConfirmedAt.IsZero() && time.Since(state.ConfirmedAt) < 30*time.Minute
			isHealthy := state.IsHealthy
			state.mu.Unlock()

			if isHealthy && recentlyConfirmed && state.Hash != "" {
				hHash := metainfo.NewHashFromHex(state.Hash)
				if t := web.BTS.GetTorrent(hHash); t != nil {
					if !t.IsPriority {
						t.IsPriority = true
						logger.Printf("[NativeBridge] Priority RESTORED for Silent Re-Open: %s", state.Hash)
					}
				}
			}
		}
	}

	now := time.Now()

	// V690: Direct ID Injection — skip resolveTargetFile for warmup HIT path.
	// Warmup data was written with (hashStr, urlFileIdx) as the key, so they are
	// guaranteed to be correct. Saves 20-500ms per open (5×100ms retry loop).
	// V272 (sync Wake fallback) is no longer needed — hasWarmup path bypasses resolution entirely.
	var finalHash string
	var fileIdx int
	var isNative bool

	if hasWarmup && hashStr != "" {
		finalHash = hashStr
		fileIdx = urlFileIdx
		isNative = true
	} else {
		var err error
		finalHash, fileIdx, err = resolveTargetFile(n.vMeta.URL, n.vMeta.Size, n.vMeta.Path)
		isNative = (err == nil)
		if !isNative && globalConfig.LogLevel == "DEBUG" {
			logger.Printf("[NativeBridge] Resolution failed for %s: %v. Access will rely on cache/retry.", filepath.Base(n.vMeta.Path), err)
		}
	}

	h := &MkvHandle{
		url:              n.vMeta.URL,
		magnet:           magnetCandidate, // Store for potential re-wake
		size:             n.vMeta.Size,
		path:             n.vMeta.Path,
		lastTime:         now,
		lastOff:          -1,
		lastActivityTime: now,       // Initialize activity tracking
		hasWarmup:        hasWarmup, // V272: Scanner-aware retention
	}

	if isNative {
		h.hash = finalHash
		h.fileID = fileIdx

		// V264: Simplified proactive pump start using centralized method
		h.startNativePump(finalHash, fileIdx)
	}

	// V182: Register for cleanup
	activeHandles.Store(h, true)

	return h, 0, 0
}

type MkvHandle struct {
	url, path        string
	size             int64
	lastOff          int64 // V226: Atomic tracking for pump sync
	lastLen          int
	lastTime         time.Time
	lastActivityTime time.Time // FASE 2.3: Idle Grace Reset
	monitorStarted   bool      // Impedisce monitoraggi duplicati per lo stesso handle
	lastGlobalUpdate time.Time // Debounce global activity updates

	// V226: Unified Bridge Reader (Zero-Network)
	nativeReader *NativeReader
	hash         string     // V227: Store hash for stateless FetchBlock
	magnet       string     // V237: Store magnet link for Native Wake
	fileID       int        // V227: Store fileID for stateless FetchBlock
	mu           sync.Mutex // V227: Protecting activity and timing fields
	pumpCancel   context.CancelFunc
	hasSlot      bool
	isWatching   bool // V258: Handle is attached to a shared pump
	hasWarmup    bool // V272: True if both head+tail warmup available at Open time
}

// V264: startNativePump centralizes the logic for acquiring a slot and starting the background pump.
// It can be called from Open (proactive) or Read (rescue for late resolution).
func (h *MkvHandle) startNativePump(finalHash string, fileIdx int) {
	// 1. Verify we don't already have a slot or an active pump
	if h.hasSlot {
		return
	}

	// V238: Strategic Reserve Logic - Hardened for Pi 4
	isHealthy := false
	if val, ok := playbackRegistry.Load(h.path); ok {
		if ps, ok := val.(*PlaybackState); ok && ps.GetStatus() {
			isHealthy = true
		}
	}

	// Only allow unconfirmed (background scan) streams to take a slot if we have at least 20 free.
	canTakeSlot := true
	if !isHealthy && len(masterDataSemaphore) >= (globalConfig.MasterConcurrencyLimit-5) {
		canTakeSlot = false
		logger.Printf("[StrategicReserve] Denying pump slot to background scan (Saturation: %d/%d): %s",
			len(masterDataSemaphore), globalConfig.MasterConcurrencyLimit, filepath.Base(h.path))
	}

	if !canTakeSlot {
		return
	}

	// V259: Lock pump creation to prevent race condition
	pumpCreationMu.Lock()

	// V260: Check activePumps first (Singleton pattern)
	if val, ok := activePumps.Load(h.path); ok {
		ps := val.(*NativePumpState)
		atomic.AddInt32(&ps.refCount, 1)
		h.mu.Lock()
		h.hasSlot = true
		h.isWatching = true
		h.nativeReader = ps.reader
		h.pumpCancel = ps.cancel
		h.mu.Unlock()
		logger.Printf("[V264] Attached to existing pump (Refs: %d): %s", atomic.LoadInt32(&ps.refCount), filepath.Base(h.path))
		pumpCreationMu.Unlock()
		return
	}

	// V272: Optimization - Release global mutex before blocking on semaphore
	// or performing SSD I/O. We use a secondary check inside the semaphore block.
	pumpCreationMu.Unlock()

	// V226: Unified Concurrency - Try to acquire a persistent slot
	select {
	case masterDataSemaphore <- struct{}{}:
		// Double-check activePumps after acquiring semaphore to prevent race
		pumpCreationMu.Lock()
		if val, ok := activePumps.Load(h.path); ok {
			// Someone else created it while we were waiting for the semaphore
			<-masterDataSemaphore // Release our newly acquired slot
			ps := val.(*NativePumpState)
			atomic.AddInt32(&ps.refCount, 1)
			h.mu.Lock()
			h.hasSlot = true
			h.isWatching = true
			h.nativeReader = ps.reader
			h.pumpCancel = ps.cancel
			h.mu.Unlock()
			pumpCreationMu.Unlock()
			return
		}

		h.hasSlot = true
		h.nativeReader = nativeBridge.NewStreamReader(finalHash, fileIdx, h.size)

		// Register in activePumps BEFORE releasing lock, but BEFORE doing I/O
		pumpCtx, pumpCancel := context.WithCancel(context.Background())
		h.pumpCancel = pumpCancel
		sharedState := &NativePumpState{
			cancel:   pumpCancel,
			reader:   h.nativeReader,
			path:     h.path,
			refCount: 1,
		}
		activePumps.Store(h.path, sharedState)
		pumpCreationMu.Unlock() // SUCCESS: Shared state registered, global lock released

		logger.Printf("[V264] Native Pump Started (Slot Acquired): %s", filepath.Base(h.path))

		// Start background pump — resume from last cached position OR end of disk warmup
		resumeOffset := raCache.MaxCachedOffset(h.path)

		// V261: Strategic pump skip — start pump near end of warmup zone.
		if diskWarmup != nil && h.hash != "" {
			diskOffset := diskWarmup.GetAvailableRange(h.hash, h.fileID)

			// V262: Header Validation (Performed OUTSIDE global mutex)
			if diskOffset > 0 {
				buf := make([]byte, 16)
				nRead, _ := diskWarmup.ReadAt(h.hash, h.fileID, buf, 0)
				isHeaderValid := false
				if nRead >= 4 {
					if (buf[0] == 0x1A && buf[1] == 0x45 && buf[2] == 0xDF && buf[3] == 0xA3) ||
						(buf[4] == 'f' && buf[5] == 't' && buf[6] == 'y' && buf[7] == 'p') ||
						(buf[0] == 0x00 && buf[1] == 0x00 && buf[2] == 0x00) {
						isHeaderValid = true
					}
				}

				if !isHeaderValid {
					logger.Printf("[DiskWarmup] INVALID HEADER detected for %s. Resetting cache.", h.hash[:8])
					diskWarmup.RemoveHash(h.hash)
					diskOffset = 0
				}
			}

			if diskOffset > 16*1024*1024 {
				safetyMargin := int64(16 * 1024 * 1024)
				skipOffset := diskOffset - safetyMargin
				if skipOffset > resumeOffset {
					resumeOffset = skipOffset
					logger.Printf("[DiskWarmup] PUMP SKIP: Starting from %.1fMB (Disk: %.1fMB, Margin: 16MB)",
						float64(resumeOffset)/(1<<20), float64(diskOffset)/(1<<20))
				}
			}
		}

		// V700: Anchor pump to player position if MaxCachedOffset is stale-high.
		// After a forward jump (V284), MaxCachedOffset stays at the jump destination
		// (e.g. 1344MB for a 1417MB file). Subsequent pump restarts all start at 1344MB
		// → immediate EOF → V238/V239 rapid loop. Fix: if resumeOffset is more than
		// 2 chunks ahead of the player, start the pump at the player's position instead.
		// V700b: Also handle new handles (lastOff=-1): MaxCachedOffset may be stale from
		// a previous session (same path re-opened). Player hasn't read yet → start from 0.
		if playerOff := atomic.LoadInt64(&h.lastOff); playerOff > 0 {
			chunkSize := int64(globalConfig.ReadAheadBase)
			if chunkSize == 0 {
				chunkSize = 16 * 1024 * 1024
			}
			if resumeOffset > playerOff+chunkSize*2 {
				aligned := (playerOff / chunkSize) * chunkSize
				logger.Printf("[V700] Pump anchored to player: %.1fMB (MaxCached was %.1fMB)",
					float64(aligned)/(1<<20), float64(resumeOffset)/(1<<20))
				resumeOffset = aligned
			}
		} else if playerOff < 0 && resumeOffset > 0 {
			logger.Printf("[V700] New handle: reset stale MaxCachedOffset %.1fMB → 0",
				float64(resumeOffset)/(1<<20))
			resumeOffset = 0
		}

		if resumeOffset > 0 {
			atomic.StoreInt64(&h.lastOff, resumeOffset)
		}

		safeGo(func() {
			h.nativePump(pumpCtx, resumeOffset)
		})
	default:
		// If slots are full, it will fall back to per-request slots in Read
		logger.Printf("[MasterSemaphore] Limit reached, %s will use Fallback mode", filepath.Base(h.path))
	}
}

// V226: nativePump reads continuously from the Native pipe and fills raCache.
// FUSE Read() never touches the NativeReader directly — it reads from raCache.
func (h *MkvHandle) nativePump(ctx context.Context, startOffset int64) {
	// V260: Capture reader at pump start — Release() cannot nil this
	pumpReader := h.nativeReader
	if pumpReader == nil {
		logger.Printf("[V260] Native Pump Error: reader is nil at startup for %s", filepath.Base(h.path))
		return
	}

	if h.hash == "" {
		// V260: Try one last time to resolve the hash if it was missing during Open
		if hash, fileID, err := resolveTargetFile(h.url, h.size, h.path); err == nil {
			h.hash = hash
			h.fileID = fileID
			logger.Printf("[V260] Pump late resolution success: %s", h.hash[:8])
		} else {
			logger.Printf("[V260] Native Pump Warning: hash is empty for %s, warmup will be disabled", filepath.Base(h.path))
		}
	}
	defer func() {
		h.mu.Lock()
		// V258: Deregister from global registry (Safety double-check)
		activePumps.Delete(h.path)

		if h.hasSlot {
			select {
			case <-masterDataSemaphore:
				// Slot released
			default:
				// Should not happen
			}
			h.hasSlot = false
		}
		// V260: Close the captured reader
		pumpReader.Close()
		h.mu.Unlock()
		logger.Printf("[V239] Native Pump Goroutine Ended: %s", filepath.Base(h.path))
	}()

	// V238: Refined chunkSize (16MB) for stability on Pi 4.
	chunkSize := int64(globalConfig.ReadAheadBase)
	if chunkSize == 0 {
		chunkSize = 8 * 1024 * 1024
	}

	// Track bytes pumped in this session for the Grace Period Boost
	pumpedBytes := int64(0)
	// Align startOffset to chunk boundary
	offset := (startOffset / chunkSize) * chunkSize

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// V239: Release-on-Idle Logic
		// If the player hasn't requested data, yield the slot based on health status.
		// V262: Intelligent Timeout - confirmed playback gets 2 hours, background scans get 45s.
		h.mu.Lock()
		lastAct := h.lastActivityTime
		h.mu.Unlock()

		timeoutLimit := 45 * time.Second
		if val, ok := playbackRegistry.Load(h.path); ok {
			if ps, ok := val.(*PlaybackState); ok && ps.GetStatus() {
				timeoutLimit = 2 * time.Hour
			}
		}

		if time.Since(lastAct) > timeoutLimit {
			logger.Printf("[V262] Idle timeout (%v) for %s - yielding slot", timeoutLimit, filepath.Base(h.path))
			return
		}

		// V260: Advanced Player Sync - Find the most advanced player position among all handles
		playerOff := int64(0)
		activeHandles.Range(func(key, value interface{}) bool {
			handle := key.(*MkvHandle)
			if handle.path == h.path {
				off := atomic.LoadInt64(&handle.lastOff)
				if off > playerOff {
					playerOff = off
				}
			}
			return true
		})

		// V284: Reactive Jump — if player has seeked more than ReadAheadBudget ahead of the
		// pump, snap the pump to (playerOff / chunkSize) * chunkSize so anacrolix
		// prioritises the new pieces exactly where the player is.
		jumpThreshold := int64(globalConfig.ReadAheadBudget)
		if playerOff > offset+jumpThreshold {
			// V284-Fix: Align perfectly to the nearest chunk boundary.
			// Never step back a full chunkSize arbitrarily (Audit finding).
			jumpTo := (playerOff / chunkSize) * chunkSize
			if jumpTo < 0 {
				jumpTo = 0
			}
			logger.Printf("[V284] Pump jump: %dMB → %dMB (player at %dMB, gap %dMB)",
				offset/(1024*1024), jumpTo/(1024*1024), playerOff/(1024*1024),
				(playerOff-offset)/(1024*1024))
			offset = jumpTo
			pumpedBytes = 0 // reset grace period so throttle doesn't fire immediately
		}

		// V238: Adaptive Throttle Logic with Grace Period (64MB)
		if pumpedBytes > 64*1024*1024 {
			isHealthy := false
			// V260: Check health for ANY handle on this path
			if val, ok := playbackRegistry.Load(h.path); ok {
				if ps, ok := val.(*PlaybackState); ok && ps.GetStatus() {
					isHealthy = true
				}
			}

			if !isHealthy {
				// Throttle background tasks: 150ms delay between 16MB chunks.
				time.Sleep(150 * time.Millisecond)
			}
		}

		// V260: Use the captured reader (immune to outside nilling)
		stop, nextOffset := h.nativePumpChunk(pumpReader, offset, chunkSize, playerOff)
		if stop {
			// V288: Don't die if not at EOF — the error might be transient
			// (interrupted by seek, Stream() reconnect, piece timeout).
			if offset < h.size {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			return // genuine EOF
		}

		pumpedBytes += (nextOffset - offset)
		offset = nextOffset
	}
}

// nativePumpChunk handles reading a single chunk from the Native pipe and returning the buffer to the pool.
// V227: Uses defer to ensure buffer return even on panic.
func (h *MkvHandle) nativePumpChunk(r *NativeReader, offset, chunkSize, playerOff int64) (stop bool, nextOffset int64) {
	// Don't pump beyond file size
	if offset >= h.size {
		return true, offset
	}

	// 2. Throttle: Maintain a proactive buffer of 256MB
	// V260: Advanced Player Sync for throttle (captured from pump loop)
	budget := globalConfig.ReadAheadBudget
	diff := offset - playerOff

	// V260: Emergency Overdrive.
	// If the player is still in the Disk Warmup zone (<128MB),
	// we DISABLE throttling. The pump must run at full speed to bridge the handover.
	if playerOff < warmupFileSize {
		// No sleep, full speed ahead.
	} else if diff > budget {
		// Hard limit reached: Wait for player to advance.
		time.Sleep(100 * time.Millisecond)
		return false, offset
	} else if diff > (budget * 7 / 10) {
		// Soft limit (70%): Slow down gradually.
		sleepMs := (diff - (budget * 7 / 10)) / (1024 * 1024)
		if sleepMs > 50 {
			sleepMs = 50
		}
		if sleepMs > 0 {
			time.Sleep(time.Duration(sleepMs) * time.Millisecond)
		}
	}

	// Check if this chunk is already cached in RAM
	if data := raCache.Get(h.path, offset, offset); data != nil {
		// V260: Even if in RAM, we might need to write it to Disk Warmup if not there yet
		if diskWarmup != nil && h.hash != "" && offset < warmupFileSize {
			diskWarmup.WriteChunk(h.hash, h.fileID, data, offset)
		}
		return false, offset + chunkSize
	}

	end := offset + chunkSize
	if end > h.size {
		end = h.size
	}

	// Use buffer from pool to reduce allocations
	bufPtr := readBufferPool.Get().(*[]byte)
	defer readBufferPool.Put(bufPtr)

	n, err := r.ReadAt((*bufPtr)[:end-offset], offset)
	if n > 0 {
		raCache.Put(h.path, offset, offset+int64(n)-1, (*bufPtr)[:n])
		// V256: Write to disk warmup cache (first 128MB only)
		// V260: Sequential write via Async Worker
		if diskWarmup != nil && h.hash != "" && offset < warmupFileSize {
			diskWarmup.WriteChunk(h.hash, h.fileID, (*bufPtr)[:n], offset)
		}
	}

	if err != nil {
		return true, offset + int64(n)
	}

	return false, offset + int64(n)
}

// safeGo runs a function in a new goroutine with panic recovery.
// V227: Prevents background tasks from crashing the entire FUSE server.
func safeGo(fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[PANIC] Background goroutine recovered: %v", r)
			}
		}()
		fn()
	}()
}

// Compile-time interface checks for MkvHandle
var _ fs.FileReader = (*MkvHandle)(nil)
var _ fs.FileReleaser = (*MkvHandle)(nil)

func (h *MkvHandle) Read(fuseCtx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	// V79-Profiling: Start timing
	now := time.Now()
	timing := &ReadTiming{StartTime: now}
	defer func() {
		timing.TotalTime = time.Since(timing.StartTime)
		globalProfilingStats.RecordRead(timing)
	}()

	if off >= h.size {
		return fuse.ReadResultData(nil), 0
	}

	// V227: Protect activity tracking with mutex
	h.mu.Lock()
	// V260: Late Metadata Recovery
	// If Open failed to resolve the hash (metadata lag), retry now.
	if h.hash == "" && h.url != "" {
		if hash, fileID, err := resolveTargetFile(h.url, h.size, h.path); err == nil {
			h.hash = hash
			h.fileID = fileID
			logger.Printf("[LateResolution] Recovered hash for %s: %s", filepath.Base(h.path), h.hash[:8])

			// V264: Late Pump Rescue
			// We just resolved the hash! If we don't have a slot yet, start the pump now.
			if !h.hasSlot {
				go h.startNativePump(h.hash, h.fileID)
			}
		}
	}

	idleTime := now.Sub(h.lastActivityTime)
	isFirstBlock := (off == 0) || (idleTime > time.Duration(globalConfig.WarmStartIdleSeconds)*time.Second)
	h.lastActivityTime = now

	// FASE 3.3: Track file activity in cleanup manager (debounced)
	if now.Sub(h.lastGlobalUpdate) > 1*time.Minute {
		globalCleanupManager.UpdateActivity(h.path)
		h.lastGlobalUpdate = now
	}
	h.mu.Unlock()

	// V285: Eager lastOff update so pump sees seek target immediately
	// V570: Only streaming reads steer the pump (SSD reads are handled above)
	prevOff := atomic.LoadInt64(&h.lastOff)
	atomic.StoreInt64(&h.lastOff, off)

	// V560: Detect pre-confirmation tail probe to suppress V286 interrupt.
	// V560-Dynamic: Threshold is 5% of file size, capped between 64MB and 2GB.
	// This covers "far" metadata probes in large Remux files (e.g. The Long Walk).
	dynamicThreshold := h.size / 20 // 5%
	if dynamicThreshold < 64*1024*1024 {
		dynamicThreshold = 64 * 1024 * 1024
	}
	if dynamicThreshold > 2*1024*1024*1024 {
		dynamicThreshold = 2 * 1024 * 1024 * 1024
	}

	isTailProbe := false
	if h.hash != "" && h.size > dynamicThreshold && off >= h.size-dynamicThreshold {
		if val, ok := playbackRegistry.Load(h.path); ok {
			ps := val.(*PlaybackState)
			ps.mu.RLock()
			isTailProbe = ps.ConfirmedAt.IsZero()
			ps.mu.RUnlock()
		} else {
			isTailProbe = true // no registry entry = unconfirmed discovery phase
		}
	}

	// V286: Seek-Aware Interrupt. If player jumped far, wake up the pump immediately.
	// V560: Skip for pre-confirmation tail reads — served by SSD, pump must not be interrupted.
	budget := int64(globalConfig.ReadAheadBudget)
	if h.nativeReader != nil && !isTailProbe && (off > prevOff+budget || (prevOff > off+budget && prevOff > 0)) {
		h.nativeReader.Interrupt()
		logger.Printf("[V286] Interrupt pump for seek: %dMB → %dMB",
			prevOff/(1024*1024), off/(1024*1024))
	}

	// V256: Disk warmup cache — serves first 128MB from SSD while torrent activates.
	// V261: Read directly into dest — no pool buffer, no 2MB over-read.
	// Skip warmup if this read would straddle the 128MB boundary (partial read
	// confuses players → micro-stutter). Let raCache serve the full read instead.
	if diskWarmup != nil && h.hash != "" && off < warmupFileSize && off+int64(len(dest)) <= warmupFileSize {
		n, _ := diskWarmup.ReadAt(h.hash, h.fileID, dest, off)
		if n == len(dest) {
			timing.UsedCache = true
			timing.BytesRead = n
			if off == 0 {
				logger.Printf("[DiskWarmup] HIT %s off=0 (%dKB)", filepath.Base(h.path), n/1024)
			}

			// V570: Restore lastOff for Head Warmup to keep pump in sync during start.
			atomic.StoreInt64(&h.lastOff, off)

			h.mu.Lock()
			h.lastLen = n
			h.lastTime = now
			h.mu.Unlock()
			return fuse.ReadResultData(dest[:n]), 0
		}
	}

	// V265: Tail warmup — serve last 16MB from SSD for MKV Cues/seek index.
	// V560: Discovery-Only Tail Warmup. isTailProbe=true means off is in tail area AND
	// ConfirmedAt.IsZero(). Post-confirmation tail reads fall through to the pump for fresh data.
	if isTailProbe {
		// V560-Fix: Tail reads must NOT steer the pump. Restore lastOff to prevOff so
		// V284 doesn't jump the pump to the cold end-of-file on the next tick.
		atomic.StoreInt64(&h.lastOff, prevOff)
		n, _ := diskWarmup.ReadTail(h.hash, h.fileID, dest, off, h.size)
		if n == len(dest) {
			timing.UsedCache = true
			timing.BytesRead = n
			h.mu.Lock()
			h.lastLen, h.lastTime = n, now
			h.mu.Unlock()
			return fuse.ReadResultData(dest[:n]), 0
		}

		// V560-Optimization: If SSD tail miss during probe, use stateless FetchBlock
		// to keep the head pump alive. Discovery is non-critical, safety first.
		nFetch, err := nativeBridge.FetchBlock(h.hash, h.fileID, off, dest)
		if err == nil && nFetch > 0 {
			// V1.4.0-Fix: Save captured tail data to SSD for next time
			if diskWarmup != nil {
				diskWarmup.WriteTail(h.hash, h.fileID, dest[:nFetch], off, h.size)
			}

			timing.UsedCache = false
			timing.BytesRead = nFetch
			h.mu.Lock()
			h.lastLen, h.lastTime = nFetch, now
			h.mu.Unlock()
			return fuse.ReadResultData(dest[:nFetch]), 0
		}
	}
	end := off + int64(len(dest)) - 1

	// V79-Profiling: Check cache hit
	cacheStart := time.Now()
	if n := raCache.CopyTo(h.path, off, end, dest); n > 0 {
		timing.CacheHitTime = time.Since(cacheStart)
		timing.UsedCache = true
		timing.BytesRead = n

		// V257: Sync player position even on cache hit to prevent pump stall
		atomic.StoreInt64(&h.lastOff, off)

		// V227-Fix: Predictive prefetch on raCache hit path.
		// V257-Optimization: Intelligent Prefetch (Rescue Mode)
		// Simplified in V295: Removed expensive MaxCachedOffset O(N) scan.
		chunkSize := int64(globalConfig.ReadAheadBase)
		nextChunkStart := (off/chunkSize + 1) * chunkSize

		if (!h.hasSlot || (nextChunkStart-off < chunkSize/4)) && !raCache.Exists(h.path, nextChunkStart) {
			prefetchKey := fmt.Sprintf("%s:%d", h.path, nextChunkStart)
			if _, loaded := inFlightPrefetches.LoadOrStore(prefetchKey, true); !loaded {
				goStart, goKey, goHash, goFileID := nextChunkStart, prefetchKey, h.hash, h.fileID
				safeGo(func() {
					defer inFlightPrefetches.Delete(goKey)
					fetchEnd := goStart + chunkSize - 1
					if fetchEnd >= h.size {
						fetchEnd = h.size - 1
					}
					if fetchEnd <= goStart {
						return
					}

					select {
					case masterDataSemaphore <- struct{}{}:
						defer func() { <-masterDataSemaphore }()
					case <-time.After(500 * time.Millisecond):
						return
					}

					// Zero-Network Native Prefetch (V227 Phase 7)
					if goHash != "" {
						bufPtr := readBufferPool.Get().(*[]byte)
						defer readBufferPool.Put(bufPtr)

						limit := int64(len(*bufPtr))
						if fetchEnd-goStart+1 < limit {
							limit = fetchEnd - goStart + 1
						}

						n, err := nativeBridge.FetchBlock(goHash, goFileID, goStart, (*bufPtr)[:limit])
						if err == nil && n > 0 {
							raCache.Put(h.path, goStart, goStart+int64(n)-1, (*bufPtr)[:n])
						}
						return
					}

					// HTTP Fallback REMOVED
				})
			}
		}

		h.mu.Lock()
		h.lastLen = n
		h.lastTime = now
		h.mu.Unlock()

		return fuse.ReadResultData(dest[:n]), 0
	}

	target := int(end - off + 1)
	isSeq := (off == h.lastOff+int64(h.lastLen)) || (h.lastOff >= 0 && abs(off-(h.lastOff+int64(h.lastLen))) <= globalConfig.SequentialTolerance)
	isStreaming := (len(dest) >= int(globalConfig.StreamingThreshold)) || isSeq
	timing.IsStreaming = isStreaming

	fetchEnd := end
	var fetchSize int64 = int64(target)
	if isStreaming {
		raSize := int64(globalConfig.ReadAheadBase)

		// FASE 2.3: If idle reset, use initial read-ahead
		if isFirstBlock {
			raSize = int64(globalConfig.ReadAheadInitial)
		} else {
			// FASE 2.2: Read-Ahead Boost REMOVED (V86-Gold)
			// Reason: Boosting to 28MB bypassed the 8MB buffer pool, causing massive GC pressure.
			// We stick to the standard ReadAheadBase (8MB) which fits perfectly in the pool.
		}

		fetchEnd = off + raSize - 1
		if fetchEnd >= h.size {
			fetchEnd = h.size - 1
		}
		fetchSize = fetchEnd - off + 1
	}
	h.mu.Lock()
	h.lastLen, h.lastTime = len(dest), now
	h.mu.Unlock()
	atomic.StoreInt64(&h.lastOff, off)

	// V226: Master Semaphore Management
	// If the handle already has a persistent slot (Native WATCHING), we don't need to acquire another one.
	if !h.hasSlot {
		// V259: Lock pump creation to prevent race condition during startup
		pumpCreationMu.Lock()

		// V258: Check for existing active pump for this path (Singleton pattern)
		if val, ok := activePumps.Load(h.path); ok {
			ps := val.(*NativePumpState)
			atomic.AddInt32(&ps.refCount, 1) // Increment reference count
			h.mu.Lock()
			h.hasSlot = true
			h.isWatching = true
			h.nativeReader = ps.reader
			h.pumpCancel = ps.cancel
			h.mu.Unlock()
			logger.Printf("[V258] Handle ATTACHED to existing active pump (Refs: %d): %s", atomic.LoadInt32(&ps.refCount), filepath.Base(h.path))
		}
		// Unlock early if attached or not needed
		if h.hasSlot {
			pumpCreationMu.Unlock()
		} else {
			// Proceed to try upgrade under lock
			// V238: On-the-fly Pump Upgrade (if not already attached)
			if isStreaming && h.hash != "" {
				if val, ok := playbackRegistry.Load(h.path); ok {
					if ps, ok := val.(*PlaybackState); ok && ps.GetStatus() {
						select {
						case masterDataSemaphore <- struct{}{}:
							h.hasSlot = true
							h.nativeReader = nativeBridge.NewStreamReader(h.hash, h.fileID, h.size)
							pumpCtx, pumpCancel := context.WithCancel(context.Background())
							h.pumpCancel = pumpCancel

							// V258: Register this pump in the global registry IMMEDIATELY
							sharedState := &NativePumpState{
								cancel:   pumpCancel,
								reader:   h.nativeReader,
								path:     h.path,
								refCount: 1, // Start with 1
							}
							activePumps.Store(h.path, sharedState)

							logger.Printf("[V238] Native Pump UPGRADED on-the-fly for confirmed playback: %s", filepath.Base(h.path))

							// V238: Unlock FULL Aggressive Mode on upgrade
							hHash := metainfo.NewHashFromHex(h.hash)
							if t := web.BTS.GetTorrent(hHash); t != nil {
								t.SetAggressiveMode(true, globalConfig.MasterConcurrencyLimit)
								logger.Printf("[V238] FULL Aggressive Mode enabled on-the-fly for: %s", h.hash[:8])
							}

							safeGo(func() {
								h.nativePump(pumpCtx, off)
							})
						default:
							// Reserve full, stay in burst mode for now
						}
					}
				}
			}
			pumpCreationMu.Unlock() // Final unlock
		}

		// If still no slot (scan or reserve full), acquire a temporary slot for this read
		if !h.hasSlot {
			select {
			case masterDataSemaphore <- struct{}{}:
				defer func() { <-masterDataSemaphore }()
			case <-fuseCtx.Done():
				return nil, syscall.EINTR
			case <-time.After(30 * time.Second): // V238: Increased to 30s for scan resilience
				logger.Printf("[MasterSemaphore] Timeout waiting for slot: %s", filepath.Base(h.path))
				return nil, syscall.ETIMEDOUT
			}
		}
	}

	// FASE 4.11: Streaming Priority - Skip rate limiting for streaming requests
	// Python version behavior: streaming bypasses rate limiter, only metadata uses it
	// This ensures active playback has priority over background Plex scanning
	// FASE 4.11: Streaming Priority - Skip rate limiting for streaming requests
	// Python version behavior: streaming bypasses rate limiter, only metadata uses it
	// This ensures active playback has priority over background Plex scanning
	if !isStreaming {
		// Rate limiting ONLY for non-streaming (metadata) requests (FASE 1.2)
		rateLimitCtx, rateLimitCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer rateLimitCancel()
		if err := globalRateLimiter.Acquire(rateLimitCtx); err != nil {
			logger.Printf("Rate limit timeout: %v", err)
			return nil, ToErrno(err)
		}
	}

	// V226: Data Path Selection
	// V257: Unified Architecture - We prefer cache hits from the reactive pump.
	// But we MUST NOT block or return EAGAIN if data is missing, as it kills the player.
	if n := raCache.CopyTo(h.path, off, end, dest); n > 0 {
		atomic.StoreInt64(&h.lastOff, off)
		h.mu.Lock()
		h.lastLen = n
		h.lastTime = now
		h.mu.Unlock()
		return fuse.ReadResultData(dest[:n]), 0
	}

	// FALLBACK: Data Fetch with V265 Retry
	// If cache miss, use FetchBlock. Retry up to 3 times if torrent not ready (async Wake).
	var buf []byte
	var n int

	if h.hash != "" {
		bufPtr := readBufferPool.Get().(*[]byte)
		defer readBufferPool.Put(bufPtr)

		limit := fetchSize
		if limit > int64(len(*bufPtr)) {
			limit = int64(len(*bufPtr))
		}

		buf = (*bufPtr)[:limit]

		// V283: 3 retries × 8s timeout (FetchBlock) = 27s max FUSE block.
		// Previously 6 × 30s = 180s → smbd D-state for 3 minutes on seek to uncached positions.
		for attempt := 0; attempt < 3; attempt++ {
			nFetch, err := nativeBridge.FetchBlock(h.hash, h.fileID, off, buf)
			if err == nil && nFetch > 0 {
				n = nFetch
				timing.HTTPFetchTime = 0
				goto DATA_READY
			}
			if attempt < 2 {
				select {
				case <-fuseCtx.Done():
					return nil, syscall.EINTR
				case <-time.After(500 * time.Millisecond):
				}
			}
		}
	}

	// If everything fails, return EAGAIN as last resort
	return nil, syscall.EAGAIN

DATA_READY:

	// V79-Profiling: Record bytes read
	timing.BytesRead = target

	// V86-Gold: Removed hot path logging (causes 30-50% CPU overhead at 100Mbps)
	// Metrics still recorded in globalProfilingStats for /metrics endpoint

	if n > 0 {
		// V257: Cache the data (raCache.Put handles its own copy)
		raCache.Put(h.path, off, off+int64(n)-1, buf[:n])

		// V265: Seed warmup from Read path (both head and tail)
		if diskWarmup != nil && h.hash != "" {
			if off < warmupFileSize {
				diskWarmup.WriteChunk(h.hash, h.fileID, buf[:n], off)
			} else if h.size > tailWarmupSize && off >= h.size-tailWarmupSize {
				// V610: Frozen Tail — only update SSD tail cache before playback is confirmed.
				// This preserves the clean discovery-phase metadata snapshot.
				isConfirmed := false
				if val, ok := playbackRegistry.Load(h.path); ok {
					ps := val.(*PlaybackState)
					ps.mu.RLock()
					isConfirmed = !ps.ConfirmedAt.IsZero()
					ps.mu.RUnlock()
				}
				if !isConfirmed {
					diskWarmup.WriteTail(h.hash, h.fileID, buf[:n], off, h.size)
				}
			}
		}

		// Update sequential detection state
		atomic.StoreInt64(&h.lastOff, off)
		h.mu.Lock()
		h.lastLen = target
		h.mu.Unlock()

		// FASE 3.3: Track file offset in cleanup manager
		globalCleanupManager.UpdateOffset(h.path, off, target)

		// V257: SAFE COPY LOGIC
		// Copy data to FUSE provided 'dest' buffer before returning.
		nCopy := copy(dest, buf[:n])

		// V143-Performance: Predictive Next Chunk Prefetch
		// ... (logic follows after copy)

		// If we are reading the last 25% of the current chunk, trigger a fetch for the next chunk
		// This bridges the "synchronous gap" between chunks on high-latency connections
		// V257-Optimization: Intelligent Prefetch (Rescue Mode)
		// We prefetch if:
		// 1. Pump is NOT active (!h.hasSlot) OR
		// 2. Pump IS active but Lagging (MaxCachedOffset < nextChunkStart)
		chunkSize := int64(globalConfig.ReadAheadBase)
		// Check if we are in the last 25% boundary of what looks like a chunk
		// We approximate "current chunk end" using (off / chunkSize + 1) * chunkSize
		currentChunkIndex := off / chunkSize
		nextChunkStart := (currentChunkIndex + 1) * chunkSize
		distanceToNext := nextChunkStart - off

		maxCached := raCache.MaxCachedOffset(h.path)
		isLagging := maxCached < nextChunkStart

		if isStreaming && (!h.hasSlot || isLagging) {
			if distanceToNext < chunkSize/4 {
				// ... (prefetch logic remains the same)
				// V227: Deduplicate prefetch to prevent goroutine storm
				prefetchKey := fmt.Sprintf("%s:%d", h.path, nextChunkStart)
				if _, loaded := inFlightPrefetches.LoadOrStore(prefetchKey, true); !loaded {
					// Run in background to not block current read delivery
					// V227: Capture variables explicitly for safety
					goStart, goSize, goKey, goHash, goFileID := nextChunkStart, int64(globalConfig.ReadAheadBase), prefetchKey, h.hash, h.fileID
					safeGo(func() {
						defer inFlightPrefetches.Delete(goKey)

						// Check if already in cache to avoid useless work
						if raCache.Exists(h.path, goStart) {
							return // Already cached
						}

						// Rate limit to prevent aggressive storms
						ctx, cancel := context.WithTimeout(fuseCtx, 5*time.Second)
						defer cancel()
						if err := globalRateLimiter.Acquire(ctx); err != nil {
							return
						}

						select {
						case <-fuseCtx.Done():
							return
						default:
						}

						fetchEnd := goStart + goSize - 1
						if fetchEnd >= h.size {
							fetchEnd = h.size - 1
						}
						if fetchEnd <= goStart {
							return
						}

						select {
						case masterDataSemaphore <- struct{}{}:
							defer func() { <-masterDataSemaphore }()
						default:
							return // Skip if pool is saturated
						}

						// Zero-Network Native Prefetch (V227 Phase 7)
						if goHash != "" {
							bufPtr := readBufferPool.Get().(*[]byte)
							defer readBufferPool.Put(bufPtr)

							limit := int64(len(*bufPtr))
							if fetchEnd-goStart+1 < limit {
								limit = fetchEnd - goStart + 1
							}

							n, err := nativeBridge.FetchBlock(goHash, goFileID, goStart, (*bufPtr)[:limit])
							if err == nil && n > 0 {
								raCache.Put(h.path, goStart, goStart+int64(n)-1, (*bufPtr)[:n])
							}
							return
						}

						// HTTP Fallback REMOVED
					})
				}
			}
		}

		return fuse.ReadResultData(dest[:nCopy]), 0
	}

	// V238: Protection against slice bounds out of range panic
	// If buf is smaller than target (e.g. metadata request with small buffer pool),
	// we must adjust target to avoid a crash.
	if target > len(buf) {
		target = len(buf)
	}

	nCopy := copy(dest, buf[:target])
	return fuse.ReadResultData(dest[:nCopy]), 0
}

func (h *MkvHandle) Release(fuseCtx context.Context) syscall.Errno {
	logger.Printf("=== RELEASE VIRTUAL === path=%s", h.path)

	// V260: Shared Pump Reference Counting
	if val, ok := activePumps.Load(h.path); ok {
		ps := val.(*NativePumpState)
		newRefs := atomic.AddInt32(&ps.refCount, -1)
		logger.Printf("[V260] Release handle for %s (Remaining Refs: %d)", filepath.Base(h.path), newRefs)

		if newRefs <= 0 {
			// V420: 30s Grace Period — allows Plex to close/reopen handles during seek
			// without killing the background pump.
			time.AfterFunc(30*time.Second, func() {
				if val, ok := activePumps.Load(h.path); ok {
					psNow := val.(*NativePumpState)
					if atomic.LoadInt32(&psNow.refCount) <= 0 {
						if psNow.cancel != nil {
							psNow.cancel()
						}
						activePumps.Delete(h.path)
						logger.Printf("[V420] Shared Pump Terminated (Grace Period Expired): %s", filepath.Base(h.path))
					}
				}
			})
			logger.Printf("[V420] Last handle closed: Shared Pump entering 30s grace period for %s", filepath.Base(h.path))
		}
	}

	// V260: Only nil local reference. Pump owns reader lifecycle via captured copy.
	h.nativeReader = nil

	// V182: Remove from active handles
	activeHandles.Delete(h)

	// V272: Fast-drop only for scanner/probe reads that were never confirmed by webhook
	retentionDelay := 30 * time.Second
	lastOffset := atomic.LoadInt64(&h.lastOff)
	isProbeOnly := lastOffset < 2*1024*1024 // < 2MB = probe/scanner, not real playback
	if h.hasWarmup && isProbeOnly {
		if val, ok := playbackRegistry.Load(h.path); ok {
			state := val.(*PlaybackState)
			state.mu.RLock()
			stopped := state.IsStopped
			everConfirmed := !state.ConfirmedAt.IsZero()
			state.mu.RUnlock()
			// Fast-drop only if: explicitly stopped OR never confirmed by any webhook
			if stopped || !everConfirmed {
				retentionDelay = 5 * time.Second
			}
		}
	}

	// V140: Use AfterFunc to avoid spawning a goroutine stack
	time.AfterFunc(retentionDelay, func() {
		// V211: Double-check if any handle for this path is STILL active (e.g. Seek or new session)
		stillActive := false
		activeHandles.Range(func(key, value interface{}) bool {
			activeH := key.(*MkvHandle)
			if activeH.path == h.path {
				stillActive = true
				return false
			}
			return true
		})

		if stillActive {
			// A new session or handle is active for the same file, do not cleanup.
			return
		}

		// V201-Fix: Zombie Priority Cleanup
		// Before deleting the registry entry, ensure Priority is disabled in the Core
		if val, ok := playbackRegistry.Load(h.path); ok {
			state := val.(*PlaybackState)
			if state.Hash != "" {
				hHash := metainfo.NewHashFromHex(state.Hash)

				// V215: Hash-Aware Safe Release
				// Check if ANY other active handle is using the same hash before disabling priority
				hashStillInUse := false
				activeHandles.Range(func(key, value interface{}) bool {
					activeH := key.(*MkvHandle)
					// We need to resolve the hash for this active handle to compare
					// Since we don't store hash in MkvHandle, we check playbackRegistry
					if regVal, ok := playbackRegistry.Load(activeH.path); ok {
						regState := regVal.(*PlaybackState)
						if regState.Hash == state.Hash {
							hashStillInUse = true
							return false
						}
					}
					return true
				})

				if hashStillInUse {
					// Another file from the same torrent is still open, keep priority.
					return
				}

				if t := web.BTS.GetTorrent(hHash); t != nil {
					t.IsPriority = false
					t.SetAggressiveMode(false, 0) // V217: Back to normal download priority

					// V272: Scanner fast-drop — only for probe/scanner reads never confirmed by webhook
					scannerDrop := false
					if h.hasWarmup && isProbeOnly {
						state.mu.RLock()
						everConfirmed := !state.ConfirmedAt.IsZero()
						state.mu.RUnlock()
						if !state.IsHealthy && !everConfirmed {
							scannerDrop = true
						}
					}

					if scannerDrop {
						t.AddExpiredTime(5 * time.Second)
						logger.Printf("[V272] Scanner fast-drop for Hash %s (5s retention)", state.Hash)
					} else {
						t.AddExpiredTime(30 * time.Second)
						logger.Printf("[NativeBridge] Priority disabled for Hash %s (All handles closed)", state.Hash)
					}
				}
			}
		}
		// V216-Fix: Do NOT delete from registry here.
		// We leave the entry (without priority) to allow fast resume (via Webhook) to re-enable it.
		// Cleanup is handled by GlobalCleanupManager (15 min timeout).
		// playbackRegistry.Delete(h.path)
	})

	return 0
}

// approximateMetadataSize estimates the memory footprint of a Metadata entry
// Used for LRU cache size tracking
func approximateMetadataSize(m *Metadata) int64 {
	// Approximate size:
	// - URL string: len(URL)
	// - Path string: len(Path)
	// - Size: 8 bytes (int64)
	// - Mtime: 24 bytes (time.Time structure)
	// - Overhead: 64 bytes (pointers, struct alignment)
	return int64(len(m.URL) + len(m.Path) + 8 + 24 + 64)
}

func getOrReadMeta(path string) (*Metadata, error) {
	var m *Metadata

	// Check cache first (fast path without lock)
	if val, ok := metaCache.Get(path); ok {
		m = val
	} else {
		// Acquire lock to prevent stampede
		unlock := globalLockManager.Lock(path)
		defer unlock()

		// Double-check cache after acquiring lock
		if val, ok := metaCache.Get(path); ok {
			m = val
		} else {
			fileMeta, err := ReadMetadataFromFile(path)
			if err != nil {
				return nil, err
			}

			m = &Metadata{
				URL:    fileMeta.URL,
				Size:   fileMeta.Size,
				Mtime:  fileMeta.Mtime,
				Path:   fileMeta.Path,
				ImdbID: fileMeta.ImdbID,
			}

			metaCache.Put(path, m, approximateMetadataSize(m))
		}
	}

	return m, nil
}

type RaBuffer struct {
	start, end int64
	data       []byte
	lastAccess int64 // V244: Atomic timestamp (UnixNano)
	sessionID  int64 // V246: Session ID for Proactive Cleanup
}

// V238: Sharded ReadAheadCache
type ReadAheadCache struct {
	shards    [32]*raShard
	shardMask uint64
	used      int64
	pool      chan []byte // V294: Private pool for recycling 16MB chunks

	// V246: Proactive Context Switching
	muContext        sync.Mutex
	activePath       string
	currentSessionID int64
	isEvicting       int32 // V247: Atomic flag to prevent concurrent global evictions
}

type raShard struct {
	mu      sync.RWMutex
	buffers map[string]*RaBuffer
	order   []string
	total   int64
}

func newReadAheadCache() *ReadAheadCache {
	c := &ReadAheadCache{
		shardMask: 31,
		pool:      make(chan []byte, 32), // Cap at 32 chunks (512MB max pool)
	}
	for i := range c.shards {
		c.shards[i] = &raShard{
			buffers: make(map[string]*RaBuffer),
		}
	}
	return c
}

func (c *ReadAheadCache) getShard(path string) *raShard {
	h := fnv.New64a()
	h.Write([]byte(path))
	return c.shards[h.Sum64()&c.shardMask]
}

// V294: recycle returns a buffer to the pool if it matches standard chunk size.
func (c *ReadAheadCache) recycle(b []byte) {
	chunkSize := int(16 * 1024 * 1024)
	if globalConfig.ReadAheadBase > 0 {
		chunkSize = int(globalConfig.ReadAheadBase)
	}
	if len(b) == chunkSize {
		select {
		case c.pool <- b:
		default:
			// Pool full, let GC handle it
		}
	}
}

// MaxCachedOffset returns the highest cached byte end for a given path.
func (c *ReadAheadCache) MaxCachedOffset(p string) int64 {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	prefix := p + ":"
	var maxEnd int64
	for key, b := range s.buffers {
		if strings.HasPrefix(key, prefix) && b.end > maxEnd {
			maxEnd = b.end
		}
	}
	return maxEnd
}

// raChunkKey returns a compound key so multiple chunks per file can coexist.
func raChunkKey(path string, offset int64) string {
	chunkSize := int64(16 * 1024 * 1024)
	if globalConfig.ReadAheadBase > 0 {
		chunkSize = int64(globalConfig.ReadAheadBase)
	}
	return fmt.Sprintf("%s:%d", path, offset/chunkSize)
}

// V246: Proactive Context Switching
// Called when a NEW stream starts (e.g. at ServeHTTP/Open).
// If path differs from active, it increments SessionID to invalidate old data.
func (c *ReadAheadCache) SwitchContext(newPath string) {
	c.muContext.Lock()
	pathChanged := newPath != c.activePath
	if pathChanged {
		c.activePath = newPath
		// Increment session ID: All old buffers (with old ID) become "stale" instantly
		c.currentSessionID++
	}
	activePath := c.activePath
	activeSessionID := c.currentSessionID
	c.muContext.Unlock()

	// V270: Trigger global eviction on context switch to free stale data immediately.
	// Without this, 200MB+ of old stream data persists in shards → new stream stutters.
	if pathChanged {
		safeGo(func() {
			c.triggerGlobalEviction(activePath, activeSessionID)
		})
	}
}

func (c *ReadAheadCache) Get(p string, off, end int64) []byte {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := raChunkKey(p, off)
	if b, ok := s.buffers[key]; ok && off >= b.start && end <= b.end {
		// V244-Fix: Update activity timestamp atomically (Lock-Free)
		atomic.StoreInt64(&b.lastAccess, time.Now().UnixNano())
		// V274-Fix: Defensive copy — channel-based pool evicts buffers immediately,
		// returning a sub-slice reference causes use-after-free if eviction races the caller.
		// Put() already copies on write; Get() must copy on read for symmetric safety.
		src := b.data[off-b.start : end-b.start+1]
		out := make([]byte, len(src))
		copy(out, src)
		return out
	}
	return nil
}

// V293: Exists checks if a chunk is present in cache without allocating.
func (c *ReadAheadCache) Exists(p string, off int64) bool {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := raChunkKey(p, off)
	_, found := s.buffers[key]
	return found
}

// V293: CopyTo copies data directly into the provided destination buffer.
// This eliminates the redundant intermediate 'make' call in the FUSE Read hot path.
func (c *ReadAheadCache) CopyTo(p string, off, end int64, dest []byte) int {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := raChunkKey(p, off)
	if b, ok := s.buffers[key]; ok && off >= b.start && end <= b.end {
		atomic.StoreInt64(&b.lastAccess, time.Now().UnixNano())
		src := b.data[off-b.start : end-b.start+1]
		n := copy(dest, src)
		return n
	}
	return 0
}

func (c *ReadAheadCache) Put(p string, start, end int64, d []byte) {
	// V246: Get current session ID with robust context check
	c.muContext.Lock()
	sessID := c.currentSessionID
	// V247-Fix: If we are caching for the ACTIVE stream, force the current session ID.
	// This prevents race conditions where a pump started during Open might use an old ID.
	if p == c.activePath {
		// Use currentSessionID
	} else {
		// For background streams/scans, we use a special "background" ID (0)
		// or just the current one. Let's stick to current but tag as potentially stale.
	}
	c.muContext.Unlock()

	shard := c.getShard(p)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	key := raChunkKey(p, start)

	dataSize := int64(len(d))

	// V294: Try to pull a buffer from the recycle pool
	var dataCopy []byte
	select {
	case buf := <-c.pool:
		if int64(len(buf)) == dataSize {
			dataCopy = buf
		} else {
			dataCopy = make([]byte, dataSize)
		}
	default:
		dataCopy = make([]byte, dataSize)
	}
	copy(dataCopy, d)

	// V244: Strict Global Enforcement
	// Check if this write would exceed global budget
	globalLimit := globalConfig.ReadAheadBudget
	if globalLimit <= 0 {
		globalLimit = 256 * 1024 * 1024 // Fail-safe default
	}

	// 1. Account for overwrite
	if old, ok := shard.buffers[key]; ok {
		shard.total -= int64(len(old.data))
		atomic.AddInt64(&c.used, -int64(len(old.data)))
		// V294: Recycle old data
		c.recycle(old.data)
	} else {
		shard.order = append(shard.order, key)
	}

	// 2. Add new data
	shard.total += dataSize
	newUsed := atomic.AddInt64(&c.used, dataSize)
	// V246: Use atomic timestamp AND SessionID
	shard.buffers[key] = &RaBuffer{start, end, dataCopy, time.Now().UnixNano(), sessID}

	// 3. Strict Eviction Loop (Global)
	// While we are over budget, evict from THIS shard
	// V248-Fix: Do NOT evict the very last item (which is likely the one we just added)
	// before trying global eviction.
	for newUsed > globalLimit && len(shard.order) > 1 {
		v := shard.order[0]
		shard.order = shard.order[1:]
		if old, ok := shard.buffers[v]; ok {
			evictedSize := int64(len(old.data))
			shard.total -= evictedSize
			delete(shard.buffers, v)
			newUsed = atomic.AddInt64(&c.used, -evictedSize)
			// V294: Recycle evicted data
			c.recycle(old.data)
		}
	}

	// V270: Rescue eviction — local shard exhausted but still over budget.
	// Trigger global eviction to free stale data from OTHER shards.
	if newUsed > globalLimit && len(shard.order) <= 1 {
		c.muContext.Lock()
		ap := c.activePath
		sid := c.currentSessionID
		c.muContext.Unlock()
		safeGo(func() {
			c.triggerGlobalEviction(ap, sid)
		})
	}

	// 5. Hard Limit: If STILL over budget, DROP
	// V246: Only drop if we are NOT the first chunk of a new session
	if newUsed > globalLimit && len(shard.order) == 0 {
		// If we reach here, it means even after global eviction we are over budget.
		// However, to allow the new stream to start, we must NOT drop the only chunk it has.
		// So we do nothing and allow a tiny over-budget for the first few chunks.
		logger.Printf("[RaCache] Budget full (%d MB), allowing active chunk to persist for handoff: %s", newUsed/(1024*1024), filepath.Base(p))
	}

	// Periodic compaction of order slice
	if len(shard.order) > 100 && len(shard.order) > len(shard.buffers)*2 {
		newOrder := make([]string, 0, len(shard.buffers))
		for _, v := range shard.order {
			if _, exists := shard.buffers[v]; exists {
				newOrder = append(newOrder, v)
			}
		}
		shard.order = newOrder
	}
}

// Stats returns read-ahead cache statistics for metrics endpoint
func (c *ReadAheadCache) Stats() (totalBytes int64, activeBytes int64, entries int) {
	// V244: Use atomic global counter for accurate total
	totalBytes = atomic.LoadInt64(&c.used)

	now := time.Now().UnixNano()
	// V250: Coherent 120s threshold for 4K
	staleThreshold := (120 * time.Second).Nanoseconds()

	for _, shard := range c.shards {
		shard.mu.RLock()
		entries += len(shard.buffers)
		for _, buf := range shard.buffers {
			if now-atomic.LoadInt64(&buf.lastAccess) < staleThreshold {
				activeBytes += int64(len(buf.data))
			}
		}
		shard.mu.RUnlock()
	}

	return totalBytes, activeBytes, entries
}

// V246: Global Cleanup Logic
// Iterates all shards and removes:
// 1. OLD Session Data (Immediate Forced Lock) - Critical for Handoff
// 2. STALE Data (>30s) (TryLock) - Routine maintenance
func (c *ReadAheadCache) triggerGlobalEviction(activePath string, activeSessionID int64) {
	// V247: Single-Flight protection
	if !atomic.CompareAndSwapInt32(&c.isEvicting, 0, 1) {
		return // Already evicting, skip redundant work
	}
	defer atomic.StoreInt32(&c.isEvicting, 0) // V249: StoreInt32 for consistency

	now := time.Now().UnixNano()
	// V250: Increased stale threshold to 120s for 4K stability
	staleThreshold := (120 * time.Second).Nanoseconds()

	for _, s := range c.shards {
		// V249: NEVER block on a shard lock during global eviction.
		// If the shard is busy, skip it. We have 32 shards to choose from.
		if !s.mu.TryLock() {
			continue
		}

		// Scan order from oldest to newest
		var newOrder []string
		for _, key := range s.order {
			keep := true
			if buf, ok := s.buffers[key]; ok {
				// 1. Session ID Check (Fastest)
				if buf.sessionID != activeSessionID && !strings.HasPrefix(key, activePath+":") {
					keep = false
				} else {
					// 2. Stale Check
					// If it's the active path, we are much more lenient (60s instead of 30s)
					threshold := staleThreshold
					if strings.HasPrefix(key, activePath+":") {
						threshold = (60 * time.Second).Nanoseconds()
					}

					lastAcc := atomic.LoadInt64(&buf.lastAccess)
					if now-lastAcc > threshold {
						keep = false
					}
				}

				if !keep {
					size := int64(len(buf.data))
					s.total -= size
					delete(s.buffers, key)
					atomic.AddInt64(&c.used, -size)
					// V294: Recycle globally evicted data
					c.recycle(buf.data)
				}
			}

			if keep {
				newOrder = append(newOrder, key)
			}
		}
		s.order = newOrder
		s.mu.Unlock()
	}
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func extractHashSuffix(filename string) string {
	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filename, ext)
	idx := strings.LastIndex(base, "_")
	if idx != -1 && len(base)-idx-1 == 8 {
		return base[idx+1:]
	}
	return ""
}

// handlePlexWebhook gestisce i messaggi in arrivo dal server Plex
func handlePlexWebhook(w http.ResponseWriter, r *http.Request) {
	// V239-Debug: Log entry to confirm connectivity
	logger.Printf("[PLEX] Webhook connection from %s", r.RemoteAddr)

	if err := r.ParseMultipartForm(10 * 1024 * 1024); err != nil {
		http.Error(w, "Bad request", 400)
		return
	}

	payloadStr := r.FormValue("payload")
	if payloadStr == "" {
		return
	}

	// Logga il payload per debug (limitato ai primi 500 caratteri per non intasare)
	displayPayload := payloadStr
	if len(displayPayload) > 500 {
		displayPayload = displayPayload[:500] + "..."
	}
	logger.Printf("[DEBUG] Webhook received: %s", displayPayload)

	var payload struct {
		Event    string `json:"event"`
		Metadata struct {
			Title              string `json:"title"`
			GrandparentTitle   string `json:"grandparentTitle"` // V150: Per le serie TV
			Year               int    `json:"year"`
			LibrarySectionType string `json:"librarySectionType"` // V256: Filter by library type
			Media              []struct {
				Part []struct {
					File string `json:"file"`
				} `json:"Part"`
			} `json:"Media"`
		} `json:"Metadata"`
	}

	if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
		return
	}

	// V256: Only process Video libraries (movie/show). Ignore music (artist) and others.
	// Verified via logs: Movies use "movie", TV Shows use "show", Music uses "artist".
	if payload.Metadata.LibrarySectionType != "movie" && payload.Metadata.LibrarySectionType != "show" {
		return
	}

	if payload.Event == "media.play" || payload.Event == "media.resume" {
		targetTitle := strings.ToLower(payload.Metadata.Title)
		seriesTitle := strings.ToLower(payload.Metadata.GrandparentTitle)
		targetYear := payload.Metadata.Year

		logger.Printf("[DEBUG] Webhook for '%s' / '%s' (%d). Current registry:", targetTitle, seriesTitle, targetYear)
		playbackRegistry.Range(func(key, value interface{}) bool {
			logger.Printf("  - Registered: %s", key.(string))
			return true
		})

		// V271: Two-pass matching — exact first, fuzzy only as fallback.
		// Previously single-pass with sync.Map random iteration caused false positives:
		// "the housemaid" matched "Ben.The.Men" via Tentativo 4 (first word "the" + year).

		// Extract hash suffix from webhook payload (once, outside loop)
		targetSuffix := ""
		for _, m := range payload.Metadata.Media {
			for _, p := range m.Part {
				if suffix := extractHashSuffix(p.File); suffix != "" {
					targetSuffix = suffix
					break
				}
			}
			if targetSuffix != "" {
				break
			}
		}

		// V281: Extract IMDB ID directly from raw payload string (regex).
		// Cannot use struct field: Plex sends both lowercase "guid" (string) and
		// capital "Guid" (array) — Go's case-insensitive json matching would cause
		// an UnmarshalTypeError if both are in the same struct.
		webhookImdbID := ""
		if m := reImdbID.FindStringSubmatch(payloadStr); len(m) > 1 {
			webhookImdbID = m[1]
		}

		// Pass 1: Exact matches only (IMDB ID, hash suffix, filename)
		var exactMatch string
		var exactState *PlaybackState
		playbackRegistry.Range(func(key, value interface{}) bool {
			path := key.(string)
			state := value.(*PlaybackState)

			// Tentativo 0a: Match per IMDB ID (V281 — immune a titoli localizzati)
			if webhookImdbID != "" && state.ImdbID != "" && state.ImdbID == webhookImdbID {
				exactMatch = path
				exactState = state
				return false
			}

			// Tentativo 0b: Match per Hash Suffix (V220 - Precision Match)
			if targetSuffix != "" && extractHashSuffix(path) == targetSuffix {
				exactMatch = path
				exactState = state
				return false
			}

			// Tentativo 1: Match per Filename (se presente nel payload)
			for _, m := range payload.Metadata.Media {
				for _, p := range m.Part {
					if filepath.Base(p.File) == filepath.Base(path) {
						exactMatch = path
						exactState = state
						return false
					}
				}
			}
			return true
		})

		// Pass 2: Fuzzy matches only if no exact match found
		if exactMatch == "" {
			var bestMatch string
			var bestState *PlaybackState
			bestLevel := 0 // higher = better match

			playbackRegistry.Range(func(key, value interface{}) bool {
				path := key.(string)
				filename := strings.ToLower(filepath.Base(path))
				level := 0

				// Tentativo 2: Match per Titolo completo (Movies o Episodio con nome)
				// V273: Try both dot and underscore separators (gostream uses underscores, some sources use dots)
				if targetTitle != "" && (strings.Contains(filename, strings.ReplaceAll(targetTitle, " ", ".")) ||
					strings.Contains(filename, strings.ReplaceAll(targetTitle, " ", "_"))) {
					level = 3
				}

				// Tentativo 3: Match per Titolo Serie (V150 - TV Shows)
				// V273: Try underscore separator before falling back to first-word heuristic
				if level == 0 && seriesTitle != "" {
					if strings.Contains(filename, strings.ReplaceAll(seriesTitle, " ", ".")) ||
						strings.Contains(filename, strings.ReplaceAll(seriesTitle, " ", "_")) {
						level = 2
					} else {
						words := strings.Fields(seriesTitle)
						if len(words) > 0 && len(words[0]) > 4 {
							cleanWord := strings.TrimRight(words[0], ":.,;!?")
							if len(cleanWord) > 4 && strings.Contains(filename, cleanWord) {
								level = 1
							}
						}
					}
				}

				// Tentativo 4: Match per prima parola + anno (fallback estremo)
				// V271: Require first word length > 3 to prevent "the", "a", "an" false positives
				if level == 0 && len(strings.Fields(targetTitle)) > 0 {
					firstWord := strings.Fields(targetTitle)[0]
					if len(firstWord) > 3 && strings.Contains(filename, firstWord) {
						if targetYear > 0 && strings.Contains(filename, strconv.Itoa(targetYear)) {
							level = 1
						}
					}
				}

				if level > bestLevel {
					bestLevel = level
					bestMatch = path
					bestState = value.(*PlaybackState)
				}
				return true
			})
			exactMatch = bestMatch
			exactState = bestState
		}

		if exactMatch != "" && exactState != nil {
			exactState.SetHealthy(true)
			exactState.mu.Lock()
			exactState.IsStopped = false // V272: Reset stop flag on re-play
			exactState.mu.Unlock()
			logger.Printf("[PLEX] Playback confirmed by webhook for: %s", filepath.Base(exactMatch))

			// V185: Link priority to core Torrent engine
			if exactState.Hash != "" {
				h := metainfo.NewHashFromHex(exactState.Hash)
				if t := web.BTS.GetTorrent(h); t != nil {
					t.IsPriority = true
					// V238: Unlock full aggression for confirmed playback
					t.SetAggressiveMode(true, globalConfig.MasterConcurrencyLimit)
					logger.Printf("[PLEX] High Priority + FULL Aggressive Mode enabled for torrent: %s", exactState.Hash)
				}
			}
		}
	} else if payload.Event == "media.stop" {
		// V255: Ignore media.pause entirely — pause is a player-side concept.
		// The torrent must keep downloading at full speed so resume is instant.
		// Plex never sends media.resume, so disabling anything on pause breaks resume.
		targetTitle := strings.ToLower(payload.Metadata.Title)
		seriesTitle := strings.ToLower(payload.Metadata.GrandparentTitle)
		targetYear := payload.Metadata.Year

		// V271: Two-pass matching for media.stop (same as media.play fix)
		stopTargetSuffix := ""
		for _, m := range payload.Metadata.Media {
			for _, p := range m.Part {
				if suffix := extractHashSuffix(p.File); suffix != "" {
					stopTargetSuffix = suffix
					break
				}
			}
			if stopTargetSuffix != "" {
				break
			}
		}

		// V281: Extract IMDB ID from raw payload (same regex, see media.play)
		stopImdbID := ""
		if m := reImdbID.FindStringSubmatch(payloadStr); len(m) > 1 {
			stopImdbID = m[1]
		}

		// Pass 1: Exact matches (IMDB ID, hash suffix, filename)
		var stopMatch string
		var stopState *PlaybackState
		playbackRegistry.Range(func(key, value interface{}) bool {
			path := key.(string)
			state := value.(*PlaybackState)

			// V281: IMDB ID match — immune a titoli localizzati
			if stopImdbID != "" && state.ImdbID != "" && state.ImdbID == stopImdbID {
				stopMatch = path
				stopState = state
				return false
			}

			if stopTargetSuffix != "" && extractHashSuffix(path) == stopTargetSuffix {
				stopMatch = path
				stopState = state
				return false
			}

			for _, m := range payload.Metadata.Media {
				for _, p := range m.Part {
					if filepath.Base(p.File) == filepath.Base(path) {
						stopMatch = path
						stopState = state
						return false
					}
				}
			}
			return true
		})

		// Pass 2: Fuzzy matches only if no exact match
		if stopMatch == "" {
			bestLevel := 0
			playbackRegistry.Range(func(key, value interface{}) bool {
				path := key.(string)
				filename := strings.ToLower(filepath.Base(path))
				level := 0

				// V273: Try both dot and underscore separators
				if targetTitle != "" && (strings.Contains(filename, strings.ReplaceAll(targetTitle, " ", ".")) ||
					strings.Contains(filename, strings.ReplaceAll(targetTitle, " ", "_"))) {
					level = 3
				}

				// V273: Try underscore separator before falling back to first-word heuristic
				if level == 0 && seriesTitle != "" {
					if strings.Contains(filename, strings.ReplaceAll(seriesTitle, " ", ".")) ||
						strings.Contains(filename, strings.ReplaceAll(seriesTitle, " ", "_")) {
						level = 2
					} else {
						words := strings.Fields(seriesTitle)
						if len(words) > 0 && len(words[0]) > 4 {
							cleanWord := strings.TrimRight(words[0], ":.,;!?")
							if len(cleanWord) > 4 && strings.Contains(filename, cleanWord) {
								level = 1
							}
						}
					}
				}

				if level == 0 && len(strings.Fields(targetTitle)) > 0 {
					firstWord := strings.Fields(targetTitle)[0]
					if len(firstWord) > 3 && strings.Contains(filename, firstWord) {
						if targetYear > 0 && strings.Contains(filename, strconv.Itoa(targetYear)) {
							level = 1
						}
					}
				}

				if level > bestLevel {
					bestLevel = level
					stopMatch = path
					stopState = value.(*PlaybackState)
				}
				return true
			})
		}

		if stopMatch != "" && stopState != nil {
			stopState.SetHealthy(false)
			stopState.mu.Lock()
			stopState.IsStopped = true // V272: Mark explicit stop for fast-drop
			stopState.mu.Unlock()
			logger.Printf("[PLEX] Priority removed for: %s (Event: %s)", filepath.Base(stopMatch), payload.Event)

			// V271: Force-terminate the active pump on media.stop.
			if val, ok := activePumps.Load(stopMatch); ok {
				ps := val.(*NativePumpState)
				if ps.cancel != nil {
					ps.cancel()
				}
				activePumps.Delete(stopMatch)
				logger.Printf("[PLEX] STOP: force-terminated pump for %s", filepath.Base(stopMatch))
			}

			// Deactivate Core Priority
			if stopState.Hash != "" {
				h := metainfo.NewHashFromHex(stopState.Hash)
				if t := web.BTS.GetTorrent(h); t != nil {
					t.IsPriority = false
					t.SetAggressiveMode(false, 0) // Back to normal download priority
					t.AddExpiredTime(30 * time.Second)
					logger.Printf("[PLEX] STOP detected. Grace period 30s for: %s", stopState.Hash)
				}
			}
		}
	}
	w.WriteHeader(200)
}

//go:embed settings.html
var settingsHTML []byte

func main() {
	var dbPath string
	flag.StringVar(&dbPath, "path", "", "path to database and config")
	flag.Parse()

	// Default --path to the directory containing the binary (portable install)
	if dbPath == "" {
		if exe, err := os.Executable(); err == nil {
			dbPath = filepath.Dir(exe)
		}
	}
	if dbPath != "" {
		if settings.Args == nil {
			settings.Args = &settings.ExecArgs{}
		}
		settings.Args.Path = dbPath
	}

	source, mount := flag.Arg(0), flag.Arg(1)

	// Load configuration first (FASE 4.16) — needed before path check to allow config fallback
	globalConfig = LoadConfig()
	logger.Printf("[DEBUG] BlockListURL loaded: '%s'", globalConfig.BlockListURL)

	// V150: Centralize paths (Phase 3)
	if dbPath != "" {
		// Use the directory of the passed db/config file as root
		globalConfig.RootPath = filepath.Dir(dbPath)
	} else {
		// Default to /home/pi if no flag provided (for backward compat)
		globalConfig.RootPath = "/home/pi"
	}

	// CLI args take precedence; fall back to config.json values if omitted
	if source == "" {
		source = globalConfig.PhysicalSourcePath
	}
	if mount == "" {
		mount = globalConfig.FuseMountPath
	}
	if source == "" || mount == "" {
		fmt.Println("Usage: gostream [--path /path/to/db] <source_path> <mount_path>")
		fmt.Println("  Or set physical_source_path and fuse_mount_path in config.json")
		os.Exit(1)
	}
	physicalSourcePath = source
	virtualMountPath = mount

	globalConfig.LogConfig(logger)

	// V144-Integration: Start Embedded GoStorm Engine
	go func() {
		logger.Println("Starting Embedded GoStorm Engine...")
		server.Start() // Starts Web Server on 8090 and Engine
	}()
	// Give engine a moment to init (hash maps etc)
	time.Sleep(2 * time.Second)

	// V256: Initialize disk warmup cache after settings are loaded
	InitDiskWarmup()

	// V228: Launch NAT-PMP sidecar (independent of FUSE, uses backgroundStopChan)
	go natpmpLoop(backgroundStopChan, globalConfig.NatPMP, logger)

	// Initialize global concurrency controls (V226: Unified Master Semaphore)
	// Strictly honors MasterConcurrencyLimit (default: 25) for all data requests
	masterDataSemaphore = make(chan struct{}, globalConfig.MasterConcurrencyLimit)

	// V239: Start Orphan Handle Garbage Collector
	startHandleGC()

	// Initialize global helpers
	globalRateLimiter = NewRateLimiter(globalConfig.RateLimitRequestsPerSec, 1*time.Second)
	globalLockManager = NewLockManager(1 * time.Hour)

	// V143-Performance: Initialize buffer pool with dynamic size based on Config
	// V238: Pool size must match the doubled chunkSize used in nativePump (32MB default)
	poolSize := int(globalConfig.ReadAheadBase) * 2
	if poolSize == 0 {
		poolSize = 32 * 1024 * 1024
	}
	readBufferPool = &sync.Pool{
		New: func() interface{} {
			buf := make([]byte, poolSize)
			return &buf
		},
	}
	logger.Printf("ReadBufferPool initialized with size: %d bytes (matches 2x ReadAheadBase)", poolSize)

	// V79: Initialize httpClient with globalConfig timeout values
	// This fixes the 11.8x performance degradation caused by hardcoded 5s timeout
	// V80-Connection-Optimization: Increased from Python values (10/15) to leverage Go efficiency
	// Python needed 15 due to GIL constraints, Go can handle more with lower overhead
	// Target: Match GoStorm max seeders (25) + overhead while maintaining low latency I/O
	httpClient = &http.Client{
		Transport: &http.Transport{
			// Dialer configuration - matches Python socket creation
			DialContext: (&net.Dialer{
				Timeout:   globalConfig.HTTPConnectTimeout, // 10s (matches Python CONNECT_TIMEOUT)
				KeepAlive: 30 * time.Second,                // Keep TCP connection alive
			}).DialContext,

			// Connection pool limits - OPTIMIZED FOR GO EFFICIENCY
			// V81: Increased to 64 based on real-world multi-stream testing
			// V80 at 30 removed the 270Mbps cap and achieved stable 250-400Mbps peaks
			// Increasing all three parameters together ensures optimal connection reuse
			MaxIdleConns:        globalConfig.MaxIdleConns,        // Global pool size (up from 30)
			MaxIdleConnsPerHost: globalConfig.MaxIdleConnsPerHost, // Keep up to 64 idle connections ready (up from 30)
			MaxConnsPerHost:     globalConfig.MaxConnsPerHost,     // Cap total connections to 64 (up from 30)

			// Timeouts
			ResponseHeaderTimeout: globalConfig.HTTPReadTimeout, // 45s (matches Python READ_TIMEOUT)
			IdleConnTimeout:       90 * time.Second,             // Close idle connections after 90s
			TLSHandshakeTimeout:   10 * time.Second,             // TLS handshake timeout (even for localhost)
			ExpectContinueTimeout: 1 * time.Second,              // Expect: 100-continue timeout

			// HTTP protocol settings - match Python defaults
			DisableKeepAlives:  false, // Enable HTTP keepalive (Python default)
			DisableCompression: false, // Enable gzip compression (Python default)
			ForceAttemptHTTP2:  false, // Use HTTP/1.1 only (Python urllib3 default)

			// V86-Gold: Increased buffer sizes for high-throughput streaming
			// 64KB buffers reduce syscall overhead for 50GB+ file transfers
			// Memory impact: 64KB × ConcurrencyLimit (64) = 4MB max (acceptable on Pi)
			WriteBufferSize: globalConfig.WriteBufferSize,
			ReadBufferSize:  globalConfig.ReadBufferSize,
		},
	}
	logger.Printf("HTTP client initialized: ConnectTimeout=%v, ReadTimeout=%v, MaxIdleConns=%d, MaxIdleConnsPerHost=%d, MaxConnsPerHost=%d (V81-optimized)",
		globalConfig.HTTPConnectTimeout, globalConfig.HTTPReadTimeout, globalConfig.MaxIdleConns, globalConfig.MaxIdleConnsPerHost, globalConfig.MaxConnsPerHost)

	// V160: Initialize Native Bridge for Zero-Network metadata operations
	nativeBridge = NewNativeClient()

	// V298: Automatic BlockList Update
	if globalConfig.BlockListURL != "" {
		safeGo(func() {
			// Initial update
			updateBlockList(globalConfig.BlockListURL)

			// Periodic update every 24 hours
			ticker := time.NewTicker(24 * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					updateBlockList(globalConfig.BlockListURL)
				case <-backgroundStopChan:
					return
				}
			}
		})
	}

	// V79: Initialize peerPreloader after httpClient is ready
	// V160: Updated to use Native Bridge
	peerPreloader = NewPeerPreloader(nativeBridge)

	// Initialize metadata LRU cache (FASE 3.1)
	// Capacity from config (default 50MB - V178 Optimization), 24h TTL
	metaCache = NewLRUCache(globalConfig.MetadataCacheSize, 24*time.Hour)

	// V133: Initialize inode map for deterministic inode generation
	// This ensures Plex doesn't see "new files" after proxy restarts
	inodeMapPath := filepath.Join(GetStateDir(), "inode_map.json")
	if err := InitGlobalInodeMap(inodeMapPath, logger); err != nil {
		logger.Printf("WARNING: Failed to initialize inode map: %v (falling back to filename hash)", err)
	} else {
		files, dirs, _, _ := GetInodeMapStats()
		logger.Printf("InodeMap: Initialized with %d files, %d dirs from %s", files, dirs, inodeMapPath)
	}

	// Start background cache pre-population (FASE 3.2)
	// This dramatically improves Plex scan performance
	cacheBuilder := NewStartupCacheBuilder(source, metaCache, logger)
	cacheBuilder.Start()

	// Initialize and start cleanup manager (FASE 3.3)
	// V238: Integrated metaCache and nativeBridge for centralized resource management (Audit 1.A)
	globalCleanupManager = NewCleanupManager(logger, peerPreloader, metaCache, nativeBridge)
	globalCleanupManager.Start()

	// Initialize torrent remover (FASE 4.2)
	// V160: Updated to use Native Bridge
	globalTorrentRemover = NewTorrentRemover(nativeBridge, logger)

	// Initialize sync cache manager (FASE 4.13)
	globalSyncCacheManager = NewSyncCacheManager(GetStateDir(), logger)

	// V83: Load caches from disk into memory (one-time at startup)
	if err := globalSyncCacheManager.LoadCachesFromDisk(); err != nil {
		logger.Printf("WARNING: Failed to load sync caches from disk: %v", err)
	}

	// V83: Start background sync to disk (every 30s)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := globalSyncCacheManager.SyncToDisk(); err != nil {
					logger.Printf("SyncCache: Failed to sync to disk: %v", err)
				}
			case <-backgroundStopChan:
				return
			}
		}
	}()

	// Start periodic cleanup of sync caches (every 1 hour)
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// Cleanup stale entries: negative cache 12h TTL, fullpack cache 7 days TTL
				globalSyncCacheManager.CleanupStaleEntries(12*time.Hour, 7*24*time.Hour)
			case <-backgroundStopChan:
				return
			}
		}
	}()

	// V140: Initialize directory cache (10s TTL - balances interactivity with IO reduction)
	globalDirCache = NewDirCache(10 * time.Second)

	http.HandleFunc("/plex/webhook", handlePlexWebhook)

	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		cacheStats := metaCache.Stats()
		cleanupStats := globalCleanupManager.Stats()
		lockStats := globalLockManager.Stats()
		syncCacheStats := globalSyncCacheManager.Stats()

		// Get read-ahead buffer stats (for dashboard FUSE Buffer display)
		raTotal, raActive, raEntries := raCache.Stats()
		raStale := raTotal - raActive
		raBudget := globalConfig.ReadAheadBudget
		raPercent := float64(raTotal) / float64(raBudget) * 100
		raActivePercent := float64(raActive) / float64(raBudget) * 100
		raStalePercent := float64(raStale) / float64(raBudget) * 100

		// V229: Expose NAT-PMP port from atomic memory (Zero-Overhead)
		natPort := atomic.LoadInt64(&currentNatPort)

		fmt.Fprintf(w, `{"version":"V265-Stable", "config_source":"%s", "uptime":"%s", "cache_entries":%d, "cache_size_mb":%.2f, "cleanup_hashes":%d, "cleanup_offsets":%d, "cleanup_activities":%d, "locks_total":%d, "master_concurrency_limit":%d, "negative_cache_entries":%d, "fullpack_cache_entries":%d, "streaming_threshold_kb":%d, "config_preload_workers":%d, "max_conns_per_host":%d, "read_ahead_total_bytes":%d, "read_ahead_active_bytes":%d, "read_ahead_stale_bytes":%d, "read_ahead_entries":%d, "read_ahead_budget":%d, "read_ahead_percent":%.2f, "read_ahead_active_percent":%.2f, "read_ahead_stale_percent":%.2f, "natpmp_port":%d}`,
			globalConfig.ConfigPath,
			time.Since(startTime),
			cacheStats.Entries, float64(cacheStats.Size)/(1024*1024),
			cleanupStats.DeletedHashesTotal, cleanupStats.OffsetsTotal, cleanupStats.ActivitiesTotal,
			lockStats.TotalLocks,
			globalConfig.MasterConcurrencyLimit,
			syncCacheStats.NegativeCacheEntries,
			syncCacheStats.FullpackCacheEntries,
			globalConfig.StreamingThreshold/1024,
			globalConfig.PreloadWorkers,
			globalConfig.MaxConnsPerHost, // V81: MaxConnsPerHost value
			raTotal, raActive, raStale, raEntries, raBudget,
			raPercent, raActivePercent, raStalePercent,
			natPort)
	})

	// V145-Webhook: Register Plex Webhook handler for priority management
	http.HandleFunc("/webhook", handlePlexWebhook)

	// V79-Profiling: Detailed performance profiling endpoint
	http.HandleFunc("/metrics/profiling", func(w http.ResponseWriter, r *http.Request) {
		totalReads, cacheHits, httpFetches, streamingReads, avgHTTPLatency, avgCacheLatency, cacheHitRate := globalProfilingStats.Stats()

		fmt.Fprintf(w, `{"version":"V155-Unified", "total_reads":%d, "cache_hits":%d, "cache_hit_rate_pct":%.2f, "http_fetches":%d, "streaming_reads":%d, "non_streaming_reads":%d, "avg_http_latency_ms":%.2f, "avg_cache_latency_ms":%.2f, "max_conns_per_host":%d}`,
			totalReads,
			cacheHits,
			cacheHitRate,
			httpFetches,
			streamingReads,
			totalReads-streamingReads,
			float64(avgHTTPLatency.Microseconds())/1000.0,
			float64(avgCacheLatency.Microseconds())/1000.0,
			globalConfig.MaxConnsPerHost) // V81: MaxConnsPerHost value (was missing, caused %!d(MISSING) bug)
	})

	http.HandleFunc("/control", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(settingsHTML)
	})

	http.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(globalConfig)
			return
		}
		if r.Method == "POST" {
			var newCfg Config
			if err := json.NewDecoder(r.Body).Decode(&newCfg); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			// Update file
			data, _ := json.MarshalIndent(newCfg, "", "  ")
			if err := os.WriteFile(globalConfig.ConfigPath, data, 0644); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			// Reload in memory (V1.4.0 Live Update)
			oldURL := globalConfig.BlockListURL
			globalConfig = LoadConfig()
			if globalConfig.BlockListURL != "" && globalConfig.BlockListURL != oldURL {
				safeGo(func() {
					updateBlockList(globalConfig.BlockListURL)
				})
			}
			logger.Printf("[Config] Updated via Dashboard API")
			w.WriteHeader(200)
		}
	})

	http.HandleFunc("/api/restart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
		// Flush response before exiting
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Trigger graceful shutdown — systemd Restart=always will bring it back up
		go func() {
			time.Sleep(150 * time.Millisecond)
			p, _ := os.FindProcess(os.Getpid())
			p.Signal(syscall.SIGTERM)
		}()
	})

	go http.ListenAndServe(":8096", nil)

	// V133: Setup signal handler for graceful shutdown
	// This ensures inode map is saved before exit
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	var server *fuse.Server // Declare here to be accessible in goroutine
	var err error           // Declare err here too
	go func() {
		sig := <-sigChan
		logger.Printf("Received signal %v, initiating graceful shutdown...", sig)

		// Save inode map before exit
		if globalInodeMap != nil {
			if globalInodeMap.IsDirty() {
				if err := globalInodeMap.SaveToDisk(); err != nil {
					logger.Printf("InodeMap: Shutdown save FAILED: %v", err)
				} else {
					files, dirs, _, _ := GetInodeMapStats()
					logger.Printf("InodeMap: Shutdown save complete (%d files, %d dirs)", files, dirs)
				}
			}
			ShutdownGlobalInodeMap()
		}

		// Clean up sync caches
		if globalSyncCacheManager != nil {
			if err := globalSyncCacheManager.SyncToDisk(); err != nil {
				logger.Printf("SyncCache: Shutdown save FAILED: %v", err)
			}
		}

		// CRITICAL FIX: Stop all background managers explicitly before os.Exit()
		// because defer statements are bypassed by os.Exit().
		backgroundStopOnce.Do(func() { close(backgroundStopChan) }) // V227: Safe single close

		if globalRateLimiter != nil {
			globalRateLimiter.Stop()
		}
		if globalLockManager != nil {
			globalLockManager.Stop()
		}
		if globalCleanupManager != nil {
			globalCleanupManager.Stop()
		}

		// Try to unmount gracefully
		if server != nil {
			server.Unmount()
		}

		logger.Println("Graceful shutdown complete, exiting...")
		os.Exit(0)
	}()

	// Crea root node virtuale
	rootData := &VirtualMkvRoot{sourcePath: source}

	// Enable attribute caching from config
	attrTimeout := time.Duration(globalConfig.AttrTimeoutSeconds * float64(time.Second))
	entryTimeout := time.Duration(globalConfig.EntryTimeoutSeconds * float64(time.Second))
	negativeTimeout := time.Duration(globalConfig.NegativeTimeoutSeconds * float64(time.Second))

	server, err = fs.Mount(mount, rootData, &fs.Options{
		AttrTimeout: &attrTimeout, EntryTimeout: &entryTimeout,
		NegativeTimeout: &negativeTimeout,
		MountOptions: fuse.MountOptions{
			AllowOther:    true,
			MaxBackground: globalConfig.ConcurrencyLimit,
			// MaxWrite:                 1024 * 1024,
			MaxWrite: 4 * 1024 * 1024, // Samba Turbo: 4MB write buffer
			// MaxReadAhead:             1024 * 1024,
			MaxReadAhead:             4 * 1024 * 1024, // Samba Turbo: 4MB read-ahead
			RememberInodes:           true,            // ENABLED: safe with explicit cache control
			ExplicitDataCacheControl: true,            // PREVENTS kernel freezes during invalidation
			SyncRead:                 false,           // ENABLED ASYNC READS for 4K performance
			// NFS Export: Stable filesystem identification
			FsName: "gostream",
		},
		UID: globalConfig.UID, // Default file ownership: pi user (1000)
		GID: globalConfig.GID, // Default file ownership: pi group (1000)
	})
	if err != nil {
		log.Fatal(err)
	}

	logger.Printf("FUSE mounted at %s with VirtualMkvRoot, all systems active", mount)

	// V255: SMB D-state watchdog — detects smbd processes stuck on FUSE I/O
	// and triggers graceful self-restart before the Synology CIFS mount goes stale.
	go smbdWatchdog()

	server.Wait()
}

// V255: SMB D-state watchdog
// Detects smbd child processes stuck in D-state (uninterruptible sleep on FUSE I/O).
// When smbd enters D-state, the Synology CIFS mount becomes unresponsive and can't
// be remounted until gostream restarts (only way to unblock kernel FUSE operations).
// After 4 consecutive detections (180s), sends SIGTERM to self for graceful restart.
func smbdWatchdog() {
	const checkInterval = 60 * time.Second
	const threshold = 5 // consecutive D-state detections before restart (300s total)
	consecutiveHits := 0

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for range ticker.C {
		if countDStateSmbd() > 0 {
			consecutiveHits++
			logger.Printf("[Watchdog] D-state smbd detected (%d/%d)", consecutiveHits, threshold)
			if consecutiveHits >= threshold {
				logger.Printf("[Watchdog] D-state smbd persistent for %ds — triggering graceful restart",
					consecutiveHits*int(checkInterval/time.Second))
				// SIGTERM triggers the existing signal handler for graceful shutdown.
				// systemd Restart=always will restart us.
				syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
				return
			}
		} else {
			if consecutiveHits > 0 {
				logger.Printf("[Watchdog] D-state cleared after %d hit(s)", consecutiveHits)
			}
			consecutiveHits = 0
		}
	}
}

// countDStateSmbd returns the number of smbd processes in D-state (uninterruptible sleep).
func countDStateSmbd() int {
	// ps -eo stat,comm: STAT column starts with D for uninterruptible sleep
	out, err := exec.Command("ps", "-eo", "stat,comm").Output()
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range bytes.Split(out, []byte("\n")) {
		// Match lines where STAT starts with D and command is smbd
		fields := bytes.Fields(line)
		if len(fields) >= 2 && fields[0][0] == 'D' && string(fields[1]) == "smbd" {
			count++
		}
	}
	return count
}

// V239: Start Orphan Handle Garbage Collector
func startHandleGC() {
	ticker := time.NewTicker(15 * time.Minute)
	safeGo(func() {
		for range ticker.C {
			count := 0
			activeHandles.Range(func(key, value interface{}) bool {
				h := key.(*MkvHandle)
				h.mu.Lock()
				idle := time.Since(h.lastActivityTime)
				path := h.path
				h.mu.Unlock()

				if idle > 1*time.Hour {
					logger.Printf("[V239] Force closing orphan handle (idle 1h): %s", path)
					// We simulate a FUSE Release to cleanup all resources
					h.Release(context.Background())
					count++
				}
				return true
			})
			if count > 0 {
				logger.Printf("[V239] Handle GC: cleaned %d orphan handles", count)
			}
		}
	})
}

// updateBlockList downloads and updates the BitTorrent blocklist
func updateBlockList(urlStr string) {
	if urlStr == "" {
		return
	}

	exePath, err := os.Executable()
	if err != nil {
		logger.Printf("[BlockList] Error getting executable path: %v", err)
		return
	}
	destPath := filepath.Join(filepath.Dir(exePath), "blocklist")

	// Check if file exists and is recent (e.g., less than 24h old)
	if info, err := os.Stat(destPath); err == nil {
		if time.Since(info.ModTime()) < 24*time.Hour {
			logger.Printf("[BlockList] Existing blocklist is recent, skipping update")
			return
		}
	}

	logger.Printf("[BlockList] Updating from %s...", urlStr)

	resp, err := http.Get(urlStr)
	if err != nil {
		logger.Printf("[BlockList] Download error: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logger.Printf("[BlockList] Download failed: status %d", resp.StatusCode)
		return
	}

	var reader io.Reader = resp.Body
	if strings.HasSuffix(urlStr, ".gz") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			logger.Printf("[BlockList] Gzip error: %v", err)
			return
		}
		defer gz.Close()
		reader = gz
	}

	out, err := os.Create(destPath)
	if err != nil {
		logger.Printf("[BlockList] File create error: %v", err)
		return
	}
	defer out.Close()

	n, err := io.Copy(out, reader)
	if err != nil {
		logger.Printf("[BlockList] File write error: %v", err)
		return
	}

	logger.Printf("[BlockList] Updated successfully: %d bytes saved to %s", n, destPath)
}
