package vfs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Metadata is the in-memory representation used by the cache layer.
type Metadata struct {
	URL, Path, ImdbID string
	Size              int64
	Mtime             time.Time
}

// FileMetadata represents metadata extracted from a virtual .mkv file
type FileMetadata struct {
	URL    string    // Stream URL from line 1
	Size   int64     // File size in bytes from line 2
	Mtime  time.Time // File modification time
	Path   string    // Original file path
	ImdbID string    // IMDB ID from line 4 (optional)
}

// Validation constants
const (
	MinFileSize = 100 * 1024 * 1024        // 100 MB minimum
	MaxFileSize = 100 * 1024 * 1024 * 1024 // 100 GB maximum
)

// Error definitions
var (
	ErrInvalidSize   = errors.New("file size outside valid range (100MB-100GB)")
	ErrInvalidFormat = errors.New("invalid .mkv file format")
	ErrMissingURL    = errors.New("missing stream URL on line 1")
	ErrMissingSize   = errors.New("missing file size on line 2")
	ErrInvalidURL    = errors.New("stream URL must start with http:// or https://")
)

// MkvJSON is the internal JSON representation of a .mkv file.
type MkvJSON struct {
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	Magnet string `json:"magnet"`
	Imdb   string `json:"imdb"`
}

// ReadMetadataFromFile reads metadata from a virtual .mkv file.
// Supports both JSON (new) and line-based (legacy) formats.
func ReadMetadataFromFile(path string) (*FileMetadata, error) {
	// Get file info for mtime
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat file: %w", err)
	}

	// Read entire file (small virtual file)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}

	content := string(data)
	trimmed := strings.TrimSpace(content)

	// Detect JSON format
	if strings.HasPrefix(trimmed, "{") {
		return parseJSONFormat(trimmed, info, path)
	}

	// Legacy line-based format
	return parseLineFormat(content, info, path)
}

func parseJSONFormat(content string, info os.FileInfo, path string) (*FileMetadata, error) {
	var j MkvJSON
	if err := json.Unmarshal([]byte(content), &j); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}

	url := strings.TrimSpace(j.URL)
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, ErrInvalidURL
	}

	if j.Size < MinFileSize || j.Size > MaxFileSize {
		return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidSize, j.Size)
	}

	imdbID := j.Imdb
	if imdbID == "" {
		reID := regexp.MustCompile(`tt\d{7,10}`)
		imdbID = reID.FindString(content)
	}

	return &FileMetadata{
		URL:    url,
		Size:   j.Size,
		Mtime:  info.ModTime(),
		Path:   path,
		ImdbID: imdbID,
	}, nil
}

func parseLineFormat(content string, info os.FileInfo, path string) (*FileMetadata, error) {
	lines := strings.Split(content, "\n")
	if len(lines) < 2 {
		return nil, ErrInvalidFormat
	}

	// Line 1: Stream URL
	url := strings.TrimSpace(lines[0])
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, ErrInvalidURL
	}

	// Line 2: File size
	sizeStr := strings.TrimSpace(lines[1])
	size, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse size: %w", err)
	}

	// Validate size range (100MB - 100GB)
	if size < MinFileSize || size > MaxFileSize {
		return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidSize, size)
	}

	// Line 4: IMDB ID (optional, index 3)
	imdbID := ""
	if len(lines) >= 4 {
		idCandidate := strings.TrimSpace(lines[3])
		if strings.HasPrefix(idCandidate, "tt") {
			imdbID = idCandidate
		}
	}

	// Fallback regex (just in case there are extra lines or slightly different format)
	if imdbID == "" {
		reID := regexp.MustCompile(`tt\d{7,10}`)
		imdbID = reID.FindString(content)
	}

	return &FileMetadata{
		URL:    url,
		Size:   size,
		Mtime:  info.ModTime(),
		Path:   path,
		ImdbID: imdbID,
	}, nil
}

// WriteMetadataToFile writes metadata to a virtual .mkv file in JSON format.
func WriteMetadataToFile(path, url string, size int64, imdbID string) error {
	j := MkvJSON{
		URL:    url,
		Size:   size,
		Magnet: "",
		Imdb:   imdbID,
	}
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
