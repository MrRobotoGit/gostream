package main

import (
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/cespare/xxhash/v2"
	"gostream/ai"
	server "gostream/internal/gostorm"
	"gostream/internal/gostorm/settings"
	torrstor "gostream/internal/gostorm/torr/storage/torrstor"
	tsutils "gostream/internal/gostorm/utils"
	"gostream/internal/gostorm/web"
	"gostream/internal/metadb"
	"gostream/internal/monitor/collector"
	"gostream/internal/monitor/dashboard"
	"gostream/internal/opentracker"
	"gostream/internal/prowlarr"
	"gostream/internal/syncer/engines"
	"gostream/internal/syncer/scheduler"
	"gostream/internal/updater"
	"io"
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

var httpClient *http.Client

// masterDataSemaphore limits concurrent data operations (Native, HTTP, Prefetch).
var masterDataSemaphore chan struct{}

var startTime = time.Now()
var metaCache *LRUCache
var raCache = newReadAheadCache()

var globalRateLimiter *RateLimiter
var globalLockManager *LockManager

// Global peer-based preloader (FASE 2 - Performance)
var peerPreloader *PeerPreloader
var nativeBridge *NativeClient

var globalCleanupManager *CleanupManager
var globalTorrentRemover *TorrentRemover
var globalConfig Config

// Global Prowlarr client for indexer queries (nil when disabled).
var prowlarrClient *prowlarr.Client

// GetEffectiveConcurrencyLimit returns AI limit if set, otherwise globalConfig default
func GetEffectiveConcurrencyLimit() int {
	aiLimit := int(atomic.LoadInt32(&ai.CurrentLimit))
	if aiLimit > 0 {
		return aiLimit
	}
	return globalConfig.MasterConcurrencyLimit
}

// PlaybackState traccia lo stato di una sessione di visione reale
type PlaybackState struct {
	mu          sync.RWMutex
	Path        string
	Hash        string // InfoHash for GoStorm priority management
	ImdbID      string // IMDB ID from MKV line 4, used for webhook matching
	OpenedAt    time.Time
	ConfirmedAt time.Time // Set when Plex webhook arrives
	IsHealthy   bool      // Confirmed by Plex
	IsStopped   bool      // Set on explicit media.stop webhook
	// V750: Inferred playback detection (self-healing when webhook is lost)
	ReadCount   int64
	LastSeekOff int64
	LastReadAt  time.Time
}

func (ps *PlaybackState) SetHealthy(healthy bool) {
	ps.mu.Lock()
	ps.IsHealthy = healthy
	ps.ConfirmedAt = time.Now()
	ps.mu.Unlock()
	// V750: Persist to SQLite
	savePlaybackStateToDB(ps)
}

// V750: Determine if this path shows evidence of active playback,
// even without a webhook confirmation.
func (ps *PlaybackState) IsInferredPlayback() bool {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	if ps.IsHealthy {
		return false // Already confirmed, no need for inference
	}
	now := time.Now()
	active := !ps.LastReadAt.IsZero() && now.Sub(ps.LastReadAt) < 10*time.Minute
	significantReads := ps.ReadCount >= 3
	streaming := ps.LastSeekOff > 2*1024*1024 // >2MB offset = real playback, not scanner
	return active && significantReads && streaming
}

func (ps *PlaybackState) GetStatus() bool {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.IsHealthy
}

var playbackRegistry sync.Map // path -> *PlaybackState

// Global sync cache manager (FASE 4.13 - Sync Script Caches)
var globalSyncCacheManager *SyncCacheManager

// StateDB is the optional SQLite backend for persistent state.
var stateDB *metadb.DB

var physicalSourcePath string
var virtualMountPath string

var backgroundStopChan = make(chan struct{})
var backgroundStopOnce sync.Once

// readBufferPool size matches Config.ReadAheadBase (set in main).
var readBufferPool *sync.Pool

// reImdbID matches "imdb://tt1234567" in the Guid array of Plex webhook payloads.
var reImdbID = regexp.MustCompile(`"imdb://(tt\d+)"`)
var reEmptyNumber = regexp.MustCompile(`"(\w+)":\s*,`)

var activeHandles sync.Map      // key: *MkvHandle, value: bool
var inFlightPrefetches sync.Map // key: "path:offset", value: bool
var activePumps sync.Map        // Map[string]*NativePumpState — one pump per file path
var pumpTimers sync.Map         // key: path, value: *time.Timer
var priorityTimers sync.Map     // key: path, value: *time.Timer

// OpenTracker: contatori O(1) per handle aperti (per hash e per path).
// Permette query rapide da cleanup e priority timer senza scansionare activeHandles.
var globalOpenTracker = opentracker.New()

// Serializes concurrent pump creation for the same file.
var pumpCreationMu sync.Mutex

// NativePumpState tracks a shared pump across multiple handles for the same file.
type NativePumpState struct {
	cancel    context.CancelFunc
	reader    *NativeReader
	path      string
	refCount  int32
	playerOff int64 // last known player position, saved on handle release
}

// resolveTargetFile finds the torrent hash and file index for a given URL and size.
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

		t := web.BTS.GetTorrent(hash)

		if t != nil {

			files := t.Files()
			sort.Slice(files, func(i, j int) bool {
				return tsutils.CompareStrings(files[i].Path(), files[j].Path())
			})

			var sizeMatchIndex int
			var matchesBySize int

			// Normalize names for matching: strip hash suffixes and separators.
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

			// Single size match: trust it even when name normalization fails (e.g. Plex renames).
			if matchesBySize == 1 {
				return hashStr, sizeMatchIndex, nil
			}
		}

		// Fallback: extract index from URL if torrent not in RAM or name match failed.
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

// Fast deterministic inode from FNV-1a hash to avoid syscalls in Readdir.
// Uses POSIX bits (syscall.S_IFDIR/S_IFREG) for FUSE/Samba/kernel compatibility.
func hashFilenameToInode(name string) uint64 {
	return xxhash.Sum64String(name)
}

type Metadata struct {
	URL, Path, ImdbID string
	Size              int64
	Mtime             time.Time
}

// ReadTiming collects per-read latency metrics for profiling.
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

// fillAttrFromStat populates FUSE attributes from a standard syscall.Stat_t.
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

	// Use time.Now() as cross-platform baseline for virtualized FUSE attributes.
	now := time.Now()
	out.Mtime = sanitizeTime(now)
	out.Atime = sanitizeTime(now)
	out.Ctime = sanitizeTime(now)
}

// fillAttrFromMetadata populates FUSE attributes from our internal Metadata.
func fillAttrFromMetadata(m *Metadata, out *fuse.Attr) {
	out.Size = uint64(m.Size)
	out.Mode = syscall.S_IFREG | 0644
	out.Uid, out.Gid = globalConfig.UID, globalConfig.GID
	out.Nlink = 1
	// out.Blksize = 4096                                 // Standard block size
	out.Blksize = uint32(globalConfig.FuseBlockSize) // Configurable block size (default 1MB)
	out.Blocks = (uint64(m.Size) + 511) / 512        // Estimate blocks based on size

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

	fillAttrFromStat(&st, &out.Attr)

	// Root inode must be constant (required by Plex).
	out.Ino = InodeRoot
	out.Mode = syscall.S_IFDIR | 0755
	out.Size = 4096

	return 0
}

func (r *VirtualMkvRoot) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	fullPath := filepath.Join(r.sourcePath, name)

	if strings.HasSuffix(name, ".mkv") {
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
var _ fs.NodeUnlinker = (*VirtualDirNode)(nil)

func (d *VirtualDirNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
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

	globalDirCache.Put(d.physicalPath, result)

	return &nfsDirStream{entries: result}, 0
}

func (d *VirtualDirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	fullPath := filepath.Join(d.physicalPath, name)

	if strings.HasSuffix(name, ".mkv") {
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
	st := syscall.Stat_t{}
	if err := syscall.Stat(d.physicalPath, &st); err != nil {
		logger.Printf("GETATTR DIR ERROR: %v", err)
		return ToErrno(err)
	}

	fillAttrFromStat(&st, &out.Attr)

	// Use full-path hash to avoid inode collisions (e.g. Season.01 dirs).
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

	// Force-close active pump and handles before removing torrent.
	// Without this, smbd D-states on a file with an active blocking read.
	// V-OpenTracker: attendi che i Read() in volo completino prima di cancellare
	// la pump, per evitare nil-deref su nativeReader durante cancel concorrente.
	if globalOpenTracker.IsPathOpen(fullPath) {
		for i := 0; i < 3; i++ {
			if !globalOpenTracker.IsPathOpen(fullPath) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
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

	RemoveFromRegistry(fullPath)
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
	fillAttrFromMetadata(n.vMeta, &out.Attr)
	out.Ino = getFileInodeFromMap(n.vMeta.Path)
	return 0
}

func (n *VirtualMkvNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if globalConfig.LogLevel == "DEBUG" {
		logger.Printf("=== OPEN VIRTUAL === path=%s", n.vMeta.Path)
	}

	// PROACTIVE CLEANUP TRIGGER (V246): must be sync before any Read() can arrive.
	raCache.SwitchContext(n.vMeta.Path)

	// Cancel any pending release timers (Debounce)
	if oldTimer, ok := pumpTimers.LoadAndDelete(n.vMeta.Path); ok {
		oldTimer.(*time.Timer).Stop()
	}
	if oldTimer, ok := priorityTimers.LoadAndDelete(n.vMeta.Path); ok {
		oldTimer.(*time.Timer).Stop()
	}

	hashStr, urlFileIdx := ExtractHashAndIndex(n.vMeta.URL)

	// hasFullWarmup: Open returns instantly only if both head and tail warmup files are ready.
	// headReady: Allows async Wake and direct ID injection for instant start.
	headReady := false
	if diskWarmup != nil && hashStr != "" {
		headReady = diskWarmup.GetAvailableRange(hashStr, urlFileIdx) > 0
	}

	magnetCandidate := n.vMeta.URL
	if hashStr != "" && (strings.HasPrefix(n.vMeta.URL, "http://") || strings.HasPrefix(n.vMeta.URL, "https://")) {
		magnetCandidate = "magnet:?xt=urn:btih:" + hashStr
	}

	// Async Wake when head warmup is ready (Open returns instantly); sync Wake otherwise.
	if nativeBridge != nil && magnetCandidate != "" {
		if headReady {
			safeGo(func() {
				_ = nativeBridge.Wake(magnetCandidate, urlFileIdx)
			})
		} else {
			_ = nativeBridge.Wake(magnetCandidate, urlFileIdx)
		}
	}

	if val, exists := playbackRegistry.Load(n.vMeta.Path); !exists {
		playbackRegistry.Store(n.vMeta.Path, &PlaybackState{
			Path:     n.vMeta.Path,
			Hash:     hashStr,
			ImdbID:   n.vMeta.ImdbID,
			OpenedAt: time.Now(),
		})
	} else {
		if val != nil {
			state := val.(*PlaybackState)
			state.mu.Lock()
			state.OpenedAt = time.Now()
			state.IsStopped = false

			// Restore priority only if webhook confirmed recently (<30m) to avoid zombie torrents.
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

	// Use warmup IDs directly when available, skipping the resolveTargetFile retry loop.
	var finalHash string
	var fileIdx int
	var isNative bool

	if headReady && hashStr != "" {
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
		hasWarmup:        headReady, // Eligibility for fast SSD probes
	}
	h.state.Store(stateWarmup) // Initial state; transitions to stateStreaming on seek/resume.

	if isNative {
		h.hash = finalHash
		h.fileID = fileIdx
		// Gillian: proactive pump start at Open() — pump ready before first Read().
		// pumpOnce ensures single start; late rescue path in Read() handles hash=='' case.
		h.pumpOnce.Do(func() {
			h.startNativePump(finalHash, fileIdx)
		})
	}

	activeHandles.Store(h, true)

	return h, 0, 0
}

// handleState values for MkvHandle.state (atomic.Uint32).
// A handle transitions one-way: stateWarmup → stateStreaming or stateTailProbe.
const (
	stateWarmup    uint32 = 0 // Initial: SSD warmup eligible (TTFF phase)
	stateStreaming uint32 = 1 // Pump-only streaming; no SSD warmup
	stateTailProbe uint32 = 2 // Plex scan probe: tail region, stateless FetchBlock
)

func stateName(s uint32) string {
	switch s {
	case stateWarmup:
		return "WARMUP"
	case stateStreaming:
		return "STREAMING"
	case stateTailProbe:
		return "TAIL_PROBE"
	default:
		return "UNKNOWN"
	}
}

type MkvHandle struct {
	url, path        string
	size             int64
	lastOff          int64
	lastLen          int
	lastTime         time.Time
	lastActivityTime time.Time
	monitorStarted   bool
	lastGlobalUpdate time.Time

	nativeReader    *NativeReader
	hash            string
	magnet          string
	fileID          int
	mu              sync.Mutex
	pumpCancel      context.CancelFunc
	hasSlot         bool
	isWatching      bool
	hasWarmup       bool          // true if both head+tail warmup available at Open time
	state           atomic.Uint32 // handleState: stateWarmup | stateStreaming | stateTailProbe
	pumpOnce        sync.Once
	isPrimaryHandle bool // true for pump creator and primary reconnects (refCount 0→1)
}

// startNativePump acquires a slot and starts the background pump.
// Called from Open (proactive) or Read (rescue for late resolution).
func (h *MkvHandle) startNativePump(finalHash string, fileIdx int) {
	// 1. Verify we don't already have a slot or an active pump
	if h.hasSlot {
		return
	}

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

	pumpCreationMu.Lock()

	if val, ok := activePumps.Load(h.path); ok {
		ps := val.(*NativePumpState)
		newRefs := atomic.AddInt32(&ps.refCount, 1)
		h.mu.Lock()
		h.hasSlot = true
		h.isWatching = true
		h.nativeReader = ps.reader
		h.pumpCancel = ps.cancel
		h.mu.Unlock()
		globalOpenTracker.Inc(h.hash, h.path)
		// Primary reconnect (refCount 0→1): inherit player position.
		// Secondary handles (Plex probes at arbitrary offsets): no inheritance.
		if newRefs == 1 {
			h.isPrimaryHandle = true
			if curPos := atomic.LoadInt64(&ps.playerOff); curPos > 0 {
				atomic.StoreInt64(&h.lastOff, curPos)
			}
		}
		logger.Printf("[V264] Attached to existing pump (Refs: %d, primary=%v): %s", newRefs, h.isPrimaryHandle, filepath.Base(h.path))
		pumpCreationMu.Unlock()
		return
	}

	// Release mutex before blocking on semaphore to avoid holding it during I/O.
	pumpCreationMu.Unlock()

	select {
	case masterDataSemaphore <- struct{}{}:
		// Double-check activePumps after acquiring semaphore (another goroutine may have created it).
		pumpCreationMu.Lock()
		if val, ok := activePumps.Load(h.path); ok {
			<-masterDataSemaphore
			ps := val.(*NativePumpState)
			newRefs := atomic.AddInt32(&ps.refCount, 1)
			h.mu.Lock()
			h.hasSlot = true
			h.isWatching = true
			h.nativeReader = ps.reader
			h.pumpCancel = ps.cancel
			h.mu.Unlock()
			globalOpenTracker.Inc(h.hash, h.path)
			if newRefs == 1 {
				h.isPrimaryHandle = true
				if curPos := atomic.LoadInt64(&ps.playerOff); curPos > 0 {
					atomic.StoreInt64(&h.lastOff, curPos)
				}
			}
			pumpCreationMu.Unlock()
			return
		}

		h.hasSlot = true
		h.isPrimaryHandle = true // pump creator is always primary
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
		globalOpenTracker.Inc(h.hash, h.path)
		pumpCreationMu.Unlock() // SUCCESS: Shared state registered, global lock released

		logger.Printf("[V264] Native Pump Started (Slot Acquired): %s", filepath.Base(h.path))

		// Start background pump — resume from last cached position
		resumeOffset := raCache.MaxCachedOffset(h.path)

		// Start pump near end of warmup zone so it buffers past 64MB before SSD handover.
		if diskWarmup != nil && h.hash != "" {
			diskOffset := diskWarmup.GetAvailableRange(h.hash, h.fileID)
			if diskOffset > 16*1024*1024 {
				safetyMargin := int64(16 * 1024 * 1024)
				skipOffset := diskOffset - safetyMargin
				if skipOffset > resumeOffset {
					resumeOffset = skipOffset
					logger.Printf("[DiskWarmup] PUMP SKIP: Starting from %.1fMB to bridge SSD handover", float64(resumeOffset)/(1<<20))
				}
			}
		}

		// Anchor pump to player position when MaxCachedOffset is stale-high to prevent EOF loops.
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
			// New handle: reset stale MaxCachedOffset unless warmup is active and covers the range.
			// If resumeOffset >= warmupFileSize, pump skip cannot fire → dead zone in raCache.
			warmupCoverage := int64(0)
			if diskWarmup != nil && h.hash != "" {
				warmupCoverage = diskWarmup.GetAvailableRange(h.hash, h.fileID)
			}
			if warmupCoverage == 0 || resumeOffset >= warmupFileSize {
				logger.Printf("[V700] New handle: reset stale MaxCachedOffset %.1fMB → 0",
					float64(resumeOffset)/(1<<20))
				resumeOffset = 0
			}
		}

		// Anchor pump to player position on resume to eliminate anacrolix priority competition.
		{
			chunkSize := int64(globalConfig.ReadAheadBase)
			if chunkSize == 0 {
				chunkSize = 16 * 1024 * 1024
			}
			if raV310 := atomic.LoadInt64(&h.lastOff); raV310 > 0 && resumeOffset+chunkSize < raV310 {
				pumpStartV310 := (raV310 / chunkSize) * chunkSize
				logger.Printf("[V310] Resume anchor: pump start → %dMB (player at %dMB)",
					pumpStartV310/(1024*1024), raV310/(1024*1024))
				resumeOffset = pumpStartV310
			}
		}

		if resumeOffset > 0 {
			atomic.StoreInt64(&h.lastOff, resumeOffset)
		}

		pumpStart := resumeOffset
		capturedState := sharedState
		safeGo(func() {
			h.nativePump(pumpCtx, pumpStart, capturedState)
		})
	default:
		// If slots are full, it will fall back to per-request slots in Read
		logger.Printf("[MasterSemaphore] Limit reached, %s will use Fallback mode", filepath.Base(h.path))
	}
}

// nativePump reads continuously from the Native pipe and fills raCache.
// sharedState guards against orphan-delete in defer.
func (h *MkvHandle) nativePump(ctx context.Context, startOffset int64, sharedState *NativePumpState) {
	pumpReader := h.nativeReader
	if pumpReader == nil {
		logger.Printf("[Pump] reader is nil at startup for %s", filepath.Base(h.path))
		return
	}

	if h.hash == "" {
		// Late hash resolution for handles where Open() didn't complete it.
		if hash, fileID, err := resolveTargetFile(h.url, h.size, h.path); err == nil {
			h.hash = hash
			h.fileID = fileID
			logger.Printf("[Pump] Late resolution success: %s", h.hash[:8])
		} else {
			logger.Printf("[Pump] Warning: hash empty for %s, warmup disabled", filepath.Base(h.path))
		}
	}
	defer func() {
		h.mu.Lock()
		// Only delete if our sharedState is still the registered one (prevents pump A's defer from deleting pump B).
		if val, ok := activePumps.Load(h.path); ok && val == sharedState {
			activePumps.Delete(h.path)
		}

		if h.hasSlot {
			select {
			case <-masterDataSemaphore:
				// Slot released
			default:
				// Should not happen
			}
			h.hasSlot = false
		}
		pumpReader.Close()
		h.mu.Unlock()
		logger.Printf("[V239] Native Pump Goroutine Ended: %s", filepath.Base(h.path))
	}()

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

		// Release idle slot: confirmed playback gets 2h, background scans get 45s.
		// Check all handles for this path, not just the pump creator.
		lastAct := time.Time{}
		activeHandles.Range(func(key, value interface{}) bool {
			handle := key.(*MkvHandle)
			if handle.path == h.path {
				handle.mu.Lock()
				if handle.lastActivityTime.After(lastAct) {
					lastAct = handle.lastActivityTime
				}
				handle.mu.Unlock()
			}
			return true
		})
		// Fallback to pump creator's activity if no active handles found
		if lastAct.IsZero() {
			h.mu.Lock()
			lastAct = h.lastActivityTime
			h.mu.Unlock()
		}

		timeoutLimit := 45 * time.Second
		if val, ok := playbackRegistry.Load(h.path); ok {
			if ps, ok := val.(*PlaybackState); ok {
				if ps.GetStatus() {
					timeoutLimit = 2 * time.Hour
				} else if ps.IsInferredPlayback() {
					timeoutLimit = 10 * time.Minute // V750: Inferred playback
				}
			}
		}

		if time.Since(lastAct) > timeoutLimit {
			logger.Printf("[V262] Idle timeout (%v) for %s - yielding slot", timeoutLimit, filepath.Base(h.path))
			return
		}

		// Sync to primary handles only; secondary metadata probes cause false 10GB+ jumps.
		playerOff := int64(0)
		activeHandles.Range(func(key, value interface{}) bool {
			handle := key.(*MkvHandle)
			if handle.path == h.path && handle.isPrimaryHandle {
				off := atomic.LoadInt64(&handle.lastOff)
				if off > playerOff {
					playerOff = off
				}
			}
			return true
		})

		// Snap pump to player position when seek gap exceeds budget, aligned to chunk boundary.
		jumpThreshold := int64(globalConfig.ReadAheadBudget)
		if playerOff > offset+jumpThreshold {
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

		// Throttle background pump after 64MB grace period.
		if pumpedBytes > 64*1024*1024 {
			isHealthy := false
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

		stop, nextOffset := h.nativePumpChunk(pumpReader, offset, chunkSize, playerOff)
		if stop {
			// Transient errors (seek interrupt, reconnect, piece timeout): retry until genuine EOF.
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

// nativePumpChunk reads a single chunk from the Native pipe into raCache.
func (h *MkvHandle) nativePumpChunk(r *NativeReader, offset, chunkSize, playerOff int64) (stop bool, nextOffset int64) {
	// Don't pump beyond file size
	if offset >= h.size {
		return true, offset
	}

	budget := globalConfig.ReadAheadBudget
	diff := offset - playerOff

	if diff > budget {
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

	if data := raCache.Get(h.path, offset, offset); data != nil {
		if diskWarmup != nil && h.hash != "" && offset <= warmupFileSize {
			diskWarmup.WriteChunk(h.hash, h.fileID, data, offset)
		}
		return false, offset + chunkSize
	}

	// Skip warmup zone during initial play (SSD serves 0-80MB); pump jumps ahead to pre-fill raCache.
	// Gated on stateWarmup to avoid skip on resume/seek.
	if diskWarmup != nil && h.hash != "" && h.state.Load() == stateWarmup {
		warmupCoverage := diskWarmup.GetAvailableRange(h.hash, h.fileID)
		if warmupCoverage >= offset+chunkSize {
			return false, offset + chunkSize
		}
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
		if diskWarmup != nil && h.hash != "" && offset <= warmupFileSize {
			diskWarmup.WriteChunk(h.hash, h.fileID, (*bufPtr)[:n], offset)
		}
	}

	if err != nil {
		return true, offset + int64(n)
	}

	return false, offset + int64(n)
}

// shouldInterruptForSeek returns true for genuine seeks beyond budget.
// Ignores: new handles (prevOff<=0), Samba header probes (off==0), sequential reads.
func shouldInterruptForSeek(prevOff, off, budget int64) bool {
	if prevOff <= 0 || off == 0 {
		return false
	}
	return off > prevOff+budget || prevOff > off+budget
}

// safeGo runs a function in a new goroutine with panic recovery.
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
	now := time.Now()
	timing := &ReadTiming{StartTime: now}
	defer func() {
		timing.TotalTime = time.Since(timing.StartTime)
		globalProfilingStats.RecordRead(timing)
	}()

	if off >= h.size {
		return fuse.ReadResultData(nil), 0
	}

	h.mu.Lock()
	// Late hash recovery: if Open() failed to resolve (metadata lag), retry now.
	if h.hash == "" && h.url != "" {
		if hash, fileID, err := resolveTargetFile(h.url, h.size, h.path); err == nil {
			h.hash = hash
			h.fileID = fileID
			logger.Printf("[LateResolution] Recovered hash for %s: %s", filepath.Base(h.path), h.hash[:8])
			go h.pumpOnce.Do(func() {
				h.startNativePump(h.hash, h.fileID)
			})
		}
	}

	idleTime := now.Sub(h.lastActivityTime)
	isFirstBlock := (off == 0) || (idleTime > time.Duration(globalConfig.WarmStartIdleSeconds)*time.Second)
	h.lastActivityTime = now

	// V750: Update PlaybackState inference tracking
	if val, ok := playbackRegistry.Load(h.path); ok {
		if ps, ok := val.(*PlaybackState); ok {
			ps.mu.Lock()
			ps.ReadCount++
			ps.LastReadAt = now
			if off > 2*1024*1024 && off > ps.LastSeekOff {
				ps.LastSeekOff = off
			}
			ps.mu.Unlock()
		}
	}

	if now.Sub(h.lastGlobalUpdate) > 1*time.Minute {
		globalCleanupManager.UpdateActivity(h.path)
		h.lastGlobalUpdate = now
	}
	h.mu.Unlock()

	prevOff := atomic.LoadInt64(&h.lastOff)
	atomic.StoreInt64(&h.lastOff, off)

	// Transition WARMUP→STREAMING on resume (first read >= warmupFileSize) or seek (jump > budget).
	// Checked after SSD path above so initial reads within warmup zone are still served.
	if h.state.Load() == stateWarmup {
		isSeek := false
		if prevOff == -1 {
			if off >= warmupFileSize {
				isSeek = true
			}
		} else if off != 0 {
			budget := int64(globalConfig.ReadAheadBudget)
			if off > prevOff+budget || prevOff > off+budget {
				isSeek = true
			}
		}
		if isSeek {
			h.state.Store(stateStreaming)
			logger.Printf("[Warmup] Seek/Resume detected (off=%dMB): %s→%s.", off/(1024*1024), stateName(stateWarmup), stateName(stateStreaming))
		}
	}

	// Detect pre-confirmation tail probe (5% of file, 64MB–2GB) to suppress pump interrupt.
	dynamicThreshold := h.size / 20 // 5%
	if dynamicThreshold < 64*1024*1024 {
		dynamicThreshold = 64 * 1024 * 1024
	}
	if dynamicThreshold > 2*1024*1024*1024 {
		dynamicThreshold = 2 * 1024 * 1024 * 1024
	}

	// Transition WARMUP→TAIL_PROBE on first tail region read during discovery phase.
	if h.state.Load() == stateWarmup && h.hash != "" && h.size > dynamicThreshold && off >= h.size-dynamicThreshold {
		isUnconfirmed := true
		if val, ok := playbackRegistry.Load(h.path); ok {
			ps := val.(*PlaybackState)
			ps.mu.RLock()
			isUnconfirmed = ps.ConfirmedAt.IsZero()
			ps.mu.RUnlock()
		}
		if isUnconfirmed {
			h.state.Store(stateTailProbe)
		}
	}
	isTailProbe := h.state.Load() == stateTailProbe

	// Interrupt pump on genuine seeks; skip for SSD tail reads (pump must stay alive).
	budget := int64(globalConfig.ReadAheadBudget)
	if h.nativeReader != nil && !isTailProbe && shouldInterruptForSeek(prevOff, off, budget) {
		h.nativeReader.Interrupt()
		torrstor.ResetShield()
		h.state.Store(stateStreaming)
		logger.Printf("[V286] Interrupt pump for seek+shield reset: %dMB → %dMB (%s→%s)",
			prevOff/(1024*1024), off/(1024*1024), stateName(stateWarmup), stateName(stateStreaming))
	}

	// Serve warmup zone from SSD (up to 80MB with boundary chunk); stateWarmup gate skips SSD on resume/seek.
	if diskWarmup != nil && h.hash != "" && h.state.Load() == stateWarmup {
		warmupCoverage := diskWarmup.GetAvailableRange(h.hash, h.fileID)
		if off < warmupCoverage {
			n, _ := diskWarmup.ReadAt(h.hash, h.fileID, dest, off)
			if n > 0 {
				timing.UsedCache = true
				timing.BytesRead = n
				if off == 0 {
					logger.Printf("[DiskWarmup] HIT %s off=0 (%dKB)", filepath.Base(h.path), n/1024)
				}
				atomic.StoreInt64(&h.lastOff, off)

				h.mu.Lock()
				h.lastLen = n
				h.lastTime = now
				h.mu.Unlock()
				return fuse.ReadResultData(dest[:n]), 0
			}
		}
	}

	// Serve tail from SSD only during discovery (pre-confirmation); post-confirmation uses pump.
	if isTailProbe && diskWarmup != nil {
		n, _ := diskWarmup.ReadTail(h.hash, h.fileID, dest, off, h.size)
		if n > 0 {
			timing.UsedCache = true
			timing.BytesRead = n
			h.mu.Lock()
			h.lastLen, h.lastTime = n, now
			h.mu.Unlock()
			return fuse.ReadResultData(dest[:n]), 0
		}

		// On SSD tail miss, use stateless FetchBlock to preserve head pump.
		nFetch, err := nativeBridge.FetchBlock(h.hash, h.fileID, off, dest)
		if err == nil && nFetch > 0 {
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

	cacheStart := time.Now()
	if n := raCache.CopyTo(h.path, off, end, dest); n > 0 {
		timing.CacheHitTime = time.Since(cacheStart)
		timing.UsedCache = true
		timing.BytesRead = n

		atomic.StoreInt64(&h.lastOff, off)

		// Predictive prefetch: fetch next chunk if pump is absent or near boundary.
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

		if isFirstBlock {
			raSize = int64(globalConfig.ReadAheadInitial)
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

	if !h.hasSlot {
		pumpCreationMu.Lock()

		// Attach to existing pump if one is already running for this path.
		if val, ok := activePumps.Load(h.path); ok {
			ps := val.(*NativePumpState)
			atomic.AddInt32(&ps.refCount, 1) // Increment reference count
			h.mu.Lock()
			h.hasSlot = true
			h.isWatching = true
			h.nativeReader = ps.reader
			h.pumpCancel = ps.cancel
			h.mu.Unlock()
			globalOpenTracker.Inc(h.hash, h.path)
			logger.Printf("[V258] Handle ATTACHED to existing active pump (Refs: %d): %s", atomic.LoadInt32(&ps.refCount), filepath.Base(h.path))
		}
		// Unlock early if attached or not needed
		if h.hasSlot {
			pumpCreationMu.Unlock()
		} else {
			// On-the-fly pump upgrade for confirmed playback with available slot.
			if isStreaming && h.hash != "" {
				if val, ok := playbackRegistry.Load(h.path); ok {
					if ps, ok := val.(*PlaybackState); ok && ps.GetStatus() {
						select {
						case masterDataSemaphore <- struct{}{}:
							h.hasSlot = true
							h.nativeReader = nativeBridge.NewStreamReader(h.hash, h.fileID, h.size)
							pumpCtx, pumpCancel := context.WithCancel(context.Background())
							h.pumpCancel = pumpCancel

							sharedState := &NativePumpState{
								cancel:   pumpCancel,
								reader:   h.nativeReader,
								path:     h.path,
								refCount: 1,
							}
							activePumps.Store(h.path, sharedState)
							globalOpenTracker.Inc(h.hash, h.path)

							logger.Printf("[Pump] Upgraded on-the-fly for confirmed playback: %s", filepath.Base(h.path))

							hHash := metainfo.NewHashFromHex(h.hash)
							if t := web.BTS.GetTorrent(hHash); t != nil {
								t.SetAggressiveMode(true, GetEffectiveConcurrencyLimit())
								logger.Printf("[Pump] Aggressive mode enabled on-the-fly for: %s", h.hash[:8])
							}

							upgradedState := sharedState
							safeGo(func() {
								h.nativePump(pumpCtx, off, upgradedState)
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
			case <-time.After(30 * time.Second):
				logger.Printf("[MasterSemaphore] Timeout waiting for slot: %s", filepath.Base(h.path))
				return nil, syscall.ETIMEDOUT
			}
		}
	}

	// Rate limiting for non-streaming (metadata) requests only; streaming bypasses to preserve playback priority.
	if !isStreaming {
		rateLimitCtx, rateLimitCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer rateLimitCancel()
		if err := globalRateLimiter.Acquire(rateLimitCtx); err != nil {
			logger.Printf("Rate limit timeout: %v", err)
			return nil, ToErrno(err)
		}
	}

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

	timing.BytesRead = target

	if n > 0 {
		raCache.Put(h.path, off, off+int64(n)-1, buf[:n])

		if diskWarmup != nil && h.hash != "" {
			if off <= warmupFileSize {
				diskWarmup.WriteChunk(h.hash, h.fileID, buf[:n], off)
			} else if h.size > tailWarmupSize && off >= h.size-tailWarmupSize {
				// Freeze tail SSD cache after playback confirmation to preserve discovery snapshot.
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

		globalCleanupManager.UpdateOffset(h.path, off, target)

		nCopy := copy(dest, buf[:n])

		// Prefetch next chunk if in last 25% of current chunk and pump is absent or lagging.
		chunkSize := int64(globalConfig.ReadAheadBase)
		currentChunkIndex := off / chunkSize
		nextChunkStart := (currentChunkIndex + 1) * chunkSize
		distanceToNext := nextChunkStart - off

		maxCached := raCache.MaxCachedOffset(h.path)
		isLagging := maxCached < nextChunkStart

		if isStreaming && (!h.hasSlot || isLagging) {
			if distanceToNext < chunkSize/4 {
				prefetchKey := fmt.Sprintf("%s:%d", h.path, nextChunkStart)
				if _, loaded := inFlightPrefetches.LoadOrStore(prefetchKey, true); !loaded {
					goStart, goSize, goKey, goHash, goFileID := nextChunkStart, int64(globalConfig.ReadAheadBase), prefetchKey, h.hash, h.fileID
					safeGo(func() {
						defer inFlightPrefetches.Delete(goKey)

						// Check if already in cache to avoid useless work
						if raCache.Exists(h.path, goStart) {
							return // Already cached
						}

						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						if err := globalRateLimiter.Acquire(ctx); err != nil {
							return
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

	if target > len(buf) {
		target = len(buf)
	}

	nCopy := copy(dest, buf[:target])
	return fuse.ReadResultData(dest[:nCopy]), 0
}

func (h *MkvHandle) Release(fuseCtx context.Context) syscall.Errno {
	logger.Printf("=== RELEASE VIRTUAL === path=%s", h.path)

	if val, ok := activePumps.Load(h.path); ok {
		ps := val.(*NativePumpState)
		// Only primary handles persist position; secondary probes have arbitrary offsets.
		if h.isPrimaryHandle {
			if pos := atomic.LoadInt64(&h.lastOff); pos > 0 {
				atomic.StoreInt64(&ps.playerOff, pos)
			}
		}
		// Only decrement if this handle acquired a slot; probe/header reads must not decrement.
		if !h.hasSlot {
			return 0
		}
		newRefs := atomic.AddInt32(&ps.refCount, -1)
		globalOpenTracker.Dec(h.hash, h.path)
		logger.Printf("[Pump] Release handle for %s (Remaining Refs: %d)", filepath.Base(h.path), newRefs)

		if newRefs <= 0 {
			// V306: If playback is healthy (webhook media.play confirmed), skip grace period.
			// The pump stays alive until media.stop webhook sets IsHealthy=false and kills it.
			// This survives long pauses, buffering gaps, and Apple TV re-reads without
			// killing the pump and causing freeze on resume.
			if pbVal, ok := playbackRegistry.Load(h.path); ok {
				if pbState := pbVal.(*PlaybackState); pbState.IsHealthy {
					logger.Printf("[V306] Healthy playback — pump stays alive (no grace period) for %s", filepath.Base(h.path))
					// Stop any pending grace timer from a previous release cycle
					if oldTimer, ok := pumpTimers.LoadAndDelete(h.path); ok {
						oldTimer.(*time.Timer).Stop()
					}
					goto skipGrace
				}
			}

			// Grace period: 30s for unconfirmed probes/scans.
			graceDuration := 30 * time.Second
			if pbVal, ok := playbackRegistry.Load(h.path); ok {
				if pbState := pbVal.(*PlaybackState); !pbState.ConfirmedAt.IsZero() {
					graceDuration = 90 * time.Second
					if pbState.IsInferredPlayback() {
						graceDuration = 5 * time.Minute // V750
						logger.Printf("[V750] Inferred playback — extended grace (5m) for %s", filepath.Base(h.path))
					}
				} else if pbState.IsInferredPlayback() {
					graceDuration = 5 * time.Minute // V750
					logger.Printf("[V750] Inferred playback — extended grace (5m) for %s", filepath.Base(h.path))
				}
			}
			if oldTimer, ok := pumpTimers.LoadAndDelete(h.path); ok {
				oldTimer.(*time.Timer).Stop()
			}
			var t *time.Timer
			t = time.AfterFunc(graceDuration, func() {
				pumpTimers.CompareAndDelete(h.path, t)
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
			pumpTimers.Store(h.path, t)
			logger.Printf("[V420] Last handle closed: Shared Pump entering %s grace period for %s", graceDuration, filepath.Base(h.path))
		}

	skipGrace:
	}

	// Nil local reference only; pump goroutine owns the reader lifecycle via captured copy.
	h.nativeReader = nil

	activeHandles.Delete(h)

	// Fast-drop (5s) for scanner probes never confirmed by webhook; 30s otherwise.
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

	if oldTimer, ok := priorityTimers.LoadAndDelete(h.path); ok {
		oldTimer.(*time.Timer).Stop()
	}
	var t *time.Timer
	t = time.AfterFunc(retentionDelay, func() {
		priorityTimers.CompareAndDelete(h.path, t)

		// V750: Don't remove priority if pump is still active for this path
		if _, pumpOk := activePumps.Load(h.path); pumpOk {
			logger.Printf("[V750] Priority retained — pump still active for %s", filepath.Base(h.path))
			return
		}

		// O(1): controlla se il path ha ancora handle aperti prima di disabilitare priority.
		if globalOpenTracker.IsPathOpen(h.path) {
			return
		}

		if val, ok := playbackRegistry.Load(h.path); ok {
			state := val.(*PlaybackState)
			if state.Hash != "" {
				hHash := metainfo.NewHashFromHex(state.Hash)

				// O(1): controlla se altri file dello stesso torrent sono ancora aperti.
				if globalOpenTracker.IsHashOpen(state.Hash) {
					// Another file from the same torrent is still open, keep priority.
					return
				}

				if t := web.BTS.GetTorrent(hHash); t != nil {
					t.IsPriority = false
					t.SetAggressiveMode(false, 0)

					// Fast-drop scanner handles never confirmed by webhook.
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
		// Registry entry kept (without priority) for fast webhook-triggered resume.
		// Cleanup handled by GlobalCleanupManager (15 min timeout).
		// playbackRegistry.Delete(h.path)
	})
	priorityTimers.Store(h.path, t)

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
	start, end     int64
	data           []byte
	lastAccess     int64
	sessionID      int64
	responsiveOnly bool // true if written in non-verified mode (responsive shield active)
}

// ReadAheadCache is a 32-shard LRU cache with session-aware eviction.
type ReadAheadCache struct {
	shards    [32]*raShard
	shardMask uint64
	used      int64
	pool      chan []byte // recycled 16MB chunks

	muContext        sync.Mutex
	activePath       string
	currentSessionID int64
	isEvicting       int32 // atomic flag prevents concurrent global evictions
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
	return c.shards[xxhash.Sum64String(path)&c.shardMask]
}

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

// SwitchContext increments SessionID on path change to invalidate stale data.
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
	if b, ok := s.buffers[key]; ok && off >= b.start && off <= b.end {
		atomic.StoreInt64(&b.lastAccess, time.Now().UnixNano())
		if end <= b.end {
			// Defensive copy: pool evicts buffers immediately; sub-slice reference causes use-after-free.
			src := b.data[off-b.start : end-b.start+1]
			out := make([]byte, len(src))
			copy(out, src)
			return out
		}
		// Cross-boundary read: stitch two adjacent chunks to avoid FetchBlock on chunk boundary straddles.
		if b2, ok2 := s.buffers[raChunkKey(p, end)]; ok2 && b2.start == b.end+1 && b2.end >= end {
			atomic.StoreInt64(&b2.lastAccess, time.Now().UnixNano())
			out := make([]byte, end-off+1)
			n1 := copy(out, b.data[off-b.start:])
			copy(out[n1:], b2.data[:end-b2.start+1])
			return out
		}
	}
	return nil
}

// Exists checks if a chunk is present in cache without allocating.
func (c *ReadAheadCache) Exists(p string, off int64) bool {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := raChunkKey(p, off)
	_, found := s.buffers[key]
	return found
}

// CopyTo copies data directly into dest, avoiding an intermediate allocation in the FUSE Read hot path.
func (c *ReadAheadCache) CopyTo(p string, off, end int64, dest []byte) int {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := raChunkKey(p, off)
	if b, ok := s.buffers[key]; ok && off >= b.start && off <= b.end {
		atomic.StoreInt64(&b.lastAccess, time.Now().UnixNano())
		if end <= b.end {
			// Fast path: entirely within one chunk.
			src := b.data[off-b.start : end-b.start+1]
			return copy(dest, src)
		}
		// Cross-boundary read: same logic as Get().
		if b2, ok2 := s.buffers[raChunkKey(p, end)]; ok2 && b2.start == b.end+1 && b2.end >= end {
			atomic.StoreInt64(&b2.lastAccess, time.Now().UnixNano())
			n1 := copy(dest, b.data[off-b.start:])
			n2 := copy(dest[n1:], b2.data[:end-b2.start+1])
			return n1 + n2
		}
	}
	return 0
}

func (c *ReadAheadCache) Put(p string, start, end int64, d []byte) {
	c.muContext.Lock()
	sessID := c.currentSessionID
	// Active path uses current session ID; stale pumps use sessID=0 so eviction can identify them.
	if p != c.activePath {
		sessID = 0
	}
	c.muContext.Unlock()

	shard := c.getShard(p)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	key := raChunkKey(p, start)

	dataSize := int64(len(d))

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

	globalLimit := globalConfig.ReadAheadBudget
	if globalLimit <= 0 {
		globalLimit = 256 * 1024 * 1024 // Fail-safe default
	}

	// 1. Account for overwrite
	if old, ok := shard.buffers[key]; ok {
		shard.total -= int64(len(old.data))
		atomic.AddInt64(&c.used, -int64(len(old.data)))
		c.recycle(old.data)
		// Promote to MRU on overwrite to prevent premature eviction of recently refreshed chunks.
		for i, k := range shard.order {
			if k == key {
				shard.order = append(shard.order[:i], shard.order[i+1:]...)
				break
			}
		}
	}
	shard.order = append(shard.order, key)

	// 2. Add new data
	shard.total += dataSize
	newUsed := atomic.AddInt64(&c.used, dataSize)
	responsiveOnly := torrstor.IsResponsive()
	shard.buffers[key] = &RaBuffer{start, end, dataCopy, time.Now().UnixNano(), sessID, responsiveOnly}

	// Evict from this shard while over budget; keep last item to avoid evicting the chunk just added.
	for newUsed > globalLimit && len(shard.order) > 1 {
		v := shard.order[0]
		shard.order = shard.order[1:]
		if old, ok := shard.buffers[v]; ok {
			evictedSize := int64(len(old.data))
			shard.total -= evictedSize
			delete(shard.buffers, v)
			newUsed = atomic.AddInt64(&c.used, -evictedSize)
			c.recycle(old.data)
		}
	}

	// Local shard exhausted: trigger global eviction to free stale data from other shards.
	if newUsed > globalLimit && len(shard.order) <= 1 {
		c.muContext.Lock()
		ap := c.activePath
		sid := c.currentSessionID
		c.muContext.Unlock()
		safeGo(func() {
			c.triggerGlobalEviction(ap, sid)
		})
	}

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
	totalBytes = atomic.LoadInt64(&c.used)

	now := time.Now().UnixNano()
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

// triggerGlobalEviction removes stale session data and old chunks from all shards.
func (c *ReadAheadCache) triggerGlobalEviction(activePath string, activeSessionID int64) {
	// Single-flight: skip if already evicting.
	if !atomic.CompareAndSwapInt32(&c.isEvicting, 0, 1) {
		return
	}
	defer atomic.StoreInt32(&c.isEvicting, 0)

	now := time.Now().UnixNano()
	staleThreshold := (120 * time.Second).Nanoseconds()

	evictShard := func(s *raShard) {
		var newOrder []string
		for _, key := range s.order {
			keep := true
			if buf, ok := s.buffers[key]; ok {
				// 1. Session ID Check (Fastest)
				if buf.sessionID != activeSessionID && !strings.HasPrefix(key, activePath+":") {
					keep = false
				} else {
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
					c.recycle(buf.data)
				}
			}

			if keep {
				newOrder = append(newOrder, key)
			}
		}
		s.order = newOrder
	}

	skipped := 0
	for _, s := range c.shards {
		if !s.mu.TryLock() {
			skipped++
			continue
		}
		evictShard(s)
		s.mu.Unlock()
	}

	// If all shards were busy, force blocking eviction on first shard to prevent budget overflow.
	if skipped == len(c.shards) {
		s := c.shards[0]
		s.mu.Lock()
		evictShard(s)
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
	logger.Printf("[PLEX] Webhook connection from %s", r.RemoteAddr)

	var payloadStr string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
		if err != nil {
			http.Error(w, "Bad request", 400)
			return
		}
		payloadStr = string(body)
	} else {
		if err := r.ParseMultipartForm(10 * 1024 * 1024); err != nil {
			http.Error(w, "Bad request", 400)
			return
		}
		payloadStr = r.FormValue("payload")
	}
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
			GrandparentTitle   string `json:"grandparentTitle"` // for TV series
			Year               int    `json:"year"`
			LibrarySectionType string `json:"librarySectionType"`
			Media              []struct {
				Part []struct {
					File string `json:"file"`
				} `json:"Part"`
			} `json:"Media"`
		} `json:"Metadata"`
	}

	// Sanitize empty numeric fields produced by Jellyfin templates (e.g. "year":, → "year":0,)
	sanitized := reEmptyNumber.ReplaceAllString(payloadStr, `"$1":0`)
	if err := json.Unmarshal([]byte(sanitized), &payload); err != nil {
		return
	}

	// Normalize Jellyfin event names to Plex-style
	switch payload.Event {
	case "PlaybackStart":
		payload.Event = "media.play"
	case "PlaybackStop":
		payload.Event = "media.stop"
	case "PlaybackProgress":
		payload.Event = "media.resume"
	}

	// Normalize Jellyfin ItemType values to Plex-style ("Movie"→"movie", "Episode"→"show")
	switch payload.Metadata.LibrarySectionType {
	case "Movie":
		payload.Metadata.LibrarySectionType = "movie"
	case "Episode":
		payload.Metadata.LibrarySectionType = "show"
	}

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

		// Two-pass matching: exact first (IMDB, hash, filename), fuzzy only as fallback.

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

		// Extract IMDB ID via regex on raw payload (struct unmarshal would cause UnmarshalTypeError
		// due to Plex sending both lowercase "guid" and capital "Guid" fields).
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

		// Pass 1c: IMDB bootstrap — if webhookImdbID is available but no state has it yet,
		// find the unique registered path of the matching library type with empty ImdbID.
		// One-time bootstrap: saves webhookImdbID into state so future sessions match via 0a.
		if exactMatch == "" && webhookImdbID != "" {
			sectionDir := ""
			switch payload.Metadata.LibrarySectionType {
			case "show":
				sectionDir = "/tv/"
			case "movie":
				sectionDir = "/movies/"
			}
			if sectionDir != "" {
				var bootPath string
				var bootState *PlaybackState
				bootCount := 0
				playbackRegistry.Range(func(key, value interface{}) bool {
					path := key.(string)
					state := value.(*PlaybackState)
					if strings.Contains(path, sectionDir) && state.ImdbID == "" {
						bootPath = path
						bootState = state
						bootCount++
					}
					return true
				})
				if bootCount == 1 {
					exactMatch = bootPath
					exactState = bootState
				}
			}
		}

		// Pass 2: Fuzzy matches only if no exact match found
		if exactMatch == "" {
			var bestMatch string
			var bestState *PlaybackState
			bestLevel := 0 // higher = better match

			playbackRegistry.Range(func(key, value interface{}) bool {
				path := key.(string)
				filename := strings.ToLower(filepath.Base(path))
				level := 0

				// Tentativo 2: Match per Titolo completo (dot and underscore separators)
				if targetTitle != "" && (strings.Contains(filename, strings.ReplaceAll(targetTitle, " ", ".")) ||
					strings.Contains(filename, strings.ReplaceAll(targetTitle, " ", "_"))) {
					level = 3
				}

				// Tentativo 3: Match per Titolo Serie (TV Shows)
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

				// Tentativo 4: Match per prima parola + anno (first word > 3 chars to avoid "the", "a")
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
			exactState.IsStopped = false
			// Cache webhookImdbID into state for fast IMDB matching in future sessions.
			if exactState.ImdbID == "" && webhookImdbID != "" {
				exactState.ImdbID = webhookImdbID
				logger.Printf("[PLEX] IMDB ID cached for future matching: %s → %s", filepath.Base(exactMatch), webhookImdbID)
			}
			exactState.mu.Unlock()
			logger.Printf("[PLEX] Playback confirmed by webhook for: %s", filepath.Base(exactMatch))

			if exactState.Hash != "" {
				h := metainfo.NewHashFromHex(exactState.Hash)
				if t := web.BTS.GetTorrent(h); t != nil {
					t.IsPriority = true
					t.SetAggressiveMode(true, GetEffectiveConcurrencyLimit())
					logger.Printf("[PLEX] High Priority + Aggressive Mode for: %s", exactState.Hash)
				}
			}
		}
	} else if payload.Event == "media.stop" {
		targetTitle := strings.ToLower(payload.Metadata.Title)
		seriesTitle := strings.ToLower(payload.Metadata.GrandparentTitle)
		targetYear := payload.Metadata.Year

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

				if targetTitle != "" && (strings.Contains(filename, strings.ReplaceAll(targetTitle, " ", ".")) ||
					strings.Contains(filename, strings.ReplaceAll(targetTitle, " ", "_"))) {
					level = 3
				}

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
			// V750: Persist state change
			savePlaybackStateToDB(stopState)
			stopState.mu.Lock()
			stopState.IsStopped = true
			stopState.mu.Unlock()
			logger.Printf("[PLEX] Priority removed for: %s (Event: %s)", filepath.Base(stopMatch), payload.Event)

			if val, ok := activePumps.Load(stopMatch); ok {
				ps := val.(*NativePumpState)
				if ps.cancel != nil {
					ps.cancel()
				}
				activePumps.Delete(stopMatch)
				logger.Printf("[PLEX] STOP: force-terminated pump for %s", filepath.Base(stopMatch))
			}

			torrstor.ResetShield()
			logger.Printf("[AdaptiveShield] Shield reset on media.stop")

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

// V750: Persist a PlaybackState to SQLite (best-effort)
func savePlaybackStateToDB(ps *PlaybackState) {
	if stateDB == nil {
		return
	}
	ps.mu.RLock()
	rec := &metadb.PlaybackRecord{
		Path:        ps.Path,
		Hash:        ps.Hash,
		ImdbID:      ps.ImdbID,
		OpenedAt:    ps.OpenedAt,
		ConfirmedAt: ps.ConfirmedAt,
		IsHealthy:   ps.IsHealthy,
		IsStopped:   ps.IsStopped,
		LastReadAt:  ps.LastReadAt,
		ReadCount:   ps.ReadCount,
		LastSeekOff: ps.LastSeekOff,
	}
	ps.mu.RUnlock()
	if err := stateDB.SavePlaybackState(rec); err != nil {
		logger.Printf("[V750] Failed to persist playback state for %s: %v", filepath.Base(rec.Path), err)
	}
}

// V750: Restore PlaybackState from SQLite at boot
func restorePlaybackStates(db *metadb.DB) {
	records, err := db.LoadPlaybackStates(4 * time.Hour)
	if err != nil {
		logger.Printf("[V750] Failed to load playback states: %v", err)
		return
	}
	restored := 0
	for _, rec := range records {
		if rec.IsHealthy && !rec.ConfirmedAt.IsZero() {
			ps := &PlaybackState{
				Path:        rec.Path,
				Hash:        rec.Hash,
				ImdbID:      rec.ImdbID,
				OpenedAt:    rec.OpenedAt,
				ConfirmedAt: rec.ConfirmedAt,
				IsHealthy:   true,
				ReadCount:   rec.ReadCount,
				LastSeekOff: rec.LastSeekOff,
				LastReadAt:  rec.LastReadAt,
			}
			playbackRegistry.Store(rec.Path, ps)
			// Restore GoStorm priority
			if rec.Hash != "" {
				hHash := metainfo.NewHashFromHex(rec.Hash)
				if t := web.BTS.GetTorrent(hHash); t != nil {
					t.IsPriority = true
					t.SetAggressiveMode(true, GetEffectiveConcurrencyLimit())
					logger.Printf("[V750] Priority RESTORED from DB: %s", filepath.Base(rec.Path))
				}
			}
			restored++
		}
	}
	logger.Printf("[V750] Restored %d/%d playback states from DB", restored, len(records))
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

	globalConfig = LoadConfig()
	prowlarrClient = prowlarr.NewClient(globalConfig.Prowlarr)
	SendHeartbeat(globalConfig)
	logger.Printf("[DEBUG] BlockListURL loaded: '%s'", globalConfig.BlockListURL)

	if dbPath != "" {
		// If dbPath is a directory, use it as RootPath; if a file, use its parent.
		if fi, err := os.Stat(dbPath); err == nil && fi.IsDir() {
			globalConfig.RootPath = dbPath
		} else {
			globalConfig.RootPath = filepath.Dir(dbPath)
		}
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

	go func() {
		logger.Println("Starting Embedded GoStorm Engine...")
		server.Start() // Starts Web Server on 8090 and Engine
	}()
	// Give engine a moment to init (hash maps etc)
	time.Sleep(2 * time.Second)

	InitDiskWarmup()
	go StartRegistryWatchdog(backgroundStopChan)
	go natpmpLoop(backgroundStopChan, globalConfig.NatPMP, logger)

	masterDataSemaphore = make(chan struct{}, globalConfig.MasterConcurrencyLimit)
	startHandleGC()

	// Initialize global helpers
	globalRateLimiter = NewRateLimiter(globalConfig.RateLimitRequestsPerSec, 1*time.Second)
	globalLockManager = NewLockManager(1 * time.Hour)

	poolSize := int(globalConfig.ReadAheadBase)
	if poolSize == 0 {
		poolSize = 16 * 1024 * 1024
	}
	readBufferPool = &sync.Pool{
		New: func() interface{} {
			buf := make([]byte, poolSize)
			return &buf
		},
	}
	logger.Printf("ReadBufferPool initialized with size: %d bytes (matches ReadAheadBase)", poolSize)

	httpClient = &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   globalConfig.HTTPConnectTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,

			MaxIdleConns:        globalConfig.MaxIdleConns,
			MaxIdleConnsPerHost: globalConfig.MaxIdleConnsPerHost,
			MaxConnsPerHost:     globalConfig.MaxConnsPerHost,

			ResponseHeaderTimeout: globalConfig.HTTPReadTimeout,
			IdleConnTimeout:       90 * time.Second, // Close idle connections after 90s
			TLSHandshakeTimeout:   10 * time.Second, // TLS handshake timeout (even for localhost)
			ExpectContinueTimeout: 1 * time.Second,  // Expect: 100-continue timeout

			// HTTP protocol settings - match Python defaults
			DisableKeepAlives:  false, // Enable HTTP keepalive (Python default)
			DisableCompression: false, // Enable gzip compression (Python default)
			ForceAttemptHTTP2:  false, // Use HTTP/1.1 only (Python urllib3 default)

			WriteBufferSize: globalConfig.WriteBufferSize,
			ReadBufferSize:  globalConfig.ReadBufferSize,
		},
	}
	logger.Printf("HTTP client initialized: ConnectTimeout=%v, ReadTimeout=%v, MaxIdleConns=%d, MaxIdleConnsPerHost=%d, MaxConnsPerHost=%d (V81-optimized)",
		globalConfig.HTTPConnectTimeout, globalConfig.HTTPReadTimeout, globalConfig.MaxIdleConns, globalConfig.MaxIdleConnsPerHost, globalConfig.MaxConnsPerHost)

	nativeBridge = NewNativeClient()

	if globalConfig.AIURL != "" {
		provider := ai.AIProvider{
			URL:     globalConfig.AIURL,
			APIKey:  globalConfig.AI_API_KEY,
			Model:   globalConfig.AIModel,
			IsLocal: globalConfig.AIProvider == "" || globalConfig.AIProvider == "local",
			GetBufferPct: func() int {
				total, _, _ := raCache.Stats()
				budget := globalConfig.ReadAheadBudget
				if budget <= 0 {
					return 100
				}
				pct := int(total * 100 / budget)
				if pct > 100 {
					pct = 100
				}
				return pct
			},
			GetSaturation: func() int {
				return len(masterDataSemaphore)
			},
		}
		go ai.StartAITuner(context.Background(), provider)
	}

	if globalConfig.BlockListURL != "" {
		safeGo(func() {
			updateBlockList(globalConfig.BlockListURL)
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

	safeGo(func() {
		updater.Start(AppVersion, backgroundStopChan)
	})

	peerPreloader = NewPeerPreloader(nativeBridge)

	// Metadata LRU cache: capacity from config, 24h TTL.
	metaCache = NewLRUCache(globalConfig.MetadataCacheSize, 24*time.Hour)

	// Deterministic inode map ensures Plex doesn't see "new files" after restarts.
	inodeMapPath := filepath.Join(GetStateDir(), "inode_map.json")
	if err := InitGlobalInodeMap(inodeMapPath, logger); err != nil {
		logger.Printf("WARNING: Failed to initialize inode map: %v (falling back to filename hash)", err)
	} else {
		files, dirs, _, _ := GetInodeMapStats()
		logger.Printf("InodeMap: Initialized with %d files, %d dirs from %s", files, dirs, inodeMapPath)
	}

	// V1.7.1: Optional SQLite State DB for unified persistence.
	if globalConfig.EnableStateDB {
		dbPath := globalConfig.StateDBPath
		if dbPath == "" {
			dbPath = filepath.Join(GetStateDir(), "gostream.db")
		}
		var err error
		stateDB, err = metadb.New(dbPath, logger)
		if err != nil {
			logger.Printf("WARNING: Failed to open StateDB: %v (falling back to JSON)", err)
			stateDB = nil
		} else {
			// Migrate JSON files if needed
			if err := stateDB.MigrateFromJSON(GetStateDir()); err != nil {
				logger.Printf("WARNING: StateDB migration failed: %v (falling back to JSON)", err)
				stateDB.Close()
				stateDB = nil
			} else {
				// Wire up DB to InodeMap, then reload from DB (covers boot 2+ where JSON is gone)
				globalInodeMap.SetDB(stateDB)
				if err := globalInodeMap.LoadFromDisk(); err != nil {
					logger.Printf("WARNING: Failed to reload InodeMap from DB: %v", err)
				}
				SetRegistryDB(stateDB)
				// V750: Restore playback states from previous sessions
				restorePlaybackStates(stateDB)
				logger.Printf("[StateDB] Active: %s", dbPath)
			}
		}
	}

	// Pre-populate cache at startup to improve Plex scan performance.
	cacheBuilder := NewStartupCacheBuilder(source, metaCache, logger)
	cacheBuilder.Start()

	globalCleanupManager = NewCleanupManager(logger, peerPreloader, metaCache, nativeBridge)
	globalCleanupManager.Start()

	globalTorrentRemover = NewTorrentRemover(nativeBridge, logger)
	globalSyncCacheManager = NewSyncCacheManager(GetStateDir(), logger)

	// V1.7.1: Wire up StateDB to sync cache manager (after it's created)
	if stateDB != nil {
		globalSyncCacheManager.SetDB(stateDB)
	}

	if err := globalSyncCacheManager.LoadCachesFromDisk(); err != nil {
		logger.Printf("WARNING: Failed to load sync caches from disk: %v", err)
	}

	// Sync caches to disk every 30s.
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

		natPort := atomic.LoadInt64(&currentNatPort)

		fmt.Fprintf(w, `{"version":"%s", "config_source":"%s", "uptime":"%s", "cache_entries":%d, "cache_size_mb":%.2f, "cleanup_hashes":%d, "cleanup_offsets":%d, "cleanup_activities":%d, "locks_total":%d, "master_concurrency_limit":%d, "negative_cache_entries":%d, "fullpack_cache_entries":%d, "streaming_threshold_kb":%d, "config_preload_workers":%d, "max_conns_per_host":%d, "read_ahead_total_bytes":%d, "read_ahead_active_bytes":%d, "read_ahead_stale_bytes":%d, "read_ahead_entries":%d, "read_ahead_budget":%d, "read_ahead_percent":%.2f, "read_ahead_active_percent":%.2f, "read_ahead_stale_percent":%.2f, "natpmp_port":%d, "latest_version":"%s", "update_available":%t}`,
			AppVersion,
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
			globalConfig.MaxConnsPerHost,
			raTotal, raActive, raStale, raEntries, raBudget,
			raPercent, raActivePercent, raStalePercent,
			natPort,
			updater.LatestVersion(), updater.UpdateAvailable())
	})

	http.HandleFunc("/webhook", handlePlexWebhook)

	http.HandleFunc("/metrics/profiling", func(w http.ResponseWriter, r *http.Request) {
		totalReads, cacheHits, httpFetches, streamingReads, avgHTTPLatency, avgCacheLatency, cacheHitRate := globalProfilingStats.Stats()

		fmt.Fprintf(w, `{"version":"%s", "total_reads":%d, "cache_hits":%d, "cache_hit_rate_pct":%.2f, "http_fetches":%d, "streaming_reads":%d, "non_streaming_reads":%d, "avg_http_latency_ms":%.2f, "avg_cache_latency_ms":%.2f, "max_conns_per_host":%d}`,
			AppVersion,
			totalReads,
			cacheHits,
			cacheHitRate,
			httpFetches,
			streamingReads,
			totalReads-streamingReads,
			float64(avgHTTPLatency.Microseconds())/1000.0,
			float64(avgCacheLatency.Microseconds())/1000.0,
			globalConfig.MaxConnsPerHost)
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
			prowlarrClient = prowlarr.NewClient(globalConfig.Prowlarr)
			if globalConfig.BlockListURL != "" && globalConfig.BlockListURL != oldURL {
				safeGo(func() {
					updateBlockList(globalConfig.BlockListURL)
				})
			}
			logger.Printf("[Config] Updated via Dashboard API")
			w.WriteHeader(200)
		}
	})

	http.HandleFunc("/api/prowlarr/search", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if prowlarrClient == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			w.Write([]byte(`[]`))
			return
		}
		imdbID := r.URL.Query().Get("imdb_id")
		contentType := r.URL.Query().Get("type")
		title := r.URL.Query().Get("title")
		if imdbID == "" {
			http.Error(w, "missing imdb_id parameter", http.StatusBadRequest)
			return
		}
		if contentType == "" {
			contentType = "movie"
		}
		streams := prowlarrClient.FetchTorrents(imdbID, contentType, title)
		if streams == nil {
			streams = []prowlarr.Stream{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(streams)
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

	// Sync Scheduler (Fase 1)
	if globalConfig.Scheduler.Enabled {
		schedCfg := scheduler.SchedulerConfig{
			Enabled:       globalConfig.Scheduler.Enabled,
			MoviesSync:    scheduler.DailyJobConfig(globalConfig.Scheduler.MoviesSync),
			TVSync:        scheduler.DailyJobConfig(globalConfig.Scheduler.TVSync),
			WatchlistSync: scheduler.WatchlistSyncConfig(globalConfig.Scheduler.WatchlistSync),
		}

		statePath := filepath.Join(GetStateDir(), "scheduler_state.json")

		logsDir := filepath.Join(filepath.Dir(globalConfig.ConfigPath), "logs")

		// Start midnight log truncation
		engines.StartLogTruncator(logsDir, backgroundStopChan)

		syncers := map[string]scheduler.Syncer{
			"movies": engines.NewMoviesSyncer(engines.MoviesSyncerConfig{
				GoStormURL:   globalConfig.GoStormBaseURL,
				TMDBAPIKey:   globalConfig.TMDBAPIKey,
				TorrentioURL: globalConfig.TorrentioURL,
				PlexURL:      globalConfig.Plex.URL,
				PlexToken:    globalConfig.Plex.Token,
				PlexLib:      globalConfig.Plex.LibraryID,
				MoviesDir:    filepath.Join(globalConfig.PhysicalSourcePath, "movies"),
				StateDir:     GetStateDir(),
				LogsDir:      logsDir,
				ProwlarrCfg:  globalConfig.Prowlarr,
			}),
			"tv": engines.NewTVSyncer(engines.TVSyncerConfig{
				GoStormURL:   globalConfig.GoStormBaseURL,
				TMDBAPIKey:   globalConfig.TMDBAPIKey,
				TorrentioURL: globalConfig.TorrentioURL,
				PlexURL:      globalConfig.Plex.URL,
				PlexToken:    globalConfig.Plex.Token,
				PlexTVLib:    globalConfig.Plex.TVLibraryID,
				TVDir:        filepath.Join(globalConfig.PhysicalSourcePath, "tv"),
				StateDir:     GetStateDir(),
				LogsDir:      logsDir,
				ProwlarrCfg:  globalConfig.Prowlarr,
				DB:           stateDB,
			}),
			"watchlist": engines.NewWatchlistSyncer(engines.WatchlistSyncerConfig{
				GoStormURL:      globalConfig.GoStormBaseURL,
				TMDBAPIKey:      globalConfig.TMDBAPIKey,
				TorrentioURL:    globalConfig.TorrentioURL,
				PlexURL:         globalConfig.Plex.URL,
				PlexToken:       globalConfig.Plex.Token,
				PlexSection:     globalConfig.Plex.LibraryID,
				MoviesDir:       filepath.Join(globalConfig.PhysicalSourcePath, "movies"),
				MediaServerType: globalConfig.MediaServerType,
				LogsDir:         logsDir,
				ProwlarrCfg:     globalConfig.Prowlarr,
			}),
		}

		sched := scheduler.New(schedCfg, syncers, statePath)

		http.HandleFunc("/api/scheduler/status", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(sched.Status())
		})
		http.HandleFunc("/api/scheduler/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", 405)
				return
			}
			path := strings.TrimPrefix(r.URL.Path, "/api/scheduler/")
			if strings.HasSuffix(path, "/stop") {
				name := strings.TrimSuffix(path, "/stop")
				if err := sched.StopJob(name); err != nil {
					http.Error(w, err.Error(), 409)
					return
				}
				w.WriteHeader(http.StatusAccepted)
				return
			}
			name := strings.TrimSuffix(path, "/run")
			if err := sched.TriggerRun(name); err != nil {
				http.Error(w, err.Error(), 409)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		})

		safeGo(func() {
			sched.Run(backgroundStopChan)
		})
		logger.Printf("[Scheduler] enabled (Go native)")
	}

	// Health Monitor + Dashboard (Fase 5)
	monCollector := collector.New(
		"http://127.0.0.1:8090",
		globalConfig.FuseMountPath,
		physicalSourcePath,
		globalConfig.NatPMP.VPNInterface,
		globalConfig.Plex.URL,
		globalConfig.Plex.Token,
		globalConfig.NatPMP.LocalPort,
		globalConfig.MetricsPort,
	)
	logsDir := filepath.Join(filepath.Dir(globalConfig.ConfigPath), "logs")
	dashHandler := dashboard.New(monCollector, logsDir)
	http.HandleFunc("/dashboard", dashHandler.Dashboard)
	http.HandleFunc("/api/health", dashHandler.Health)
	http.HandleFunc("/api/torrents", dashHandler.Torrents)
	http.HandleFunc("/api/speed-history", dashHandler.SpeedHistory)
	http.HandleFunc("/api/logs", dashHandler.Logs)
	http.HandleFunc("/api/plex-thumb", dashHandler.PlexThumb)
	http.HandleFunc("/api/kill-stream/", dashHandler.KillStream)
	safeGo(func() {
		monCollector.Run(backgroundStopChan)
	})
	logger.Printf("[Dashboard] enabled at :%d/dashboard", globalConfig.MetricsPort)

	go http.ListenAndServe(fmt.Sprintf(":%d", globalConfig.MetricsPort), nil)

	// Graceful shutdown: saves inode map and sync caches before exit.
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
		backgroundStopOnce.Do(func() { close(backgroundStopChan) })

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

	go smbdWatchdog()

	server.Wait()
}

// smbdWatchdog detects smbd processes stuck in D-state (uninterruptible FUSE I/O).
// Level 1 (3 hits / 180s): interrupt all pumps. Level 2 (10 hits / 600s): graceful restart.
func smbdWatchdog() {
	const checkInterval = 60 * time.Second
	const unblockThreshold = 3  // 180s - Emergency unblock (interrupt all pumps)
	const restartThreshold = 10 // 600s - Full restart (persistent stall)
	consecutiveHits := 0

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for range ticker.C {
		if countDStateSmbd() > 0 {
			consecutiveHits++
			logger.Printf("[Watchdog] D-state smbd detected (%d/%d)", consecutiveHits, restartThreshold)

			// Level 1 (3 consecutive hits): soft-interrupt all pumps to unblock hung FUSE reads.
			// Only closes pipe readers — pumps stay alive and will retry on next FUSE read.
			// Preserves pump survival during temporary peer shortages (swarm may recover).
			if consecutiveHits == unblockThreshold {
				logger.Printf("[Watchdog] D-state persistent for %ds — triggering soft interrupt (pumps stay alive)",
					consecutiveHits*int(checkInterval/time.Second))
				activePumps.Range(func(key, value interface{}) bool {
					if ps, ok := value.(*NativePumpState); ok {
						if ps.reader != nil {
							logger.Printf("[Watchdog] Soft-interrupting pump: %s", filepath.Base(ps.path))
							ps.reader.Interrupt()
						}
					}
					return true
				})
			}

			// Level 2 (10 consecutive hits): graceful restart.
			if consecutiveHits >= restartThreshold {
				logger.Printf("[Watchdog] D-state STILL persistent for %ds — triggering graceful restart",
					consecutiveHits*int(checkInterval/time.Second))
				syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
				return
			}
		} else {
			if consecutiveHits > 0 {
				logger.Printf("[Watchdog] D-state cleared after %d hit(s) (Max: %d)", consecutiveHits, restartThreshold)
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
