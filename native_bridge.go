package main

import (
	"context"
	"fmt"
	"io"

	"gostream/internal/gostorm/torr"
	"gostream/internal/gostorm/torr/state"
	"gostream/internal/gostorm/torr/storage/torrstor"
	apiUtils "gostream/internal/gostorm/web/api/utils"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// NativeClient abstracts direct calls to the internal GoStorm instance
// eliminating HTTP overhead for metadata operations.
type NativeClient struct {
	// Stateless client
	activeHashes  sync.Map     // Map[string]bool - Fast lookup for active torrents
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
		// Note: ForceSaveTorrentToDB at torrent expiry captures the full peer swarm
		// safely (no streaming active). The previous 90s delayed goroutine was
		// removed (V306) because it fired during active playback, causing cl._mu
		// contention (PeerConns snapshot) that briefly stalled the pump.
		torr.SaveTorrentToDB(t)

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
	reader       *torrstor.Reader
	readerPtr    atomic.Pointer[torrstor.Reader] // V286: lock-free access for Interrupt()
	closed       bool
	lastActivity time.Time
	interrupted  atomic.Bool // V286: set by Interrupt(), cleared by next openReader
}

// ErrInterrupted is returned by ReadAt when the reader was closed by Interrupt().
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
		r.closeReader()
		return 0, ErrInterrupted
	}

	// 1. Sequential Match
	if r.reader != nil && off == r.offset {
		n, err = io.ReadFull(r.reader, p)
		r.offset += int64(n)

		if err == nil || err == io.EOF || err == io.ErrUnexpectedEOF {
			return n, nil
		}

		// V286: If the reader was closed by Interrupt(), return ErrInterrupted
		if r.interrupted.Swap(false) {
			r.closeReader()
			return 0, ErrInterrupted
		}

		// V257: Resilience Fix - If read fails, attempt one transparent reconnect
		log.Printf("[NativeReader] Sequential Read Error: %v - Attempting Transparent Reconnect at offset %d", err, off)
	}

	// 2. Smart Seek — real seek instead of drain
	if r.reader != nil && off > r.offset && off-r.offset < 2*1024*1024 {
		if _, errSeek := r.reader.Seek(off, io.SeekStart); errSeek == nil {
			r.offset = off
			n, err = io.ReadFull(r.reader, p)
			r.offset += int64(n)
			if err == nil || err == io.EOF || err == io.ErrUnexpectedEOF {
				return n, nil
			}
			log.Printf("[NativeReader] Smart Seek Read Error: %v - Attempting Transparent Reconnect at offset %d", err, off)
		}

		if r.interrupted.Swap(false) {
			r.closeReader()
			return 0, ErrInterrupted
		}
	}

	// V286: Final interrupt check before Hard Seek
	if r.interrupted.Swap(false) {
		if r.reader != nil {
			r.closeReader()
		}
		return 0, ErrInterrupted
	}

	// 4. Hard Seek (Recovery Path for errors or large seeks)
	if r.reader != nil {
		r.closeReader()
	}

	if err := r.openReader(off); err != nil {
		return 0, err
	}

	n, err = io.ReadFull(r.reader, p)
	r.offset += int64(n)
	if err == io.ErrUnexpectedEOF {
		return n, nil
	}
	return n, err
}

// V286: Interrupt unblocks a blocked ReadAt by closing the underlying anacrolix reader.
// Uses lock-free readerPtr so Interrupt() never blocks even if ReadAt holds r.mu.
func (r *NativeReader) Interrupt() {
	r.interrupted.Store(true)
	if reader := r.readerPtr.Load(); reader != nil {
		reader.Reader.Close() // sblocca reader.Read() bloccata sull'anacrolix reader
	}
}

func (r *NativeReader) openReader(off int64) error {
	t := torr.PeekTorrent(r.hash)
	if t == nil || t.Torrent == nil {
		return fmt.Errorf("torrent not found")
	}
	reader, err := t.OpenFile(r.fileID)
	if err != nil {
		return err
	}
	if _, err := reader.Seek(off, io.SeekStart); err != nil {
		t.CloseReader(reader)
		return err
	}

	// V166-Fix: Force initial readahead to trigger anacrolix downloader.
	// Without this, the native reader might not "wake up" the torrent swarm.
	reader.SetReadahead(64 << 20) // 64MB initial readahead

	r.reader = reader
	r.readerPtr.Store(reader) // V286: expose for lock-free Interrupt()
	r.offset = off
	return nil
}

func (r *NativeReader) closeReader() {
	if r.reader != nil {
		r.readerPtr.Store(nil) // V286: clear before close
		t := torr.PeekTorrent(r.hash)
		if t != nil {
			t.CloseReader(r.reader)
		}
		r.reader = nil
	}
}

func (r *NativeReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.closeReader()
	return nil
}

func (r *NativeReader) IsIdle(d time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Since(r.lastActivity) > d
}

// FetchBlock performs an atomic, stateless read from the Torrent Core.
func (c *NativeClient) FetchBlock(hash string, fileID int, offset int64, p []byte) (int, error) {
	t := torr.PeekTorrent(hash)
	if t == nil || t.Torrent == nil {
		return 0, fmt.Errorf("torrent not found")
	}

	reader, err := t.OpenFile(fileID)
	if err != nil {
		return 0, err
	}
	defer t.CloseReader(reader)

	if _, err := reader.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}

	// V283: 8s timeout (was 30s). 3 retries × 8s = 27s max → under 60s watchdog threshold.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := io.ReadFull(reader, p)
		ch <- result{n, err}
	}()

	select {
	case res := <-ch:
		if res.err == io.ErrUnexpectedEOF {
			return res.n, nil
		}
		return res.n, res.err
	case <-ctx.Done():
		reader.Reader.Close() // sblocca la goroutine
		res := <-ch           // attendi completamento
		if res.n > 0 {
			return res.n, nil
		}
		return 0, fmt.Errorf("FetchBlock timeout (8s) at offset %d", offset)
	}
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
