package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// TorrentRemover handles automatic torrent removal from GoStorm
// Extracts hash directly from .mkv file content and handles blacklisting
// TorrentRemover handles automatic torrent removal from GoStorm
// Extracts hash directly from .mkv file content and handles blacklisting
type TorrentRemover struct {
	nativeBridge *NativeClient // V160: Native Bridge
	logger       *log.Logger
	hashPattern  *regexp.Regexp // Still useful for fallback
}

// NewTorrentRemover creates a new torrent remover
func NewTorrentRemover(nativeBridge *NativeClient, logger *log.Logger) *TorrentRemover {
	return &TorrentRemover{
		nativeBridge: nativeBridge,
		logger:       logger,
		hashPattern:  regexp.MustCompile(`([a-f0-9]{40})`),
	}
}

// RemoveTorrentFromFile extracts hash from file and removes torrent
func (tr *TorrentRemover) RemoveTorrentFromFile(fullPath string) (bool, error) {
	filename := filepath.Base(fullPath)
	title := tr.deriveTitleFromPath(fullPath)

	// V271: Extract hash from filename suffix FIRST (zero I/O, no FUSE access).
	// Previously read the file content first, which could deadlock if the file
	// was being streamed via FUSE (smbd holds read lock → Unlink blocks).
	var fullHash string
	suffixPattern := regexp.MustCompile(`_([a-f0-9]{8})\.mkv$`)
	if matches := suffixPattern.FindStringSubmatch(filename); len(matches) > 1 {
		fullHash, _, _ = tr.findFullHashBySuffix(matches[1])
	}

	// Fallback: read hash from file content (only if suffix lookup failed)
	if fullHash == "" {
		fullHash = tr.extractHashFromFile(fullPath)
	}

	// CRITICAL FIX: Ensure hash is valid and 40 chars before proceeding
	if len(fullHash) != 40 {
		return false, fmt.Errorf("invalid or missing hash (%s) for: %s", fullHash, filename)
	}

	tr.logger.Printf("AutoRemove: identified hash %s for %s", fullHash[:8], title)

	// 2. ALWAYS add to blacklist (FASE 5.1)
	tr.addToBlacklist(fullHash, title)

	// 3. Attempt to remove from GoStorm
	err := tr.removeTorrent(fullHash)
	if err != nil {
		tr.logger.Printf("AutoRemove: removal from GoStorm failed for %s: %v", fullHash[:8], err)
		return false, nil // Return false but no error, as blacklist worked
	}

	return true, nil
}

// RemoveTorrentByHash allows direct removal by hash (exposed for testing/manual use)
func (tr *TorrentRemover) RemoveTorrentByHash(hash string) error {
	return tr.removeTorrent(hash)
}

// extractHashFromFile reads the first line of the virtual mkv to get the hash
func (tr *TorrentRemover) extractHashFromFile(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if scanner.Scan() {
		line := scanner.Text()
		// Search for 40-char hex string (link=HASH...)
		match := tr.hashPattern.FindString(line)
		return strings.ToLower(match)
	}
	return ""
}

// deriveTitleFromPath gets the show or movie name from the directory structure
func (tr *TorrentRemover) deriveTitleFromPath(path string) string {
	// Example: /mnt/torrserver/tv/9-1-1_Nashville (2025)/Season.01/file.mkv
	// We want "9-1-1_Nashville (2025)"
	parts := strings.Split(filepath.Dir(path), string(os.PathSeparator))

	// Default to filename if path is shallow
	if len(parts) < 2 {
		return filepath.Base(path)
	}

	// If it's a TV show (contains "Season.XX"), take the parent
	lastDir := parts[len(parts)-1]
	if strings.HasPrefix(lastDir, "Season.") && len(parts) >= 2 {
		return parts[len(parts)-2]
	}

	return lastDir
}

// addToBlacklist adds a hash and title to the persistent blacklist file
func (tr *TorrentRemover) addToBlacklist(hash, title string) {
	// blacklistPath := "/home/pi/STATE/blacklist.json"
	blacklistPath := filepath.Join(GetStateDir(), "blacklist.json")

	type Blacklist struct {
		Hashes map[string]string `json:"hashes"`
		Titles []string          `json:"titles"`
	}

	bl := Blacklist{Hashes: make(map[string]string), Titles: []string{}}

	// Read existing
	if data, err := ioutil.ReadFile(blacklistPath); err == nil {
		json.Unmarshal(data, &bl)
	}

	changed := false
	if _, exists := bl.Hashes[hash]; !exists {
		bl.Hashes[hash] = title
		changed = true
	}

	// SUPER-CLEAN Title Normalization (for robust matching)
	if title != "" {
		// 1. Convert to lowercase
		cleanTitle := strings.ToLower(title)
		// 2. Replace underscores and dots with spaces
		cleanTitle = strings.ReplaceAll(cleanTitle, "_", " ")
		cleanTitle = strings.ReplaceAll(cleanTitle, ".", " ")
		// 3. Remove year (e.g., "(2023)" or "2023")
		yearRegex := regexp.MustCompile(`\(?\d{4}\)?`)
		cleanTitle = yearRegex.ReplaceAllString(cleanTitle, "")
		// 4. Remove ALL non-alphanumeric characters AND spaces (SQUEEZE everything)
		// This ensures "Grey's Anatomy" becomes "greysanatomy" and "9-1-1" becomes "911"
		nonAlphaRegex := regexp.MustCompile(`[^a-z0-9]`)
		cleanTitle = nonAlphaRegex.ReplaceAllString(cleanTitle, "")

		if cleanTitle != "" {

			found := false
			for _, t := range bl.Titles {
				if t == cleanTitle {
					found = true
					break
				}
			}
			if !found {
				bl.Titles = append(bl.Titles, cleanTitle)
				changed = true
			}
		}
	}

	if changed {
		if newData, err := json.MarshalIndent(bl, "", "  "); err == nil {
			tmpPath := blacklistPath + ".tmp"
			if err := ioutil.WriteFile(tmpPath, newData, 0644); err == nil {
				os.Rename(tmpPath, blacklistPath)
				tr.logger.Printf("BLACKLISTED: %s (%s)", hash[:8], title)
			}
		}
	}
}

// findFullHashBySuffix (Fallback logic)
func (tr *TorrentRemover) findFullHashBySuffix(suffix string) (string, string, error) {
	// Use Native Bridge to list torrents (O(N) operation but in-memory so fast)
	torrents, err := tr.nativeBridge.ListTorrents()
	if err != nil {
		return "", "", err
	}

	for _, t := range torrents {
		if strings.HasSuffix(strings.ToLower(t.Hash), strings.ToLower(suffix)) {
			return t.Hash, t.Title, nil
		}
	}
	return "", "", nil
}

// removeTorrent calls GoStorm API to remove a torrent
func (tr *TorrentRemover) removeTorrent(hash string) error {
	// Use Native Bridge to remove torrent
	tr.nativeBridge.RemoveTorrent(hash)

	if globalSyncCacheManager != nil {
		globalSyncCacheManager.ClearNegativeCache(hash)
		globalSyncCacheManager.ClearFullpackCache(hash)
	}
	return nil
}
