package torrstor

import (
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/torrent"

	"gostream/internal/gostorm/log"
	"gostream/internal/gostorm/settings"
	"gostream/internal/gostorm/torr/storage/state"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	torrenttypes "github.com/anacrolix/torrent/types"
)

// safeGo runs a function in a new goroutine with panic recovery.
func safeGo(fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.TLogln("[PANIC] Background goroutine recovered:", r)
			}
		}()
		fn()
	}()
}

type Cache struct {
	storage.TorrentImpl
	storage *Storage

	capacity int64
	filled   int64
	hash     metainfo.Hash

	pieceLength int64
	pieceCount  int

	pieces map[int]*Piece

	readers   map[*Reader]struct{}
	muReaders sync.RWMutex

	activeReaders atomic.Int32

	isRemove     bool
	isClosed     bool
	IsAggressive bool // V217: Aggressive download priority
	MasterLimit  int  // V218: Master limit from config.json
	lastClean    time.Time
	muRemove     sync.Mutex
	torrent      *torrent.Torrent
	cleanTrigger chan struct{} // V227: Rate-limited cleanup trigger (never closed — use cleanStop)
	cleanStop    chan struct{} // V280: Closed on Cache.Close() to stop the goroutine

	// V279: Track which pieces have non-None priority set by us.
	// clearPriority() iterates this (~25 entries) instead of all c.pieces (~512).
	// Eliminates O(N_cached) PieceState(rLock) calls per cleanup cycle.
	localPriority map[int]torrenttypes.PiecePriority
	muPriority    sync.Mutex // protects localPriority
}

func NewCache(capacity int64, storage *Storage) *Cache {
	ret := &Cache{
		capacity:      capacity,
		filled:        0,
		pieces:        make(map[int]*Piece),
		storage:       storage,
		readers:       make(map[*Reader]struct{}),
		cleanTrigger:  make(chan struct{}, 1), // V227: Non-blocking trigger, never closed
		cleanStop:     make(chan struct{}),    // V280: Closed on Cache.Close()
		localPriority: make(map[int]torrenttypes.PiecePriority),
	}
	// V227: Background cleaning goroutine
	safeGo(func() {
		for {
			select {
			case <-ret.cleanStop:
				return
			case <-ret.cleanTrigger:
				ret.cleanPieces()
			}
		}
	})
	return ret
}

func (c *Cache) Init(info *metainfo.Info, hash metainfo.Hash) {
	log.TLogln("Create cache for:", info.Name, hash.HexString())
	if c.capacity == 0 {
		c.capacity = info.PieceLength * 4
	}

	c.pieceLength = info.PieceLength
	c.pieceCount = info.NumPieces()
	c.hash = hash

	for i := 0; i < c.pieceCount; i++ {
		c.pieces[i] = NewPiece(i, c)
	}
}

func (c *Cache) SetTorrent(torr *torrent.Torrent) {
	c.torrent = torr
}

func (c *Cache) SetAggressive(enabled bool, masterLimit int) {
	c.muReaders.Lock()
	defer c.muReaders.Unlock()
	c.IsAggressive = enabled
	if masterLimit > 0 {
		c.MasterLimit = masterLimit
	}
}

func (c *Cache) Piece(m metainfo.Piece) storage.PieceImpl {
	if val, ok := c.pieces[m.Index()]; ok {
		return val
	}
	return &PieceFake{}
}

func (c *Cache) Close() error {
	if c.torrent != nil {
		log.TLogln("Close cache for:", c.torrent.Name(), c.hash)
	} else {
		log.TLogln("Close cache for:", c.hash)
	}
	c.isClosed = true
	close(c.cleanStop) // V280: Stop background goroutine (cleanTrigger never closed → no panic on send)

	delete(c.storage.caches, c.hash)

	if settings.BTsets.RemoveCacheOnDrop {
		name := filepath.Join(settings.BTsets.TorrentsSavePath, c.hash.HexString())
		if name != "" && name != "/" {
			os.Remove(name)
		}
	}

	c.muReaders.Lock()
	c.readers = nil
	c.pieces = nil
	c.muReaders.Unlock()

	return nil
}

func (c *Cache) removePiece(piece *Piece) {
	if !c.isClosed {
		piece.Release()
	}
}

func (c *Cache) AdjustRA(readahead int64) {
	if settings.BTsets.CacheSize == 0 {
		c.capacity = readahead * 3
	}
	if c.Readers() > 0 {
		c.muReaders.RLock()
		for r := range c.readers {
			r.SetReadahead(readahead)
		}
		c.muReaders.RUnlock()
	}
}

func (c *Cache) GetState() *state.CacheState {
	cState := new(state.CacheState)

	piecesState := make(map[int]state.ItemState, 0)
	var fill int64 = 0

	if len(c.pieces) > 0 {
		for _, p := range c.pieces {
			if p.Size > 0 {
				fill += p.Size
				priority := 0
				if c.torrent != nil {
					priority = int(c.torrent.PieceState(p.Id).Priority)
				}
				piecesState[p.Id] = state.ItemState{
					Id:        p.Id,
					Size:      p.Size,
					Length:    c.pieceLength,
					Completed: p.Complete,
					Priority:  priority,
				}
			}
		}
	}

	readersState := make([]*state.ReaderState, 0)

	if c.Readers() > 0 {
		c.muReaders.RLock()
		for r := range c.readers {
			rng := r.getPiecesRange()
			pc := r.getReaderPiece()
			readersState = append(readersState, &state.ReaderState{
				Start:  rng.Start,
				End:    rng.End,
				Reader: pc,
			})
		}
		c.muReaders.RUnlock()
	}

	c.filled = fill
	cState.Capacity = c.capacity
	cState.PiecesLength = c.pieceLength
	cState.PiecesCount = c.pieceCount
	cState.Hash = c.hash.HexString()
	cState.Filled = fill
	cState.Pieces = piecesState
	cState.Readers = readersState
	return cState
}

// V255: Lightweight priority update without cleanup/eviction.
// Only iterates reader ranges + ~25 active pieces. Safe to call on every trigger.
// Prevents micro-stutters at cache boundary by keeping piece priorities aligned
// with the reader position without waiting for the 1-second cleanup throttle.
func (c *Cache) refreshPriorities() {
	if c.isClosed || c.torrent == nil {
		return
	}
	ranges := make([]Range, 0)
	c.muReaders.RLock()
	if c.pieces == nil || c.readers == nil {
		c.muReaders.RUnlock()
		return
	}
	for r := range c.readers {
		if r.isUse {
			ranges = append(ranges, r.getPiecesRange())
		}
	}
	c.muReaders.RUnlock()
	ranges = mergeRange(ranges)
	c.setLoadPriority(ranges)
}

func (c *Cache) cleanPieces() {
	if c.isRemove || c.isClosed {
		return
	}

	// V255: Always update priorities immediately (cheap, ~25 pieces).
	// This prevents micro-stutters at cache/download boundary where the reader
	// would block up to 1s on waitAvailable() before priorities were updated.
	c.refreshPriorities()

	// V138: Throttle eviction to at most once per second,
	// unless we are near capacity (>90%)
	now := time.Now()
	if now.Sub(c.lastClean) < time.Second && c.filled < (c.capacity*9)/10 {
		return
	}

	// V94: Use TryLock to avoid goroutine pile-up during high-speed streaming
	if !c.muRemove.TryLock() {
		return
	}
	c.isRemove = true
	c.lastClean = now
	defer func() {
		c.isRemove = false
		c.muRemove.Unlock()
	}()

	remPieces := c.getRemPieces()
	if c.filled > c.capacity {
		rems := (c.filled-c.capacity)/c.pieceLength + 1
		for _, p := range remPieces {
			c.removePiece(p)
			rems--
			if rems <= 0 {
				// V244-Fix: Removed FreeOSMemGC() - Stop-The-World latency killer!
				// Go Runtime handles GC automatically. Forcing it here causes buffer underrun.
				return
			}
		}
	}

}

func (c *Cache) getRemPieces() []*Piece {
	piecesRemove := make([]*Piece, 0)
	fill := int64(0)

	ranges := make([]Range, 0)
	c.muReaders.RLock()
	if c.isClosed || c.pieces == nil || c.readers == nil {
		c.muReaders.RUnlock()
		return nil
	}
	for r := range c.readers {
		r.checkReader()
		if r.isUse {
			ranges = append(ranges, r.getPiecesRange())
		}
	}
	c.muReaders.RUnlock()
	ranges = mergeRange(ranges)

	for id, p := range c.pieces {
		if p.Size > 0 {
			fill += p.Size
		}
		if len(ranges) > 0 {
			if !inRanges(ranges, id) {
				if p.Size > 0 && !c.isIdInFileBE(ranges, id) {
					piecesRemove = append(piecesRemove, p)
				}
			}
		} else {
			// on preload clean
			if p.Size > 0 && !c.isIdInFileBE(ranges, id) {
				piecesRemove = append(piecesRemove, p)
			}
		}
	}

	c.clearPriority()
	c.setLoadPriority(ranges)

	sort.Slice(piecesRemove, func(i, j int) bool {
		return atomic.LoadInt64(&piecesRemove[i].Accessed) < atomic.LoadInt64(&piecesRemove[j].Accessed)
	})

	c.filled = fill
	return piecesRemove
}

func (c *Cache) setLoadPriority(ranges []Range) {
	if c.torrent == nil {
		return
	}
	c.muReaders.RLock()
	// Dynamic priority window based on cache capacity (10% of cache in pieces, minimum 5)
	highPriorityWindow := int(c.capacity / c.pieceLength / 10)
	if highPriorityWindow < 5 {
		highPriorityWindow = 5
	}
	for r := range c.readers {
		if !r.isUse {
			continue
		}
		if c.isIdInFileBE(ranges, r.getReaderPiece()) {
			continue
		}
		readerPos := r.getReaderPiece()
		readerRAHPos := r.getReaderRAHPiece()
		end := r.getPiecesRange().End

		numReaders := len(c.readers)
		if numReaders == 0 {
			numReaders = 1
		}

		// V218: Use MasterLimit from config.json if available, otherwise fallback to GoStorm settings
		effectiveLimit := settings.BTsets.ConnectionsLimit
		if c.MasterLimit > 0 {
			effectiveLimit = c.MasterLimit
		}

		// V217: Aggressive Mode Priority
		count := effectiveLimit / numReaders
		if c.IsAggressive {
			// V243: Safety - If cache is overfilled, disable aggressive expansion
			if c.filled > c.capacity {
				count = 1 // Fallback to minimal download
			} else {
				// V218: Aggressive but benevolent (80% rule).
				count = int(float64(effectiveLimit) * 0.8)
			}

			if count < 1 {
				count = 1 // Ensure at least 1 slot
			}
		}

		if count < 1 {
			count = 1
		}

		// V279: Hold muPriority for the loop to check/update localPriority without
		// per-piece lock overhead. SetPriority acquires cl.lock() internally (safe,
		// no circular dep since muPriority is torrstor-internal only).
		limit := 0
		c.muPriority.Lock()
		for i := readerPos; i < end && i < c.pieceCount && limit < count; i++ {
			if !c.pieces[i].Complete {
				if i == readerPos {
					c.torrent.Piece(i).SetPriority(torrent.PiecePriorityNow)
					c.localPriority[i] = torrent.PiecePriorityNow
				} else if i == readerPos+1 {
					c.torrent.Piece(i).SetPriority(torrent.PiecePriorityNext)
					c.localPriority[i] = torrent.PiecePriorityNext
				} else if i > readerPos && i <= readerRAHPos {
					c.torrent.Piece(i).SetPriority(torrent.PiecePriorityReadahead)
					c.localPriority[i] = torrent.PiecePriorityReadahead
				} else if i > readerRAHPos && i <= readerRAHPos+highPriorityWindow {
					// V279: localPriority replaces PieceState(rLock) guard
					if c.localPriority[i] != torrent.PiecePriorityHigh {
						c.torrent.Piece(i).SetPriority(torrent.PiecePriorityHigh)
						c.localPriority[i] = torrent.PiecePriorityHigh
					}
				} else if i > readerRAHPos+highPriorityWindow {
					// V279: localPriority replaces PieceState(rLock) guard
					if c.localPriority[i] != torrent.PiecePriorityNormal {
						c.torrent.Piece(i).SetPriority(torrent.PiecePriorityNormal)
						c.localPriority[i] = torrent.PiecePriorityNormal
					}
				}
				limit++
			}
		}
		c.muPriority.Unlock()
	}
	c.muReaders.RUnlock()
}

func (c *Cache) isIdInFileBE(ranges []Range, id int) bool {
	// keep 8/16 MB
	FileRangeNotDelete := int64(c.pieceLength)
	if FileRangeNotDelete < 8<<20 {
		FileRangeNotDelete = 8 << 20
	}

	for _, rng := range ranges {
		ss := int(rng.File.Offset() / c.pieceLength)
		se := int((rng.File.Offset() + FileRangeNotDelete) / c.pieceLength)

		es := int((rng.File.Offset() + rng.File.Length() - FileRangeNotDelete) / c.pieceLength)
		ee := int((rng.File.Offset() + rng.File.Length()) / c.pieceLength)

		if id >= ss && id < se || id > es && id <= ee {
			return true
		}
	}
	return false
}

//////////////////
// Reader section
////////

func (c *Cache) NewReader(file *torrent.File) *Reader {
	if c == nil {
		return nil
	}
	return newReader(file, c)
}

func (c *Cache) GetUseReaders() int {
	if c == nil {
		return 0
	}
	return int(c.activeReaders.Load())
}

func (c *Cache) Readers() int {
	if c == nil {
		return 0
	}
	c.muReaders.RLock()
	defer c.muReaders.RUnlock()
	if c.readers == nil {
		return 0
	}
	return len(c.readers)
}

func (c *Cache) CloseReader(r *Reader) {
	c.muReaders.Lock()
	if c.readers == nil || c.isClosed {
		c.muReaders.Unlock()
		return
	}
	r.Close()
	delete(c.readers, r)
	c.muReaders.Unlock()
	safeGo(func() {
		c.clearPriority()
	})
}

func (c *Cache) clearPriority() {
	if c.torrent == nil {
		return
	}
	// V180-Fix: Sleep REMOVED (V243). Immediate cleanup required to prevent OOM.
	// time.Sleep(time.Second)

	c.muReaders.RLock()
	if c.isClosed || c.pieces == nil || c.readers == nil {
		c.muReaders.RUnlock()
		return
	}

	ranges := make([]Range, 0)
	for r := range c.readers {
		r.checkReader()
		if r.isUse {
			ranges = append(ranges, r.getPiecesRange())
		}
	}

	c.muReaders.RUnlock()
	ranges = mergeRange(ranges)

	// V279: Iterate only pieces we explicitly prioritized (~25) instead of all c.pieces (~512).
	// Eliminates O(N_cached) PieceState(rLock) calls. localPriority is our authoritative
	// record of which pieces have non-None priority, so no PieceState() query needed.
	c.muPriority.Lock()
	for id := range c.localPriority {
		if len(ranges) == 0 || !inRanges(ranges, id) {
			c.torrent.Piece(id).SetPriority(torrent.PiecePriorityNone)
			delete(c.localPriority, id)
		}
	}
	c.muPriority.Unlock()
}

func (c *Cache) GetCapacity() int64 {
	if c == nil {
		return 0
	}
	return c.capacity
}

