package metadb

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Logger interface for optional logging.
type Logger interface {
	Printf(format string, v ...interface{})
}

// DB wraps a SQLite database connection for gostream state persistence.
type DB struct {
	db     *sql.DB
	path   string
	logger Logger
}

// New opens or creates a SQLite database at dbPath, applies pragmas,
// creates tables, and returns a ready-to-use DB instance.
func New(dbPath string, logger Logger) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("metadb: create dir: %w", err)
	}

	d, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("metadb: open: %w", err)
	}

	d.SetMaxOpenConns(1)

	db := &DB{
		db:     d,
		path:   dbPath,
		logger: logger,
	}

	if err := db.applyPragmas(); err != nil {
		d.Close()
		return nil, fmt.Errorf("metadb: pragmas: %w", err)
	}

	if err := db.ExecSchema(); err != nil {
		d.Close()
		return nil, fmt.Errorf("metadb: schema: %w", err)
	}

	return db, nil
}

// Close performs a WAL checkpoint and closes the database connection.
func (d *DB) Close() error {
	_, _ = d.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return d.db.Close()
}

// SetLogger sets a custom logger for the database.
func (d *DB) SetLogger(l Logger) {
	if l != nil {
		d.logger = l
	}
}

// SQL returns the underlying *sql.DB for direct access (e.g. transactions).
func (d *DB) SQL() *sql.DB {
	return d.db
}

func (d *DB) applyPragmas() error {
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	}
	for _, p := range pragmas {
		if _, err := d.db.Exec(p); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	return nil
}

// ExecSchema creates all tables if they do not exist. Idempotent.
func (d *DB) ExecSchema() error {
	schema := `
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER PRIMARY KEY,
    applied_at TEXT DEFAULT (datetime('now')),
    description TEXT
);

CREATE TABLE IF NOT EXISTS inodes (
    type        TEXT NOT NULL CHECK(type IN ('file','dir')),
    infohash    TEXT,
    file_idx    INTEGER,
    full_path   TEXT UNIQUE,
    rel_path    TEXT UNIQUE,
    basename    TEXT,
    inode_value INTEGER NOT NULL,
    PRIMARY KEY (type, full_path)
);
CREATE INDEX IF NOT EXISTS idx_inodes_infohash ON inodes(infohash, file_idx);
CREATE INDEX IF NOT EXISTS idx_inodes_basename ON inodes(basename);

CREATE TABLE IF NOT EXISTS sync_caches (
    hash        TEXT NOT NULL,
    cache_type  TEXT NOT NULL CHECK(cache_type IN ('negative','fullpack')),
    title       TEXT,
    timestamp   TEXT NOT NULL,
    PRIMARY KEY (hash, cache_type)
);
CREATE INDEX IF NOT EXISTS idx_sync_type ON sync_caches(cache_type);

CREATE TABLE IF NOT EXISTS tv_episodes (
    episode_key   TEXT PRIMARY KEY,
    quality_score INTEGER NOT NULL,
    hash          TEXT NOT NULL,
    file_path     TEXT NOT NULL,
    source        TEXT NOT NULL,
    created       INTEGER NOT NULL,
    updated_at    TEXT DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_tv_hash ON tv_episodes(hash);
`
	_, err := d.db.Exec(schema)
	return err
}
