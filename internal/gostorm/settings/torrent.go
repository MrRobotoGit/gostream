package settings

import (
	"encoding/json"
	"sort"
	"sync"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

type TorrentDB struct {
	*torrent.TorrentSpec

	Title    string `json:"title,omitempty"`
	Category string `json:"category,omitempty"`
	Poster   string `json:"poster,omitempty"`
	Data     string `json:"data,omitempty"`

	Timestamp int64 `json:"timestamp,omitempty"`
	Size      int64 `json:"size,omitempty"`
}

var mu sync.Mutex

func AddTorrent(torr *TorrentDB) {
	// V270: Only write the single torrent, not the entire list.
	// Previously this read ALL torrents, then re-wrote ALL of them (O(N) BoltDB transactions).
	// With 520 torrents, that was 520 fsync calls per add (~5s on SD card).
	mu.Lock()
	buf, err := json.Marshal(torr)
	if err == nil {
		tdb.Set("Torrents", torr.InfoHash.HexString(), buf)
	}
	mu.Unlock()
}

func ListTorrent() []*TorrentDB {
	// Use read lock to prevent migration during read
	dbMigrationLock.RLock()
	defer dbMigrationLock.RUnlock()

	mu.Lock()
	defer mu.Unlock()

	var list []*TorrentDB
	keys := tdb.List("Torrents")
	for _, key := range keys {
		buf := tdb.Get("Torrents", key)
		if len(buf) > 0 {
			var torr *TorrentDB
			err := json.Unmarshal(buf, &torr)
			if err == nil {
				list = append(list, torr)
			}
		}
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Timestamp > list[j].Timestamp
	})
	return list
}

// V271: O(1) single torrent lookup by hash — reads one key from BoltDB.
// Previously GetTorrentDB called ListTorrent() which loaded and unmarshalled ALL torrents.
func GetTorrent(hash metainfo.Hash) *TorrentDB {
	mu.Lock()
	defer mu.Unlock()

	buf := tdb.Get("Torrents", hash.HexString())
	if len(buf) == 0 {
		return nil
	}
	var torr *TorrentDB
	if err := json.Unmarshal(buf, &torr); err != nil {
		return nil
	}
	return torr
}

func RemTorrent(hash metainfo.Hash) {
	mu.Lock()
	tdb.Rem("Torrents", hash.HexString())
	mu.Unlock()
}
