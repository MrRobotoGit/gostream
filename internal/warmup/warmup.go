package warmup

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gostream/internal/gostorm/settings"
)

// FileSize is the per-file head cache cap. Set at init from config, default 64 MB.
var FileSize int64 = 64 * 1024 * 1024

const (
	TailWarmupSize int64 = 16 * 1024 * 1024        // V265: 16 MB tail (Cues/seek index)
	warmupQuota    int64 = 32 * 1024 * 1024 * 1024 // fallback default 32 GB (overridden by config)
	warmupSuffix         = ".warmup"
	tailSuffix           = ".warmup-tail"   // V265: separate file for tail
	warmupWriteBuf       = 16 * 1024 * 1024 // 16 MB — matches pump chunk size
	handleIdleMax        = 30 * time.Second // close idle file handles after 30s
)

var diskQuotaGB int64

// SetQuotaGB sets the disk warmup quota in gigabytes.
func SetQuotaGB(gb int64) { diskQuotaGB = gb }

// DiskWarmup is the global instance, nil when disabled.
var DiskWarmup *DiskWarmupCache

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
	f            *os.File
	lastUsedNano atomic.Int64 // atomic to prevent race with handleReaper
	closed       atomic.Bool  // set by closeHandle before f.Close() so writeWorker can detect stale handles
}

// V264: sizeEntry stores the cached size of a warmup file with TTL.
type sizeEntry struct {
	size      int64
	updatedAt time.Time
}

// V265: tailRange tracks contiguous bytes written from relOffset=0 in a tail warmup file.
type tailRange struct {
	mu            sync.Mutex // protects highWatermark against concurrent WriteTail/ReadTail
	highWatermark int64      // contiguous bytes written from relOffset=0
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
var logf = log.New(os.Stdout, "[DiskWarmup] ", log.LstdFlags)

func InitDiskWarmup(quotaGB int64) {
	diskQuotaGB = quotaGB
	FileSize = 64 * 1024 * 1024 // default, overridden by config
	for i := 0; i < 15; i++ {
		if settings.BTsets != nil {
			break
		}
		time.Sleep(1 * time.Second)
	}

	if settings.BTsets == nil || !settings.BTsets.UseDisk {
		return
	}

	dir := settings.BTsets.TorrentsSavePath
	if dir == "" || dir == "/" {
		return
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}

	DiskWarmup = &DiskWarmupCache{
		dir:     dir,
		writeCh: make(chan warmupWrite, 32),
	}

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
		atomic.StoreInt64(&DiskWarmup.totalSize, initialTotal)
		logf.Printf("[DiskWarmup] Initial size: %.1fGB", float64(initialTotal)/(1<<30))
	}

	go DiskWarmup.writeWorker()
	go DiskWarmup.handleReaper()

	logf.Printf("[DiskWarmup] Active — dir=%s quota=%dGB warmup=%dMB", dir, quotaGB, FileSize/1024/1024)
}

func (d *DiskWarmupCache) handleReaper() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		d.handles.Range(func(key, val interface{}) bool {
			ch := val.(*cachedHandle)
			if now.UnixNano()-ch.lastUsedNano.Load() > handleIdleMax.Nanoseconds() {
				d.handles.Delete(key)
				ch.f.Close()
			}
			return true
		})
	}
}

func (d *DiskWarmupCache) getHandle(path string) (*cachedHandle, error) {
	if val, ok := d.handles.Load(path); ok {
		ch := val.(*cachedHandle)
		ch.lastUsedNano.Store(time.Now().UnixNano())
		return ch, nil
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	ch := &cachedHandle{f: f}
	ch.lastUsedNano.Store(time.Now().UnixNano())
	if actual, loaded := d.handles.LoadOrStore(path, ch); loaded {
		f.Close()
		existing := actual.(*cachedHandle)
		existing.lastUsedNano.Store(time.Now().UnixNano())
		return existing, nil
	}
	return ch, nil
}

func (d *DiskWarmupCache) closeHandle(path string) {
	if val, ok := d.handles.LoadAndDelete(path); ok {
		ch := val.(*cachedHandle)
		ch.closed.Store(true) // mark before Close so writeWorker can detect stale handle
		ch.f.Close()
	}
}

func (d *DiskWarmupCache) writeWorker() {
	for w := range d.writeCh {
		d.processWrite(w.hash, w.fileID, (*w.buf)[:w.len], w.off)
		warmupWritePool.Put(w.buf)
	}
}

func (d *DiskWarmupCache) WriteChunk(hash string, fileID int, data []byte, off int64) {
	if off > FileSize || d.writeCh == nil {
		return
	}

	bufPtr := warmupWritePool.Get().(*[]byte)
	if len(*bufPtr) < len(data) {
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
	if off > FileSize {
		return
	}
	// Only truncate chunks that straddle the boundary from below.
	// Chunks starting AT FileSize are written in full (the boundary chunk).
	if off < FileSize && off+int64(len(data)) > FileSize {
		data = data[:FileSize-off]
	}

	path := d.filePath(hash, fileID)

	if val, ok := d.sizeCache.Load(path); ok {
		if entry := val.(sizeEntry); entry.size > FileSize {
			return
		}
	} else if fi, err := os.Stat(path); err == nil && fi.Size() > FileSize {
		d.sizeCache.Store(path, sizeEntry{size: fi.Size(), updatedAt: time.Now()})
		return
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		os.MkdirAll(d.dir, 0755)

		d.mu.Lock()
		d.enforceQuotaLocked(FileSize)
		d.mu.Unlock()
		d.missing.Delete(path)

		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			logf.Printf("[DiskWarmup] Error creating file: %v", err)
			return
		}
		newCh := &cachedHandle{f: f}
		newCh.lastUsedNano.Store(time.Now().UnixNano())
		d.handles.Store(path, newCh)
		logf.Printf("[DiskWarmup] STARTING %s at offset %d", filepath.Base(path), off)
	}

	ch, err := d.getHandle(path)
	if err != nil {
		return
	}
	if ch.closed.Load() {
		logf.Printf("[DiskWarmup] Write skipped: handle closed for %s", filepath.Base(path))
		return
	}

	var prevSize int64
	if val, ok := d.sizeCache.Load(path); ok {
		prevSize = val.(sizeEntry).size
	} else if fi, err := ch.f.Stat(); err == nil {
		prevSize = fi.Size()
	}

	n, err := ch.f.WriteAt(data, off)
	if err != nil {
		logf.Printf("[DiskWarmup] WriteAt error for %s: %v", filepath.Base(path), err)
		return
	}

	currentSize := off + int64(n)
	if currentSize > prevSize {
		atomic.AddInt64(&d.totalSize, currentSize-prevSize)
	}

	d.sizeCache.Store(path, sizeEntry{size: currentSize, updatedAt: time.Now()})

	if off+int64(n) >= FileSize {
		logf.Printf("[DiskWarmup] COMPLETED %s", filepath.Base(path))
	}
}

func (d *DiskWarmupCache) filePath(hash string, fileID int) string {
	return filepath.Join(d.dir, hash+"-"+strconv.Itoa(fileID)+warmupSuffix)
}

func (d *DiskWarmupCache) tailPath(hash string, fileID int) string {
	return filepath.Join(d.dir, hash+"-"+strconv.Itoa(fileID)+tailSuffix)
}

func (d *DiskWarmupCache) GetAvailableRange(hash string, fileID int) int64 {
	path := d.filePath(hash, fileID)
	if _, ok := d.missing.Load(path); ok {
		return 0
	}

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

	if fi.Size() > FileSize+(16*1024*1024) {
		logf.Printf("[DiskWarmup] CORRUPT CACHE detected (Size: %.1fMB > 128MB) for %s. Removing.", float64(fi.Size())/(1<<20), hash[:8])
		d.closeHandle(path)
		d.sizeCache.Delete(path)
		os.Remove(path)
		d.missing.Store(path, time.Now())
		return 0
	}

	d.sizeCache.Store(path, sizeEntry{size: fi.Size(), updatedAt: time.Now()})
	return fi.Size()
}

func (d *DiskWarmupCache) ReadAt(hash string, fileID int, buf []byte, off int64) (int, error) {
	if off > FileSize {
		return 0, nil
	}
	path := d.filePath(hash, fileID)

	ch, err := d.getHandle(path)
	if err != nil {
		return 0, nil
	}

	availSize := d.GetAvailableRange(hash, fileID)
	if off >= availSize {
		return 0, nil
	}

	if avail := availSize - off; int64(len(buf)) > avail {
		buf = buf[:avail]
	}

	n, err := ch.f.ReadAt(buf, off)
	return n, err
}

func (d *DiskWarmupCache) WriteTail(hash string, fileID int, data []byte, absoluteOffset, fileSize int64) {
	path := d.tailPath(hash, fileID)

	tailStart := fileSize - TailWarmupSize
	if tailStart < 0 {
		tailStart = 0
	}
	if absoluteOffset < tailStart {
		return
	}

	relOffset := absoluteOffset - tailStart
	if relOffset+int64(len(data)) > TailWarmupSize {
		data = data[:TailWarmupSize-relOffset]
	}
	if len(data) == 0 {
		return
	}

	if val, ok := d.tailCoverage.Load(path); ok {
		tr := val.(*tailRange)
		tr.mu.Lock()
		done := tr.highWatermark >= TailWarmupSize
		tr.mu.Unlock()
		if done {
			return
		}
	} else if fi, err := os.Stat(path); err == nil && fi.Size() >= TailWarmupSize {
		d.tailCoverage.Store(path, &tailRange{highWatermark: fi.Size()})
		return
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		os.MkdirAll(d.dir, 0755)
		d.missing.Delete(path)

		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			return
		}
		tailCh := &cachedHandle{f: f}
		tailCh.lastUsedNano.Store(time.Now().UnixNano())
		d.handles.Store(path, tailCh)
		logf.Printf("[DiskWarmup] TAIL STARTING %s at relOffset %d", filepath.Base(path), relOffset)
	}

	ch, err := d.getHandle(path)
	if err != nil {
		return
	}
	if ch.closed.Load() {
		logf.Printf("[DiskWarmup] Write skipped (tail): handle closed for %s", filepath.Base(path))
		return
	}

	n, _ := ch.f.WriteAt(data, relOffset)
	d.sizeCache.Store(path, sizeEntry{size: relOffset + int64(n), updatedAt: time.Now()})

	endOff := relOffset + int64(n)
	if val, ok := d.tailCoverage.Load(path); ok {
		tr := val.(*tailRange)
		tr.mu.Lock()
		if endOff > tr.highWatermark {
			tr.highWatermark = endOff
		}
		tr.mu.Unlock()
	} else {
		d.tailCoverage.Store(path, &tailRange{highWatermark: endOff})
	}
}

func (d *DiskWarmupCache) ReadTail(hash string, fileID int, buf []byte, absoluteOffset, fileSize int64) (int, error) {
	tailStart := fileSize - TailWarmupSize
	if tailStart < 0 {
		tailStart = 0
	}
	if absoluteOffset < tailStart {
		return 0, nil
	}

	relOffset := absoluteOffset - tailStart
	path := d.tailPath(hash, fileID)

	readEnd := relOffset + int64(len(buf))
	if val, ok := d.tailCoverage.Load(path); ok {
		tr := val.(*tailRange)
		tr.mu.Lock()
		miss := readEnd > tr.highWatermark
		tr.mu.Unlock()
		if miss {
			fi, err := os.Stat(path)
			if err != nil || fi.Size() < readEnd {
				return 0, nil
			}
		}
	} else {
		fi, err := os.Stat(path)
		if err != nil || fi.Size() < readEnd {
			return 0, nil
		}
		d.tailCoverage.Store(path, &tailRange{highWatermark: fi.Size()})
	}

	ch, err := d.getHandle(path)
	if err != nil {
		return 0, nil
	}

	n, err := ch.f.ReadAt(buf, relOffset)
	return n, err
}

func (d *DiskWarmupCache) GetTailRange(hash string, fileID int) int64 {
	path := d.tailPath(hash, fileID)
	if _, ok := d.missing.Load(path); ok {
		return 0
	}

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

			if fi, err := e.Info(); err == nil {
				atomic.AddInt64(&d.totalSize, -fi.Size())
			}

			d.closeHandle(fullPath)
			d.sizeCache.Delete(fullPath)
			d.tailCoverage.Delete(fullPath)
			os.Remove(fullPath)
		}
	}
}

func (d *DiskWarmupCache) enforceQuotaLocked(needed int64) {
	quota := warmupQuota
	if diskQuotaGB > 0 {
		quota = diskQuotaGB * 1024 * 1024 * 1024
	}

	totalSize := atomic.LoadInt64(&d.totalSize)
	if totalSize+needed <= quota {
		return
	}

	entries, _ := os.ReadDir(d.dir)
	type wFile struct {
		path    string
		size    int64
		modTime int64
	}
	var files []wFile
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
		d.sizeCache.Delete(fi.path)
		d.tailCoverage.Delete(fi.path)
		os.Remove(fi.path)
		diskTotal -= fi.size
	}
	atomic.StoreInt64(&d.totalSize, diskTotal)
}
