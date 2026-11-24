package engine

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"tiramisu/internal/engine/bt"
	"tiramisu/internal/metadb"
)

var errNoDB = errors.New("engine: no database")

// RegisterInfo stores a torrent row from info bytes without hydrating it; they must hash to hash.
// An existing row is kept: the same hash is the same content.
func (e *Engine) RegisterInfo(hash string, info []byte, title string, trackers [][]string) error {
	h, err := ParseHash(hash)
	if err != nil {
		return err
	}
	if sum := sha1.Sum(info); hex.EncodeToString(sum[:]) != h {
		return fmt.Errorf("engine: info bytes do not hash to %s", h)
	}
	if e.deps.DB == nil {
		return errNoDB
	}
	if has, err := e.deps.DB.HasGoStormInfo(h); err != nil || has {
		return err
	}
	files, err := infoFileIDs(info)
	if err != nil {
		return err
	}
	var doc filesDoc
	doc.GoStorm.Files = files
	data, _ := json.Marshal(doc)
	var size int64
	for _, f := range files {
		size += f.Length
	}
	spec, err := json.Marshal(storedSpec{Trackers: trackers, InfoHash: h})
	if err != nil {
		return err
	}
	now := e.now().Unix()
	err = e.deps.DB.SaveGoStormTorrent(metadb.GoStormTorrent{Hash: h, Title: title, Data: string(data), Timestamp: now,
		Size: size, Spec: spec, InfoBytes: info}, now)
	e.rows.invalidate(h)
	return err
}

// InfoBytes returns the stored metainfo of hash, nil when it has no row. It reads the DB around the
// row cache: thousands of library reads must not evict the rows playback uses.
func (e *Engine) InfoBytes(hash string) []byte {
	h, err := ParseHash(hash)
	if err != nil || e.deps.DB == nil {
		return nil
	}
	r, err := e.deps.DB.GetGoStormTorrent(h)
	if err != nil {
		e.deps.logf("[Tentacle] read %s: %v", h, err)
		return nil
	}
	if r == nil || len(r.InfoBytes) == 0 {
		return nil
	}
	return r.InfoBytes
}

// RowFiles returns the files of a stored row with their stream IDs, without hydrating.
func (e *Engine) RowFiles(hash string) ([]File, bool) {
	info := e.InfoBytes(hash)
	if info == nil {
		return nil, false
	}
	files, err := infoFileIDs(info)
	if err != nil {
		e.deps.logf("[Tentacle] files of %s: %v", hash, err)
		return nil, false
	}
	return files, true
}

// infoFileIDs numbers the files of info bytes as a hydration does.
func infoFileIDs(info []byte) ([]File, error) {
	files, _, _, err := bt.InfoFiles(info)
	if err != nil {
		return nil, err
	}
	return assignIDs(files), nil
}
