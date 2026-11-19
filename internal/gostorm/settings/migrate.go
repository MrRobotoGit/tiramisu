package settings

import (
	"encoding/json"
	"fmt"
	"reflect"

	"tiramisu/internal/gostorm/log"
)

// MigrateSettingsToJson migrates Settings from the DB to JSON
func MigrateSettingsToJson(sqlDB, jsonDB GoStormDB) error {
	// if BTsets != nil {
	// 	return errors.New("migration must be called before initializing BTSets")
	// }
	migrated, err := MigrateSingle(sqlDB, jsonDB, "Settings", "BitTorr")
	if migrated {
		log.TLogln("Settings migrated from the DB to JSON")
	}
	return err
}

// MigrateSettingsFromJson migrates Settings from JSON to the DB
func MigrateSettingsFromJson(jsonDB, sqlDB GoStormDB) error {
	// if BTsets != nil {
	// 	return errors.New("migration must be called before initializing BTSets")
	// }
	migrated, err := MigrateSingle(jsonDB, sqlDB, "Settings", "BitTorr")
	if migrated {
		log.TLogln("Settings migrated from JSON to the DB")
	}
	return err
}

// MigrateSingle migrates a single entry with validation
// Returns: (migrated bool, error)
func MigrateSingle(source, target GoStormDB, xpath, name string) (bool, error) {
	sourceData := source.Get(xpath, name)
	if sourceData == nil {
		if IsDebug() {
			log.TLogln(fmt.Sprintf("No data to migrate for %s/%s", xpath, name))
		}
		return false, nil
	}

	targetData := target.Get(xpath, name)
	if targetData != nil {
		// Check if already identical
		if equal, err := isByteArraysEqualJson(sourceData, targetData); err == nil && equal {
			if IsDebug() {
				log.TLogln(fmt.Sprintf("Skipping %s/%s (already identical)", xpath, name))
			}
			return false, nil
		}
	}

	// Perform migration
	target.Set(xpath, name, sourceData)
	if IsDebug() {
		log.TLogln(fmt.Sprintf("Migrating %s/%s", xpath, name))
	}

	// Verify migration
	if err := verifyMigration(source, target, xpath, name, sourceData); err != nil {
		return false, fmt.Errorf("migration verification failed for %s/%s: %w", xpath, name, err)
	}
	if IsDebug() {
		log.TLogln(fmt.Sprintf("Successfully migrated %s/%s", xpath, name))
	}
	return true, nil
}

// SmartMigrate - keep for manual/advanced use
func SmartMigrate(sqlDB, jsonDB GoStormDB, forceDirection string) error {
	// if BTsets != nil {
	// 	return errors.New("migration must be called before initializing BTSets")
	// }
	switch forceDirection {
	case "settings_to_json":
		return MigrateSettingsToJson(sqlDB, jsonDB)
	case "settings_to_bbolt", "settings_to_sqlite":
		return MigrateSettingsFromJson(jsonDB, sqlDB)
	case "sync_both":
		// Simple sync: copy missing data both ways
		if err := migrateMissing(sqlDB, jsonDB, "Settings", "BitTorr"); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("unknown migration direction: %s", forceDirection)
	}
}

func isByteArraysEqualJson(a, b []byte) (bool, error) {
	if len(a) == 0 && len(b) == 0 {
		return true, nil
	}
	if len(a) == 0 || len(b) == 0 {
		return false, nil
	}
	// Quick check: same length and byte equality
	if len(a) == len(b) {
		equal := true
		for i := range a {
			if a[i] != b[i] {
				equal = false
				break
			}
		}
		if equal {
			return true, nil
		}
	}
	// Parse as JSON for structural comparison
	var objectA, objectB interface{}

	if err := json.Unmarshal(a, &objectA); err != nil {
		return false, fmt.Errorf("error unmarshalling A: %w", err)
	}

	if err := json.Unmarshal(b, &objectB); err != nil {
		return false, fmt.Errorf("error unmarshalling B: %w", err)
	}

	return reflect.DeepEqual(objectA, objectB), nil
}

// Optimized version for performance
func isByteArraysEqualJsonOptimized(a, b []byte) (bool, error) {
	// Fast paths
	if a == nil && b == nil {
		return true, nil
	}
	if len(a) != len(b) {
		return false, nil
	}
	if len(a) == 0 {
		return true, nil
	}
	// Byte equality (fastest check)
	equal := true
	for i := range a {
		if a[i] != b[i] {
			equal = false
			break
		}
	}
	if equal {
		return true, nil
	}
	// Parse as JSON (slower but accurate)
	return isByteArraysEqualJson(a, b)
}

func verifyMigration(source, target GoStormDB, xpath, name string, originalData []byte) error {
	// Get migrated data
	migratedData := target.Get(xpath, name)
	if migratedData == nil {
		return fmt.Errorf("migration failed: no data after migration for %s/%s", xpath, name)
	}
	// Compare with original
	if equal, err := isByteArraysEqualJsonOptimized(originalData, migratedData); err != nil {
		return fmt.Errorf("verification failed for %s/%s: %w", xpath, name, err)
	} else if !equal {
		return fmt.Errorf("data mismatch after migration for %s/%s", xpath, name)
	}
	if IsDebug() {
		log.TLogln(fmt.Sprintf("Verified migration of %s/%s", xpath, name))
	}
	return nil
}

func migrateMissing(db1, db2 GoStormDB, xpath, name string) error {
	// Copy from db1 to db2 if missing
	if db2.Get(xpath, name) == nil {
		if data := db1.Get(xpath, name); data != nil {
			db2.Set(xpath, name, data)
		}
	}
	// Copy from db2 to db1 if missing
	if db1.Get(xpath, name) == nil {
		if data := db2.Get(xpath, name); data != nil {
			db1.Set(xpath, name, data)
		}
	}
	return nil
}
