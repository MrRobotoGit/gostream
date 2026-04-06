package metadb

import (
	"database/sql"
	"errors"
)

// EpisodeEntry represents a TV episode registry entry.
type EpisodeEntry struct {
	EpisodeKey   string
	QualityScore int
	Hash         string
	FilePath     string
	Source       string
	Created      int64 // unix timestamp
}

// UpsertEpisode inserts or updates an episode entry.
func (d *DB) UpsertEpisode(key string, entry EpisodeEntry) error {
	_, err := d.db.Exec(
		`INSERT OR REPLACE INTO tv_episodes
		 (episode_key, quality_score, hash, file_path, source, created)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		key, entry.QualityScore, entry.Hash, entry.FilePath, entry.Source, entry.Created,
	)
	return err
}

// GetEpisode returns a single episode by its key.
func (d *DB) GetEpisode(key string) (*EpisodeEntry, bool, error) {
	var e EpisodeEntry
	err := d.db.QueryRow(
		"SELECT episode_key, quality_score, hash, file_path, source, created FROM tv_episodes WHERE episode_key = ?",
		key,
	).Scan(&e.EpisodeKey, &e.QualityScore, &e.Hash, &e.FilePath, &e.Source, &e.Created)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &e, true, nil
}

// DeleteEpisode deletes an episode by its key.
func (d *DB) DeleteEpisode(key string) error {
	_, err := d.db.Exec("DELETE FROM tv_episodes WHERE episode_key = ?", key)
	return err
}

// AllEpisodes returns all episode entries.
func (d *DB) AllEpisodes() ([]EpisodeEntry, error) {
	rows, err := d.db.Query(
		"SELECT episode_key, quality_score, hash, file_path, source, created FROM tv_episodes",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []EpisodeEntry
	for rows.Next() {
		var e EpisodeEntry
		if err := rows.Scan(&e.EpisodeKey, &e.QualityScore, &e.Hash, &e.FilePath, &e.Source, &e.Created); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}

	return entries, rows.Err()
}

// EpisodesByFilePath returns all episodes that reference a given file path.
// Used for cleanup by file existence.
func (d *DB) EpisodesByFilePath(filePath string) ([]EpisodeEntry, error) {
	rows, err := d.db.Query(
		"SELECT episode_key, quality_score, hash, file_path, source, created FROM tv_episodes WHERE file_path = ?",
		filePath,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []EpisodeEntry
	for rows.Next() {
		var e EpisodeEntry
		if err := rows.Scan(&e.EpisodeKey, &e.QualityScore, &e.Hash, &e.FilePath, &e.Source, &e.Created); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}

	return entries, rows.Err()
}
