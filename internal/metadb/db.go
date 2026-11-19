package metadb

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Logger interface for optional logging.
type Logger interface {
	Printf(format string, v ...interface{})
}

// DB wraps a SQLite database connection for tiramisu state persistence.
type DB struct {
	db       *sql.DB
	ro       *sql.DB // read-only pool for hot reads; nil falls back to db
	path     string
	logger   Logger
	readOnly bool // from OpenReadOnly: SELECT only, no checkpoint on close
}

// New opens or creates a SQLite database at dbPath, applies pragmas,
// creates tables, and returns a ready-to-use DB instance.
func New(dbPath string, logger Logger) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("metadb: create dir: %w", err)
	}
	// The DB holds the configuration and its secrets: created owner-only.
	if f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_WRONLY, 0600); err == nil {
		f.Close()
	}

	d, err := sql.Open("sqlite", dbPath+"?"+rwPragmas)
	if err != nil {
		return nil, fmt.Errorf("metadb: open: %w", err)
	}

	d.SetMaxOpenConns(1)

	db := &DB{
		db:     d,
		path:   dbPath,
		logger: logger,
	}

	if err := db.applyPragmas(); err != nil {
		d.Close()
		return nil, fmt.Errorf("metadb: pragmas: %w", err)
	}
	db.tightenPerms()

	if err := db.ExecSchema(); err != nil {
		d.Close()
		return nil, fmt.Errorf("metadb: schema: %w", err)
	}
	db.openReaders()

	return db, nil
}

// readConns is the size of the read pool: WAL lets readers run beside the one writer.
const readConns = 4

// openReaders opens the read-only pool. Without it every read waits behind the writer, as
// before, so a failure only logs.
func (d *DB) openReaders() {
	ro, err := sql.Open("sqlite", d.path+"?"+roPoolPragmas)
	if err == nil {
		err = ro.Ping()
	}
	if err != nil {
		if ro != nil {
			ro.Close()
		}
		if d.logger != nil {
			d.logger.Printf("[StateDB] WARN read pool unavailable, reads share the writer: %v", err)
		}
		return
	}
	ro.SetMaxOpenConns(readConns)
	ro.SetMaxIdleConns(readConns)
	d.ro = ro
}

// reader is the pool for reads that must not wait behind a write transaction.
func (d *DB) reader() *sql.DB {
	if d.ro != nil {
		return d.ro
	}
	return d.db
}

// Close performs a WAL checkpoint and closes the database connection. A handle from
// OpenReadOnly only closes: the checkpoint belongs to the engine that owns the DB.
func (d *DB) Close() error {
	if d.ro != nil {
		d.ro.Close() // a TRUNCATE checkpoint needs no open readers
	}
	if !d.readOnly {
		_, _ = d.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	}
	return d.db.Close()
}

// walSizeLimit is the WAL size kept after a checkpoint: readers on the pool can stop the
// WAL from restarting, and without a limit the file stays at its peak until restart.
const walSizeLimit = 64 << 20

// Checkpoint folds the WAL back into the DB and truncates it. A reader still on the WAL
// makes it busy: that is reported, since the result row, not the error, carries it.
func (d *DB) Checkpoint() error {
	var busy, logFrames, done int
	if err := d.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &done); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("metadb: checkpoint incomplete, a reader holds the WAL (%d of %d frames)", done, logFrames)
	}
	return nil
}

// Path is the DB file.
func (d *DB) Path() string { return d.path }

// hasColumn reports whether a table already carries a column.
func (d *DB) hasColumn(table, column string) bool {
	var n int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// addColumn applies an additive migration guarded by hasColumn. A lookup that fails for
// its own reasons answers "missing", so the ALTER is attempted on a table that already
// has the column: that duplicate is tolerated here, because the alternative is New()
// returning an error and tiramisu refusing to start.
func (d *DB) addColumn(table, column, ddl string) error {
	if d.hasColumn(table, column) {
		return nil
	}
	if _, err := d.db.Exec(ddl); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// SetLogger sets a custom logger for the database.
func (d *DB) SetLogger(l Logger) {
	if l != nil {
		d.logger = l
	}
}

// tightenPerms makes the DB and its WAL files owner-only; older installs created them 0644.
func (d *DB) tightenPerms() {
	for _, p := range []string{d.path, d.path + "-wal", d.path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() == 0600 {
			continue
		}
		if err := os.Chmod(p, 0600); err != nil && d.logger != nil {
			d.logger.Printf("[StateDB] WARN cannot restrict %s to 0600: %v", p, err)
		}
	}
}

// SQL returns the underlying *sql.DB for direct access (e.g. transactions).
func (d *DB) SQL() *sql.DB {
	return d.db
}

// Pragmas go in the DSN so the driver applies them to every connection it opens: set with
// Exec they held only on the first one, and a reopened connection silently lost them.
const (
	rwPragmas = "_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)" +
		"&_pragma=journal_size_limit(67108864)"
	roPragmas = "_pragma=busy_timeout(5000)"
	// The read pool: query_only makes any write on it fail, so the writer stays the only one.
	roPoolPragmas = "_pragma=busy_timeout(5000)&_pragma=query_only(1)"
)

// applyPragmas opens the first connection, so a pragma that fails is reported by New.
func (d *DB) applyPragmas() error {
	return d.db.Ping()
}

// ExecSchema creates all tables if they do not exist. Idempotent.
func (d *DB) ExecSchema() error {
	schema := `
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER PRIMARY KEY,
    applied_at TEXT DEFAULT (datetime('now')),
    description TEXT
);

CREATE TABLE IF NOT EXISTS inodes (
    type        TEXT NOT NULL CHECK(type IN ('file','dir')),
    infohash    TEXT,
    file_idx    INTEGER,
    full_path   TEXT UNIQUE,
    rel_path    TEXT UNIQUE,
    basename    TEXT,
    inode_value INTEGER NOT NULL,
    PRIMARY KEY (type, full_path)
);
CREATE INDEX IF NOT EXISTS idx_inodes_infohash ON inodes(infohash, file_idx);
CREATE INDEX IF NOT EXISTS idx_inodes_basename ON inodes(basename);

CREATE TABLE IF NOT EXISTS sync_caches (
    hash        TEXT NOT NULL,
    cache_type  TEXT NOT NULL CHECK(cache_type IN ('negative','fullpack')),
    title       TEXT,
    timestamp   TEXT NOT NULL,
    PRIMARY KEY (hash, cache_type)
);
CREATE INDEX IF NOT EXISTS idx_sync_type ON sync_caches(cache_type);

CREATE TABLE IF NOT EXISTS tv_episodes (
    episode_key   TEXT PRIMARY KEY,
    quality_score INTEGER NOT NULL,
    hash          TEXT NOT NULL,
    file_path     TEXT NOT NULL,
    source        TEXT NOT NULL,
    created       INTEGER NOT NULL,
    updated_at    TEXT DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_tv_hash ON tv_episodes(hash);

CREATE TABLE IF NOT EXISTS playback_states (
    path          TEXT PRIMARY KEY,
    hash          TEXT,
    imdb_id       TEXT,
    opened_at     TEXT,
    confirmed_at  TEXT,
    is_healthy    INTEGER DEFAULT 0,
    is_stopped    INTEGER DEFAULT 0,
    last_read_at  TEXT,
    read_count    INTEGER DEFAULT 0,
    last_seek_off INTEGER DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_playback_hash ON playback_states(hash);
CREATE INDEX IF NOT EXISTS idx_playback_healthy ON playback_states(is_healthy);

CREATE TABLE IF NOT EXISTS v304_bans (
    ip        TEXT PRIMARY KEY,
    banned_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS metadata_failures (
    hash       TEXT PRIMARY KEY,
    fail_count INTEGER NOT NULL DEFAULT 0,
    first_fail INTEGER NOT NULL,
    last_fail  INTEGER NOT NULL
);
`
	_, err := d.db.Exec(schema)
	if err != nil {
		return err
	}

	// V750: Register schema versions
	_, _ = d.db.Exec(`INSERT OR IGNORE INTO schema_version (version, description) VALUES (1, 'initial schema')`)
	_, _ = d.db.Exec(`INSERT OR IGNORE INTO schema_version (version, description) VALUES (2, 'add playback_states table')`)
	_, _ = d.db.Exec(`INSERT OR IGNORE INTO schema_version (version, description) VALUES (3, 'add v304_bans table')`)
	// 4 is taken on installations that ran the V754 webhook-position build, so this
	// lands on 5: INSERT OR IGNORE would otherwise drop it there and leave the
	// registry claiming two different things for the same version.
	_, _ = d.db.Exec(`INSERT OR IGNORE INTO schema_version (version, description) VALUES (5, 'add metadata_failures table')`)

	// Additive column: SQLite has no ADD COLUMN IF NOT EXISTS, so it is guarded by a
	// lookup instead. Without the show id an episode file cannot be traced back to
	// TMDB, which is what a re-search needs.
	if err := d.addColumn("tv_episodes", "show_imdb",
		`ALTER TABLE tv_episodes ADD COLUMN show_imdb TEXT DEFAULT ''`); err != nil {
		return err
	}
	_, _ = d.db.Exec(`INSERT OR IGNORE INTO schema_version (version, description) VALUES (6, 'add tv_episodes.show_imdb')`)

	// Episodes the reaper removed. Discovery returns a fraction of the library, so a
	// season it does not reach would keep the hole forever: this is what a later run
	// consults to try again.
	if _, err := d.db.Exec(`
		CREATE TABLE IF NOT EXISTS episode_gaps (
		    episode_key TEXT PRIMARY KEY,
		    show_imdb   TEXT DEFAULT '',
		    season      INTEGER NOT NULL,
		    file_path   TEXT NOT NULL,
		    dead_hash   TEXT NOT NULL,
		    removed_at  INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_gaps_season ON episode_gaps(show_imdb, season);`); err != nil {
		return err
	}
	_, _ = d.db.Exec(`INSERT OR IGNORE INTO schema_version (version, description) VALUES (7, 'add episode_gaps table')`)

	// last_attempt sends a gap to the back of the queue once it has been tried. Without
	// it a show whose name cannot be resolved is retried first on every run, and the
	// rest of the backlog is never reached.
	if err := d.addColumn("episode_gaps", "last_attempt",
		`ALTER TABLE episode_gaps ADD COLUMN last_attempt INTEGER DEFAULT 0`); err != nil {
		return err
	}
	_, _ = d.db.Exec(`INSERT OR IGNORE INTO schema_version (version, description) VALUES (8, 'add episode_gaps.last_attempt')`)

	// Schema 11, additive: a playback state remembers the audio projection's
	// caller-supplied identity, so a music webhook can match the session after a
	// restart the way an IMDb id matches a movie.
	if err := d.addColumn("playback_states", "external_id",
		`ALTER TABLE playback_states ADD COLUMN external_id TEXT DEFAULT ''`); err != nil {
		return err
	}
	if err := d.addColumn("playback_states", "external_id_ns",
		`ALTER TABLE playback_states ADD COLUMN external_id_ns TEXT DEFAULT ''`); err != nil {
		return err
	}
	_, _ = d.db.Exec(`INSERT OR IGNORE INTO schema_version (version, description) VALUES (11, 'add playback_states external identity')`)

	// The audio projection registry is authoritative for audio ownership, so it is
	// created here rather than lazily: a missing table would look like an empty
	// library and let cleanup drop torrents that are still projected.
	// Auth tables: additive, created on every start so upgraded installs have them.
	if err := d.execAuthSchema(); err != nil {
		return err
	}
	if err := d.execConfigSchema(); err != nil {
		return err
	}
	if err := d.execGoStormSchema(); err != nil {
		return err
	}
	if err := d.execGoStormVersionsSchema(); err != nil {
		return err
	}
	return d.execAudioSchema()
}

// OpenReadOnly opens an existing DB for reading next to a running engine: no directory,
// no pragmas, no schema. It is opened read-write on purpose: mode=ro fails on a stopped
// engine's DB that has no -wal/-shm yet. Callers only SELECT.
func OpenReadOnly(dbPath string) (*DB, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("metadb: %w", err)
	}
	d, err := sql.Open("sqlite", dbPath+"?"+roPragmas)
	if err != nil {
		return nil, fmt.Errorf("metadb: open: %w", err)
	}
	d.SetMaxOpenConns(1)
	if err := d.Ping(); err != nil {
		d.Close()
		return nil, fmt.Errorf("metadb: busy_timeout: %w", err)
	}
	return &DB{db: d, path: dbPath, readOnly: true}, nil
}
