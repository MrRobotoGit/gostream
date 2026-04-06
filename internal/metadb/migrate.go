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
// Migration is atomic per table: all inserts happen in a single transaction,
// then JSON files are renamed to .migrated only after successful commit.
// If the DB exists but .migrated files don't, the DB is considered incomplete
// (previous crash) and is recreated from JSON.
func (d *DB) MigrateFromJSON(stateDir string) error {
	inodePath := filepath.Join(stateDir, "inode_map.json")
	negPath := filepath.Join(stateDir, "no_mkv_hashes.json")
	fullPath := filepath.Join(stateDir, "tv_fullpacks.json")
	epPath := filepath.Join(stateDir, "tv_episode_registry.json")

	// Check if migration was already completed (all JSON files renamed)
	allMigrated := true
	for _, p := range []string{inodePath, negPath, fullPath, epPath} {
		if _, err := os.Stat(p); err == nil {
			allMigrated = false
			break
		}
	}
	if allMigrated {
		if d.logger != nil {
			d.logger.Printf("[StateDB] Migration already completed (all JSON files renamed)")
		}
		return nil
	}

	// Check if DB has data but files aren't migrated -> crash recovery
	hasData, err := d.hasAnyData()
	if err != nil {
		return fmt.Errorf("metadb: check data: %w", err)
	}
	if hasData {
		if d.logger != nil {
			d.logger.Printf("[StateDB] DB has data but JSON not migrated — crash recovery, recreating DB")
		}
		if err := d.recreateDB(); err != nil {
			return fmt.Errorf("metadb: recreate: %w", err)
		}
	}

	// Migrate inodes
	var inodeCount int
	if _, err := os.Stat(inodePath); err == nil {
		n, err := d.migrateInodes(inodePath)
		if err != nil {
			return fmt.Errorf("metadb: migrate inodes: %w", err)
		}
		inodeCount = n
	}

	// Migrate sync caches
	var cacheCount int
	negExists := fileExists(negPath)
	fullExists := fileExists(fullPath)
	if negExists || fullExists {
		n, err := d.migrateCaches(negPath, fullPath)
		if err != nil {
			return fmt.Errorf("metadb: migrate caches: %w", err)
		}
		cacheCount = n
	}

	// Migrate episodes
	var epCount int
	if _, err := os.Stat(epPath); err == nil {
		n, err := d.migrateEpisodes(epPath)
		if err != nil {
			return fmt.Errorf("metadb: migrate episodes: %w", err)
		}
		epCount = n
	}

	// Only rename files after all migrations succeed
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

func (d *DB) hasAnyData() (bool, error) {
	var count int
	err := d.db.QueryRow("SELECT COUNT(*) FROM inodes").Scan(&count)
	if err != nil {
		return false, err
	}
	if count > 0 {
		return true, nil
	}
	err = d.db.QueryRow("SELECT COUNT(*) FROM sync_caches").Scan(&count)
	if err != nil {
		return false, err
	}
	if count > 0 {
		return true, nil
	}
	err = d.db.QueryRow("SELECT COUNT(*) FROM tv_episodes").Scan(&count)
	return count > 0, err
}

func (d *DB) recreateDB() error {
	if err := d.db.Close(); err != nil {
		return err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		p := d.path + suffix
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	newDB, err := New(d.path, d.logger)
	if err != nil {
		return err
	}
	d.db = newDB.db
	return nil
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
