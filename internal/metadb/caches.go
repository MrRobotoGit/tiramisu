package metadb

import (
	"time"
)

// NegativeCacheEntry represents a hash that should be skipped (no MKV).
type NegativeCacheEntry struct {
	Hash      string
	Reason    string // why it was skipped: diagnostic only, never read by the sync
	Timestamp string // ISO 8601
}

// AddNegative inserts or updates a negative cache entry. The reason travels in the
// title column: the schema predates it, and nothing reads it back except a human.
func (d *DB) AddNegative(hash, reason string, timestamp time.Time) error {
	_, err := d.db.Exec(
		"INSERT OR REPLACE INTO sync_caches (hash, cache_type, title, timestamp) VALUES (?, 'negative', ?, ?)",
		hash, reason, timestamp.UTC().Format(time.RFC3339),
	)
	return err
}

// ReplaceNegatives swaps the whole negative set in one transaction, leaving fullpack
// rows untouched. The sync owns that set in memory and writes it back at the end of a
// run, so a partial write would resurrect hashes it had just expired.
func (d *DB) ReplaceNegatives(entries []NegativeCacheEntry) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM sync_caches WHERE cache_type = 'negative'"); err != nil {
		return err
	}
	stmt, err := tx.Prepare(
		"INSERT INTO sync_caches (hash, cache_type, title, timestamp) VALUES (?, 'negative', ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range entries {
		if _, err := stmt.Exec(e.Hash, e.Reason, e.Timestamp); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RemoveNegative deletes a negative cache entry.
func (d *DB) RemoveNegative(hash string) error {
	_, err := d.db.Exec(
		"DELETE FROM sync_caches WHERE hash = ? AND cache_type = 'negative'",
		hash,
	)
	return err
}

// CleanupStale deletes negative entries past the TTL and returns how many went.
func (d *DB) CleanupStale(negativeTTL time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-negativeTTL).Format(time.RFC3339)
	res, err := d.db.Exec(
		"DELETE FROM sync_caches WHERE cache_type = 'negative' AND timestamp < ?",
		cutoff,
	)
	if err != nil {
		return 0, err
	}
	rows, _ := res.RowsAffected()
	return int(rows), nil
}

// LoadNegatives reads the hashes to skip. The 'fullpack' half of this table has had
// no writer since the Go port moved pack idempotence into the episode registry.
func (d *DB) LoadNegatives() (map[string]NegativeCacheEntry, error) {
	neg := make(map[string]NegativeCacheEntry)

	rows, err := d.db.Query(
		"SELECT hash, title, timestamp FROM sync_caches WHERE cache_type = 'negative'",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var hash, title, timestamp string
		if err := rows.Scan(&hash, &title, &timestamp); err != nil {
			return nil, err
		}
		neg[hash] = NegativeCacheEntry{Hash: hash, Reason: title, Timestamp: timestamp}
	}

	return neg, rows.Err()
}
