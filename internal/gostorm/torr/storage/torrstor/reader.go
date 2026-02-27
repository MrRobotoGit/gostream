package torrstor

import (
	"io"
	"sync"
	"time"

	"github.com/anacrolix/torrent"

	"gostream/internal/gostorm/log"
	"gostream/internal/gostorm/settings"
)

type Reader struct {
	torrent.Reader
	offset    int64
	readahead int64
	file      *torrent.File

	cache    *Cache
	isClosed bool

	///Preload
	lastAccess int64
	isUse      bool
	mu         sync.Mutex
}

func newReader(file *torrent.File, cache *Cache) *Reader {
	r := new(Reader)
	r.file = file
	r.Reader = file.NewReader()

	// V231: Activate Aggressive mode immediately on reader creation
	// to boost initial buffering before the Plex webhook triggers.
	cache.SetAggressive(true, 0)

	r.SetReadahead(0)
	r.cache = cache
	r.isUse = true
	cache.activeReaders.Add(1)

	cache.muReaders.Lock()
	if cache.readers != nil {
		cache.readers[r] = struct{}{}
	}
	cache.muReaders.Unlock()
	return r
}

func (r *Reader) Seek(offset int64, whence int) (n int64, err error) {
	r.mu.Lock()
	if r.isClosed {
		r.mu.Unlock()
		return 0, io.EOF
	}
	switch whence {
	case io.SeekStart:
		r.offset = offset
	case io.SeekCurrent:
		r.offset += offset
	case io.SeekEnd:
		r.offset = r.file.Length() + offset
	}
	r.mu.Unlock()

	r.readerOn()
	n, err = r.Reader.Seek(offset, whence)

	r.mu.Lock()
	r.offset = n
	r.lastAccess = time.Now().Unix()
	r.mu.Unlock()
	return
}

func (r *Reader) Read(p []byte) (n int, err error) {
	err = io.EOF
	r.mu.Lock()
	if r.isClosed {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()

	if r.file.Torrent() != nil && r.file.Torrent().Info() != nil {
		r.readerOn()
		n, err = r.Reader.Read(p)

		r.mu.Lock()
		r.offset += int64(n)
		r.lastAccess = time.Now().Unix()
		r.mu.Unlock()
	} else {
		log.TLogln("Torrent closed and readed")
	}
	return
}

func (r *Reader) SetReadahead(length int64) {
	// V244: Panic Protection
	if r.cache == nil {
		return
	}
	// V255: Removed V244 85% clamp — it reduced prefetch by ~13MB without benefit.
	// Cache eviction is LRU-based and doesn't use readahead boundaries.
	// Unbounded readahead growth is now prevented by maxDefaultReadahead cap in anacrolix fork.
	if r.isUse {
		r.Reader.SetReadahead(length)
	}
	r.readahead = length
}

func (r *Reader) Offset() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.offset
}

func (r *Reader) Readahead() int64 {
	return r.readahead
}

func (r *Reader) Close() {
	// file reader close in gotorrent
	// this struct close in cache
	r.mu.Lock()
	if r.isClosed {
		r.mu.Unlock()
		return
	}
	r.isClosed = true
	r.mu.Unlock()

	r.readerOff()

	if len(r.file.Torrent().Files()) > 0 {
		r.Reader.Close()
	}
	safeGo(func() {
		r.cache.getRemPieces()
	})
}

func (r *Reader) getPiecesRange() Range {
	startOff, endOff := r.getOffsetRange()
	return Range{r.getPieceNum(startOff), r.getPieceNum(endOff), r.file}
}

func (r *Reader) getReaderPiece() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getPieceNum(r.offset)
}

func (r *Reader) getReaderRAHPiece() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getPieceNum(r.offset + r.readahead)
}

func (r *Reader) getPieceNum(offset int64) int {
	return int((offset + r.file.Offset()) / r.cache.pieceLength)
}

func (r *Reader) getOffsetRange() (int64, int64) {
	prc := int64(settings.BTsets.ReaderReadAHead)
	readers := int64(r.getUseReaders())
	if readers == 0 {
		readers = 1
	}

	r.mu.Lock()
	currentOffset := r.offset
	r.mu.Unlock()

	// V243-Fix: Reader Protected Range Safety Factor (0.85)
	// We MUST leave room (15%) for swappable chunks, otherwise cleanPieces can't remove anything
	// when cache is full, leading to deadlock/OOM.
	safetyFactor := 0.85

	beginOffset := currentOffset - int64(float64(r.cache.capacity/readers)*safetyFactor*float64(100-prc)/100.0)
	endOffset := currentOffset + int64(float64(r.cache.capacity/readers)*safetyFactor*float64(prc)/100.0)

	if beginOffset < 0 {
		beginOffset = 0
	}

	if endOffset > r.file.Length() {
		endOffset = r.file.Length()
	}
	return beginOffset, endOffset
}

func (r *Reader) checkReader() {
	r.mu.Lock()
	accessTime := r.lastAccess
	r.mu.Unlock()

	if time.Now().Unix() > accessTime+60 && len(r.cache.readers) > 1 {
		r.readerOff()
	} else {
		r.readerOn()
	}
}

func (r *Reader) readerOn() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.isUse {
		if pos, err := r.Reader.Seek(0, io.SeekCurrent); err == nil && pos == 0 {
			r.Reader.Seek(r.offset, io.SeekStart)
		}
		r.SetReadahead(r.readahead)
		r.isUse = true
		r.cache.activeReaders.Add(1)
	}
}

func (r *Reader) readerOff() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.isUse {
		r.SetReadahead(0)
		r.isUse = false
		r.cache.activeReaders.Add(-1)
		if r.offset > 0 {
			r.Reader.Seek(0, io.SeekStart)
		}
	}
}

func (r *Reader) getUseReaders() int {
	if r.cache != nil {
		return int(r.cache.activeReaders.Load())
	}
	return 0
}
