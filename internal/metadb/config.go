package metadb

import (
	"database/sql"
	"errors"
)

// The configuration, one JSON document per saved version. Append-only: the newest row is
// the configuration in use, older rows are the history.
const configSchema = `
CREATE TABLE IF NOT EXISTS config_versions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    json        TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    source      TEXT    NOT NULL,
    actor       TEXT    NOT NULL DEFAULT '',
    note        TEXT    NOT NULL DEFAULT '',
    file_sha256 TEXT    NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS config_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);`

// ConfigVersion is one saved configuration.
type ConfigVersion struct {
	ID        int64  `json:"id"`
	JSON      []byte `json:"-"`
	CreatedAt int64  `json:"created_at"`
	Source    string `json:"source"` // import | ui | api | internal | cli | rollback | default
	Actor     string `json:"actor"`
	Note      string `json:"note"`
	FileSHA   string `json:"-"`
}

func (d *DB) execConfigSchema() error {
	if _, err := d.db.Exec(configSchema); err != nil {
		return err
	}
	_, _ = d.db.Exec(`INSERT OR IGNORE INTO schema_version (version, description) VALUES (14, 'add config_versions and config_meta')`)
	return nil
}

// InsertConfigVersion appends a version and keeps the newest `keep`, in one transaction.
func (d *DB) InsertConfigVersion(v ConfigVersion, keep int) (int64, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO config_versions (json, created_at, source, actor, note, file_sha256) VALUES (?, ?, ?, ?, ?, ?)`,
		string(v.JSON), v.CreatedAt, v.Source, v.Actor, v.Note, v.FileSHA)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if keep > 0 {
		if _, err := tx.Exec(`DELETE FROM config_versions WHERE id NOT IN (SELECT id FROM config_versions ORDER BY id DESC LIMIT ?)`, keep); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

const configCols = `id, json, created_at, source, actor, note, file_sha256`

func scanConfigVersion(row interface{ Scan(...any) error }) (ConfigVersion, error) {
	var v ConfigVersion
	var js string
	err := row.Scan(&v.ID, &js, &v.CreatedAt, &v.Source, &v.Actor, &v.Note, &v.FileSHA)
	v.JSON = []byte(js)
	return v, err
}

// LatestConfigVersion is the configuration in use; ok is false on an empty table.
func (d *DB) LatestConfigVersion() (ConfigVersion, bool, error) {
	v, err := scanConfigVersion(d.db.QueryRow(`SELECT ` + configCols + ` FROM config_versions ORDER BY id DESC LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return v, false, nil
	}
	return v, err == nil, err
}

// ConfigVersion returns one version by id.
func (d *DB) ConfigVersion(id int64) (ConfigVersion, bool, error) {
	v, err := scanConfigVersion(d.db.QueryRow(`SELECT `+configCols+` FROM config_versions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return v, false, nil
	}
	return v, err == nil, err
}

// ListConfigVersions returns up to limit versions, newest first.
func (d *DB) ListConfigVersions(limit int) ([]ConfigVersion, error) {
	rows, err := d.db.Query(`SELECT `+configCols+` FROM config_versions ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConfigVersion
	for rows.Next() {
		v, err := scanConfigVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ConfigMeta reads a value that survives version pruning (e.g. the last imported file's hash).
func (d *DB) ConfigMeta(key string) (string, bool, error) {
	var v string
	err := d.db.QueryRow(`SELECT value FROM config_meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// SetConfigMeta writes or replaces a meta value.
func (d *DB) SetConfigMeta(key, value string) error {
	_, err := d.db.Exec(`INSERT INTO config_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
