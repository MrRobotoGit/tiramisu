package settings

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"tiramisu/internal/metadb"
)

// ErrUnimportedBBolt: a BBolt config.db is still there; Tentacle no longer reads it.
var ErrUnimportedBBolt = errors.New("settings: config.db (BBolt) was never imported. Start Tiramisu v1.12.03 once to import it " +
	"(Docker: the mrrobotogit/tiramisu:v1.12.03 image with the same volumes), then this version")

// ImportLegacy runs the one-off settings.json import before the engine starts. A config.db
// left in dir with no settings row stops the start instead of opening an empty library. Errors are fatal.
func ImportLegacy(db *metadb.DB, dir string, logf func(string, ...any)) error {
	if db == nil {
		return fmt.Errorf("settings: no state DB")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.db")); err == nil {
		row, err := db.GoStormKVGet(kvXPath, kvName)
		if err != nil {
			return err
		}
		if row == nil {
			return ErrUnimportedBBolt
		}
		// Settings already in the DB: a leftover copy, kept for the previous binary.
		logf("[Settings] WARN %s ignored: the settings are already in the DB", filepath.Join(dir, "config.db"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("settings: checking config.db: %w", err)
	}
	return importSettingsJSON(db, dir, time.Now().Format("20060102-150405"), logf)
}
