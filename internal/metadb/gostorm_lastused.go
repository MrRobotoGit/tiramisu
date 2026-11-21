package metadb

import (
	"database/sql"
	"errors"
)

// Schema 17, additive: last_used_at is the last activation; timestamp keeps "added" for rows
// the new engine writes. Today's binary names its columns, so it reads and writes around it.
func (d *DB) execGoStormLastUsedSchema() error {
	if err := d.addColumn("gostorm_torrents", "last_used_at",
		`ALTER TABLE gostorm_torrents ADD COLUMN last_used_at INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	_, _ = d.db.Exec(`INSERT OR IGNORE INTO schema_version (version, description) VALUES (17, 'add gostorm_torrents.last_used_at')`)
	return nil
}

// SaveGoStormTorrent upserts t, keeping the stored timestamp of an existing row, and sets
// last_used_at.
func (d *DB) SaveGoStormTorrent(t GoStormTorrent, lastUsed int64) error {
	args := append(torrentArgs(t), lastUsed)
	_, err := d.db.Exec(`INSERT INTO gostorm_torrents (`+torrentColumns+`, last_used_at) VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(hash) DO UPDATE SET title=excluded.title, category=excluded.category,
		poster=excluded.poster, data=excluded.data, size=excluded.size, spec=excluded.spec,
		info_bytes=COALESCE(excluded.info_bytes, gostorm_torrents.info_bytes),
		last_used_at=excluded.last_used_at`, args...)
	return err
}

// GoStormLastUsed returns a row's last_used_at, 0 when absent or never set.
func (d *DB) GoStormLastUsed(hash string) (int64, error) {
	var v int64
	err := d.reader().QueryRow(`SELECT last_used_at FROM gostorm_torrents WHERE hash = ?`, hash).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}
