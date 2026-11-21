package engine

import (
	"encoding/json"

	"tiramisu/internal/metadb"
)

// saveLocked writes t's row: metainfo, the connected peers (when any), files JSON as data when
// the caller gave none. Needs t.life; a closed or replaced wrapper writes nothing.
func (e *Engine) saveLocked(t *Torrent) {
	if e.deps.DB == nil || t.bt == nil || !e.live(t) {
		return
	}
	info := t.bt.InfoBytes()
	if info == nil {
		return // never store a torrent without metadata
	}
	connected := t.bt.PeerAddrs()
	if len(connected) > 0 {
		t.mu.Lock()
		t.peers = connected
		t.mu.Unlock()
	}
	var spec storedSpec // an existing row keeps its other spec fields
	if r := e.row(t.hash); r != nil {
		spec = decodeSpec(r.Spec)
	}
	files := t.Files()
	t.mu.Lock()
	spec.Trackers, spec.InfoHash = t.tiers, t.hash
	if len(connected) > 0 || len(spec.PeerAddrs) == 0 {
		spec.PeerAddrs = t.peers
	}
	if t.meta.Data == "" {
		var doc filesDoc
		doc.GoStorm.Files = files
		b, _ := json.Marshal(doc)
		t.meta.Data = string(b)
	}
	row := metadb.GoStormTorrent{
		Hash: t.hash, Title: t.meta.Title, Category: t.meta.Category, Poster: t.meta.Poster,
		Data: t.meta.Data, Timestamp: t.added, Size: t.size, InfoBytes: info,
	}
	t.mu.Unlock()
	if row.Size == 0 {
		row.Size = t.bt.Length()
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		e.deps.logf("[Tentacle] save %s: %v", t.hash, err)
		return
	}
	row.Spec = specJSON
	err = e.deps.DB.SaveGoStormTorrent(row, e.now().Unix())
	e.rows.invalidate(t.hash)
	if err != nil {
		e.deps.logf("[Tentacle] save %s: %v", t.hash, err)
		return
	}
	t.mu.Lock()
	t.stored = true
	t.mu.Unlock()
}

// filesDoc is the data column written when the caller gave none.
type filesDoc struct {
	GoStorm struct {
		Files []File `json:"Files"`
	} `json:"GoStorm"`
}

// row reads one DB row through the cache; nil when absent or without a DB.
func (e *Engine) row(hash string) *metadb.GoStormTorrent {
	if e.deps.DB == nil {
		return nil
	}
	r, ok, gen := e.rows.get(hash)
	if ok {
		return r
	}
	r, err := e.deps.DB.GetGoStormTorrent(hash)
	if err != nil {
		e.deps.logf("[Tentacle] read %s: %v", hash, err)
		return nil
	}
	e.rows.put(hash, r, gen)
	return r
}
