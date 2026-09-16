package metadb

import (
	"database/sql"
	"errors"
	"time"
)

// RecordMetadataFailure counts one failed reachability check for a torrent: a
// metainfo that never arrived, or a read that waited out its timeout against a
// swarm with nothing connected. first_fail is kept so a burst inside one window
// cannot reach the threshold: what proves a dead swarm is failures spread over
// time, not their number. The table name predates the second case and is kept
// for the databases already in production.
func (d *DB) RecordMetadataFailure(hash string) error {
	now := time.Now().Unix()
	_, err := d.db.Exec(`
		INSERT INTO metadata_failures (hash, fail_count, first_fail, last_fail)
		VALUES (?, 1, ?, ?)
		ON CONFLICT(hash) DO UPDATE SET
			fail_count = fail_count + 1,
			last_fail  = excluded.last_fail`,
		hash, now, now)
	return err
}

// ClearMetadataFailure drops the counter after the swarm answers: one metainfo
// taken from it, or one byte served, is enough to say the release is alive.
func (d *DB) ClearMetadataFailure(hash string) error {
	_, err := d.db.Exec(`DELETE FROM metadata_failures WHERE hash = ?`, hash)
	return err
}

// MetadataFailuresOver returns the hashes that failed at least minCount times
// with at least minSpan between the first and the last failure.
func (d *DB) MetadataFailuresOver(minCount int, minSpan time.Duration) ([]string, error) {
	rows, err := d.db.Query(`
		SELECT hash FROM metadata_failures
		WHERE fail_count >= ? AND (last_fail - first_fail) >= ?
		ORDER BY fail_count DESC`,
		minCount, int64(minSpan.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// MetadataFailureCount reports the stored count for one hash, 0 when absent.
func (d *DB) MetadataFailureCount(hash string) (int, error) {
	var n int
	err := d.db.QueryRow(`SELECT fail_count FROM metadata_failures WHERE hash = ?`, hash).Scan(&n)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return n, nil
}
