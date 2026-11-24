package metadb

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
)

const friendLibSchema = `
CREATE TABLE IF NOT EXISTS friend_library (
    name       TEXT PRIMARY KEY,
    side       TEXT NOT NULL,
    state      TEXT NOT NULL,
    nonce      TEXT NOT NULL DEFAULT '',
    expires_at INTEGER NOT NULL DEFAULT 0,
    cursor     INTEGER NOT NULL DEFAULT 0,
    left_count INTEGER NOT NULL DEFAULT -1,
    since      INTEGER NOT NULL DEFAULT 0,
    aligned_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS library_feed (
    path  TEXT PRIMARY KEY,
    seq   INTEGER NOT NULL UNIQUE,
    del   INTEGER NOT NULL DEFAULT 0,
    entry TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS library_mirror (
    path   TEXT PRIMARY KEY,
    seq    INTEGER NOT NULL,
    state  TEXT NOT NULL,
    entry  TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    at     INTEGER NOT NULL,
    sess   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_library_mirror_at ON library_mirror(at);`

// settingLibrarySeq is the feed counter in friend_settings: it never goes back, not even on ClearFeed.
const settingLibrarySeq = "library_seq"

func (d *DB) execFriendLibSchema() error {
	if _, err := d.db.Exec(friendLibSchema); err != nil {
		return err
	}
	_, _ = d.db.Exec(`INSERT OR IGNORE INTO schema_version (version, description) VALUES (20, 'add friend library roles, feed and mirror')`)
	return nil
}

// FriendLibrary is a library role with a friend. Side is ours: "master" (we send) or "slave" (we get).
type FriendLibrary struct {
	Name, Side, State, Nonce string
	ExpiresAt, Cursor        int64
	LeftCount                int64 // slave side: entries still to receive, -1 unknown
	Since, AlignedAt         int64
	UpdatedAt                int64
}

const friendLibraryCols = `name, side, state, nonce, expires_at, cursor, left_count, since, aligned_at, updated_at`

func scanFriendLibrary(row interface{ Scan(...any) error }) (FriendLibrary, error) {
	var r FriendLibrary
	err := row.Scan(&r.Name, &r.Side, &r.State, &r.Nonce, &r.ExpiresAt, &r.Cursor, &r.LeftCount, &r.Since, &r.AlignedAt, &r.UpdatedAt)
	return r, err
}

// PutFriendLibrary inserts or replaces the role with r.Name.
func (d *DB) PutFriendLibrary(r FriendLibrary) error {
	_, err := d.db.Exec(`INSERT OR REPLACE INTO friend_library (`+friendLibraryCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Name, r.Side, r.State, r.Nonce, r.ExpiresAt, r.Cursor, r.LeftCount, r.Since, r.AlignedAt, r.UpdatedAt)
	return err
}

func (d *DB) GetFriendLibrary(name string) (FriendLibrary, bool, error) {
	r, err := scanFriendLibrary(d.db.QueryRow(`SELECT `+friendLibraryCols+` FROM friend_library WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return FriendLibrary{}, false, nil
	}
	return r, err == nil, err
}

// ListFriendLibrary returns every role, ordered by friend name.
func (d *DB) ListFriendLibrary() ([]FriendLibrary, error) {
	rows, err := d.db.Query(`SELECT ` + friendLibraryCols + ` FROM friend_library ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FriendLibrary
	for rows.Next() {
		r, err := scanFriendLibrary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) SetFriendLibraryState(name, state string, updated int64) error {
	_, err := d.db.Exec(`UPDATE friend_library SET state = ?, updated_at = ? WHERE name = ?`, state, updated, name)
	return err
}

func (d *DB) SetFriendLibraryCursor(name string, cursor, left, updated int64) error {
	_, err := d.db.Exec(`UPDATE friend_library SET cursor = ?, left_count = ?, updated_at = ? WHERE name = ?`, cursor, left, updated, name)
	return err
}

func (d *DB) SetFriendLibraryAligned(name string, at int64) error {
	_, err := d.db.Exec(`UPDATE friend_library SET aligned_at = ? WHERE name = ?`, at, name)
	return err
}

func (d *DB) DeleteFriendLibrary(name string) error {
	_, err := d.db.Exec(`DELETE FROM friend_library WHERE name = ?`, name)
	return err
}

// FeedRow is one path of the master's library feed; Entry is its JSON.
type FeedRow struct {
	Path  string
	Seq   int64
	Del   bool
	Entry string
}

// PutFeedEntry gives path a new seq from the counter, in the same transaction as the row.
func (d *DB) PutFeedEntry(path string, del bool, entry string) (int64, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	seq, err := feedHead(tx)
	if err != nil {
		return 0, err
	}
	seq++
	if _, err := tx.Exec(`INSERT INTO friend_settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		settingLibrarySeq, strconv.FormatInt(seq, 10)); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO library_feed (path, seq, del, entry) VALUES (?, ?, ?, ?)`, path, seq, del, entry); err != nil {
		return 0, err
	}
	return seq, tx.Commit()
}

func feedHead(q interface {
	QueryRow(string, ...any) *sql.Row
}) (int64, error) {
	var v string
	err := q.QueryRow(`SELECT value FROM friend_settings WHERE key = ?`, settingLibrarySeq).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

// FeedHead is the last seq handed out.
func (d *DB) FeedHead() (int64, error) { return feedHead(d.db) }

func (d *DB) GetFeedEntry(path string) (FeedRow, bool, error) {
	var r FeedRow
	err := d.db.QueryRow(`SELECT path, seq, del, entry FROM library_feed WHERE path = ?`, path).Scan(&r.Path, &r.Seq, &r.Del, &r.Entry)
	if errors.Is(err, sql.ErrNoRows) {
		return FeedRow{}, false, nil
	}
	return r, err == nil, err
}

// FeedScan visits the rows with seq > cursor in seq order until visit returns false.
func (d *DB) FeedScan(cursor int64, skipDeletes bool, visit func(FeedRow) bool) error {
	q := `SELECT path, seq, del, entry FROM library_feed WHERE seq > ?`
	if skipDeletes {
		q += ` AND del = 0`
	}
	rows, err := d.db.Query(q+` ORDER BY seq`, cursor)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r FeedRow
		if err := rows.Scan(&r.Path, &r.Seq, &r.Del, &r.Entry); err != nil {
			return err
		}
		if !visit(r) {
			break
		}
	}
	return rows.Err()
}

// FeedSince returns up to limit rows with seq > cursor in seq order (limit < 0: all).
func (d *DB) FeedSince(cursor int64, skipDeletes bool, limit int) ([]FeedRow, error) {
	var out []FeedRow
	err := d.FeedScan(cursor, skipDeletes, func(r FeedRow) bool {
		out = append(out, r)
		return limit < 0 || len(out) < limit
	})
	return out, err
}

// FeedCount counts the rows with seq > afterSeq, live ones only with live.
func (d *DB) FeedCount(live bool, afterSeq int64) (int64, error) {
	q := `SELECT COUNT(*) FROM library_feed WHERE seq > ?`
	if live {
		q += ` AND del = 0`
	}
	var n int64
	err := d.db.QueryRow(q, afterSeq).Scan(&n)
	return n, err
}

// PruneFeedDeletes drops the deletions every slave has confirmed (seq <= upTo).
func (d *DB) PruneFeedDeletes(upTo int64) error {
	_, err := d.db.Exec(`DELETE FROM library_feed WHERE del = 1 AND seq <= ?`, upTo)
	return err
}

// ClearFeed drops every feed row; the counter stays.
func (d *DB) ClearFeed() error {
	_, err := d.db.Exec(`DELETE FROM library_feed`)
	return err
}

// MirrorRow is what a slave did with one entry of its master: present, removed, pending or failed.
type MirrorRow struct {
	Path, State, Entry, Reason string
	Seq, At, Sess              int64
}

const mirrorCols = `path, seq, state, entry, reason, at, sess`

func scanMirror(row interface{ Scan(...any) error }) (MirrorRow, error) {
	var r MirrorRow
	err := row.Scan(&r.Path, &r.Seq, &r.State, &r.Entry, &r.Reason, &r.At, &r.Sess)
	return r, err
}

func (d *DB) PutMirror(r MirrorRow) error {
	_, err := d.db.Exec(`INSERT OR REPLACE INTO library_mirror (`+mirrorCols+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.Path, r.Seq, r.State, r.Entry, r.Reason, r.At, r.Sess)
	return err
}

func (d *DB) GetMirror(path string) (MirrorRow, bool, error) {
	r, err := scanMirror(d.db.QueryRow(`SELECT `+mirrorCols+` FROM library_mirror WHERE path = ?`, path))
	if errors.Is(err, sql.ErrNoRows) {
		return MirrorRow{}, false, nil
	}
	return r, err == nil, err
}

func (d *DB) queryMirror(q string, args ...any) ([]MirrorRow, error) {
	rows, err := d.db.Query(`SELECT `+mirrorCols+` FROM library_mirror `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MirrorRow
	for rows.Next() {
		r, err := scanMirror(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListMirrorRecent returns the n latest rows, newest first.
func (d *DB) ListMirrorRecent(n int) ([]MirrorRow, error) {
	return d.queryMirror(`ORDER BY at DESC, seq DESC LIMIT ?`, n)
}

// ListMirrorState returns the rows in any of states, oldest first.
func (d *DB) ListMirrorState(states ...string) ([]MirrorRow, error) {
	if len(states) == 0 {
		return nil, nil
	}
	args := make([]any, len(states))
	for i, s := range states {
		args[i] = s
	}
	return d.queryMirror(`WHERE state IN (?`+strings.Repeat(", ?", len(states)-1)+`) ORDER BY at, path`, args...)
}

func (d *DB) ListMirrorFailed() ([]MirrorRow, error) { return d.ListMirrorState("failed") }

// MirrorCounts counts the present films and episodes and the failed entries.
func (d *DB) MirrorCounts() (films, episodes, failed int64, err error) {
	err = d.db.QueryRow(`SELECT
		COUNT(CASE WHEN state = 'present' AND path LIKE 'movies/%' THEN 1 END),
		COUNT(CASE WHEN state = 'present' AND path LIKE 'tv/%' THEN 1 END),
		COUNT(CASE WHEN state = 'failed' THEN 1 END) FROM library_mirror`).Scan(&films, &episodes, &failed)
	return
}

// PruneMirrorRemoved drops the removals recorded by sessions before sess, except the paths in keep.
func (d *DB) PruneMirrorRemoved(beforeSess int64, keep ...string) error {
	q := `DELETE FROM library_mirror WHERE state = 'removed' AND sess < ?`
	args := []any{beforeSess}
	if len(keep) > 0 {
		q += ` AND path NOT IN (?` + strings.Repeat(", ?", len(keep)-1) + `)`
		for _, k := range keep {
			args = append(args, k)
		}
	}
	_, err := d.db.Exec(q, args...)
	return err
}

// ClearMirror forgets everything a master sent: the files stay.
func (d *DB) ClearMirror() error {
	_, err := d.db.Exec(`DELETE FROM library_mirror`)
	return err
}

// HasGoStormInfo reports whether hash has a stored row with its metainfo, without reading the blob.
func (d *DB) HasGoStormInfo(hash string) (bool, error) {
	var n int
	err := d.reader().QueryRow(`SELECT COUNT(*) FROM gostorm_torrents WHERE hash = ? AND info_bytes IS NOT NULL AND LENGTH(info_bytes) > 0`, hash).Scan(&n)
	return n > 0, err
}
