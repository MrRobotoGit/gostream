package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/MrRobotoGit/gostream/internal/gostorm/log"
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

	bboltDB := NewTDB()
	if bboltDB == nil {
		log.TLogln("Error open bboltDB:", filepath.Join(Path, "config.db"))
		os.Exit(1)
	}

	jsonDB := NewJsonDB()
	if jsonDB == nil {
		log.TLogln("Error open jsonDB")
		os.Exit(1)
	}

	// Optional forced migration (for manual control)
	if migrationMode := os.Getenv("TS_MIGRATION_MODE"); migrationMode != "" {
		log.TLogln(fmt.Sprintf("Executing forced migration: %s", migrationMode))
		if err := SmartMigrate(bboltDB, jsonDB, migrationMode); err != nil {
			log.TLogln("Migration warning:", err)
		}
	}

	// Determine storage preferences
	settingsStoragePref := determineStoragePreferences(bboltDB, jsonDB)

	// Apply migrations (clean, one-way)
	applyCleanMigrations(bboltDB, jsonDB, settingsStoragePref)

	// Setup routing
	setupDatabaseRouting(bboltDB, jsonDB, settingsStoragePref)

	// Load settings
	loadBTSets()

	// Update preferences if they changed
	if BTsets != nil && BTsets.StoreSettingsInJson != settingsStoragePref {
		BTsets.StoreSettingsInJson = settingsStoragePref
		SetBTSets(BTsets)
	}

	// Migrate old torrents
	MigrateTorrents()

	logConfiguration(settingsStoragePref)
}

func determineStoragePreferences(bboltDB, jsonDB GoStormDB) (settingsInJson bool) {
	// Try to load existing settings first
	if existing := loadExistingSettings(bboltDB, jsonDB); existing != nil {
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

func loadExistingSettings(bboltDB, jsonDB GoStormDB) *BTSets {
	// Try JSON first
	if buf := jsonDB.Get("Settings", "BitTorr"); buf != nil {
		var sets BTSets
		if err := json.Unmarshal(buf, &sets); err == nil {
			return &sets
		}
	}
	// Try BBolt
	if buf := bboltDB.Get("Settings", "BitTorr"); buf != nil {
		var sets BTSets
		if err := json.Unmarshal(buf, &sets); err == nil {
			return &sets
		}
	}
	return nil
}

func applyCleanMigrations(bboltDB, jsonDB GoStormDB, settingsInJson bool) {
	// Settings migration
	if settingsInJson {
		safeMigrate(bboltDB, jsonDB, "Settings", "BitTorr", "JSON", true)
	} else {
		safeMigrate(jsonDB, bboltDB, "Settings", "BitTorr", "BBolt", true)
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
		if clearSource {
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

func setupDatabaseRouting(bboltDB, jsonDB GoStormDB, settingsInJson bool) {
	dbRouter := NewXPathDBRouter()

	if settingsInJson {
		dbRouter.RegisterRoute(jsonDB, "Settings")
	} else {
		dbRouter.RegisterRoute(bboltDB, "Settings")
	}

	dbRouter.RegisterRoute(bboltDB, "Torrents")
	tdb = NewDBReadCache(dbRouter)
}

func logConfiguration(settingsInJson bool) {
	settingsLoc := "JSON"
	if !settingsInJson {
		settingsLoc = "BBolt"
	}

	log.TLogln(fmt.Sprintf("Storage: Settings->%s, Torrents->BBolt",
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

	bboltDB := NewTDB()
	if bboltDB == nil {
		return errors.New("failed to open BBolt DB")
	}
	// DON'T CLOSE! They're still in use by tdb
	// defer bboltDB.CloseDB()

	jsonDB := NewJsonDB()
	if jsonDB == nil {
		return errors.New("failed to open JSON DB")
	}
	// DON'T CLOSE! They're still in use by tdb
	// defer jsonDB.CloseDB()

	log.TLogln(fmt.Sprintf("Switching Settings storage to %s",
		map[bool]string{true: "JSON", false: "BBolt"}[useJson]))

	// Update storage preference (must be called before migrate as this setting migrate too)
	if BTsets != nil {
		BTsets.StoreSettingsInJson = useJson
		SetBTSets(BTsets)
	}

	var err error
	if useJson {
		err = MigrateSettingsToJson(bboltDB, jsonDB)
	} else {
		err = MigrateSettingsFromJson(jsonDB, bboltDB)
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
			prefs["settings"] = "bbolt"
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
