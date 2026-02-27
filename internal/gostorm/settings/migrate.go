package settings

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"gostream/internal/gostorm/log"
	"gostream/internal/gostorm/web/api/utils"

	bolt "go.etcd.io/bbolt"
)

var dbTorrentsName = []byte("Torrents")

type torrentBackupDB struct {
	Name      string
	Magnet    string
	InfoBytes []byte
	Hash      string
	Size      int64
	Timestamp int64
}

// Migrate from torrserver.db to config.db
// TODO: migrate categories and data too
func MigrateTorrents() {
	if _, err := os.Lstat(filepath.Join(Path, "torrserver.db")); os.IsNotExist(err) {
		return
	}

	db, err := bolt.Open(filepath.Join(Path, "torrserver.db"), 0o666, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		log.TLogln("MigrateTorrents", err)
		return
	}

	torrs := make([]*torrentBackupDB, 0)
	err = db.View(func(tx *bolt.Tx) error {
		tdb := tx.Bucket(dbTorrentsName)
		if tdb == nil {
			return nil
		}
		c := tdb.Cursor()
		for h, _ := c.First(); h != nil; h, _ = c.Next() {
			hdb := tdb.Bucket(h)
			if hdb != nil {
				torr := new(torrentBackupDB)
				torr.Hash = string(h)
				tmp := hdb.Get([]byte("Name"))
				if tmp == nil {
					return fmt.Errorf("error load torrent")
				}
				torr.Name = string(tmp)

				tmp = hdb.Get([]byte("Link"))
				if tmp == nil {
					return fmt.Errorf("error load torrent")
				}
				torr.Magnet = string(tmp)

				tmp = hdb.Get([]byte("Size"))
				if tmp == nil {
					return fmt.Errorf("error load torrent")
				}
				torr.Size = b2i(tmp)

				tmp = hdb.Get([]byte("Timestamp"))
				if tmp == nil {
					return fmt.Errorf("error load torrent")
				}
				torr.Timestamp = b2i(tmp)

				torrs = append(torrs, torr)
			}
		}
		return nil
	})
	db.Close()
	if err == nil && len(torrs) > 0 {
		for _, torr := range torrs {
			spec, err := utils.ParseLink(torr.Magnet)
			if err != nil {
				continue
			}

			title := torr.Name
			if len(spec.DisplayName) > len(title) {
				title = spec.DisplayName
			}
			log.TLogln("Migrate torrent", torr.Name, torr.Hash, torr.Size)
			AddTorrent(&TorrentDB{
				TorrentSpec: spec,
				Title:       title,
				Timestamp:   torr.Timestamp,
				Size:        torr.Size,
			})
		}
	}
	os.Remove(filepath.Join(Path, "torrserver.db"))
}

// MigrateSettingsToJson migrates Settings from BBolt to JSON
func MigrateSettingsToJson(bboltDB, jsonDB GoStormDB) error {
	// if BTsets != nil {
	// 	return errors.New("migration must be called before initializing BTSets")
	// }
	migrated, err := MigrateSingle(bboltDB, jsonDB, "Settings", "BitTorr")
	if migrated {
		log.TLogln("Settings migrated from BBolt to JSON")
	}
	return err
}

// MigrateSettingsFromJson migrates Settings from JSON to BBolt
func MigrateSettingsFromJson(jsonDB, bboltDB GoStormDB) error {
	// if BTsets != nil {
	// 	return errors.New("migration must be called before initializing BTSets")
	// }
	migrated, err := MigrateSingle(jsonDB, bboltDB, "Settings", "BitTorr")
	if migrated {
		log.TLogln("Settings migrated from JSON to BBolt")
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

// MigrateAll migrates all entries in an xpath with validation
// Returns: (migratedCount, skippedCount, error)
func MigrateAll(source, target GoStormDB, xpath string) (int, int, error) {
	names := source.List(xpath)
	if len(names) == 0 {
		if IsDebug() {
			log.TLogln(fmt.Sprintf("No entries to migrate for %s", xpath))
		}
		return 0, 0, nil
	}

	migratedCount := 0
	skippedCount := 0
	var firstError error
	if IsDebug() {
		log.TLogln(fmt.Sprintf("Starting migration of %d %s entries", len(names), xpath))
	}
	for i, name := range names {
		sourceData := source.Get(xpath, name)
		if sourceData == nil {
			skippedCount++
			if IsDebug() {
				log.TLogln(fmt.Sprintf("[%d/%d] Skipping %s/%s (no data in source)",
					i+1, len(names), xpath, name))
			}
			continue
		}

		targetData := target.Get(xpath, name)
		if targetData != nil {
			// Check if already identical
			if equal, err := isByteArraysEqualJson(sourceData, targetData); err == nil && equal {
				skippedCount++
				if IsDebug() {
					log.TLogln(fmt.Sprintf("[%d/%d] Skipping %s/%s (already identical)",
						i+1, len(names), xpath, name))
				}
				continue
			}
		}

		// Perform migration
		target.Set(xpath, name, sourceData)

		// Verify migration
		if err := verifyMigration(source, target, xpath, name, sourceData); err != nil {
			log.TLogln(fmt.Sprintf("[%d/%d] Migration failed for %s/%s: %v",
				i+1, len(names), xpath, name, err))
			if firstError == nil {
				firstError = err
			}
		} else {
			migratedCount++
			if IsDebug() {
				log.TLogln(fmt.Sprintf("[%d/%d] Successfully migrated %s/%s",
					i+1, len(names), xpath, name))
			}
		}
	}

	summary := fmt.Sprintf("%s migration complete: %d migrated, %d skipped",
		xpath, migratedCount, skippedCount)
	if firstError != nil {
		summary += fmt.Sprintf(", 1+ errors (first: %v)", firstError)
	}
	if IsDebug() {
		log.TLogln(summary)
	}

	return migratedCount, skippedCount, firstError
}

// SmartMigrate - keep for manual/advanced use
func SmartMigrate(bboltDB, jsonDB GoStormDB, forceDirection string) error {
	// if BTsets != nil {
	// 	return errors.New("migration must be called before initializing BTSets")
	// }
	switch forceDirection {
	case "settings_to_json":
		return MigrateSettingsToJson(bboltDB, jsonDB)
	case "settings_to_bbolt":
		return MigrateSettingsFromJson(jsonDB, bboltDB)
	case "sync_both":
		// Simple sync: copy missing data both ways
		if err := migrateMissing(bboltDB, jsonDB, "Settings", "BitTorr"); err != nil {
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

func b2i(v []byte) int64 {
	return int64(binary.BigEndian.Uint64(v))
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
