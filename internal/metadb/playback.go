package metadb

import (
	"database/sql"
	"time"
)

// PlaybackRecord represents a persisted playback state entry.
type PlaybackRecord struct {
	Path        string
	Hash        string
	ImdbID      string
	OpenedAt    time.Time
	ConfirmedAt time.Time
	IsHealthy   bool
	IsStopped   bool
	LastReadAt  time.Time
	ReadCount   int64
	LastSeekOff int64
}

// SavePlaybackState persists a playback state record to SQLite.
func (d *DB) SavePlaybackState(rec *PlaybackRecord) error {
	var openedAt, confirmedAt, lastReadAt interface{}
	if !rec.OpenedAt.IsZero() {
		openedAt = rec.OpenedAt.Format(time.RFC3339Nano)
	}
	if !rec.ConfirmedAt.IsZero() {
		confirmedAt = rec.ConfirmedAt.Format(time.RFC3339Nano)
	}
	if !rec.LastReadAt.IsZero() {
		lastReadAt = rec.LastReadAt.Format(time.RFC3339Nano)
	}
	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO playback_states
		(path, hash, imdb_id, opened_at, confirmed_at, is_healthy, is_stopped, last_read_at, read_count, last_seek_off)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Path, rec.Hash, rec.ImdbID,
		openedAt, confirmedAt,
		boolToInt(rec.IsHealthy),
		boolToInt(rec.IsStopped),
		lastReadAt,
		rec.ReadCount,
		rec.LastSeekOff,
	)
	return err
}

// LoadPlaybackStates loads all playback states newer than maxAge.
func (d *DB) LoadPlaybackStates(maxAge time.Duration) ([]*PlaybackRecord, error) {
	cutoff := time.Now().Add(-maxAge).Format(time.RFC3339Nano)
	rows, err := d.db.Query(`
		SELECT path, hash, imdb_id, opened_at, confirmed_at, is_healthy, is_stopped, last_read_at, read_count, last_seek_off
		FROM playback_states
		WHERE opened_at > ? OR opened_at IS NULL`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*PlaybackRecord
	for rows.Next() {
		rec := &PlaybackRecord{}
		var openedAt, confirmedAt, lastReadAt sql.NullString
		var isHealthy, isStopped int
		err := rows.Scan(&rec.Path, &rec.Hash, &rec.ImdbID, &openedAt, &confirmedAt, &isHealthy, &isStopped, &lastReadAt, &rec.ReadCount, &rec.LastSeekOff)
		if err != nil {
			continue
		}
		if openedAt.Valid {
			rec.OpenedAt, _ = time.Parse(time.RFC3339Nano, openedAt.String)
		}
		if confirmedAt.Valid {
			rec.ConfirmedAt, _ = time.Parse(time.RFC3339Nano, confirmedAt.String)
		}
		rec.IsHealthy = isHealthy == 1
		rec.IsStopped = isStopped == 1
		if lastReadAt.Valid {
			rec.LastReadAt, _ = time.Parse(time.RFC3339Nano, lastReadAt.String)
		}
		results = append(results, rec)
	}
	return results, rows.Err()
}

// DeletePlaybackState removes a playback state by path.
func (d *DB) DeletePlaybackState(path string) error {
	_, err := d.db.Exec("DELETE FROM playback_states WHERE path = ?", path)
	return err
}

// CleanupPlaybackStates removes entries older than maxAge.
func (d *DB) CleanupPlaybackStates(maxAge time.Duration) (int, error) {
	cutoff := time.Now().Add(-maxAge).Format(time.RFC3339Nano)
	result, err := d.db.Exec("DELETE FROM playback_states WHERE opened_at < ?", cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
