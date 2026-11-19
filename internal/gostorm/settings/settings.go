package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"tiramisu/internal/gostorm/log"
)

// Add a global lock for database operations during migration
var dbMigrationLock sync.RWMutex

func IsDebug() bool {
	if BTsets != nil {
		return BTsets.EnableDebug
	}
	return false
}

var (
	tdb      GoStormDB
	Path     string
	IP       string
	Port     string
	Ssl      bool
	SslPort  string
	ReadOnly bool
	HttpAuth bool
	SearchWA bool
	PubIPv4  string
	PubIPv6  string
	TorAddr  string
	MaxSize  int64
)

func InitSets(readOnly, searchWA bool) {
	ReadOnly = readOnly
	SearchWA = searchWA

	// config.db was imported by ImportLegacyDB in main, before this goroutine started.
	if stateDB == nil {
		log.TLogln("[GoStormDB] state DB not set: SetStateDB must run before InitSets")
		os.Exit(1)
	}
	sqlDB := NewSQLiteDB(stateDB)

	jsonDB := NewJsonDB()
	if jsonDB == nil {
		log.TLogln("Error open jsonDB")
		os.Exit(1)
	}

	// Optional forced migration (for manual control)
	if migrationMode := os.Getenv("TS_MIGRATION_MODE"); migrationMode != "" {
		log.TLogln(fmt.Sprintf("Executing forced migration: %s", migrationMode))
		if err := SmartMigrate(sqlDB, jsonDB, migrationMode); err != nil {
			log.TLogln("Migration warning:", err)
		}
	}

	// A settings file we cannot parse is set aside rather than kept: leaving it in place
	// makes every later save fail, so the server could never recover on its own.
	quarantined := quarantineCorruptSettings(jsonDB)

	// Determine storage preferences
	settingsStoragePref := determineStoragePreferences(sqlDB, jsonDB)
	if quarantined {
		// The file existing at all means settings were kept in JSON. Stay there, and let
		// the migration below repopulate the fresh file from the DB copy if one exists.
		settingsStoragePref = true
	}

	// Apply migrations (clean, one-way)
	applyCleanMigrations(sqlDB, jsonDB, settingsStoragePref)

	// Setup routing
	setupDatabaseRouting(sqlDB, jsonDB, settingsStoragePref)

	// Load settings
	loadBTSets()

	// Update preferences if they changed
	if BTsets != nil && BTsets.StoreSettingsInJson != settingsStoragePref {
		BTsets.StoreSettingsInJson = settingsStoragePref
		SetBTSets(BTsets)
	}

	logConfiguration(settingsStoragePref)
}

// quarantineCorruptSettings moves an unparseable settings file aside and reports whether
// it did. The content is kept under a timestamped name instead of being deleted.
func quarantineCorruptSettings(jsonDB GoStormDB) bool {
	j, ok := jsonDB.(*JsonDB)
	if !ok || j.readable("Settings") {
		return false
	}
	filename, err := j.xPathToFilename("Settings")
	if err != nil {
		return false
	}
	path := filepath.Join(j.Path, filename)
	dest := path + ".corrupt-" + time.Now().Format("20060102-150405")
	if err := os.Rename(path, dest); err != nil {
		// Leave it alone: the guard in loadExistingSettings still prevents a backend flip.
		log.TLogln("Settings file is corrupt and could not be set aside:", err)
		return false
	}
	log.TLogln("Settings file was corrupt, moved to", dest, "- restoring from the DB copy or defaults")
	return true
}

func determineStoragePreferences(sqlDB, jsonDB GoStormDB) (settingsInJson bool) {
	// Try to load existing settings first
	if existing := loadExistingSettings(sqlDB, jsonDB); existing != nil {
		if IsDebug() {
			log.TLogln(fmt.Sprintf("Found settings: StoreSettingsInJson=%v",
				existing.StoreSettingsInJson))
		}
		// Check if these are actually set or just default zero values
		// For now, trust the stored values
		return existing.StoreSettingsInJson
	}

	// Defaults (if not set by user)
	settingsInJson = true // JSON for settings (easy editable)

	// Environment overrides
	if env := os.Getenv("TS_SETTINGS_STORAGE"); env != "" {
		settingsInJson = (env == "json")
	}

	if IsDebug() {
		log.TLogln(fmt.Sprintf("Using flags: settingsInJson=%v",
			settingsInJson))
	}
	return settingsInJson
}

func loadExistingSettings(sqlDB, jsonDB GoStormDB) *BTSets {
	// Try JSON first
	if buf := jsonDB.Get("Settings", "BitTorr"); buf != nil {
		var sets BTSets
		if err := json.Unmarshal(buf, &sets); err == nil {
			return &sets
		}
	}
	// A JSON file that exists but cannot be parsed must not fall through to the DB: the
	// fallback would flip the backend to a stale copy, and the migration then clears the
	// file we failed to read.
	if j, ok := jsonDB.(*JsonDB); ok && !j.readable("Settings") {
		log.TLogln("Settings JSON unreadable, keeping JSON storage (no fallback to the DB)")
		return &BTSets{StoreSettingsInJson: true}
	}
	// Try the DB
	if buf := sqlDB.Get("Settings", "BitTorr"); buf != nil {
		var sets BTSets
		if err := json.Unmarshal(buf, &sets); err == nil {
			return &sets
		}
	}
	return nil
}

func applyCleanMigrations(sqlDB, jsonDB GoStormDB, settingsInJson bool) {
	// Settings migration
	if settingsInJson {
		safeMigrate(sqlDB, jsonDB, "Settings", "BitTorr", "JSON", true)
	} else {
		safeMigrate(jsonDB, sqlDB, "Settings", "BitTorr", "SQLite", true)
	}
}

func safeMigrate(source, target GoStormDB, xpath, name, targetName string, clearSource bool) {
	if IsDebug() {
		log.TLogln(fmt.Sprintf("Checking migration of %s/%s to %s", xpath, name, targetName))
	}

	migrated, err := MigrateSingle(source, target, xpath, name)
	if err != nil {
		log.TLogln(fmt.Sprintf("Migration error for %s/%s: %v", xpath, name, err))
		return
	}

	if migrated {
		log.TLogln(fmt.Sprintf("Successfully migrated %s/%s to %s", xpath, name, targetName))
		// Clear source if requested
		if clearSource && source.Get(xpath, name) != nil {
			source.Rem(xpath, name)
			if IsDebug() {
				log.TLogln(fmt.Sprintf("Cleared %s/%s from source", xpath, name))
			}
		}
	} else {
		log.TLogln(fmt.Sprintf("No migration needed for %s/%s (already exists or no data)",
			xpath, name))
	}
}

func setupDatabaseRouting(sqlDB, jsonDB GoStormDB, settingsInJson bool) {
	dbRouter := NewXPathDBRouter()

	if settingsInJson {
		dbRouter.RegisterRoute(jsonDB, "Settings")
	} else {
		dbRouter.RegisterRoute(sqlDB, "Settings")
	}

	tdb = NewDBReadCache(dbRouter)
}

func logConfiguration(settingsInJson bool) {
	settingsLoc := "JSON"
	if !settingsInJson {
		settingsLoc = "SQLite"
	}

	log.TLogln(fmt.Sprintf("Storage: Settings->%s, Torrents->SQLite",
		settingsLoc))
}

// SwitchSettingsStorage - simplified version
func SwitchSettingsStorage(useJson bool) error {
	if ReadOnly {
		return errors.New("read-only mode")
	}
	// Acquire exclusive lock for migration
	dbMigrationLock.Lock()
	defer dbMigrationLock.Unlock()

	sqlDB := NewSQLiteDB(stateDB)
	if sqlDB == nil {
		return errors.New("state DB not available")
	}

	jsonDB := NewJsonDB()
	if jsonDB == nil {
		return errors.New("failed to open JSON DB")
	}

	log.TLogln(fmt.Sprintf("Switching Settings storage to %s",
		map[bool]string{true: "JSON", false: "SQLite"}[useJson]))

	// Update storage preference (must be called before migrate as this setting migrate too)
	if BTsets != nil {
		BTsets.StoreSettingsInJson = useJson
		SetBTSets(BTsets)
	}

	var err error
	if useJson {
		err = MigrateSettingsToJson(sqlDB, jsonDB)
	} else {
		err = MigrateSettingsFromJson(jsonDB, sqlDB)
	}

	if err != nil {
		return err
	}

	log.TLogln("Settings storage switched. Restart required for routing changes.")
	return nil
}

// SwitchSettingsStorage - simplified version

// Used in /storage/settings web API
func GetStoragePreferences() map[string]interface{} {
	prefs := map[string]interface{}{
		"settings": "json", // Default fallback
	}

	if BTsets != nil {
		// Convert boolean preferences to string values
		if BTsets.StoreSettingsInJson {
			prefs["settings"] = "json"
		} else {
			prefs["settings"] = "sqlite"
		}
	}

	if IsDebug() {
		log.TLogln(fmt.Sprintf("GetStoragePreferences: settings=%s",
			prefs["settings"]))
	}

	return prefs
}

// Used in /storage/settings web API
func SetStoragePreferences(prefs map[string]interface{}) error {
	if ReadOnly || BTsets == nil {
		return errors.New("cannot change storage preferences. Read-only mode")
	}

	if IsDebug() {
		log.TLogln(fmt.Sprintf("SetStoragePreferences received: %v", prefs))
	}

	// Apply changes
	if settingsPref, ok := prefs["settings"].(string); ok && settingsPref != "" {
		useJson := (settingsPref == "json")
		if IsDebug() {
			log.TLogln(fmt.Sprintf("Changing settings storage to useJson=%v (was %v)",
				useJson, BTsets.StoreSettingsInJson))
		}
		if BTsets.StoreSettingsInJson != useJson {
			if err := SwitchSettingsStorage(useJson); err != nil {
				return fmt.Errorf("failed to switch settings storage: %w", err)
			}
		}
	}

	return nil
}

func CloseDB() {
	if tdb != nil {
		tdb.CloseDB()
	}
}
