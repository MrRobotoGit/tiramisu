package metadb

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// MigrateFromJSON reads legacy JSON state files and populates the SQLite database.
// Migration is granular per table: only tables whose JSON source file still exists
// are cleared and re-migrated. Tables whose JSON is already .migrated are left intact.
// This prevents a single recreated JSON file from wiping unrelated DB tables.
func (d *DB) MigrateFromJSON(stateDir string) error {
	inodePath := filepath.Join(stateDir, "inode_map.json")
	negPath := filepath.Join(stateDir, "no_mkv_hashes.json")
	fullPath := filepath.Join(stateDir, "tv_fullpacks.json")
	epPath := filepath.Join(stateDir, "tv_episode_registry.json")

	needsInodes := fileExists(inodePath)
	needsCaches := fileExists(negPath) || fileExists(fullPath)
	needsEpisodes := fileExists(epPath)

	if !needsInodes && !needsCaches && !needsEpisodes {
		if d.logger != nil {
			d.logger.Printf("[StateDB] Migration already completed (all JSON files renamed)")
		}
		return nil
	}

	// Targeted cleanup: only clear the tables we are about to re-migrate.
	// Other tables (already migrated) are left untouched.
	if needsInodes {
		if _, err := d.db.Exec("DELETE FROM inodes"); err != nil {
			return fmt.Errorf("metadb: clear inodes: %w", err)
		}
	}
	if needsCaches {
		if _, err := d.db.Exec("DELETE FROM sync_caches"); err != nil {
			return fmt.Errorf("metadb: clear sync_caches: %w", err)
		}
	}
	if needsEpisodes {
		if _, err := d.db.Exec("DELETE FROM tv_episodes"); err != nil {
			return fmt.Errorf("metadb: clear tv_episodes: %w", err)
		}
	}

	// Migrate inodes
	var inodeCount int
	if needsInodes {
		n, err := d.migrateInodes(inodePath)
		if err != nil {
			return fmt.Errorf("metadb: migrate inodes: %w", err)
		}
		inodeCount = n
	}

	// Migrate sync caches
	var cacheCount int
	if needsCaches {
		n, err := d.migrateCaches(negPath, fullPath)
		if err != nil {
			return fmt.Errorf("metadb: migrate caches: %w", err)
		}
		cacheCount = n
	}

	// Migrate episodes
	var epCount int
	if needsEpisodes {
		n, err := d.migrateEpisodes(epPath)
		if err != nil {
			return fmt.Errorf("metadb: migrate episodes: %w", err)
		}
		epCount = n
	}

	// Rename migrated JSON files
	for _, p := range []string{inodePath, negPath, fullPath, epPath} {
		if fileExists(p) {
			if err := os.Rename(p, p+".migrated"); err != nil {
				if d.logger != nil {
					d.logger.Printf("[StateDB] Warning: failed to rename %s: %v", p, err)
				}
			}
		}
	}

	if d.logger != nil {
		d.logger.Printf("[StateDB] Migration complete: %d inodes, %d caches, %d episodes", inodeCount, cacheCount, epCount)
	}
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// migrateInodes reads inode_map.json and inserts all entries.
// The actual JSON format is:
//
//	{
//	  "version": 2,
//	  "files": {"infohash:index": "inode"},
//	  "dirs": {"/relative/path": "inode"},
//	  "filename_index": {"/full/path/file.mkv": "infohash:index"}
//	}
//
// V1 uses uint64 values instead of strings for files/dirs.
func (d *DB) migrateInodes(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	// Try V2 first (string values for inodes)
	var v2Data InodeMapDataV2
	if err := json.Unmarshal(data, &v2Data); err == nil && v2Data.Version == 2 {
		return d.insertInodesV2(v2Data)
	}

	// Fallback to V1 (uint64 values)
	var v1Data InodeMapDataV1
	if err := json.Unmarshal(data, &v1Data); err == nil && v1Data.Version == 1 {
		return d.insertInodesV1(v1Data)
	}

	return 0, fmt.Errorf("inode_map.json: unrecognized format")
}

// InodeMapDataV2 matches the current V2 JSON format.
type InodeMapDataV2 struct {
	Version       int               `json:"version"`
	Files         map[string]string `json:"files"`
	Dirs          map[string]string `json:"dirs"`
	FilenameIndex map[string]string `json:"filename_index"`
}

// InodeMapDataV1 matches the legacy V1 JSON format.
type InodeMapDataV1 struct {
	Version       int               `json:"version"`
	Files         map[string]uint64 `json:"files"`
	Dirs          map[string]uint64 `json:"dirs"`
	FilenameIndex map[string]string `json:"filename_index"`
}

func (d *DB) insertInodesV1(data InodeMapDataV1) (int, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		"INSERT OR REPLACE INTO inodes (type, infohash, file_idx, full_path, basename, inode_value) VALUES (?, ?, ?, ?, ?, ?)")
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	count := 0

	// Build reverse map: infohash:index -> inode
	for key, inodeVal := range data.Files {
		_, err := stmt.Exec("file", key, 0, "", pathBase(key), int64(inodeVal))
		if err != nil {
			return 0, err
		}
		count++
	}

	for relPath, inodeVal := range data.Dirs {
		_, err := stmt.Exec("dir", "", 0, relPath, pathBase(relPath), int64(inodeVal))
		if err != nil {
			return 0, err
		}
		count++
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	if d.logger != nil {
		d.logger.Printf("[StateDB] Migrated inode_map.json (V1 format)")
	}
	return count, nil
}

func (d *DB) insertInodesV2(data InodeMapDataV2) (int, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		"INSERT OR REPLACE INTO inodes (type, infohash, file_idx, full_path, basename, inode_value) VALUES (?, ?, ?, ?, ?, ?)")
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	count := 0

	// Parse file keys: "infohash:fileindex" -> inode (string)
	for key, inodeStr := range data.Files {
		inodeVal, err := strconv.ParseUint(inodeStr, 10, 64)
		if err != nil {
			continue
		}
		_, err = stmt.Exec("file", key, 0, "", pathBase(key), int64(inodeVal))
		if err != nil {
			return 0, err
		}
		count++
	}

	for relPath, inodeStr := range data.Dirs {
		inodeVal, err := strconv.ParseUint(inodeStr, 10, 64)
		if err != nil {
			continue
		}
		_, err = stmt.Exec("dir", "", 0, relPath, pathBase(relPath), int64(inodeVal))
		if err != nil {
			return 0, err
		}
		count++
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	if d.logger != nil {
		d.logger.Printf("[StateDB] Migrated inode_map.json (V2 format)")
	}
	return count, nil
}

// migrateCaches reads no_mkv_hashes.json and tv_fullpacks.json.
// Actual formats:
// no_mkv_hashes.json: {"hash": {"hash": "...", "timestamp": "..."}}
// tv_fullpacks.json:  {"hash": {"hash": "...", "title": "...", "processed_at": "..."}}
func (d *DB) migrateCaches(negPath, fullPath string) (int, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		"INSERT OR REPLACE INTO sync_caches (hash, cache_type, title, timestamp) VALUES (?, ?, ?, ?)")
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	count := 0

	// Negative caches
	if fileExists(negPath) {
		data, err := os.ReadFile(negPath)
		if err != nil {
			return 0, err
		}
		var negData map[string]NegativeCacheEntryJSON
		if err := json.Unmarshal(data, &negData); err == nil {
			for hash, entry := range negData {
				ts := entry.Timestamp
				if ts == "" {
					ts = time.Now().UTC().Format(time.RFC3339)
				}
				_, err := stmt.Exec(hash, "negative", "", ts)
				if err != nil {
					return 0, err
				}
				count++
			}
		}
	}

	// Fullpack caches
	if fileExists(fullPath) {
		data, err := os.ReadFile(fullPath)
		if err != nil {
			return 0, err
		}
		var fullData map[string]FullpackCacheEntryJSON
		if err := json.Unmarshal(data, &fullData); err == nil {
			for hash, entry := range fullData {
				ts := entry.Timestamp
				if ts == "" {
					ts = time.Now().UTC().Format(time.RFC3339)
				}
				_, err := stmt.Exec(hash, "fullpack", entry.Title, ts)
				if err != nil {
					return 0, err
				}
				count++
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	return count, nil
}

// NegativeCacheEntryJSON matches the JSON format of no_mkv_hashes.json entries.
type NegativeCacheEntryJSON struct {
	Hash      string `json:"hash"`
	Timestamp string `json:"timestamp"`
}

// FullpackCacheEntryJSON matches the JSON format of tv_fullpacks.json entries.
type FullpackCacheEntryJSON struct {
	Hash      string `json:"hash"`
	Title     string `json:"title"`
	Timestamp string `json:"processed_at"`
}

// migrateEpisodes reads tv_episode_registry.json.
// Format: {"Show_S01E01": {"quality_score": 85, "hash": "...", "file_path": "...", "source": "...", "created": 1712400000}}
func (d *DB) migrateEpisodes(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	var epData map[string]EpisodeEntryJSON
	if err := json.Unmarshal(data, &epData); err != nil {
		return 0, err
	}

	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		"INSERT OR REPLACE INTO tv_episodes (episode_key, quality_score, hash, file_path, source, created) VALUES (?, ?, ?, ?, ?, ?)")
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	count := 0
	for key, entry := range epData {
		_, err := stmt.Exec(key, entry.QualityScore, entry.Hash, entry.FilePath, entry.Source, entry.Created)
		if err != nil {
			return 0, err
		}
		count++
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	return count, nil
}

// EpisodeEntryJSON matches the JSON format of tv_episode_registry.json entries.
type EpisodeEntryJSON struct {
	QualityScore int    `json:"quality_score"`
	Hash         string `json:"hash"`
	FilePath     string `json:"file_path"`
	Source       string `json:"source"`
	Created      int64  `json:"created"`
}
