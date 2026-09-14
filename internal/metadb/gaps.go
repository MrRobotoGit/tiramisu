package metadb

import "time"

// EpisodeGap is an episode the reaper removed because its release was dead and
// nothing live could replace it. The record is what makes the hole recoverable:
// discovery reaches a fraction of the library, so without it a season nobody
// re-searches keeps the gap forever.
type EpisodeGap struct {
	EpisodeKey string
	ShowIMDB   string
	Season     int
	FilePath   string
	DeadHash   string
	RemovedAt  int64
	// LastAttempt is when a repair pass last tried this hole. Zero means never.
	LastAttempt int64
}

// RecordEpisodeGap remembers one removed episode.
func (d *DB) RecordEpisodeGap(g EpisodeGap) error {
	if g.RemovedAt == 0 {
		g.RemovedAt = time.Now().Unix()
	}
	_, err := d.db.Exec(
		`INSERT OR REPLACE INTO episode_gaps
		 (episode_key, show_imdb, season, file_path, dead_hash, removed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		g.EpisodeKey, g.ShowIMDB, g.Season, g.FilePath, g.DeadHash, g.RemovedAt)
	return err
}

// ClearEpisodeGap drops the record once the episode is back.
func (d *DB) ClearEpisodeGap(episodeKey string) error {
	_, err := d.db.Exec(`DELETE FROM episode_gaps WHERE episode_key = ?`, episodeKey)
	return err
}

// MarkGapAttempted stamps a hole as tried, whatever the outcome, so the next run
// starts from the ones waiting longest instead of the ones that keep failing.
func (d *DB) MarkGapAttempted(episodeKey string) error {
	_, err := d.db.Exec(`UPDATE episode_gaps SET last_attempt = ? WHERE episode_key = ?`,
		time.Now().Unix(), episodeKey)
	return err
}

// EpisodeGaps returns every hole still open, least recently tried first: a gap never
// attempted sorts by when it was created, one already tried by when it was tried.
func (d *DB) EpisodeGaps() ([]EpisodeGap, error) {
	rows, err := d.db.Query(
		`SELECT episode_key, COALESCE(show_imdb, ''), season, file_path, dead_hash, removed_at,
		        COALESCE(last_attempt, 0)
		 FROM episode_gaps
		 ORDER BY MAX(COALESCE(last_attempt, 0), removed_at), episode_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EpisodeGap
	for rows.Next() {
		var g EpisodeGap
		if err := rows.Scan(&g.EpisodeKey, &g.ShowIMDB, &g.Season, &g.FilePath, &g.DeadHash, &g.RemovedAt, &g.LastAttempt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
