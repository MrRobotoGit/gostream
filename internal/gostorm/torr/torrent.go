package torr

import (
	"errors"
	"gostream/internal/gostorm/torrshash"
	"sort"
	"strconv"
	"sync"
	"time"

	utils2 "gostream/internal/gostorm/utils"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"gostream/internal/gostorm/log"
	"gostream/internal/gostorm/settings"
	"gostream/internal/gostorm/torr/state"
	cacheSt "gostream/internal/gostorm/torr/storage/state"
	"gostream/internal/gostorm/torr/storage/torrstor"
	"gostream/internal/gostorm/torr/utils"
)

type Torrent struct {
	Title    string
	Category string
	Poster   string
	Data     string
	*torrent.TorrentSpec

	Stat      state.TorrentStat
	Timestamp int64
	Size      int64

	*torrent.Torrent
	muTorrent sync.Mutex

	bt    *BTServer
	cache *torrstor.Cache

	lastTimeSpeed       time.Time
	DownloadSpeed       float64
	UploadSpeed         float64
	BytesReadUsefulData int64
	BytesWrittenData    int64

	PreloadSize    int64
	PreloadedBytes int64

	DurationSeconds float64
	BitRate         string

	expiredTime time.Time
	IsPriority  bool // V185: If true, this torrent will never expire by timeout

	closed <-chan struct{}

	cachedFileStats []*state.TorrentFileStat
}

func NewTorrent(spec *torrent.TorrentSpec, bt *BTServer) (*Torrent, error) {
	// https://github.com/anacrolix/torrent/issues/747
	if bt == nil || bt.client == nil {
		return nil, errors.New("BT client not connected")
	}
	switch settings.BTsets.RetrackersMode {
	case 1:
		spec.Trackers = append(spec.Trackers, [][]string{utils.GetDefTrackers()}...)
	case 2:
		spec.Trackers = nil
	case 3:
		spec.Trackers = [][]string{utils.GetDefTrackers()}
	}

	trackers := utils.GetTrackerFromFile()
	if len(trackers) > 0 {
		spec.Trackers = append(spec.Trackers, [][]string{trackers}...)
	}

	goTorrent, _, err := bt.client.AddTorrentSpec(spec)
	if err != nil {
		return nil, err
	}

	bt.mu.Lock()
	defer bt.mu.Unlock()
	if tor, ok := bt.torrents[spec.InfoHash]; ok {
		return tor, nil
	}

	timeout := time.Second * time.Duration(settings.BTsets.TorrentDisconnectTimeout)
	if timeout > time.Minute {
		timeout = time.Minute
	}

	torr := new(Torrent)
	torr.Torrent = goTorrent
	torr.Stat = state.TorrentAdded
	torr.lastTimeSpeed = time.Now()
	torr.bt = bt
	torr.closed = goTorrent.Closed()
	torr.TorrentSpec = spec
	torr.AddExpiredTime(timeout)
	torr.Timestamp = time.Now().Unix()

	bt.torrents[spec.InfoHash] = torr
	return torr, nil
}

func (t *Torrent) WaitInfo() bool {
	if t == nil || t.Torrent == nil {
		return false
	}

	// Close torrent if no info in 1 minute + TorrentDisconnectTimeout config option
	tm := time.NewTimer(time.Minute + time.Second*time.Duration(settings.BTsets.TorrentDisconnectTimeout))
	defer tm.Stop()

	select {
	case <-t.Torrent.GotInfo():
		if t.bt != nil && t.bt.storage != nil {
			t.cache = t.bt.storage.GetCache(t.Hash())
			t.cache.SetTorrent(t.Torrent)
		}
		return true
	case <-t.closed:
		return false
	case <-tm.C:
		return false
	}
}

func (t *Torrent) GotInfo() bool {
	// log.TLogln("GotInfo state:", t.Stat)
	if t == nil || t.Stat == state.TorrentClosed {
		return false
	}
	// assume we have info in preload state
	// and dont override with TorrentWorking
	if t.Stat == state.TorrentPreload {
		return true
	}
	t.Stat = state.TorrentGettingInfo
	if t.WaitInfo() {
		t.Stat = state.TorrentWorking
		t.AddExpiredTime(time.Second * time.Duration(settings.BTsets.TorrentDisconnectTimeout))
		return true
	} else {
		t.Close()
		return false
	}
}

func (t *Torrent) AddExpiredTime(duration time.Duration) {
	newExpiredTime := time.Now().Add(duration)
	if t.expiredTime.Before(newExpiredTime) {
		t.expiredTime = newExpiredTime
	}
}

// UpdateStats is called periodically by the central BTServer ticker
func (t *Torrent) UpdateStats() {
	if t.expired() {
		if t.TorrentSpec != nil {
			log.TLogln("Torrent close by timeout", t.TorrentSpec.InfoHash.HexString())
			// V255: Snapshot peers to DB before drop. At expiry the swarm is fullest
			// (tracker/DHT have responded). Next Wake() injects these as Trusted peers,
			// skipping discovery delay.
			SaveTorrentToDB(t)
		}
		t.bt.RemoveTorrent(t.Hash())
		return
	}

	t.muTorrent.Lock()
	if t.Torrent != nil && t.Torrent.Info() != nil {
		st := t.Torrent.Stats()
		deltaDlBytes := st.BytesRead.Int64() - t.BytesReadUsefulData
		deltaUpBytes := st.BytesWritten.Int64() - t.BytesWrittenData
		deltaTime := time.Since(t.lastTimeSpeed).Seconds()

		t.DownloadSpeed = float64(deltaDlBytes) / deltaTime
		t.UploadSpeed = float64(deltaUpBytes) / deltaTime

		t.BytesReadUsefulData = st.BytesRead.Int64()
		t.BytesWrittenData = st.BytesWritten.Int64()

		if t.cache != nil {
			t.PreloadedBytes = t.cache.GetState().Filled
		}
	} else {
		t.DownloadSpeed = 0
		t.UploadSpeed = 0
	}
	t.muTorrent.Unlock()

	t.lastTimeSpeed = time.Now()
	t.updateRA()
}

func (t *Torrent) updateRA() {
	if t.cache == nil {
		return
	}
	// Calculate read-ahead from settings instead of hardcoded 16MB
	cacheSize := settings.BTsets.CacheSize
	if cacheSize == 0 {
		cacheSize = 64 << 20 // 64 MB default
	}
	readAheadPct := int64(settings.BTsets.ReaderReadAHead)
	if readAheadPct == 0 {
		readAheadPct = 95 // 95% default
	}
	adj := cacheSize * readAheadPct / 100
	if adj < 8<<20 {
		adj = 8 << 20 // minimum 8 MB
	}
	go t.cache.AdjustRA(adj)
}

func (t *Torrent) expired() bool {
	if t.IsPriority {
		return false // V185: Never expire if marked as high priority (active stream)
	}
	if t.Stat == state.TorrentGettingInfo || t.Stat == state.TorrentPreload {
		return false // Still working, don't expire
	}
	if !t.expiredTime.Before(time.Now()) {
		return false // Timer not expired yet
	}
	// V255: Torrent with no cache (GotInfo failed/never called) should also expire.
	// Previously cache==nil returned false, causing zombies in RAM forever.
	if t.cache == nil {
		return true
	}
	return t.cache.Readers() == 0
}

// SetAggressiveMode enables or disables aggressive download priority in the cache
func (t *Torrent) SetAggressiveMode(enabled bool, masterLimit int) {
	if t.cache != nil {
		t.cache.SetAggressive(enabled, masterLimit)
	}
}

func (t *Torrent) Files() []*torrent.File {
	if t.Torrent != nil && t.Torrent.Info() != nil {
		files := t.Torrent.Files()
		return files
	}
	return nil
}

func (t *Torrent) Hash() metainfo.Hash {
	if t.Torrent != nil {
		return t.Torrent.InfoHash()
	}
	if t.TorrentSpec != nil {
		return t.TorrentSpec.InfoHash
	}
	return [20]byte{}
}

func (t *Torrent) Length() int64 {
	if t.Info() == nil {
		return 0
	}
	return t.Torrent.Length()
}

func (t *Torrent) NewReader(file *torrent.File) *torrstor.Reader {
	// V244-Fix: Capture cache locally to avoid race where t.cache becomes nil after check
	cache := t.cache
	if t.Stat == state.TorrentClosed || cache == nil {
		return nil
	}
	reader := cache.NewReader(file)
	return reader
}

func (t *Torrent) CloseReader(reader *torrstor.Reader) {
	if t.cache != nil {
		t.cache.CloseReader(reader)
	}
	t.AddExpiredTime(time.Second * time.Duration(settings.BTsets.TorrentDisconnectTimeout))
}

func (t *Torrent) GetCache() *torrstor.Cache {
	return t.cache
}

func (t *Torrent) drop() {
	t.muTorrent.Lock()
	defer t.muTorrent.Unlock()
	if t.Torrent != nil {
		t.Torrent.Drop()
		t.Torrent = nil
	}
}

func (t *Torrent) Close() bool {
	if t == nil {
		return false
	}
	if settings.ReadOnly && t.cache != nil && t.cache.GetUseReaders() > 0 {
		return false
	}
	t.Stat = state.TorrentClosed

	if t.bt != nil {
		t.bt.mu.Lock()
		delete(t.bt.torrents, t.Hash())
		t.bt.mu.Unlock()
	}

	t.drop()
	return true
}

func (t *Torrent) Status() *state.TorrentStatus {
	st := new(state.TorrentStatus)

	t.muTorrent.Lock()
	st.Stat = t.Stat
	st.StatString = t.Stat.String()
	st.Title = t.Title
	st.Category = t.Category
	st.Poster = t.Poster
	st.Data = t.Data
	st.Timestamp = t.Timestamp
	st.TorrentSize = t.Size
	st.BitRate = t.BitRate
	st.DurationSeconds = t.DurationSeconds
	st.IsPriority = t.IsPriority // V186

	if t.TorrentSpec != nil {
		st.Hash = t.TorrentSpec.InfoHash.HexString()
	}

	torr := t.Torrent
	t.muTorrent.Unlock() // V227: Release early to prevent bottleneck during heavy stats/sort

	if torr != nil {
		st.Name = torr.Name()
		st.Hash = torr.InfoHash().HexString()
		st.LoadedSize = torr.BytesCompleted()

		st.PreloadedBytes = t.PreloadedBytes
		st.PreloadSize = t.PreloadSize
		st.DownloadSpeed = t.DownloadSpeed
		st.UploadSpeed = t.UploadSpeed

		tst := torr.Stats()
		st.BytesWritten = tst.BytesWritten.Int64()
		st.BytesWrittenData = tst.BytesWrittenData.Int64()
		st.BytesRead = tst.BytesRead.Int64()
		st.BytesReadData = tst.BytesReadData.Int64()
		st.BytesReadUsefulData = tst.BytesReadUsefulData.Int64()
		st.ChunksWritten = tst.ChunksWritten.Int64()
		st.ChunksRead = tst.ChunksRead.Int64()
		st.ChunksReadUseful = tst.ChunksReadUseful.Int64()
		st.ChunksReadWasted = tst.ChunksReadWasted.Int64()
		st.PiecesDirtiedGood = tst.PiecesDirtiedGood.Int64()
		st.PiecesDirtiedBad = tst.PiecesDirtiedBad.Int64()
		st.TotalPeers = tst.TotalPeers
		st.PendingPeers = tst.PendingPeers
		st.ActivePeers = tst.ActivePeers
		st.ConnectedSeeders = tst.ConnectedSeeders
		st.HalfOpenPeers = tst.HalfOpenPeers

		if torr.Info() != nil {
			st.TorrentSize = torr.Length()

			if t.cachedFileStats == nil {
				files := t.Files()
				sort.Slice(files, func(i, j int) bool {
					return utils2.CompareStrings(files[i].Path(), files[j].Path())
				})
				for i, f := range files {
					t.cachedFileStats = append(t.cachedFileStats, &state.TorrentFileStat{
						Id:     i + 1, // in web id 0 is undefined
						Path:   f.Path(),
						Length: f.Length(),
					})
				}
			}
			st.FileStats = t.cachedFileStats

			th := torrshash.New(st.Hash)
			th.AddField(torrshash.TagTitle, st.Title)
			th.AddField(torrshash.TagPoster, st.Poster)
			th.AddField(torrshash.TagCategory, st.Category)
			th.AddField(torrshash.TagSize, strconv.FormatInt(st.TorrentSize, 10))

			if t.TorrentSpec != nil {
				if len(t.TorrentSpec.Trackers) > 0 && len(t.TorrentSpec.Trackers[0]) > 0 {
					for _, tr := range t.TorrentSpec.Trackers[0] {
						th.AddField(torrshash.TagTracker, tr)
					}
				}
			}
			token, err := torrshash.Pack(th)
			if err == nil {
				st.TorrsHash = token
			}
		}
	}

	return st
}

func (t *Torrent) CacheState() *cacheSt.CacheState {
	if t.Torrent != nil && t.cache != nil {
		st := t.cache.GetState()
		st.Torrent = t.Status()
		return st
	}
	return nil
}

// V162-Optimization: Lightweight status for high-frequency polling (PeerPreloader)
// Avoids the massive overhead of Status() which locks for too long causing starvation
func (t *Torrent) StatHighFreq() *state.TorrentStatus {
	t.muTorrent.Lock()
	defer t.muTorrent.Unlock()

	st := new(state.TorrentStatus)
	st.Hash = t.TorrentSpec.InfoHash.HexString()

	// Only copy essential fields for PeerPreloader
	st.DownloadSpeed = t.DownloadSpeed
	st.IsPriority = t.IsPriority // V186

	if t.Torrent != nil {
		tst := t.Torrent.Stats()
		st.TotalPeers = tst.TotalPeers
		st.ActivePeers = tst.ActivePeers
		st.LoadedSize = t.Torrent.BytesCompleted()
	}

	return st
}
