package torr

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"gostream/internal/gostorm/log"
	sets "gostream/internal/gostorm/settings"
)

var (
	bts        *BTServer
	btsMu      sync.RWMutex
	settingsMu sync.Mutex // V227: Serialize SetSettings/SetDefSettings to prevent concurrent disconnect/connect
)

func InitApiHelper(bt *BTServer) {
	btsMu.Lock()
	defer btsMu.Unlock()
	bts = bt
}

func LoadTorrent(tor *Torrent) *Torrent {
	if tor.TorrentSpec == nil {
		return nil
	}
	btsMu.RLock()
	localBts := bts
	btsMu.RUnlock()

	tr, err := NewTorrent(tor.TorrentSpec, localBts)
	if err != nil {
		return nil
	}
	if !tr.WaitInfo() {
		return nil
	}
	tr.Title = tor.Title
	tr.Poster = tor.Poster
	tr.Data = tor.Data
	return tr
}

func AddTorrent(spec *torrent.TorrentSpec, title, poster string, data string, category string) (*Torrent, error) {
	// V255: Inject cached InfoBytes + PeerAddrs from DB to skip metadata re-fetch
	// and peer discovery. Magnet links always have InfoBytes=nil and PeerAddrs=nil.
	// If this torrent was previously saved, the DB has both — inject them so
	// GotInfo() fires immediately and peers connect without waiting for tracker/DHT.
	if cached := GetTorrentDB(spec.InfoHash); cached != nil && cached.TorrentSpec != nil {
		if len(spec.InfoBytes) == 0 && len(cached.TorrentSpec.InfoBytes) > 0 {
			spec.InfoBytes = cached.TorrentSpec.InfoBytes
		}
		if len(spec.PeerAddrs) == 0 && len(cached.TorrentSpec.PeerAddrs) > 0 {
			spec.PeerAddrs = cached.TorrentSpec.PeerAddrs
		}
	}

	btsMu.RLock()
	localBts := bts
	btsMu.RUnlock()

	torr, err := NewTorrent(spec, localBts)
	if err != nil {
		log.TLogln("error add torrent:", err)
		return nil, err
	}

	torDB := GetTorrentDB(spec.InfoHash)

	if torr.Title == "" {
		torr.Title = title
		if title == "" && torDB != nil {
			torr.Title = torDB.Title
		}
		if torr.Title == "" && torr.Torrent != nil && torr.Torrent.Info() != nil {
			torr.Title = torr.Info().Name
		}
	}

	if torr.Category == "" {
		torr.Category = category
		if torr.Category == "" && torDB != nil {
			torr.Category = torDB.Category
		}
	}

	if torr.Poster == "" {
		torr.Poster = poster
		if torr.Poster == "" && torDB != nil {
			torr.Poster = torDB.Poster
		}
	}

	if torr.Data == "" {
		torr.Data = data
		if torr.Data == "" && torDB != nil {
			torr.Data = torDB.Data
		}
	}

	return torr, nil
}

func SaveTorrentToDB(torr *Torrent) {
	// V255: Persist metadata InfoBytes to DB for instant re-activation.
	// Without this, TorrentSpec.InfoBytes stays nil (from original magnet parse),
	// forcing full metadata re-fetch from peers on every Wake() — adding 2-10s to TTFF.
	if torr.Torrent != nil && torr.Torrent.Info() != nil && len(torr.TorrentSpec.InfoBytes) == 0 {
		torr.TorrentSpec.InfoBytes = torr.Torrent.Metainfo().InfoBytes
	}
	// V255: Cache active peer addresses for instant reconnection on re-activation.
	// Same pattern as InfoBytes: PeerAddrs field exists in TorrentSpec but was never populated.
	// On next Wake(), AddTorrentSpec injects these as PeerSourceDirect/Trusted peers,
	// skipping the tracker/DHT discovery delay entirely.
	if torr.Torrent != nil {
		conns := torr.Torrent.PeerConns()
		if len(conns) > 0 {
			addrs := make([]string, 0, len(conns))
			for _, c := range conns {
				if addr := c.RemoteAddr.String(); addr != "" {
					addrs = append(addrs, addr)
				}
			}
			if len(addrs) > 0 {
				torr.TorrentSpec.PeerAddrs = addrs
			}
		}
	}
	log.TLogln("save to db:", torr.Hash())
	AddTorrentDB(torr)
}

// PeekTorrent gets a torrent from RAM or DB WITHOUT resetting its expiration timer (V301)
func PeekTorrent(hashHex string) *Torrent {
	btsMu.RLock()
	defer btsMu.RUnlock()

	hash := metainfo.NewHashFromHex(hashHex)
	tor := bts.GetTorrent(hash)
	if tor != nil {
		// Found in RAM, return as is (no AddExpiredTime)
		return tor
	}

	tr := GetTorrentDB(hash)
	return tr // Found in DB or nil
}

func GetTorrent(hashHex string) *Torrent {
	btsMu.RLock()
	defer btsMu.RUnlock()

	hash := metainfo.NewHashFromHex(hashHex)
	timeout := time.Second * time.Duration(sets.BTsets.TorrentDisconnectTimeout)
	if timeout > time.Minute {
		timeout = time.Minute
	}
	tor := bts.GetTorrent(hash)
	if tor != nil {
		tor.AddExpiredTime(timeout)
		return tor
	}

	tr := GetTorrentDB(hash)
	if tr != nil {
		tor = tr
		go func() {
			log.TLogln("New torrent", tor.Hash())

			btsMu.RLock()
			localBts := bts
			btsMu.RUnlock()

			tr, _ := NewTorrent(tor.TorrentSpec, localBts)
			if tr != nil {
				tr.Title = tor.Title
				tr.Poster = tor.Poster
				tr.Data = tor.Data
				tr.Size = tor.Size
				tr.Timestamp = tor.Timestamp
				tr.Category = tor.Category
				tr.GotInfo()
			}
		}()
	}
	return tor
}

func SetTorrent(hashHex, title, poster, category string, data string) *Torrent {
	btsMu.RLock()
	localBts := bts
	btsMu.RUnlock()

	hash := metainfo.NewHashFromHex(hashHex)
	torr := localBts.GetTorrent(hash)
	torrDb := GetTorrentDB(hash)

	if title == "" && torr == nil && torrDb != nil {
		torr = GetTorrent(hashHex)
		if torr != nil {
			torr.GotInfo()
			if torr.Torrent != nil && torr.Torrent.Info() != nil {
				title = torr.Info().Name
			}
		}
	}

	if torr != nil {
		if title == "" && torr.Torrent != nil && torr.Torrent.Info() != nil {
			title = torr.Info().Name
		}
		torr.Title = title
		torr.Poster = poster
		torr.Category = category
		if data != "" {
			torr.Data = data
		}
	}
	// update torrent data in DB
	if torrDb != nil {
		torrDb.Title = title
		torrDb.Poster = poster
		torrDb.Category = category
		if data != "" {
			torrDb.Data = data
		}
		AddTorrentDB(torrDb)
	}
	if torr != nil {
		return torr
	} else {
		return torrDb
	}
}

func RemTorrent(hashHex string) {
	if sets.ReadOnly {
		log.TLogln("API RemTorrent: Read-only DB mode!", hashHex)
		return
	}
	btsMu.RLock()
	defer btsMu.RUnlock()

	hash := metainfo.NewHashFromHex(hashHex)
	if bts.RemoveTorrent(hash) {
		if sets.BTsets.UseDisk && hashHex != "" && hashHex != "/" {
			name := filepath.Join(sets.BTsets.TorrentsSavePath, hashHex)
			ff, _ := os.ReadDir(name)
			for _, f := range ff {
				os.Remove(filepath.Join(name, f.Name()))
			}
			err := os.Remove(name)
			if err != nil {
				log.TLogln("Error remove cache:", err)
			}
		}
	}
	RemTorrentDB(hash)
}

func ListTorrent() []*Torrent {
	btsMu.RLock()
	localBts := bts
	btsMu.RUnlock()

	btlist := localBts.ListTorrents()
	dblist := ListTorrentsDB()

	for hash, t := range dblist {
		if _, ok := btlist[hash]; !ok {
			btlist[hash] = t
		}
	}
	var ret []*Torrent

	for _, t := range btlist {
		ret = append(ret, t)
	}

	sort.Slice(ret, func(i, j int) bool {
		if ret[i].Timestamp != ret[j].Timestamp {
			return ret[i].Timestamp > ret[j].Timestamp
		} else {
			return ret[i].Title > ret[j].Title
		}
	})

	return ret
}

func ListActiveTorrent() []*Torrent {
	btsMu.RLock()
	localBts := bts
	btsMu.RUnlock()

	btlist := localBts.ListTorrents()
	var ret []*Torrent

	for _, t := range btlist {
		ret = append(ret, t)
	}

	sort.Slice(ret, func(i, j int) bool {
		if ret[i].Timestamp != ret[j].Timestamp {
			return ret[i].Timestamp > ret[j].Timestamp
		} else {
			return ret[i].Title > ret[j].Title
		}
	})

	return ret
}

func DropTorrent(hashHex string) {
	btsMu.RLock()
	defer btsMu.RUnlock()

	hash := metainfo.NewHashFromHex(hashHex)
	bts.RemoveTorrent(hash)
}

func SetSettings(set *sets.BTSets) {
	if sets.ReadOnly {
		log.TLogln("API SetSettings: Read-only DB mode!")
		return
	}
	// V227: Serialize — prevent concurrent SetSettings from interleaving disconnect/connect
	settingsMu.Lock()
	defer settingsMu.Unlock()

	sets.SetBTSets(set)
	log.TLogln("drop all torrents")
	dropAllTorrent()
	time.Sleep(time.Millisecond * 200)
	log.TLogln("disconect")

	btsMu.Lock()
	bts.Disconnect()
	btsMu.Unlock()

	log.TLogln("connect")

	// Connect() calls InitApiHelper() which acquires btsMu.Lock() internally.
	// Do NOT wrap in btsMu.Lock here — RWMutex is not reentrant → deadlock.
	bts.Connect()

	time.Sleep(time.Millisecond * 200)
	log.TLogln("end set settings")
}

func SetDefSettings() {
	if sets.ReadOnly {
		log.TLogln("API SetDefSettings: Read-only DB mode!")
		return
	}
	// V227: Serialize — prevent concurrent settings changes
	settingsMu.Lock()
	defer settingsMu.Unlock()

	sets.SetDefaultConfig()
	log.TLogln("drop all torrents")
	dropAllTorrent()
	time.Sleep(time.Millisecond * 200)
	log.TLogln("disconect")

	btsMu.Lock()
	bts.Disconnect()
	btsMu.Unlock()

	log.TLogln("connect")

	// Connect() calls InitApiHelper() which acquires btsMu.Lock() internally.
	// Do NOT wrap in btsMu.Lock here — RWMutex is not reentrant → deadlock.
	bts.Connect()

	time.Sleep(time.Millisecond * 200)
	log.TLogln("end set default settings")
}

func dropAllTorrent() {
	btsMu.RLock()
	localBts := bts
	btsMu.RUnlock()

	for _, torr := range localBts.ListTorrents() {
		torr.drop()
		<-torr.closed
	}
}

func Shutdown() {
	btsMu.Lock()
	bts.Disconnect()
	btsMu.Unlock()

	sets.CloseDB()
	log.TLogln("Received shutdown. Quit")
	os.Exit(0)
}

func WriteStatus(w io.Writer) {
	btsMu.RLock()
	defer btsMu.RUnlock()
	bts.client.WriteStatus(w)
}

func Preload(torr *Torrent, index int) {
	cache := float32(sets.BTsets.CacheSize)
	preload := float32(sets.BTsets.PreloadCache)
	size := int64((cache / 100.0) * preload)
	if size <= 0 {
		return
	}
	if size > sets.BTsets.CacheSize {
		size = sets.BTsets.CacheSize
	}
	// V239: Leak Plumbing - Use context with timeout for API preloads
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	torr.Preload(ctx, index, size)
}
