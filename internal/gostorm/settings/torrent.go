package settings

import (
	"encoding/json"
	"errors"
	"sync"

	"tiramisu/internal/gostorm/log"
	"tiramisu/internal/metadb"

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

// torrentToRow moves the metainfo to its own column, so a list never reads it.
func torrentToRow(t *TorrentDB) (metadb.GoStormTorrent, error) {
	if t == nil || t.TorrentSpec == nil {
		return metadb.GoStormTorrent{}, errors.New("torrent without spec")
	}
	spec := *t.TorrentSpec
	info := spec.InfoBytes
	spec.InfoBytes = nil
	js, err := json.Marshal(&spec)
	if err != nil {
		return metadb.GoStormTorrent{}, err
	}
	return metadb.GoStormTorrent{
		Hash: spec.InfoHash.HexString(), Title: t.Title, Category: t.Category, Poster: t.Poster,
		Data: t.Data, Timestamp: t.Timestamp, Size: t.Size, Spec: js, InfoBytes: info,
	}, nil
}

func rowToTorrent(r metadb.GoStormTorrent) (*TorrentDB, error) {
	spec := new(torrent.TorrentSpec)
	if err := json.Unmarshal(r.Spec, spec); err != nil {
		return nil, err
	}
	spec.InfoBytes = r.InfoBytes
	return &TorrentDB{
		TorrentSpec: spec, Title: r.Title, Category: r.Category, Poster: r.Poster,
		Data: r.Data, Timestamp: r.Timestamp, Size: r.Size,
	}, nil
}

var (
	torrentCacheMu  sync.Mutex
	torrentCache    = newDataLRU(DataCacheLimit)
	torrentCacheGen uint64 // bumped by every write, under torrentCacheMu
)

func AddTorrent(torr *TorrentDB) {
	if ReadOnly || stateDB == nil {
		return
	}
	row, err := torrentToRow(torr)
	if err != nil {
		log.TLogln("Error add torrent:", err)
		return
	}
	if err := stateDB.PutGoStormTorrent(row); err != nil {
		log.TLogln("Error add torrent", row.Hash, ":", err)
	}
	invalidateTorrent(row.Hash)
}

// invalidateTorrent runs after a write: dropping the entry before it would let a concurrent
// GetTorrent re-cache the row it read earlier. The generation stops that put.
func invalidateTorrent(hash string) {
	torrentCacheMu.Lock()
	torrentCache.remove([2]string{"Torrents", hash})
	torrentCacheGen++
	torrentCacheMu.Unlock()
}

// ListTorrent returns all torrents, newest first, without InfoBytes: no list caller uses
// the metainfo, and reading it costs ~300 MB per call. GetTorrent carries it.
func ListTorrent() []*TorrentDB {
	dbMigrationLock.RLock()
	defer dbMigrationLock.RUnlock()
	if stateDB == nil {
		return nil
	}
	rows, err := stateDB.ListGoStormTorrents()
	if err != nil {
		log.TLogln("Error list torrents:", err)
		return nil
	}
	list := make([]*TorrentDB, 0, len(rows))
	for _, r := range rows {
		if t, err := rowToTorrent(r); err == nil {
			list = append(list, t)
		}
	}
	return list
}

// GetTorrent reads one torrent with its metainfo. PeekTorrent calls it from FUSE paths for
// every inactive torrent, so records (and misses) are cached as JSON, as DBReadCache did.
func GetTorrent(hash metainfo.Hash) *TorrentDB {
	if stateDB == nil {
		return nil
	}
	key := [2]string{"Torrents", hash.HexString()}
	torrentCacheMu.Lock()
	buf, hit := torrentCache.get(key)
	gen := torrentCacheGen
	torrentCacheMu.Unlock()
	if !hit {
		r, err := stateDB.GetGoStormTorrent(key[1])
		if err != nil {
			log.TLogln("Error get torrent", key[1], ":", err)
			return nil
		}
		if r != nil {
			t, err := rowToTorrent(*r)
			if err != nil {
				return nil
			}
			if buf, err = json.Marshal(t); err != nil {
				return t
			}
		}
		torrentCacheMu.Lock()
		if gen == torrentCacheGen { // no write since the read: safe to cache (nil caches the miss)
			torrentCache.put(key, buf)
		}
		torrentCacheMu.Unlock()
	}
	if buf == nil {
		return nil
	}
	var t *TorrentDB // decoded fresh each call: callers own their copy
	if err := json.Unmarshal(buf, &t); err != nil {
		return nil
	}
	return t
}

func RemTorrent(hash metainfo.Hash) {
	if ReadOnly || stateDB == nil {
		return
	}
	if err := stateDB.DeleteGoStormTorrent(hash.HexString()); err != nil {
		log.TLogln("Error rem torrent", hash.HexString(), ":", err)
	}
	invalidateTorrent(hash.HexString())
}
