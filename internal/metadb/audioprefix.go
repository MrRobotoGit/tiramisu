package metadb

import (
	"errors"
	"strings"
)

// AudioProjectionsByPrefix returns the projections under a section-relative prefix:
// the prefix itself, or anything below it as a whole path component. The matching is
// component-wise on purpose: "Artist/Album" must not reach "Artist/Album2". Rows left
// in removing by an interrupted removal are returned too, so a second call resumes
// the album instead of reporting it absent until the next boot.
func (d *DB) AudioProjectionsByPrefix(section, prefix string) ([]AudioProjection, error) {
	if d == nil || d.db == nil {
		return nil, errors.New("metadb: database is not open")
	}
	query := `SELECT ` + audioProjectionColumns + ` FROM audio_projections
		 WHERE section = ? AND state IN (?, ?)
		   AND (virtual_path = ? OR substr(virtual_path, 1, length(?)) = ?)`
	args := []interface{}{
		section, string(AudioCommitted), string(AudioRemoving),
		prefix, prefix + "/", prefix + "/",
	}
	return d.audioProjectionList(query, args...)
}

// MarkAudioProjectionsRemovingPaths claims exactly the listed projections in one
// transaction. The update is restricted to the rows the caller inspected on purpose:
// a prefix-wide update would also claim an add that landed in between, and that
// projection was never read, never unpublish, never unlinked. A crash after this
// point leaves rows in removing, which the startup sweep completes.
func (d *DB) MarkAudioProjectionsRemovingPaths(section string, virtualPaths []string, updatedAtNS int64) (int, error) {
	if d == nil || d.db == nil {
		return 0, errors.New("metadb: database is not open")
	}
	if len(virtualPaths) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(virtualPaths)), ",")
	args := make([]interface{}, 0, len(virtualPaths)+4)
	args = append(args, string(AudioRemoving), updatedAtNS, section, string(AudioCommitted))
	for _, path := range virtualPaths {
		args = append(args, path)
	}
	result, err := d.db.Exec(`UPDATE audio_projections SET state = ?, updated_at_ns = ?
		 WHERE section = ? AND state = ? AND virtual_path IN (`+placeholders+`)`, args...)
	if err != nil {
		return 0, err
	}
	marked, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(marked), nil
}
