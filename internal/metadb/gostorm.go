package metadb

import (
	"database/sql"
	"errors"
	"fmt"
)

// Tentacle's torrent list and settings, formerly in BBolt (config.db). The metainfo has its
// own column so that listing never reads it.
const gostormSchema = `
CREATE TABLE IF NOT EXISTS gostorm_torrents (
    hash       TEXT PRIMARY KEY,
    title      TEXT    NOT NULL DEFAULT '',
    category   TEXT    NOT NULL DEFAULT '',
    poster     TEXT    NOT NULL DEFAULT '',
    data       TEXT    NOT NULL DEFAULT '',
    timestamp  INTEGER NOT NULL DEFAULT 0,
    size       INTEGER NOT NULL DEFAULT 0,
    spec       TEXT    NOT NULL,
    info_bytes BLOB
);
CREATE INDEX IF NOT EXISTS idx_gostorm_torrents_ts ON gostorm_torrents(timestamp);
CREATE TABLE IF NOT EXISTS gostorm_kv (
    xpath TEXT NOT NULL,
    name  TEXT NOT NULL,
    value BLOB NOT NULL,
    PRIMARY KEY (xpath, name)
);`

// GoStormTorrent is one row of gostorm_torrents. Spec is the TorrentSpec JSON without
// InfoBytes; InfoBytes is nil when not resolved yet, and always nil from a list.
type GoStormTorrent struct {
	Hash      string
	Title     string
	Category  string
	Poster    string
	Data      string
	Timestamp int64
	Size      int64
	Spec      []byte
	InfoBytes []byte
}

const torrentColumns = `hash, title, category, poster, data, timestamp, size, spec, info_bytes`

func (d *DB) execGoStormSchema() error {
	if _, err := d.db.Exec(gostormSchema); err != nil {
		return err
	}
	_, _ = d.db.Exec(`INSERT OR IGNORE INTO schema_version (version, description) VALUES (15, 'add gostorm_torrents and gostorm_kv')`)
	return d.execGoStormLastUsedSchema()
}

func torrentArgs(t GoStormTorrent) []any {
	var info any // empty metainfo is stored as NULL
	if len(t.InfoBytes) > 0 {
		info = t.InfoBytes
	}
	return []any{t.Hash, t.Title, t.Category, t.Poster, t.Data, t.Timestamp, t.Size, string(t.Spec), info}
}

// PutGoStormTorrent inserts or replaces a torrent.
func (d *DB) PutGoStormTorrent(t GoStormTorrent) error {
	_, err := d.db.Exec(`INSERT INTO gostorm_torrents (`+torrentColumns+`) VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(hash) DO UPDATE SET title=excluded.title, category=excluded.category,
		poster=excluded.poster, data=excluded.data, timestamp=excluded.timestamp, size=excluded.size,
		spec=excluded.spec, info_bytes=excluded.info_bytes`, torrentArgs(t)...)
	return err
}

// GetGoStormTorrent reads one torrent with its metainfo; nil when absent.
func (d *DB) GetGoStormTorrent(hash string) (*GoStormTorrent, error) {
	var t GoStormTorrent
	var spec string
	err := d.reader().QueryRow(`SELECT `+torrentColumns+` FROM gostorm_torrents WHERE hash = ?`, hash).
		Scan(&t.Hash, &t.Title, &t.Category, &t.Poster, &t.Data, &t.Timestamp, &t.Size, &spec, &t.InfoBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.Spec = []byte(spec)
	if len(t.InfoBytes) == 0 {
		t.InfoBytes = nil
	}
	return &t, nil
}

// ListGoStormTorrents returns every torrent, newest first, without the metainfo.
func (d *DB) ListGoStormTorrents() ([]GoStormTorrent, error) {
	rows, err := d.reader().Query(`SELECT hash, title, category, poster, data, timestamp, size, spec
		FROM gostorm_torrents ORDER BY timestamp DESC, hash`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GoStormTorrent
	for rows.Next() {
		var t GoStormTorrent
		var spec string
		if err := rows.Scan(&t.Hash, &t.Title, &t.Category, &t.Poster, &t.Data, &t.Timestamp, &t.Size, &spec); err != nil {
			return nil, err
		}
		t.Spec = []byte(spec)
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteGoStormTorrent removes a torrent; absent is not an error.
// DeleteGoStormTorrent removes a torrent and its metadata failure counter in one
// transaction: a counter left behind would condemn the same release if it came back.
func (d *DB) DeleteGoStormTorrent(hash string) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM gostorm_torrents WHERE hash = ?`, hash); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM metadata_failures WHERE lower(hash) = lower(?)`, hash); err != nil {
		return err
	}
	return tx.Commit()
}

// GoStormKVGet returns nil when the key is absent.
func (d *DB) GoStormKVGet(xpath, name string) ([]byte, error) {
	var v []byte
	err := d.db.QueryRow(`SELECT value FROM gostorm_kv WHERE xpath = ? AND name = ?`, xpath, name).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return v, err
}

func (d *DB) GoStormKVSet(xpath, name string, value []byte) error {
	if value == nil {
		value = []byte{}
	}
	_, err := d.db.Exec(`INSERT INTO gostorm_kv (xpath, name, value) VALUES (?,?,?)
		ON CONFLICT(xpath, name) DO UPDATE SET value=excluded.value`, xpath, name, value)
	return err
}

func (d *DB) GoStormKVList(xpath string) ([]string, error) {
	rows, err := d.db.Query(`SELECT name FROM gostorm_kv WHERE xpath = ? ORDER BY name`, xpath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (d *DB) GoStormKVDelete(xpath, name string) error {
	_, err := d.db.Exec(`DELETE FROM gostorm_kv WHERE xpath = ? AND name = ?`, xpath, name)
	return err
}

func (d *DB) GoStormKVClear(xpath string) error {
	_, err := d.db.Exec(`DELETE FROM gostorm_kv WHERE xpath = ?`, xpath)
	return err
}

// GoStormImporter writes inside the import transaction; rows already present win.
type GoStormImporter struct {
	torrent, kv *sql.Stmt
}

// PutTorrent reports whether the row was new.
func (im *GoStormImporter) PutTorrent(t GoStormTorrent) (bool, error) {
	return inserted(im.torrent.Exec(torrentArgs(t)...))
}

func (im *GoStormImporter) PutKV(xpath, name string, value []byte) (bool, error) {
	if value == nil {
		value = []byte{}
	}
	return inserted(im.kv.Exec(xpath, name, value))
}

func inserted(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ImportGoStorm runs fill in one transaction. An error or a panic rolls everything back:
// the DB has a single connection, so a transaction left open would block the process.
func (d *DB) ImportGoStorm(fill func(*GoStormImporter) error) (err error) {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("import panic: %v", r)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	tStmt, err := tx.Prepare(`INSERT OR IGNORE INTO gostorm_torrents (` + torrentColumns + `) VALUES (?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer tStmt.Close()
	kStmt, err := tx.Prepare(`INSERT OR IGNORE INTO gostorm_kv (xpath, name, value) VALUES (?,?,?)`)
	if err != nil {
		return err
	}
	defer kStmt.Close()
	if err = fill(&GoStormImporter{torrent: tStmt, kv: kStmt}); err != nil {
		return err
	}
	return tx.Commit()
}
