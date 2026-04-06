package metadb

import (
	"database/sql"
	"errors"
)

// InodeEntry represents a single inode mapping (file or directory).
type InodeEntry struct {
	Type       string // "file" or "dir"
	Infohash   string
	FileIdx    int
	FullPath   string
	RelPath    string
	Basename   string
	InodeValue uint64
}

// UpsertFile inserts or updates a file inode entry.
func (d *DB) UpsertFile(fullPath, infohash string, fileIdx int, inodeValue uint64) error {
	_, err := d.db.Exec(
		`INSERT OR REPLACE INTO inodes (type, infohash, file_idx, full_path, basename, inode_value)
		 VALUES ('file', ?, ?, ?, ?, ?)`,
		infohash, fileIdx, fullPath, pathBase(fullPath), int64(inodeValue),
	)
	return err
}

// UpsertDir inserts or updates a directory inode entry.
func (d *DB) UpsertDir(relPath string, inodeValue uint64) error {
	_, err := d.db.Exec(
		`INSERT OR REPLACE INTO inodes (type, full_path, rel_path, basename, inode_value)
		 VALUES ('dir', ?, ?, ?, ?)`,
		relPath, relPath, pathBase(relPath), int64(inodeValue),
	)
	return err
}

// GetFileInode returns the inode value for a file by its full path.
func (d *DB) GetFileInode(fullPath string) (uint64, bool, error) {
	var inode uint64
	err := d.db.QueryRow(
		"SELECT inode_value FROM inodes WHERE type = 'file' AND full_path = ?",
		fullPath,
	).Scan(&inode)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return inode, true, nil
}

// GetDirInode returns the inode value for a directory by its relative path.
func (d *DB) GetDirInode(relPath string) (uint64, bool, error) {
	var inode uint64
	err := d.db.QueryRow(
		"SELECT inode_value FROM inodes WHERE type = 'dir' AND rel_path = ?",
		relPath,
	).Scan(&inode)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return inode, true, nil
}

// GetFileInodeByName returns the inode value for a file by its basename.
// Used for fastFileMap lookup. Returns the first match.
func (d *DB) GetFileInodeByName(basename string) (uint64, bool, error) {
	var inode uint64
	err := d.db.QueryRow(
		"SELECT inode_value FROM inodes WHERE type = 'file' AND basename = ? LIMIT 1",
		basename,
	).Scan(&inode)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return inode, true, nil
}

// PruneMissing deletes inode entries whose full_path is not in validFiles.
// Returns the number of entries pruned.
func (d *DB) PruneMissing(validFiles map[string]bool) (int, error) {
	if len(validFiles) == 0 {
		return 0, nil
	}

	// Build placeholders for the valid files
	placeholders := make([]interface{}, 0, len(validFiles))
	for fp := range validFiles {
		placeholders = append(placeholders, fp)
	}

	// Delete file entries not in validFiles
	query := "DELETE FROM inodes WHERE type = 'file' AND full_path NOT IN ("
	for i := 0; i < len(placeholders); i++ {
		if i > 0 {
			query += ", "
		}
		query += "?"
	}
	query += ")"

	result, err := d.db.Exec(query, placeholders...)
	if err != nil {
		return 0, err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}

	return int(rows), nil
}

// LoadAll returns all inode entries for populating in-memory maps at startup.
func (d *DB) LoadAll() ([]InodeEntry, error) {
	rows, err := d.db.Query(
		"SELECT type, infohash, file_idx, full_path, rel_path, basename, inode_value FROM inodes",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []InodeEntry
	for rows.Next() {
		var e InodeEntry
		var infohash, relPath, basename *string
		var fileIdx *int
		var rawInode int64
		if err := rows.Scan(&e.Type, &infohash, &fileIdx, &e.FullPath, &relPath, &basename, &rawInode); err != nil {
			return nil, err
		}
		e.InodeValue = uint64(rawInode) // bit pattern preserved; handles dir inodes with high bit set
		if infohash != nil {
			e.Infohash = *infohash
		}
		if fileIdx != nil {
			e.FileIdx = *fileIdx
		}
		if relPath != nil {
			e.RelPath = *relPath
		}
		if basename != nil {
			e.Basename = *basename
		}
		entries = append(entries, e)
	}

	return entries, rows.Err()
}

// pathBase returns the base name of a virtual path using path.Base.
func pathBase(p string) string {
	// Use path package for virtual paths (not filepath)
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}
