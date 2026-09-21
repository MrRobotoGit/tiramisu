package metadb

import "errors"

// Spec §14.4 caps List at 200 by default and 1000 at most.
const (
	DefaultAudioProjectionPageLimit = 200
	MaxAudioProjectionPageLimit     = 1000
)

// AudioProjectionPage returns one page of committed projections ordered by
// virtual_path. substr not LIKE: LIKE is case-insensitive and _ is a wildcard.
func (d *DB) AudioProjectionPage(section, pathPrefix, afterPath string, limit int) ([]AudioProjection, error) {
	// A typed-nil *DB satisfies a non-nil interface, so fail closed rather than
	// dereferencing it.
	if d == nil || d.db == nil {
		return nil, errors.New("metadb: database is not open")
	}
	if limit <= 0 {
		limit = DefaultAudioProjectionPageLimit
	} else if limit > MaxAudioProjectionPageLimit {
		limit = MaxAudioProjectionPageLimit
	}
	query := `SELECT ` + audioProjectionColumns + ` FROM audio_projections
		 WHERE section = ? AND state = ?`
	args := []interface{}{section, string(AudioCommitted)}
	if pathPrefix != "" {
		query += ` AND substr(virtual_path, 1, length(?)) = ?`
		args = append(args, pathPrefix, pathPrefix)
	}
	if afterPath != "" {
		query += ` AND virtual_path > ?`
		args = append(args, afterPath)
	}
	query += ` ORDER BY virtual_path ASC LIMIT ?`
	args = append(args, limit)
	return d.audioProjectionList(query, args...)
}
