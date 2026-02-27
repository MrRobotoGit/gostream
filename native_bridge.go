package main

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/MrRobotoGit/gostream/internal/gostorm/torr"
	"github.com/MrRobotoGit/gostream/internal/gostorm/torr/state"
	apiUtils "github.com/MrRobotoGit/gostream/internal/gostorm/web/api/utils"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// NativeClient abstracts direct calls to the internal GoStorm instance
// eliminating HTTP overhead for metadata operations.
type NativeClient struct {
	// Stateless client
	activeHashes  sync.Map      // Map[string]bool - Fast lookup for active torrents
	wakeSemaphore chan struct{} // V239: Limit concurrent Wake calls (max 10)
}

// NewNativeClient creates a new native bridge client
func NewNativeClient() *NativeClient {
	return &NativeClient{
		wakeSemaphore: make(chan struct{}, 25), // Max 25 concurrent Wake operations
	}
}

// Wake triggers the start of a torrent (Ghost -> Active) entirely in-memory
// Synchronous & Deduplicated.
func (c *NativeClient) Wake(magnetUrl string, fileIdx int) error {
	// V239-Semaphore: Guard against "Thread Exhaustion" during massive scans
	select {
	case c.wakeSemaphore <- struct{}{}:
		defer func() { <-c.wakeSemaphore }()
	default:
		// Fail-Fast: If >10 Opens are pending, we drop the request to save the filesystem.
		// Player will retry, or fail this specific file, but FUSE remains alive.
		return fmt.Errorf("wake semaphore exhausted (system busy)")
	}
	// 1. Parse Magnet/Link to get hash
	spec, err := apiUtils.ParseLink(magnetUrl)
	if err != nil {
		return fmt.Errorf("parse link error: %w", err)
	}
	hash := spec.InfoHash.HexString()

	// 2. Dedup: Check if already active (optimization)
	var t *torr.Torrent
	if _, ok := c.activeHashes.Load(hash); ok {
		// V255: Use PeekTorrent to check RAM only, not re-activate from DB.
		if existing := torr.PeekTorrent(hash); existing != nil && existing.Torrent != nil {
			t = existing
			// V265: If we have an existing torrent, we fall through to the metadata check
			// instead of returning nil, to ensure Open waits if metadata isn't ready.
		} else {
			// If not in core but in our map, remove it and proceed to add
			c.activeHashes.Delete(hash)
		}
	}

	// 3. Synchronous Wakeup
	if t == nil {
		// Add/Start Torrent via Internal API
		var err error
		t, err = torr.AddTorrent(spec, "", "", "", "")
		if err != nil {
			return fmt.Errorf("add torrent error: %w", err)
		}
	}

	// Wait for metadata
	if t != nil {
		if t.Torrent != nil && t.Torrent.Info() == nil {
			// Metadata NOT ready yet - wait with 45s timeout (Resilience)
			timer := time.NewTimer(45 * time.Second)
			defer timer.Stop()

			select {
			case <-t.Torrent.GotInfo():
				// Metadata ready
				log.Printf("[NativeBridge] Metadata received for %s", hash)
			case <-timer.C:
				log.Printf("[NativeBridge] Metadata timeout for %s", hash)
				return fmt.Errorf("torrent metadata timeout (45s): %s", hash)
			}
		}
		// V255: Save metadata to DB immediately so next Wake() skips GotInfo() wait.
		torr.SaveTorrentToDB(t)

		// V255: Delayed save after 90s to capture full swarm (15-25 peers).
		// The immediate save above only has 1-3 metadata peers.
		// By 90s, tracker/DHT have fully responded and peer swarm is at peak.
		// At expiry, peers have already disconnected (no data being requested).
		go func() {
			time.Sleep(90 * time.Second)
			if t.Torrent != nil {
				torr.SaveTorrentToDB(t)
			}
		}()

		// Optimistic active update
		c.activeHashes.Store(hash, true)
	}

	return nil
}

// CleanupHashes removes hashes from the local map that are no longer present in the GoStorm core.
// Prevents memory leaks in long-running sessions.
func (c *NativeClient) CleanupHashes() int {
	removed := 0
	c.activeHashes.Range(func(key, value interface{}) bool {
		hash := key.(string)
		// V255: Use PeekTorrent to avoid re-activating expired torrents from DB.
		// GetTorrent() would re-activate DB-only entries, causing infinite loops.
		// Check Torrent handle (nil = DB-only or not found, non-nil = active in engine).
		t := torr.PeekTorrent(hash)
		if t == nil || t.Torrent == nil {
			c.activeHashes.Delete(hash)
			removed++
		}
		return true
	})
	return removed
}

// Probe checks if a torrent is active
func (c *NativeClient) Probe(hash string) bool {
	_, ok := c.activeHashes.Load(hash)
	return ok
}

// GetTorrent returns statistics for a specific torrent by hash
func (c *NativeClient) GetTorrent(hash string) (*TorrentStats, error) {
	t := torr.PeekTorrent(hash)
	if t == nil {
		return nil, fmt.Errorf("torrent not found: %s", hash)
	}

	// V162: Use lightweight StatHighFreq to avoid lock contention
	st := t.StatHighFreq()
	return convertStatusToStats(st), nil
}

// NewStreamReader creates a new stateful hybrid reader for a torrent file.
func (c *NativeClient) NewStreamReader(hash string, fileID int, totalSize int64) *NativeReader {
	return &NativeReader{
		hash:         hash,
		fileID:       fileID,
		lastActivity: time.Now(),
	}
}

// NativeReader implements a hybrid stateful/stateless reader for Torrent files.
type NativeReader struct {
	mu           sync.Mutex
	hash         string
	fileID       int
	offset       int64
	pipeReader   *io.PipeReader
	pipeWriter   *io.PipeWriter
	cancelFunc   context.CancelFunc
	closed       bool
	lastActivity time.Time
	interrupted  atomic.Bool // V286: set by Interrupt(), cleared by next startStream
}

// ErrInterrupted is returned by ReadAt when the pipe was closed by Interrupt().
var ErrInterrupted = fmt.Errorf("interrupted by seek")

// ReadAt implements io.ReaderAt.
func (r *NativeReader) ReadAt(p []byte, off int64) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return 0, io.ErrClosedPipe
	}

	r.lastActivity = time.Now()

	// V286: Check for interrupt BEFORE any operation
	if r.interrupted.Swap(false) {
		r.closeStream()
		return 0, ErrInterrupted
	}

	// 1. Sequential Match
	if r.pipeReader != nil && off == r.offset {
		n, err = io.ReadFull(r.pipeReader, p)
		r.offset += int64(n)
		
		if err == nil || err == io.EOF || err == io.ErrUnexpectedEOF {
			return n, nil
		}

		// V286: If the pipe was closed by Interrupt(), return ErrInterrupted
		if r.interrupted.Swap(false) {
			r.closeStream()
			return 0, ErrInterrupted
		}

		// V257: Resilience Fix - If pipe fails, attempt one transparent reconnect
		log.Printf("[NativeReader] Sequential Read Error: %v - Attempting Transparent Reconnect at offset %d", err, off)
	}

	// 2. Smart Seek
	if r.pipeReader != nil && off > r.offset && off-r.offset < 2*1024*1024 {
		skip := off - r.offset
		_, errSkip := io.CopyN(io.Discard, r.pipeReader, skip)
		if errSkip == nil {
			r.offset = off
			n, err = io.ReadFull(r.pipeReader, p)
			r.offset += int64(n)
			if err == nil || err == io.EOF || err == io.ErrUnexpectedEOF {
				return n, nil
			}
			log.Printf("[NativeReader] Smart Seek Read Error: %v - Attempting Transparent Reconnect at offset %d", err, off)
		}
		
		if r.interrupted.Swap(false) {
			r.closeStream()
			return 0, ErrInterrupted
		}
	}

	// V286: Final interrupt check before Hard Seek
	if r.interrupted.Swap(false) {
		if r.pipeReader != nil {
			r.closeStream()
		}
		return 0, ErrInterrupted
	}

	// 4. Hard Seek (Recovery Path for errors or large seeks)
	if r.pipeReader != nil {
		r.closeStream()
	}

	if err := r.startStream(off); err != nil {
		return 0, err
	}

	n, err = io.ReadFull(r.pipeReader, p)
	r.offset += int64(n)
	if err == io.ErrUnexpectedEOF {
		return n, nil
	}
	return n, err
}

// V286: Interrupt unblocks a blocked ReadAt by closing the pipe reader.
// Sets interrupted flag so ReadAt returns ErrInterrupted reliably.
func (r *NativeReader) Interrupt() {
	r.interrupted.Store(true)
	r.mu.Lock()
	pr := r.pipeReader
	r.mu.Unlock()
	if pr != nil {
		pr.Close() // Reader side close is enough to unblock ReadFull
	}
}

func (r *NativeReader) startStream(off int64) error {
	// V255: Use PeekTorrent to avoid extending expiry timer on every Hard Seek.
	t := torr.PeekTorrent(r.hash)
	if t == nil || t.Torrent == nil {
		return fmt.Errorf("torrent not found")
	}

	pr, pw := io.Pipe()
	r.pipeReader = pr
	r.pipeWriter = pw
	r.offset = off

	// V460: Use a context that we can cancel explicitly on Close()
	ctx, cancel := context.WithCancel(context.Background())
	r.cancelFunc = cancel

	// Create request with our explicit context
	req, _ := http.NewRequestWithContext(ctx, "GET", "/stream", nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", off))

	resp := &PipeResponseWriter{
		writer: pw,
		header: make(http.Header),
	}

	go func() {
		defer pw.Close()
		if err := t.Stream(r.fileID, req, resp); err != nil {
			log.Printf("[NativeReader] Stream error at off=%dMB fileID=%d hash=%s: %v",
				off/(1024*1024), r.fileID, r.hash[:8], err)
		}
	}()

	return nil
}

func (r *NativeReader) closeStream() {
	// V460: Cancel context FIRST to trigger GoStorm exit
	if r.cancelFunc != nil {
		r.cancelFunc()
		r.cancelFunc = nil
	}

	// Small delay to allow context propagation? No, BoltDB/RAM is fast.

	if r.pipeReader != nil {
		r.pipeReader.Close()
		r.pipeReader = nil
	}
	if r.pipeWriter != nil {
		r.pipeWriter.Close()
		r.pipeWriter = nil
	}
}

func (r *NativeReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.closeStream()
	return nil
}

func (r *NativeReader) IsIdle(d time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Since(r.lastActivity) > d
}

// FetchBlock performs an atomic, stateless read from the Torrent Core.
func (c *NativeClient) FetchBlock(hash string, fileID int, offset int64, p []byte) (int, error) {
	// V255: Use PeekTorrent — same reasoning as startStream above.
	t := torr.PeekTorrent(hash)
	if t == nil || t.Torrent == nil {
		return 0, fmt.Errorf("torrent not found")
	}

	pr, pw := io.Pipe()
	// V283: 8s timeout (was 30s). 6 retries × 30s = 180s FUSE block → smbd D-state.
	// 3 retries × 8s = 27s max → under 60s watchdog threshold.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", "/stream", nil)
	endRange := offset + int64(len(p)) - 1
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, endRange))

	resp := &PipeResponseWriter{
		writer: pw,
		header: make(http.Header),
	}

	go func() {
		defer pw.Close()
		t.Stream(fileID, req, resp)
	}()

	n, err := io.ReadFull(pr, p)
	pr.Close()

	if err == io.ErrUnexpectedEOF {
		return n, nil
	}

	return n, err
}

// ListTorrents returns all torrents
func (c *NativeClient) ListTorrents() ([]TorrentStats, error) {
	list := torr.ListTorrent()
	result := make([]TorrentStats, 0, len(list))

	for _, t := range list {
		if t != nil {
			result = append(result, *convertStatusToStats(t.Status()))
		}
	}
	return result, nil
}

// RemoveTorrent removes a torrent from the server
func (c *NativeClient) RemoveTorrent(hash string) error {
	torr.RemTorrent(hash)
	// V272: Clean up disk warmup files for this hash
	if diskWarmup != nil && hash != "" {
		diskWarmup.RemoveHash(hash)
	}
	return nil
}

// Preload triggers a direct preload request
func (c *NativeClient) Preload(hash string, index int, preloadSize int64) {
	t := torr.GetTorrent(hash)
	if t != nil && t.Torrent != nil {
		// Public API preload - defaulting to background context or reasonable timeout
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		t.Preload(ctx, index, preloadSize)
	}
}

// PipeResponseWriter bridges GoStorm's HTTP responses to our Go pipe.
type PipeResponseWriter struct {
	writer *io.PipeWriter
	header http.Header
}

func (w *PipeResponseWriter) Header() http.Header         { return w.header }
func (w *PipeResponseWriter) Write(p []byte) (int, error) { return w.writer.Write(p) }
func (w *PipeResponseWriter) WriteHeader(statusCode int)  {}

// convertStatusToStats maps internal TorrentStatus to our local TorrentStats struct
func convertStatusToStats(st *state.TorrentStatus) *TorrentStats {
	if st == nil {
		return nil
	}

	return &TorrentStats{
		Hash:          st.Hash,
		Title:         st.Title,
		DownloadSpeed: st.DownloadSpeed,
		TotalPeers:    st.TotalPeers,
		ActivePeers:   st.ActivePeers,
		Downloaded:    st.LoadedSize,
	}
}
