package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"tiramisu/internal/gostorm/log"
	"tiramisu/internal/metadb"

	bolt "go.etcd.io/bbolt"
)

const (
	importMarkerXPath = "Meta"
	importMarkerName  = "bbolt_import"
)

type importResult struct {
	torrents, kept, skipped, settings int
}

// ImportLegacyDB imports config.db once, before GoStorm starts. An error stops the start: on
// defaults the real settings would be overwritten, on an empty list cleanCache drops the disk cache.
func ImportLegacyDB() error {
	if stateDB == nil {
		return errors.New("GoStorm: state DB not set")
	}
	res, err := importBBolt(stateDB, Path, Args != nil && Args.RDB)
	if err != nil {
		return fmt.Errorf("GoStorm: importing config.db: %w", err)
	}
	if res != (importResult{}) {
		log.TLogln(fmt.Sprintf("[GoStormDB] config.db imported: %d torrents, %d already present, %d unreadable, %d settings",
			res.torrents, res.kept, res.skipped, res.settings))
	}
	return nil
}

// importBBolt copies config.db into the state DB in two transactions, settings then torrents
// with the marker. On any error the file stays put and the next start retries; rows win over it.
func importBBolt(db *metadb.DB, dir string, readOnly bool) (importResult, error) {
	var res importResult
	src := filepath.Join(dir, "config.db")
	if fi, err := os.Stat(src); err != nil || fi.Size() == 0 {
		return res, nil
	}
	stamp := time.Now().Format("20060102-150405")

	if m, err := db.GoStormKVGet(importMarkerXPath, importMarkerName); err != nil {
		return res, err
	} else if m != nil {
		if readOnly {
			return res, nil
		}
		dst := src + ".stale-" + stamp
		if err := os.Rename(src, dst); err != nil {
			// The data is already in the DB: refusing to start would only loop under systemd.
			log.TLogln("[GoStormDB] WARN config.db already imported on", string(m), "and cannot be moved aside:", err)
			return res, nil
		}
		log.TLogln("[GoStormDB] WARN config.db already imported on", string(m), "- moved to", dst, "unread")
		return res, nil
	}
	if readOnly {
		return res, fmt.Errorf("%s not imported yet: start once without read-only mode", src)
	}
	return copyBBolt(db, src, stamp)
}

// copyBBolt runs the two import transactions, then renames the file.
func copyBBolt(db *metadb.DB, src, stamp string) (res importResult, err error) {
	// A truncated file makes BBolt fault on its mmap: turn that into an error, not a crash.
	defer debug.SetPanicOnFault(debug.SetPanicOnFault(true))
	defer func() {
		if r := recover(); r != nil {
			res, err = importResult{}, fmt.Errorf("%s unreadable: %v", src, r)
		}
	}()

	bdb, err := bolt.Open(src, 0o600, &bolt.Options{ReadOnly: true, Timeout: 5 * time.Second})
	if err != nil {
		return res, fmt.Errorf("open %s: %w", src, err)
	}

	err = db.ImportGoStorm(func(im *metadb.GoStormImporter) error {
		return bdb.View(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte("Settings"))
			if b == nil {
				return nil
			}
			return b.ForEach(func(k, v []byte) error {
				if v == nil { // nested bucket, never written by TDB
					return nil
				}
				added, err := im.PutKV("Settings", string(k), append([]byte(nil), v...))
				if added {
					res.settings++
				}
				return err
			})
		})
	})
	if err != nil {
		bdb.Close()
		return importResult{}, fmt.Errorf("settings: %w", err)
	}

	err = db.ImportGoStorm(func(im *metadb.GoStormImporter) error {
		return bdb.View(func(tx *bolt.Tx) error {
			if b := tx.Bucket([]byte("Torrents")); b != nil {
				err := b.ForEach(func(k, v []byte) error {
					var t TorrentDB
					if json.Unmarshal(v, &t) != nil || t.TorrentSpec == nil {
						res.skipped++
						return nil
					}
					row, err := torrentToRow(&t)
					if err != nil || row.Hash != string(k) {
						res.skipped++
						return nil
					}
					added, err := im.PutTorrent(row)
					if err != nil {
						return err
					}
					if added {
						res.torrents++
					} else {
						res.kept++
					}
					return nil
				})
				if err != nil {
					return err
				}
			}
			_, err := im.PutKV(importMarkerXPath, importMarkerName, []byte(time.Now().Format(time.RFC3339)))
			return err
		})
	})
	bdb.Close()
	if err != nil {
		return importResult{}, fmt.Errorf("torrents: %w", err)
	}

	// The import leaves a WAL the size of the data; fold it back before the engine starts.
	if err := db.Checkpoint(); err != nil {
		log.TLogln("[GoStormDB] WARN checkpoint after import:", err)
	}
	dst := src + ".migrated-" + stamp
	if err := os.Rename(src, dst); err != nil {
		log.TLogln("[GoStormDB] WARN config.db imported but not renamed:", err, "- set aside at next start")
	}
	return res, nil
}
