package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

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

// ReadMetadataFromFile reads metadata from a virtual .mkv file
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

// WriteMetadataToFile writes metadata to a virtual .mkv file
func WriteMetadataToFile(path, url string, size int64, imdbID string) error {
	content := fmt.Sprintf("%s\n%d\nmagnet:?xt=urn:btih:legacy\n%s\n", url, size, imdbID)
	return os.WriteFile(path, []byte(content), 0644)
}
