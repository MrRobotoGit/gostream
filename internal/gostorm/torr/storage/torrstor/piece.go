package torrstor

import (
	"sync/atomic"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"

	"gostream/internal/gostorm/log"
	"gostream/internal/gostorm/settings"
)

var (
	// V303: Atomic Shield Protection
	// Using atomic.Int64 to store last corruption Unix timestamp for thread-safety.
	lastCorruptionUnix atomic.Int64
	// shieldActive tracks if we are currently forcing STRICT mode due to corruption.
	shieldActive atomic.Bool
	// isWatchdogRunning prevents multiple goroutine spawns.
	isWatchdogRunning atomic.Bool
)

// IsResponsive returns the effective state of ResponsiveMode,
// taking into account both user settings and active corruption shield.
func IsResponsive() bool {
	// If user manually disabled ResponsiveMode, it stays OFF regardless of shield.
	// If user enabled it, we return true ONLY if shield is NOT active.
	return settings.GetResponsiveMode() && !shieldActive.Load()
}

type Piece struct {
	storage.PieceImpl `json:"-"`

	Id   int   `json:"-"`
	Size int64 `json:"size"`

	Complete bool  `json:"complete"`
	Accessed int64 `json:"accessed"`

	mPiece *MemPiece `json:"-"`

	cache *Cache `json:"-"`
}

func NewPiece(id int, cache *Cache) *Piece {
	p := &Piece{
		Id:    id,
		cache: cache,
	}

	// V256: RAM is always the primary torrent cache.
	// UseDisk now controls our FUSE-layer disk warmup cache, not native GoStorm piece storage.
	p.mPiece = NewMemPiece(p)
	return p
}

func (p *Piece) WriteAt(b []byte, off int64) (n int, err error) {
	return p.mPiece.WriteAt(b, off)
}

func (p *Piece) ReadAt(b []byte, off int64) (n int, err error) {
	return p.mPiece.ReadAt(b, off)
}

func (p *Piece) MarkComplete() error {
	p.Complete = true
	return nil
}

func (p *Piece) MarkNotComplete() error {
	p.Complete = false

	// V303: Adaptive Responsive Shield (Refined)
	// Corruption detected: update last seen Unix timestamp
	now := time.Now().Unix()
	lastCorruptionUnix.Store(now)

	// Activate shield if user settings allowed responsive mode but it's now dirty.
	if settings.GetResponsiveMode() && !shieldActive.Load() {
		log.TLogln("[AdaptiveShield] Corruption detected for piece", p.Id, "- Force STRICT mode (Shield: ACTIVE)")
		shieldActive.Store(true)
	}

	// Start watchdog only once
	if shieldActive.Load() && !isWatchdogRunning.Swap(true) {
		go func() {
			for {
				time.Sleep(10 * time.Second)
				
				// Calculate time since last corruption using atomic load
				last := lastCorruptionUnix.Load()
				elapsed := time.Since(time.Unix(last, 0))

				if elapsed > 60*time.Second {
					if shieldActive.Swap(false) {
						log.TLogln("[AdaptiveShield] Clean streak detected (60s) - Restoring FAST mode (Shield: OFF)")
					}
					isWatchdogRunning.Store(false)
					return // End watchdog
				}
			}
		}()
	}

	return nil
}

func (p *Piece) Completion() storage.Completion {
	return storage.Completion{
		Complete: p.Complete,
		Ok:       true,
	}
}

func (p *Piece) Release() {
	p.mPiece.Release()
	if !p.cache.isClosed {
		p.cache.torrent.Piece(p.Id).SetPriority(torrent.PiecePriorityNone)
		p.cache.torrent.Piece(p.Id).UpdateCompletion()
	}
}
