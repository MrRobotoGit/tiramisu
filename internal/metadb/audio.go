package metadb

import (
	"database/sql"
	"errors"
	"fmt"
)

// AudioProjectionState is a projection row's lifecycle state. All three count as
// live references: a torrent dropped while a removing row survives would strand
// a reader that still holds the file open.
type AudioProjectionState string

const (
	AudioStaged    AudioProjectionState = "staged"
	AudioCommitted AudioProjectionState = "committed"
	AudioRemoving  AudioProjectionState = "removing"
)

// AudioProjection is one row of the audio projection registry, which is
// authoritative for audio ownership: filename parsing is not.
type AudioProjection struct {
	ID              int64
	Section         string
	VirtualPath     string
	PortablePathKey string
	Hash            string
	FileIndex       int
	SourcePath      string
	Size            int64
	MtimeNS         int64
	Title           string
	Magnet          string
	// Caller-supplied external identity and its namespace ("musicbrainz",
	// "asin", ...). Stored and returned verbatim, never interpreted: the engine
	// holds the identity, the controller resolves it. Both empty when absent.
	ExternalID          string
	ExternalIDNamespace string
	// A cue track of a single-file image: the projection serves Header followed by
	// the image bytes from ByteOffset. CueTrack 0 is a whole torrent file.
	CueTrack    int
	ByteOffset  int64
	Header      []byte
	State       AudioProjectionState
	TxnID       string
	StagingName string
	CreatedAtNS int64
	UpdatedAtNS int64
}

// The two conflicts are distinct because they become different API answers: a
// taken path is a 409 on the destination, a taken source is a 409 on the release.
var (
	ErrAudioPathConflict   = errors.New("audio projection path conflict")
	ErrAudioSourceConflict = errors.New("audio projection source conflict")
	// ErrAudioIdentityIncomplete rejects half an external identity. A namespace
	// alone identifies nothing, and an id without one cannot be resolved.
	ErrAudioIdentityIncomplete = errors.New("audio projection external identity incomplete")
)

// audioProjectionColumns is the read order every scan below relies on.
const audioProjectionColumns = `id, section, virtual_path, portable_path_key, hash, file_index,
	source_path, size, mtime_ns, title, COALESCE(magnet, ''), state,
	COALESCE(txn_id, ''), COALESCE(staging_name, ''), created_at_ns, updated_at_ns,
	COALESCE(external_id, ''), COALESCE(external_id_ns, ''),
	cue_track, byte_offset, COALESCE(header, x'')`

// execAudioSchema creates the registry. The source uniqueness carries no section on
// purpose: one torrent file, or one cue track of it, backs at most one projection.
func (d *DB) execAudioSchema() error {
	if _, err := d.db.Exec(`
CREATE TABLE IF NOT EXISTS audio_projections (
    id                INTEGER PRIMARY KEY,
    section           TEXT NOT NULL CHECK(section IN ('music', 'audiobooks')),
    virtual_path      TEXT NOT NULL,
    portable_path_key TEXT NOT NULL,
    hash              TEXT NOT NULL,
    file_index        INTEGER NOT NULL CHECK(file_index > 0),
    source_path       TEXT NOT NULL,
    size              INTEGER NOT NULL CHECK(size > 0),
    mtime_ns          INTEGER NOT NULL,
    title             TEXT NOT NULL,
    magnet            TEXT,
    state             TEXT NOT NULL CHECK(state IN ('staged', 'committed', 'removing')),
    txn_id            TEXT,
    staging_name      TEXT,
    created_at_ns     INTEGER NOT NULL,
    updated_at_ns     INTEGER NOT NULL,
    UNIQUE(section, virtual_path),
    UNIQUE(section, portable_path_key),
    UNIQUE(hash, file_index)
);
CREATE INDEX IF NOT EXISTS idx_audio_projections_hash ON audio_projections(hash);
CREATE INDEX IF NOT EXISTS idx_audio_projections_section_path ON audio_projections(section, virtual_path);
CREATE INDEX IF NOT EXISTS idx_audio_projections_state ON audio_projections(state);
CREATE INDEX IF NOT EXISTS idx_audio_projections_txn ON audio_projections(txn_id);`); err != nil {
		return err
	}
	_, _ = d.db.Exec(`INSERT OR IGNORE INTO schema_version (version, description) VALUES (9, 'add audio_projections table')`)

	// Schema 10, additive. A caller-supplied identity needs its namespace beside
	// it: audio has no single identity space the way video has IMDb, so the id
	// alone cannot be resolved. Both are engine-opaque.
	if err := d.addColumn("audio_projections", "external_id",
		`ALTER TABLE audio_projections ADD COLUMN external_id TEXT DEFAULT ''`); err != nil {
		return err
	}
	if err := d.addColumn("audio_projections", "external_id_ns",
		`ALTER TABLE audio_projections ADD COLUMN external_id_ns TEXT DEFAULT ''`); err != nil {
		return err
	}
	_, _ = d.db.Exec(`INSERT OR IGNORE INTO schema_version (version, description) VALUES (10, 'add audio_projections external identity')`)
	return d.migrateAudioCueTracks()
}

// migrateAudioCueTracks is schema 11: the tracks of a single-file image share one
// torrent file, so the source uniqueness gains the cue track. SQLite cannot alter a
// table constraint, so the table is rebuilt once, in one transaction. Only the
// audio registry is touched.
func (d *DB) migrateAudioCueTracks() error {
	if d.hasColumn("audio_projections", "cue_track") {
		return nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	const columns = `id, section, virtual_path, portable_path_key, hash, file_index, source_path,
	size, mtime_ns, title, magnet, state, txn_id, staging_name, created_at_ns, updated_at_ns,
	external_id, external_id_ns`
	for _, stmt := range []string{
		`CREATE TABLE audio_projections_v11 (
    id                INTEGER PRIMARY KEY,
    section           TEXT NOT NULL CHECK(section IN ('music', 'audiobooks')),
    virtual_path      TEXT NOT NULL,
    portable_path_key TEXT NOT NULL,
    hash              TEXT NOT NULL,
    file_index        INTEGER NOT NULL CHECK(file_index > 0),
    source_path       TEXT NOT NULL,
    size              INTEGER NOT NULL CHECK(size > 0),
    mtime_ns          INTEGER NOT NULL,
    title             TEXT NOT NULL,
    magnet            TEXT,
    state             TEXT NOT NULL CHECK(state IN ('staged', 'committed', 'removing')),
    txn_id            TEXT,
    staging_name      TEXT,
    created_at_ns     INTEGER NOT NULL,
    updated_at_ns     INTEGER NOT NULL,
    external_id       TEXT DEFAULT '',
    external_id_ns    TEXT DEFAULT '',
    cue_track         INTEGER NOT NULL DEFAULT 0 CHECK(cue_track >= 0),
    byte_offset       INTEGER NOT NULL DEFAULT 0 CHECK(byte_offset >= 0),
    header            BLOB,
    UNIQUE(section, virtual_path),
    UNIQUE(section, portable_path_key),
    UNIQUE(hash, file_index, cue_track)
)`,
		`INSERT INTO audio_projections_v11 (` + columns + `) SELECT ` + columns + ` FROM audio_projections`,
		`DROP TABLE audio_projections`,
		`ALTER TABLE audio_projections_v11 RENAME TO audio_projections`,
		`CREATE INDEX IF NOT EXISTS idx_audio_projections_hash ON audio_projections(hash)`,
		`CREATE INDEX IF NOT EXISTS idx_audio_projections_section_path ON audio_projections(section, virtual_path)`,
		`CREATE INDEX IF NOT EXISTS idx_audio_projections_state ON audio_projections(state)`,
		`CREATE INDEX IF NOT EXISTS idx_audio_projections_txn ON audio_projections(txn_id)`,
		`INSERT OR IGNORE INTO schema_version (version, description) VALUES (11, 'audio_projections cue tracks')`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("audio schema 11: %w", err)
		}
	}
	return tx.Commit()
}

// StageAudioProjections inserts every projection in one transaction, in state
// staged, all owned by txnID. The supplied State and TxnID are ignored: only a
// commit publishes a projection, and recovery groups rows by the request that
// staged them, so a row must never carry a transaction its batch did not use.
func (d *DB) StageAudioProjections(txnID string, ps []AudioProjection) error {
	if len(ps) == 0 {
		return nil
	}
	// Checked before the transaction opens: half an identity is a caller error,
	// not a conflict, and rejecting it after an insert would leave the batch
	// half written for a fault the caller could have been told about up front.
	for _, p := range ps {
		if (p.ExternalID == "") != (p.ExternalIDNamespace == "") {
			return fmt.Errorf("%w: %s/%s has id %q and namespace %q",
				ErrAudioIdentityIncomplete, p.Section, p.VirtualPath,
				p.ExternalID, p.ExternalIDNamespace)
		}
	}
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, p := range ps {
		_, err := tx.Exec(
			`INSERT INTO audio_projections
			 (section, virtual_path, portable_path_key, hash, file_index, source_path,
			  size, mtime_ns, title, magnet, state, txn_id, staging_name,
			  created_at_ns, updated_at_ns, external_id, external_id_ns,
			  cue_track, byte_offset, header)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.Section, p.VirtualPath, p.PortablePathKey, p.Hash, p.FileIndex, p.SourcePath,
			p.Size, p.MtimeNS, p.Title, p.Magnet, string(AudioStaged), txnID, p.StagingName,
			p.CreatedAtNS, p.UpdatedAtNS, p.ExternalID, p.ExternalIDNamespace,
			p.CueTrack, p.ByteOffset, p.Header,
		)
		if err != nil {
			return classifyAudioConflict(tx, p, err)
		}
	}
	return tx.Commit()
}

// classifyAudioConflict names the constraint an insert tripped. It reads through
// the same transaction, both so a duplicate earlier in the same batch is visible
// and because the pool holds a single connection: a query on d.db here would
// wait for a connection this transaction is still using.
func classifyAudioConflict(tx *sql.Tx, p AudioProjection, cause error) error {
	var n int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM audio_projections
		 WHERE section = ? AND (virtual_path = ? OR portable_path_key = ?)`,
		p.Section, p.VirtualPath, p.PortablePathKey,
	).Scan(&n); err == nil && n > 0 {
		return fmt.Errorf("%w: %s/%s: %v", ErrAudioPathConflict, p.Section, p.VirtualPath, cause)
	}
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM audio_projections WHERE hash = ? AND file_index = ? AND cue_track = ?`,
		p.Hash, p.FileIndex, p.CueTrack,
	).Scan(&n); err == nil && n > 0 {
		return fmt.Errorf("%w: %s:%d track %d: %v", ErrAudioSourceConflict, p.Hash, p.FileIndex, p.CueTrack, cause)
	}
	return cause
}

// CommitAudioProjections publishes the staged rows of one transaction.
func (d *DB) CommitAudioProjections(txnID string, updatedAtNS int64) (int, error) {
	return d.audioRowsAffected(
		`UPDATE audio_projections SET state = ?, updated_at_ns = ?
		 WHERE txn_id = ? AND state = ?`,
		string(AudioCommitted), updatedAtNS, txnID, string(AudioStaged))
}

// RollbackAudioProjections drops the staged rows of one transaction. Rows that
// already reached another state belong to a different decision and survive.
func (d *DB) RollbackAudioProjections(txnID string) (int, error) {
	return d.audioRowsAffected(
		`DELETE FROM audio_projections WHERE txn_id = ? AND state = ?`,
		txnID, string(AudioStaged))
}

func (d *DB) audioRowsAffected(query string, args ...interface{}) (int, error) {
	res, err := d.db.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func scanAudioProjection(row interface{ Scan(...any) error }) (*AudioProjection, error) {
	var p AudioProjection
	err := row.Scan(&p.ID, &p.Section, &p.VirtualPath, &p.PortablePathKey, &p.Hash,
		&p.FileIndex, &p.SourcePath, &p.Size, &p.MtimeNS, &p.Title, &p.Magnet,
		&p.State, &p.TxnID, &p.StagingName, &p.CreatedAtNS, &p.UpdatedAtNS,
		&p.ExternalID, &p.ExternalIDNamespace, &p.CueTrack, &p.ByteOffset, &p.Header)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (d *DB) audioProjectionRow(query string, args ...interface{}) (*AudioProjection, bool, error) {
	p, err := scanAudioProjection(d.db.QueryRow(query, args...))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return p, true, nil
}

func (d *DB) audioProjectionList(query string, args ...interface{}) ([]AudioProjection, error) {
	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AudioProjection
	for rows.Next() {
		p, err := scanAudioProjection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetAudioProjection returns the row owning a section-relative virtual path.
func (d *DB) GetAudioProjection(section, virtualPath string) (*AudioProjection, bool, error) {
	return d.audioProjectionRow(
		`SELECT `+audioProjectionColumns+` FROM audio_projections
		 WHERE section = ? AND virtual_path = ?`, section, virtualPath)
}

// AudioProjectionByPortableKey returns the row owning a section's portable key.
// That key has its own UNIQUE constraint, so virtual_path missing is not enough.
func (d *DB) AudioProjectionByPortableKey(section, portableKey string) (*AudioProjection, bool, error) {
	return d.audioProjectionRow(
		`SELECT `+audioProjectionColumns+` FROM audio_projections
		 WHERE section = ? AND portable_path_key = ?`, section, portableKey)
}

// AudioProjectionBySource returns the row owning a torrent file identity, or one cue
// track of it (cueTrack 0 is the whole file).
func (d *DB) AudioProjectionBySource(hash string, fileIndex, cueTrack int) (*AudioProjection, bool, error) {
	return d.audioProjectionRow(
		`SELECT `+audioProjectionColumns+` FROM audio_projections
		 WHERE hash = ? AND file_index = ? AND cue_track = ?`, hash, fileIndex, cueTrack)
}

// AudioProjectionsByHash returns every row backed by one torrent, in any state:
// one release stands behind many tracks, so a hash is never a single-row answer.
func (d *DB) AudioProjectionsByHash(hash string) ([]AudioProjection, error) {
	return d.audioProjectionList(
		`SELECT `+audioProjectionColumns+` FROM audio_projections
		 WHERE hash = ? ORDER BY section, virtual_path`, hash)
}

// AudioProjectionsByState returns every row in one state, which is how startup
// finds interrupted staged and removing work.
func (d *DB) AudioProjectionsByState(state AudioProjectionState) ([]AudioProjection, error) {
	return d.audioProjectionList(
		`SELECT `+audioProjectionColumns+` FROM audio_projections
		 WHERE state = ? ORDER BY section, virtual_path`, string(state))
}

// CommittedAudioProjections returns the published rows, ordered by virtual path.
// An empty section spans every section.
func (d *DB) CommittedAudioProjections(section string) ([]AudioProjection, error) {
	if section == "" {
		return d.audioProjectionList(
			`SELECT `+audioProjectionColumns+` FROM audio_projections
			 WHERE state = ? ORDER BY virtual_path`, string(AudioCommitted))
	}
	return d.audioProjectionList(
		`SELECT `+audioProjectionColumns+` FROM audio_projections
		 WHERE state = ? AND section = ? ORDER BY virtual_path`,
		string(AudioCommitted), section)
}

// MarkAudioProjectionRemoving starts removal of a committed projection. A staged
// row is not removable: its transaction is rolled back instead.
func (d *DB) MarkAudioProjectionRemoving(section, virtualPath string, updatedAtNS int64) (bool, error) {
	n, err := d.audioRowsAffected(
		`UPDATE audio_projections SET state = ?, updated_at_ns = ?
		 WHERE section = ? AND virtual_path = ? AND state = ?`,
		string(AudioRemoving), updatedAtNS, section, virtualPath, string(AudioCommitted))
	return n > 0, err
}

// DeleteAudioProjection drops a projection whatever its state.
func (d *DB) DeleteAudioProjection(section, virtualPath string) error {
	_, err := d.db.Exec(
		`DELETE FROM audio_projections WHERE section = ? AND virtual_path = ?`,
		section, virtualPath)
	return err
}

// AudioHashReferenced reports whether any projection still holds the torrent.
// Every stored state is a live reference, so existence is the whole answer.
func (d *DB) AudioHashReferenced(hash string) (bool, error) {
	var exists int
	err := d.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM audio_projections WHERE hash = ?)`, hash).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists == 1, nil
}
