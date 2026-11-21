package metadb

import (
	"database/sql"
	"errors"
)

// Tentacle settings, one JSON document per manual save. Append-only, like
// config_versions; the settings in use are BTsets, not the newest row.
const gostormVersionsSchema = `
CREATE TABLE IF NOT EXISTS gostorm_settings_versions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    json       TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    source     TEXT    NOT NULL,
    actor      TEXT    NOT NULL DEFAULT '',
    note       TEXT    NOT NULL DEFAULT ''
);`

func (d *DB) execGoStormVersionsSchema() error {
	if _, err := d.db.Exec(gostormVersionsSchema); err != nil {
		return err
	}
	_, _ = d.db.Exec(`INSERT OR IGNORE INTO schema_version (version, description) VALUES (16, 'add gostorm_settings_versions')`)
	return nil
}

// InsertGoStormSettingsVersion appends a version and keeps the newest `keep`, in one transaction.
func (d *DB) InsertGoStormSettingsVersion(v ConfigVersion, keep int) (int64, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO gostorm_settings_versions (json, created_at, source, actor, note) VALUES (?, ?, ?, ?, ?)`,
		string(v.JSON), v.CreatedAt, v.Source, v.Actor, v.Note)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if keep > 0 {
		if _, err := tx.Exec(`DELETE FROM gostorm_settings_versions WHERE id NOT IN
			(SELECT id FROM gostorm_settings_versions ORDER BY id DESC LIMIT ?)`, keep); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

const gostormVersionCols = `id, json, created_at, source, actor, note`

func scanGoStormVersion(row interface{ Scan(...any) error }) (ConfigVersion, error) {
	var v ConfigVersion
	var js string
	err := row.Scan(&v.ID, &js, &v.CreatedAt, &v.Source, &v.Actor, &v.Note)
	v.JSON = []byte(js)
	return v, err
}

// LatestGoStormSettingsVersion is the newest version; ok is false on an empty table.
func (d *DB) LatestGoStormSettingsVersion() (ConfigVersion, bool, error) {
	v, err := scanGoStormVersion(d.db.QueryRow(`SELECT ` + gostormVersionCols + ` FROM gostorm_settings_versions ORDER BY id DESC LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return v, false, nil
	}
	return v, err == nil, err
}

// GoStormSettingsVersion returns one version by id.
func (d *DB) GoStormSettingsVersion(id int64) (ConfigVersion, bool, error) {
	v, err := scanGoStormVersion(d.db.QueryRow(`SELECT `+gostormVersionCols+` FROM gostorm_settings_versions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return v, false, nil
	}
	return v, err == nil, err
}

// ListGoStormSettingsVersions returns up to limit versions, newest first.
func (d *DB) ListGoStormSettingsVersions(limit int) ([]ConfigVersion, error) {
	rows, err := d.db.Query(`SELECT `+gostormVersionCols+` FROM gostorm_settings_versions ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConfigVersion
	for rows.Next() {
		v, err := scanGoStormVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
