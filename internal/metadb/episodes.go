package metadb

import (
	"database/sql"
	"errors"
)

// EpisodeEntry represents a TV episode registry entry.
type EpisodeEntry struct {
	EpisodeKey   string
	QualityScore int
	Hash         string
	FilePath     string
	Source       string
	Created      int64 // unix timestamp
	// ShowIMDB is the show's id, not the episode's: it is what lets a stub be traced
	// back to TMDB when the discovery feed no longer returns the show.
	ShowIMDB string
}

// UpsertEpisode inserts or updates an episode entry.
func (d *DB) UpsertEpisode(key string, entry EpisodeEntry) error {
	_, err := d.db.Exec(
		`INSERT OR REPLACE INTO tv_episodes
		 (episode_key, quality_score, hash, file_path, source, created, show_imdb)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		key, entry.QualityScore, entry.Hash, entry.FilePath, entry.Source, entry.Created, entry.ShowIMDB,
	)
	return err
}

// SetEpisodeShowIMDB fills in the show id of one episode, and only that.
// Deliberately not an UpsertEpisode with a modified entry: that is an INSERT OR
// REPLACE, so a caller holding a row read earlier in the run would write back its
// stale hash and path over whatever replaced them since — and resurrect a row the
// meantime deleted. An UPDATE touches the one column it means to, and touches
// nothing at all when the episode is gone.
func (d *DB) SetEpisodeShowIMDB(key, imdbID string) error {
	_, err := d.db.Exec(
		`UPDATE tv_episodes SET show_imdb = ?, updated_at = datetime('now') WHERE episode_key = ?`,
		imdbID, key,
	)
	return err
}

// GetEpisode returns a single episode by its key.
func (d *DB) GetEpisode(key string) (*EpisodeEntry, bool, error) {
	var e EpisodeEntry
	err := d.db.QueryRow(
		"SELECT episode_key, quality_score, hash, file_path, source, created, COALESCE(show_imdb, '') FROM tv_episodes WHERE episode_key = ?",
		key,
	).Scan(&e.EpisodeKey, &e.QualityScore, &e.Hash, &e.FilePath, &e.Source, &e.Created, &e.ShowIMDB)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &e, true, nil
}

// DeleteEpisode deletes an episode by its key.
func (d *DB) DeleteEpisode(key string) error {
	_, err := d.db.Exec("DELETE FROM tv_episodes WHERE episode_key = ?", key)
	return err
}

// AllEpisodes returns all episode entries.
func (d *DB) AllEpisodes() ([]EpisodeEntry, error) {
	rows, err := d.db.Query(
		"SELECT episode_key, quality_score, hash, file_path, source, created, COALESCE(show_imdb, '') FROM tv_episodes",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []EpisodeEntry
	for rows.Next() {
		var e EpisodeEntry
		if err := rows.Scan(&e.EpisodeKey, &e.QualityScore, &e.Hash, &e.FilePath, &e.Source, &e.Created, &e.ShowIMDB); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}

	return entries, rows.Err()
}

// EpisodesByFilePath returns all episodes that reference a given file path.
// Used for cleanup by file existence.
func (d *DB) EpisodesByFilePath(filePath string) ([]EpisodeEntry, error) {
	rows, err := d.db.Query(
		"SELECT episode_key, quality_score, hash, file_path, source, created, COALESCE(show_imdb, '') FROM tv_episodes WHERE file_path = ?",
		filePath,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []EpisodeEntry
	for rows.Next() {
		var e EpisodeEntry
		if err := rows.Scan(&e.EpisodeKey, &e.QualityScore, &e.Hash, &e.FilePath, &e.Source, &e.Created, &e.ShowIMDB); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}

	return entries, rows.Err()
}

// EpisodesByHash returns every episode backed by one torrent. A season pack stands
// behind many episodes, so a dead hash is never a single-file decision; idx_tv_hash
// makes this the cheap way to ask.
func (d *DB) EpisodesByHash(hash string) ([]EpisodeEntry, error) {
	rows, err := d.db.Query(
		`SELECT episode_key, quality_score, hash, file_path, source, created, COALESCE(show_imdb, '')
		 FROM tv_episodes WHERE hash = ? ORDER BY episode_key`, hash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EpisodeEntry
	for rows.Next() {
		var e EpisodeEntry
		if err := rows.Scan(&e.EpisodeKey, &e.QualityScore, &e.Hash, &e.FilePath, &e.Source, &e.Created, &e.ShowIMDB); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
