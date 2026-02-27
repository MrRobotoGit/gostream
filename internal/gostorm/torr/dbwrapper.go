package torr

import (
	"encoding/json"

	"github.com/MrRobotoGit/gostream/internal/gostorm/settings"
	"github.com/MrRobotoGit/gostream/internal/gostorm/torr/state"
	"github.com/MrRobotoGit/gostream/internal/gostorm/torr/utils"

	"github.com/anacrolix/torrent/metainfo"
)

type tsFiles struct {
	GoStorm struct {
		Files []*state.TorrentFileStat `json:"Files"`
	} `json:"GoStorm"`
}

func AddTorrentDB(torr *Torrent) {
	t := new(settings.TorrentDB)
	t.TorrentSpec = torr.TorrentSpec
	t.Title = torr.Title
	t.Category = torr.Category
	if torr.Data == "" {
		files := new(tsFiles)
		files.GoStorm.Files = torr.Status().FileStats
		buf, err := json.Marshal(files)
		if err == nil {
			t.Data = string(buf)
			torr.Data = t.Data
		}
	} else {
		t.Data = torr.Data
	}
	if utils.CheckImgUrl(torr.Poster) {
		t.Poster = torr.Poster
	}
	t.Size = torr.Size
	if t.Size == 0 && torr.Torrent != nil {
		t.Size = torr.Torrent.Length()
	}
	// don't override timestamp from DB on edit
	t.Timestamp = torr.Timestamp // time.Now().Unix()

	settings.AddTorrent(t)
}

func GetTorrentDB(hash metainfo.Hash) *Torrent {
	// V271: O(1) direct lookup instead of loading all torrents
	db := settings.GetTorrent(hash)
	if db == nil {
		return nil
	}
	torr := new(Torrent)
	torr.TorrentSpec = db.TorrentSpec
	torr.Title = db.Title
	torr.Poster = db.Poster
	torr.Category = db.Category
	torr.Timestamp = db.Timestamp
	torr.Size = db.Size
	torr.Data = db.Data
	torr.Stat = state.TorrentInDB
	return torr
}

func RemTorrentDB(hash metainfo.Hash) {
	settings.RemTorrent(hash)
}

func ListTorrentsDB() map[metainfo.Hash]*Torrent {
	ret := make(map[metainfo.Hash]*Torrent)
	list := settings.ListTorrent()
	for _, db := range list {
		torr := new(Torrent)
		torr.TorrentSpec = db.TorrentSpec
		torr.Title = db.Title
		torr.Poster = db.Poster
		torr.Category = db.Category
		torr.Timestamp = db.Timestamp
		torr.Size = db.Size
		torr.Data = db.Data
		torr.Stat = state.TorrentInDB
		ret[torr.TorrentSpec.InfoHash] = torr
	}
	return ret
}
