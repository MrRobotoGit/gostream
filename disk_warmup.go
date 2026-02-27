package main

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MrRobotoGit/gostream/internal/gostorm/settings"
)

// warmupFileSize is the per-file head cache cap. Set at init from config, default 64 MB.
var warmupFileSize int64 = 64 * 1024 * 1024

const (
	tailWarmupSize int64 = 16 * 1024 * 1024        // V265: 16 MB tail (Cues/seek index)
	warmupQuota    int64 = 32 * 1024 * 1024 * 1024 // fallback default 32 GB (overridden by config)
	warmupSuffix         = ".warmup"
	tailSuffix           = ".warmup-tail"   // V265: separate file for tail
	warmupWriteBuf       = 16 * 1024 * 1024 // 16 MB — matches pump chunk size
	handleIdleMax        = 30 * time.Second // close idle file handles after 30s
)

// diskWarmup is the global instance, nil when disabled.
var diskWarmup *DiskWarmupCache

// V261: sync.Pool for write buffers — avoids 16MB heap allocs per chunk.
var warmupWritePool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, warmupWriteBuf)
		return &buf
	},
}

type warmupWrite struct {
	hash   string
	fileID int
	buf    *[]byte // pooled buffer pointer — returned to warmupWritePool after write
	len    int     // actual data length within buf
	off    int64
}

// cachedHandle holds an open file descriptor with last-access tracking.
type cachedHandle struct {
	f        *os.File
	lastUsed time.Time
}

// V264: sizeEntry stores the cached size of a warmup file with TTL.
type sizeEntry struct {
	size      int64
	updatedAt time.Time
}

// V265: tailRange tracks the actually-written byte range in a tail warmup file.
// Prevents serving zeros from sparse file holes.
type tailRange struct {
	minOff int64 // lowest relative offset written
	maxOff int64 // highest relative offset + bytes written
}

// DiskWarmupCache persists the first 128MB of each streamed file to SSD.
type DiskWarmupCache struct {
	dir          string
	mu           sync.Mutex // protects quota enforcement
	totalSize    int64      // V288: Tracked total size of all warmup files in bytes
	missing      sync.Map   // path -> time.Time (negative cache for missing files)
	handles      sync.Map   // path -> *cachedHandle (cached file descriptors)
	sizeCache    sync.Map   // V264: path -> sizeEntry (cached file sizes with TTL)
	tailCoverage sync.Map   // V265: path -> *tailRange (written range tracking)
	writeCh      chan warmupWrite
}

// InitDiskWarmup creates the global warmup cache if UseDisk is enabled.
func InitDiskWarmup() {
	for i := 0; i < 15; i++ {
		if settings.BTsets != nil {
			break
		}
		time.Sleep(1 * time.Second)
	}

	if settings.BTsets == nil || !settings.BTsets.UseDisk {
		return
	}

	// Override warmupFileSize from config if set
	if globalConfig.WarmupHeadSizeMB > 0 {
		warmupFileSize = globalConfig.WarmupHeadSizeMB * 1024 * 1024
	}

	dir := settings.BTsets.TorrentsSavePath
	if dir == "" || dir == "/" {
		return
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}

	diskWarmup = &DiskWarmupCache{
		dir:     dir,
		writeCh: make(chan warmupWrite, 32),
	}

	// V288: Initial directory scan at startup to populate totalSize.
	// This only happens ONCE, moving the I/O burden away from critical streaming paths.
	if entries, err := os.ReadDir(dir); err == nil {
		var initialTotal int64
		for _, e := range entries {
			name := e.Name()
			if strings.HasSuffix(name, warmupSuffix) || strings.HasSuffix(name, tailSuffix) {
				if info, err := e.Info(); err == nil {
					initialTotal += info.Size()
				}
			}
		}
		atomic.StoreInt64(&diskWarmup.totalSize, initialTotal)
		logger.Printf("[DiskWarmup] Initial size: %.1fGB", float64(initialTotal)/(1<<30))
	}

	go diskWarmup.writeWorker()
	go diskWarmup.handleReaper()

	logger.Printf("[DiskWarmup] Active — dir=%s quota=%dGB warmup=%dMB", dir, globalConfig.DiskWarmupQuotaGB, warmupFileSize/1024/1024)
}

// handleReaper closes file handles that have been idle for > handleIdleMax.
func (d *DiskWarmupCache) handleReaper() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		d.handles.Range(func(key, val interface{}) bool {
			ch := val.(*cachedHandle)
			if now.Sub(ch.lastUsed) > handleIdleMax {
				d.handles.Delete(key)
				ch.f.Close()
			}
			return true
		})
	}
}

// getHandle returns a cached or freshly opened file handle for path.
// Always opens O_RDWR so both reads and writes work on the same handle.
func (d *DiskWarmupCache) getHandle(path string) (*os.File, error) {
	if val, ok := d.handles.Load(path); ok {
		ch := val.(*cachedHandle)
		ch.lastUsed = time.Now()
		return ch.f, nil
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	ch := &cachedHandle{f: f, lastUsed: time.Now()}
	if actual, loaded := d.handles.LoadOrStore(path, ch); loaded {
		// Another goroutine opened it first — close ours, use theirs
		f.Close()
		existing := actual.(*cachedHandle)
		existing.lastUsed = time.Now()
		return existing.f, nil
	}
	return f, nil
}

// closeHandle removes and closes a cached handle (e.g. after file removal).
func (d *DiskWarmupCache) closeHandle(path string) {
	if val, ok := d.handles.LoadAndDelete(path); ok {
		val.(*cachedHandle).f.Close()
	}
}

func (d *DiskWarmupCache) writeWorker() {
	for w := range d.writeCh {
		d.processWrite(w.hash, w.fileID, (*w.buf)[:w.len], w.off)
		warmupWritePool.Put(w.buf)
	}
}

func (d *DiskWarmupCache) WriteChunk(hash string, fileID int, data []byte, off int64) {
	if off >= warmupFileSize || d.writeCh == nil {
		return
	}

	// V261: Use pooled buffer instead of make() — avoids 16MB heap allocs
	bufPtr := warmupWritePool.Get().(*[]byte)
	if len(*bufPtr) < len(data) {
		// Data larger than pool buffer (shouldn't happen), fall back
		warmupWritePool.Put(bufPtr)
		buf := make([]byte, len(data))
		copy(buf, data)
		bufPtr = &buf
	} else {
		copy(*bufPtr, data)
	}

	select {
	case d.writeCh <- warmupWrite{hash, fileID, bufPtr, len(data), off}:
	default:
		warmupWritePool.Put(bufPtr)
	}
}

func (d *DiskWarmupCache) processWrite(hash string, fileID int, data []byte, off int64) {
	// V265: Cap write at warmupFileSize to prevent oversized head files
	if off >= warmupFileSize {
		return
	}
	if off+int64(len(data)) > warmupFileSize {
		data = data[:warmupFileSize-off]
	}

	path := d.filePath(hash, fileID)

	// V271: Skip write if warmup file already complete (avoid redundant SSD writes).
	// Previously, every pump chunk was written even if the file was already 128MB.
	if val, ok := d.sizeCache.Load(path); ok {
		if entry := val.(sizeEntry); entry.size >= warmupFileSize {
			return
		}
	} else if fi, err := os.Stat(path); err == nil && fi.Size() >= warmupFileSize {
		d.sizeCache.Store(path, sizeEntry{size: fi.Size(), updatedAt: time.Now()})
		return
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		os.MkdirAll(d.dir, 0755)

		d.mu.Lock()
		d.enforceQuotaLocked(warmupFileSize)
		d.mu.Unlock()
		d.missing.Delete(path)

		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			logger.Printf("[DiskWarmup] Error creating file: %v", err)
			return
		}
		// Store in handle cache immediately
		d.handles.Store(path, &cachedHandle{f: f, lastUsed: time.Now()})
		logger.Printf("[DiskWarmup] STARTING %s at offset %d", filepath.Base(path), off)
	}

	f, err := d.getHandle(path)
	if err != nil {
		return
	}

	// V288: Track size delta for atomic totalSize update
	var prevSize int64
	if val, ok := d.sizeCache.Load(path); ok {
		prevSize = val.(sizeEntry).size
	} else if fi, err := f.Stat(); err == nil {
		prevSize = fi.Size()
	}

	n, _ := f.WriteAt(data, off)

	// V288: Update real-time totalSize by adding the delta (new - old)
	currentSize := off + int64(n)
	if currentSize > prevSize {
		atomic.AddInt64(&d.totalSize, currentSize-prevSize)
	}

	// V264: Update Size Cache immediately after write to keep it in sync
	d.sizeCache.Store(path, sizeEntry{size: currentSize, updatedAt: time.Now()})

	if off+int64(n) >= warmupFileSize {
		logger.Printf("[DiskWarmup] COMPLETED %s", filepath.Base(path))
	}
}

func (d *DiskWarmupCache) filePath(hash string, fileID int) string {
	return filepath.Join(d.dir, hash+"-"+strconv.Itoa(fileID)+warmupSuffix)
}

// V265: tailPath returns the path to the tail warmup file.
func (d *DiskWarmupCache) tailPath(hash string, fileID int) string {
	return filepath.Join(d.dir, hash+"-"+strconv.Itoa(fileID)+tailSuffix)
}

// GetAvailableRange returns the size of the warmup file on disk.
func (d *DiskWarmupCache) GetAvailableRange(hash string, fileID int) int64 {
	path := d.filePath(hash, fileID)
	if _, ok := d.missing.Load(path); ok {
		return 0
	}

	// V264: Size Cache check (TTL 10s) — avoids os.Stat overhead in hot path
	if val, ok := d.sizeCache.Load(path); ok {
		entry := val.(sizeEntry)
		if time.Since(entry.updatedAt) < 10*time.Second {
			return entry.size
		}
	}

	fi, err := os.Stat(path)
	if err != nil {
		d.missing.Store(path, time.Now())
		return 0
	}

	// V262: Self-Healing — if file is larger than 128MB + 16MB margin, it's corrupt.
	// This prevents "PUMP SKIP" from jumping too far based on a bloated/corrupt cache file.
	if fi.Size() > warmupFileSize+(16*1024*1024) {
		logger.Printf("[DiskWarmup] CORRUPT CACHE detected (Size: %.1fMB > 128MB) for %s. Removing.", float64(fi.Size())/(1<<20), hash[:8])
		d.closeHandle(path)
		d.sizeCache.Delete(path)
		os.Remove(path)
		d.missing.Store(path, time.Now())
		return 0
	}

	// V264: Update Size Cache after successful Stat
	d.sizeCache.Store(path, sizeEntry{size: fi.Size(), updatedAt: time.Now()})
	return fi.Size()
}

func (d *DiskWarmupCache) ReadAt(hash string, fileID int, buf []byte, off int64) (int, error) {
	if off >= warmupFileSize {
		return 0, nil
	}
	path := d.filePath(hash, fileID)

	// V261: Use cached file handle instead of open/close per read
	f, err := d.getHandle(path)
	if err != nil {
		return 0, nil
	}

	// V272: Use sizeCache instead of os.Stat to reduce syscall overhead
	availSize := d.GetAvailableRange(hash, fileID)
	if off >= availSize {
		return 0, nil
	}

	maxRead := warmupFileSize - off
	if int64(len(buf)) > maxRead {
		buf = buf[:maxRead]
	}
	if avail := availSize - off; int64(len(buf)) > avail {
		buf = buf[:avail]
	}

	n, err := f.ReadAt(buf, off)
	// V271: No Touch on read — mtime reflects creation/write time only.
	// LRU eviction uses write-time ordering (oldest warmup files evicted first).
	// Previously, Plex scans touched ALL files, defeating LRU.
	return n, err
}

// Touch removed in V271: mtime reflects write-time only, reads don't update it.
// Plex scans were rejuvenating all warmup files, defeating LRU eviction.

// V265: WriteTail writes data to the tail warmup file.
// absoluteOffset is the offset within the original file (e.g., fileSize-64KB).
// fileSize is needed to compute the relative offset within the tail file.
func (d *DiskWarmupCache) WriteTail(hash string, fileID int, data []byte, absoluteOffset, fileSize int64) {
	tailStart := fileSize - tailWarmupSize
	if tailStart < 0 {
		tailStart = 0
	}
	if absoluteOffset < tailStart {
		return // Data is before the tail zone
	}

	relOffset := absoluteOffset - tailStart
	if relOffset+int64(len(data)) > tailWarmupSize {
		// Trim data to fit within tail bounds
		data = data[:tailWarmupSize-relOffset]
	}
	if len(data) == 0 {
		return
	}

	path := d.tailPath(hash, fileID)

	// V271: Skip write if tail warmup already complete.
	// Check in-memory cache first, then fall back to os.Stat (covers gostream restart).
	if val, ok := d.tailCoverage.Load(path); ok {
		tr := val.(*tailRange)
		if tr.minOff == 0 && tr.maxOff >= tailWarmupSize {
			return
		}
	} else if fi, err := os.Stat(path); err == nil && fi.Size() >= tailWarmupSize {
		// File exists and is complete on disk — populate coverage and skip
		d.tailCoverage.Store(path, &tailRange{minOff: 0, maxOff: fi.Size()})
		return
	}

	// Create file if it doesn't exist
	if _, err := os.Stat(path); os.IsNotExist(err) {
		os.MkdirAll(d.dir, 0755)
		d.missing.Delete(path)

		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			return
		}
		d.handles.Store(path, &cachedHandle{f: f, lastUsed: time.Now()})
		logger.Printf("[DiskWarmup] TAIL STARTING %s at relOffset %d", filepath.Base(path), relOffset)
	}

	f, err := d.getHandle(path)
	if err != nil {
		return
	}

	n, _ := f.WriteAt(data, relOffset)
	d.sizeCache.Store(path, sizeEntry{size: relOffset + int64(n), updatedAt: time.Now()})

	// V265: Update tail coverage tracking
	endOff := relOffset + int64(n)
	if val, ok := d.tailCoverage.Load(path); ok {
		tr := val.(*tailRange)
		if relOffset < tr.minOff {
			tr.minOff = relOffset
		}
		if endOff > tr.maxOff {
			tr.maxOff = endOff
		}
	} else {
		d.tailCoverage.Store(path, &tailRange{minOff: relOffset, maxOff: endOff})
	}
}

// V265: ReadTail reads data from the tail warmup file.
// Only serves data within the tracked coverage range to prevent sparse hole corruption.
func (d *DiskWarmupCache) ReadTail(hash string, fileID int, buf []byte, absoluteOffset, fileSize int64) (int, error) {
	tailStart := fileSize - tailWarmupSize
	if tailStart < 0 {
		tailStart = 0
	}
	if absoluteOffset < tailStart {
		return 0, nil
	}

	relOffset := absoluteOffset - tailStart
	path := d.tailPath(hash, fileID)

	// V265: Check coverage — only serve if the requested range is within written data
	readEnd := relOffset + int64(len(buf))
	if val, ok := d.tailCoverage.Load(path); ok {
		tr := val.(*tailRange)
		if relOffset < tr.minOff || readEnd > tr.maxOff {
			return 0, nil // Requested range extends beyond written coverage
		}
	} else {
		// V271: No coverage info (after restart) — recover from disk if file is complete.
		// Previously returned 0 ("conservative: don't serve"), wasting the tail warmup.
		fi, err := os.Stat(path)
		if err != nil || fi.Size() < tailWarmupSize {
			return 0, nil
		}
		// File is complete on disk — populate coverage and proceed to serve
		d.tailCoverage.Store(path, &tailRange{minOff: 0, maxOff: fi.Size()})
	}

	f, err := d.getHandle(path)
	if err != nil {
		return 0, nil
	}

	n, err := f.ReadAt(buf, relOffset)
	// V271: Touch from head ReadAt (off > 1MB) covers playback case.
	// Tail reads alone don't indicate real playback.
	return n, err
}

// V265: GetTailRange returns the size of the tail warmup file.
func (d *DiskWarmupCache) GetTailRange(hash string, fileID int) int64 {
	path := d.tailPath(hash, fileID)
	if _, ok := d.missing.Load(path); ok {
		return 0
	}

	// V264: Size Cache check
	if val, ok := d.sizeCache.Load(path); ok {
		entry := val.(sizeEntry)
		if time.Since(entry.updatedAt) < 10*time.Second {
			return entry.size
		}
	}

	fi, err := os.Stat(path)
	if err != nil {
		d.missing.Store(path, time.Now())
		return 0
	}

	d.sizeCache.Store(path, sizeEntry{size: fi.Size(), updatedAt: time.Now()})
	return fi.Size()
}

func (d *DiskWarmupCache) RemoveHash(hash string) {
	entries, _ := os.ReadDir(d.dir)
	prefix := hash + "-"
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, prefix) && (strings.HasSuffix(name, warmupSuffix) || strings.HasSuffix(name, tailSuffix)) {
			fullPath := filepath.Join(d.dir, name)
			
			// V288: Track size for atomic totalSize reduction
			if fi, err := e.Info(); err == nil {
				atomic.AddInt64(&d.totalSize, -fi.Size())
			}
			
			d.closeHandle(fullPath)
			d.sizeCache.Delete(fullPath)    // V264: Invalidate size cache
			d.tailCoverage.Delete(fullPath) // V265: Invalidate coverage
			os.Remove(fullPath)
		}
	}
}

func (d *DiskWarmupCache) enforceQuotaLocked(needed int64) {
	// V288-Optimization: Check memory tracker first.
	// 99.9% of the time, this is a zero-I/O return, saving the PCIe bus on the Pi 4.
	quota := warmupQuota
	if globalConfig.DiskWarmupQuotaGB > 0 {
		quota = globalConfig.DiskWarmupQuotaGB * 1024 * 1024 * 1024
	}

	totalSize := atomic.LoadInt64(&d.totalSize)
	if totalSize+needed <= quota {
		return
	}

	// FALLBACK: If over quota, perform actual directory scan for LRU eviction.
	// This only happens when we've filled the SSD, so it's a rare event.
	entries, _ := os.ReadDir(d.dir)
	type wFile struct {
		path    string
		size    int64
		modTime int64
	}
	var files []wFile
	// We re-scan disk totalSize to ensure we stay perfectly in sync during eviction
	var diskTotal int64
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, warmupSuffix) && !strings.HasSuffix(name, tailSuffix) {
			continue
		}
		if info, err := e.Info(); err == nil {
			files = append(files, wFile{filepath.Join(d.dir, name), info.Size(), info.ModTime().Unix()})
			diskTotal += info.Size()
		}
	}

	// Sync memory tracker after re-scan
	atomic.StoreInt64(&d.totalSize, diskTotal)
	if diskTotal+needed <= quota {
		return
	}

	sort.Slice(files, func(i, j int) bool { return files[i].modTime < files[j].modTime })
	for _, fi := range files {
		if diskTotal+needed <= quota {
			break
		}
		d.closeHandle(fi.path)
		d.sizeCache.Delete(fi.path)    // V265: Clear memory cache on eviction
		d.tailCoverage.Delete(fi.path) // V265: Clear tail tracking on eviction
		os.Remove(fi.path)
		diskTotal -= fi.size
	}
	// Final sync of memory tracker
	atomic.StoreInt64(&d.totalSize, diskTotal)
}
