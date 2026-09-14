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

// FullpackCacheEntry represents a hash that is a full pack.
type FullpackCacheEntry struct {
	Hash      string
	Title     string
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

// AddFullpack inserts or updates a fullpack cache entry.
func (d *DB) AddFullpack(hash, title string, timestamp time.Time) error {
	_, err := d.db.Exec(
		"INSERT OR REPLACE INTO sync_caches (hash, cache_type, title, timestamp) VALUES (?, 'fullpack', ?, ?)",
		hash, title, timestamp.UTC().Format(time.RFC3339),
	)
	return err
}

// IsNegative checks if a hash is in the negative cache.
func (d *DB) IsNegative(hash string) (bool, error) {
	var count int
	err := d.db.QueryRow(
		"SELECT COUNT(*) FROM sync_caches WHERE hash = ? AND cache_type = 'negative'",
		hash,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// IsFullpack checks if a hash is in the fullpack cache.
func (d *DB) IsFullpack(hash string) (bool, error) {
	var count int
	err := d.db.QueryRow(
		"SELECT COUNT(*) FROM sync_caches WHERE hash = ? AND cache_type = 'fullpack'",
		hash,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// RemoveNegative deletes a negative cache entry.
func (d *DB) RemoveNegative(hash string) error {
	_, err := d.db.Exec(
		"DELETE FROM sync_caches WHERE hash = ? AND cache_type = 'negative'",
		hash,
	)
	return err
}

// RemoveFullpack deletes a fullpack cache entry.
func (d *DB) RemoveFullpack(hash string) error {
	_, err := d.db.Exec(
		"DELETE FROM sync_caches WHERE hash = ? AND cache_type = 'fullpack'",
		hash,
	)
	return err
}

// CleanupStale deletes expired entries based on TTLs.
// Returns the number of entries deleted.
func (d *DB) CleanupStale(negativeTTL, fullpackTTL time.Duration) (int, error) {
	negCutoff := time.Now().UTC().Add(-negativeTTL).Format(time.RFC3339)
	fullCutoff := time.Now().UTC().Add(-fullpackTTL).Format(time.RFC3339)

	negResult, err := d.db.Exec(
		"DELETE FROM sync_caches WHERE cache_type = 'negative' AND timestamp < ?",
		negCutoff,
	)
	if err != nil {
		return 0, err
	}
	negRows, _ := negResult.RowsAffected()

	fullResult, err := d.db.Exec(
		"DELETE FROM sync_caches WHERE cache_type = 'fullpack' AND timestamp < ?",
		fullCutoff,
	)
	if err != nil {
		return int(negRows), err
	}
	fullRows, _ := fullResult.RowsAffected()

	return int(negRows) + int(fullRows), nil
}

// LoadAllCaches loads all cache entries for populating in-memory maps at startup.
func (d *DB) LoadAllCaches() (
	neg map[string]NegativeCacheEntry,
	full map[string]FullpackCacheEntry,
	err error,
) {
	neg = make(map[string]NegativeCacheEntry)
	full = make(map[string]FullpackCacheEntry)

	rows, err := d.db.Query(
		"SELECT hash, cache_type, title, timestamp FROM sync_caches",
	)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var hash, cacheType, title, timestamp string
		if err := rows.Scan(&hash, &cacheType, &title, &timestamp); err != nil {
			return nil, nil, err
		}

		switch cacheType {
		case "negative":
			neg[hash] = NegativeCacheEntry{
				Hash:      hash,
				Reason:    title,
				Timestamp: timestamp,
			}
		case "fullpack":
			full[hash] = FullpackCacheEntry{
				Hash:      hash,
				Title:     title,
				Timestamp: timestamp,
			}
		}
	}

	return neg, full, rows.Err()
}
