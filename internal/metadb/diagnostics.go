package metadb

import "database/sql"

// Light reads for the library consistency check, on the read pool: none of them takes
// the writer or reads a torrent's metainfo.

// TorrentRef is a Tentacle torrent without its spec or metainfo.
type TorrentRef struct {
	Hash      string
	Title     string
	Timestamp int64 // added
	Size      int64
	LastUsed  int64 // last activation by the new engine (schema 17), 0 when never
}

// LastActivity is the later of added and last used: a torrent played only under the new
// engine keeps its old "added" timestamp.
func (t TorrentRef) LastActivity() int64 { return max(t.Timestamp, t.LastUsed) }

func (d *DB) ListGoStormTorrentRefs() ([]TorrentRef, error) {
	return scanAll(d.reader(), `SELECT hash, title, timestamp, size, last_used_at FROM gostorm_torrents`, func(r *sql.Rows) (TorrentRef, error) {
		var t TorrentRef
		return t, r.Scan(&t.Hash, &t.Title, &t.Timestamp, &t.Size, &t.LastUsed)
	})
}

// AudioRef is what the check needs of an audio projection.
type AudioRef struct {
	Hash, State, Section, VirtualPath string
	UpdatedAtNS                       int64
}

func (d *DB) AudioRefs() ([]AudioRef, error) {
	return scanAll(d.reader(), `SELECT hash, state, section, virtual_path, updated_at_ns FROM audio_projections`, func(r *sql.Rows) (AudioRef, error) {
		var a AudioRef
		return a, r.Scan(&a.Hash, &a.State, &a.Section, &a.VirtualPath, &a.UpdatedAtNS)
	})
}

// InodeFile is a file entry of the inode map.
type InodeFile struct{ Path, Hash string }

func (d *DB) InodeFiles() ([]InodeFile, error) {
	return scanAll(d.reader(), `SELECT full_path, COALESCE(infohash, '') FROM inodes WHERE type = 'file'`, func(r *sql.Rows) (InodeFile, error) {
		var f InodeFile
		return f, r.Scan(&f.Path, &f.Hash)
	})
}

func (d *DB) MetadataFailureHashes() ([]string, error) {
	return scanAll(d.reader(), `SELECT hash FROM metadata_failures`, func(r *sql.Rows) (string, error) {
		var h string
		return h, r.Scan(&h)
	})
}

// EpisodeRefs is tv_episodes' key, hash and file, on the read pool (AllEpisodes uses the writer).
func (d *DB) EpisodeRefs() ([]EpisodeEntry, error) {
	return scanAll(d.reader(), `SELECT episode_key, hash, file_path FROM tv_episodes`, func(r *sql.Rows) (EpisodeEntry, error) {
		var e EpisodeEntry
		return e, r.Scan(&e.EpisodeKey, &e.Hash, &e.FilePath)
	})
}

func scanAll[T any](db *sql.DB, query string, scan func(*sql.Rows) (T, error)) ([]T, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
