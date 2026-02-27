package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// V133: Deterministic Inode Mapping for Plex/SMB Stability
// V298: Sharded architecture to eliminate global lock contention.
// Generates stable inodes based on torrent content identity (infohash+index)
// instead of filename, ensuring Plex doesn't see "new files" after restarts.

const (
	// Namespace bits to avoid collisions between files and directories
	InodeFileMask = 0x7FFFFFFFFFFFFFFF // bit 63 = 0 for files
	InodeDirMask  = 0x8000000000000000 // bit 63 = 1 for directories

	// Special inodes
	InodeRoot = 1

	// Save interval
	InodeSaveInterval = 60 * time.Second

	// Default save path (unified with main.go)
	DefaultInodeMapPath = "/home/pi/STATE/inode_map.json"
)

// InodeMap manages persistent content -> inode mapping
type InodeMap struct {
	shards    [32]*inodeShard
	shardMask uint64

	// Persistence
	savePath string
	dirty    int32 // V298: Atomic dirty flag
	stopChan chan struct{}
	stopOnce sync.Once
	saveMu   sync.Mutex // Serialize SaveToDisk

	// Statistics
	hits   int64
	misses int64

	logger Logger
}

type inodeShard struct {
	mu sync.RWMutex
	// Core mappings per shard
	fileMap     map[string]uint64 // "infohash:index" -> inode
	dirMap      map[string]uint64 // "/relative/path" -> inode
	nameMap     map[string]string // "/full/path/filename.mkv" -> "infohash:index"
	fastFileMap map[string]uint64 // "filename.mkv" -> inode
}

// InodeMapData is the JSON-serializable format (Merged from all shards)
type InodeMapData struct {
	Version       int               `json:"version"`
	Files         map[string]string `json:"files"`
	Dirs          map[string]string `json:"dirs"`
	FilenameIndex map[string]string `json:"filename_index"`
	LastUpdated   string            `json:"last_updated"`
}

// InodeMapDataV1 for backward compatibility
type InodeMapDataV1 struct {
	Version       int               `json:"version"`
	Files         map[string]uint64 `json:"files"`
	Dirs          map[string]uint64 `json:"dirs"`
	FilenameIndex map[string]string `json:"filename_index"`
	LastUpdated   string            `json:"last_updated"`
}

// Logger interface for compatibility
type Logger interface {
	Printf(format string, v ...interface{})
}

// NewInodeMap creates a new sharded inode map
func NewInodeMap(savePath string, logger Logger) *InodeMap {
	im := &InodeMap{
		shardMask: 31,
		savePath:  savePath,
		stopChan:  make(chan struct{}),
		logger:    &loggerWrapper{logger},
	}

	for i := range im.shards {
		im.shards[i] = &inodeShard{
			fileMap:     make(map[string]uint64),
			dirMap:      make(map[string]uint64),
			nameMap:     make(map[string]string),
			fastFileMap: make(map[string]uint64),
		}
	}

	// Pre-populate root inode in shard 0
	im.shards[0].dirMap["/"] = InodeRoot

	return im
}

func (im *InodeMap) getShard(key string) *inodeShard {
	h := fnv.New64a()
	h.Write([]byte(key))
	return im.shards[h.Sum64()&im.shardMask]
}

func (im *InodeMap) setDirty(isDirty bool) {
	if isDirty {
		atomic.StoreInt32(&im.dirty, 1)
	} else {
		atomic.StoreInt32(&im.dirty, 0)
	}
}

// IsDirty returns whether the map has unsaved changes
func (im *InodeMap) IsDirty() bool {
	return atomic.LoadInt32(&im.dirty) == 1
}

// loggerWrapper wraps the external logger
type loggerWrapper struct {
	l Logger
}

func (w *loggerWrapper) Printf(format string, v ...interface{}) {
	if w.l != nil {
		w.l.Printf(format, v...)
	}
}

// LoadFromDisk loads the inode map and distributes entries across shards
func (im *InodeMap) LoadFromDisk() error {
	data, err := os.ReadFile(im.savePath)
	if err != nil {
		if os.IsNotExist(err) {
			im.logger.Printf("InodeMap: No existing map at %s, starting fresh", im.savePath)
			return nil
		}
		return fmt.Errorf("read inode map: %w", err)
	}

	// Internal helper to add entries correctly during load
	addLoadedFile := func(fullPath, key string, inode uint64) {
		sName := im.getShard(fullPath)
		sFile := im.getShard(key)
		sFast := im.getShard(filepath.Base(fullPath))

		sName.mu.Lock()
		sName.nameMap[fullPath] = key
		sName.mu.Unlock()

		sFile.mu.Lock()
		sFile.fileMap[key] = inode
		sFile.mu.Unlock()

		sFast.mu.Lock()
		sFast.fastFileMap[filepath.Base(fullPath)] = inode
		sFast.mu.Unlock()
	}

	addLoadedDir := func(relPath string, inode uint64) {
		s := im.getShard(relPath)
		s.mu.Lock()
		s.dirMap[relPath] = inode
		s.mu.Unlock()
	}

	var totalFiles, totalDirs int

	// First try V2 format (string values)
	var mapDataV2 InodeMapData
	if err := json.Unmarshal(data, &mapDataV2); err == nil && mapDataV2.Version == 2 {
		// Temporary reverse mapping to rebuild relationships
		tempFileMap := make(map[string]uint64)
		if mapDataV2.Files != nil {
			for k, v := range mapDataV2.Files {
				if inode, err := strconv.ParseUint(v, 10, 64); err == nil {
					tempFileMap[k] = inode
				}
			}
		}

		if mapDataV2.FilenameIndex != nil {
			for path, key := range mapDataV2.FilenameIndex {
				if inode, ok := tempFileMap[key]; ok {
					addLoadedFile(path, key, inode)
					totalFiles++
				}
			}
		}

		if mapDataV2.Dirs != nil {
			for k, v := range mapDataV2.Dirs {
				if inode, err := strconv.ParseUint(v, 10, 64); err == nil {
					addLoadedDir(k, inode)
					totalDirs++
				}
			}
		}

		im.logger.Printf("InodeMap: Loaded V2 format - %d files, %d dirs from %s",
			totalFiles, totalDirs, im.savePath)
		return nil
	}

	// Fallback to V1 format (uint64 struct)
	var mapDataV1 InodeMapDataV1
	if err := json.Unmarshal(data, &mapDataV1); err != nil {
		return fmt.Errorf("unmarshal inode map (tried V2 and V1): %w", err)
	}

	if mapDataV1.Version == 1 {
		if mapDataV1.FilenameIndex != nil {
			for path, key := range mapDataV1.FilenameIndex {
				if inode, ok := mapDataV1.Files[key]; ok {
					addLoadedFile(path, key, inode)
					totalFiles++
				}
			}
		}
		if mapDataV1.Dirs != nil {
			for k, v := range mapDataV1.Dirs {
				addLoadedDir(k, v)
				totalDirs++
			}
		}
		im.logger.Printf("InodeMap: Loaded V1 format - %d files, %d dirs from %s (will save as V2)",
			totalFiles, totalDirs, im.savePath)
		im.setDirty(true)
	}

	return nil
}

// SaveToDisk merges all shards into a single JSON file
func (im *InodeMap) SaveToDisk() error {
	im.saveMu.Lock()
	defer im.saveMu.Unlock()

	mergedFiles := make(map[string]string)
	mergedDirs := make(map[string]string)
	mergedNames := make(map[string]string)

	for _, s := range im.shards {
		s.mu.RLock()
		for k, v := range s.fileMap {
			mergedFiles[k] = strconv.FormatUint(v, 10)
		}
		for k, v := range s.dirMap {
			mergedDirs[k] = strconv.FormatUint(v, 10)
		}
		for k, v := range s.nameMap {
			mergedNames[k] = v
		}
		s.mu.RUnlock()
	}

	mapData := InodeMapData{
		Version:       2,
		Files:         mergedFiles,
		Dirs:          mergedDirs,
		FilenameIndex: mergedNames,
		LastUpdated:   time.Now().UTC().Format(time.RFC3339),
	}

	data, err := json.MarshalIndent(mapData, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal inode map: %w", err)
	}

	dir := filepath.Dir(im.savePath)
	tmpFile, err := os.CreateTemp(dir, "inode_map-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp file: %w", err)
	}
	tmpFile.Close()

	if err := os.Rename(tmpPath, im.savePath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp file: %w", err)
	}

	im.setDirty(false)
	im.logger.Printf("InodeMap: Saved %d files, %d dirs to %s", len(mergedFiles), len(mergedDirs), im.savePath)
	return nil
}

func (im *InodeMap) StartBackgroundSaver() {
	go func() {
		ticker := time.NewTicker(InodeSaveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if im.IsDirty() {
					im.SaveToDisk()
				}
			case <-im.stopChan:
				if im.IsDirty() {
					im.SaveToDisk()
				}
				return
			}
		}
	}()
}

func (im *InodeMap) Stop() {
	im.stopOnce.Do(func() {
		close(im.stopChan)
		if im.IsDirty() {
			im.SaveToDisk()
		}
	})
}

func (im *InodeMap) AddFile(fullPath, infohash string, index int) uint64 {
	if infohash == "" {
		return 0
	}

	key := fmt.Sprintf("%s:%d", strings.ToLower(infohash), index)
	inode := generateFileInode(infohash, index)

	sName := im.getShard(fullPath)
	sFile := im.getShard(key)
	sFast := im.getShard(filepath.Base(fullPath))

	sName.mu.Lock()
	if existingKey, ok := sName.nameMap[fullPath]; ok && existingKey == key {
		sName.mu.Unlock()
		return inode
	}
	sName.nameMap[fullPath] = key
	sName.mu.Unlock()

	sFile.mu.Lock()
	sFile.fileMap[key] = inode
	sFile.mu.Unlock()

	sFast.mu.Lock()
	sFast.fastFileMap[filepath.Base(fullPath)] = inode
	sFast.mu.Unlock()

	im.setDirty(true)
	return inode
}

func (im *InodeMap) AddDir(relativePath string) uint64 {
	inode := generateDirInode(relativePath)
	s := im.getShard(relativePath)

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.dirMap[relativePath]; !ok {
		s.dirMap[relativePath] = inode
		im.setDirty(true)
	}
	return inode
}

func (im *InodeMap) GetFileInode(fullPath string) uint64 {
	sName := im.getShard(fullPath)
	sName.mu.RLock()
	key, ok := sName.nameMap[fullPath]
	sName.mu.RUnlock()

	if ok {
		sFile := im.getShard(key)
		sFile.mu.RLock()
		inode, ok2 := sFile.fileMap[key]
		sFile.mu.RUnlock()
		if ok2 {
			atomic.AddInt64(&im.hits, 1)
			return inode
		}
	}

	atomic.AddInt64(&im.misses, 1)
	return 0
}

func (im *InodeMap) GetFileInodeByName(filename string) uint64 {
	s := im.getShard(filename)
	s.mu.RLock()
	defer s.mu.RUnlock()

	if inode, ok := s.fastFileMap[filename]; ok {
		atomic.AddInt64(&im.hits, 1)
		return inode
	}
	atomic.AddInt64(&im.misses, 1)
	return 0
}

func (im *InodeMap) GetDirInode(relativePath string) uint64 {
	if relativePath == "/" || relativePath == "" {
		return InodeRoot
	}

	s := im.getShard(relativePath)
	s.mu.RLock()
	inode, ok := s.dirMap[relativePath]
	s.mu.RUnlock()

	if ok {
		return inode
	}
	return im.AddDir(relativePath)
}

func (im *InodeMap) RemoveFile(fullPath string) {
	sName := im.getShard(fullPath)
	sName.mu.Lock()
	key, ok := sName.nameMap[fullPath]
	if ok {
		delete(sName.nameMap, fullPath)
		sName.mu.Unlock()

		sFile := im.getShard(key)
		sFile.mu.Lock()
		targetInode := sFile.fileMap[key]
		delete(sFile.fileMap, key)
		sFile.mu.Unlock()

		filename := filepath.Base(fullPath)
		sFast := im.getShard(filename)
		sFast.mu.Lock()
		if currentInode, exists := sFast.fastFileMap[filename]; exists && currentInode == targetInode {
			delete(sFast.fastFileMap, filename)
		}
		sFast.mu.Unlock()

		im.setDirty(true)
	} else {
		sName.mu.Unlock()
	}
}

func (im *InodeMap) Stats() (files, dirs, hits, misses int64) {
	for _, s := range im.shards {
		s.mu.RLock()
		files += int64(len(s.fileMap))
		dirs += int64(len(s.dirMap))
		s.mu.RUnlock()
	}
	hits = atomic.LoadInt64(&im.hits)
	misses = atomic.LoadInt64(&im.misses)
	return
}

func (im *InodeMap) PruneMissing(foundFiles map[string]bool) int {
	var toRemove []string

	// Pass 1: Identify under read locks
	for _, s := range im.shards {
		s.mu.RLock()
		for fullPath := range s.nameMap {
			if !foundFiles[fullPath] {
				toRemove = append(toRemove, fullPath)
			}
		}
		s.mu.RUnlock()
	}

	// Pass 2: Remove using existing logic
	for _, path := range toRemove {
		im.RemoveFile(path)
	}

	if len(toRemove) > 0 {
		im.setDirty(true)
		im.logger.Printf("InodeMap GC: Pruned %d ghost entries", len(toRemove))
	}
	return len(toRemove)
}

// --- Helper functions for extracting hash and index from URLs ---

func generateFileInode(infohash string, index int) uint64 {
	key := fmt.Sprintf("%s:%d", strings.ToLower(infohash), index)
	h := fnv.New64a()
	h.Write([]byte(key))
	return h.Sum64() & InodeFileMask
}

func generateDirInode(relativePath string) uint64 {
	if relativePath == "/" || relativePath == "" {
		return InodeRoot
	}
	h := fnv.New64a()
	h.Write([]byte(relativePath))
	return h.Sum64() | InodeDirMask
}

var (
	hashPattern  = regexp.MustCompile(`link=([a-fA-F0-9]{40})`)
	indexPattern = regexp.MustCompile(`index=(\d+)`)
)

// ExtractHashAndIndex extracts the torrent hash and file index from a stream URL
func ExtractHashAndIndex(url string) (string, int) {
	hash := ""
	index := 0

	if matches := hashPattern.FindStringSubmatch(url); len(matches) > 1 {
		hash = strings.ToLower(matches[1])
	}

	if matches := indexPattern.FindStringSubmatch(url); len(matches) > 1 {
		if idx, err := strconv.Atoi(matches[1]); err == nil {
			index = idx
		}
	}

	return hash, index
}

// --- Global inode map instance ---

var globalInodeMap *InodeMap

func InitGlobalInodeMap(savePath string, logger Logger) error {
	dir := filepath.Dir(savePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create inode map directory: %w", err)
	}

	globalInodeMap = NewInodeMap(savePath, logger)
	if err := globalInodeMap.LoadFromDisk(); err != nil {
		return fmt.Errorf("load inode map: %w", err)
	}
	globalInodeMap.StartBackgroundSaver()
	return nil
}

func ShutdownGlobalInodeMap() {
	if globalInodeMap != nil {
		globalInodeMap.Stop()
	}
}

func getFileInodeFromMap(fullPath string) uint64 {
	if globalInodeMap == nil {
		return hashFilenameToInode(filepath.Base(fullPath)) & InodeFileMask
	}
	if inode := globalInodeMap.GetFileInode(fullPath); inode != 0 {
		return inode
	}
	filename := filepath.Base(fullPath)
	if inode := globalInodeMap.GetFileInodeByName(filename); inode != 0 {
		return inode
	}
	return hashFilenameToInode(filename) & InodeFileMask
}

func getDirInodeFromMap(relativePath string) uint64 {
	if globalInodeMap == nil {
		return generateDirInode(relativePath)
	}
	return globalInodeMap.GetDirInode(relativePath)
}

func addFileToInodeMap(fullPath, url string) uint64 {
	if globalInodeMap == nil {
		return 0
	}
	hash, index := ExtractHashAndIndex(url)
	if hash == "" {
		return 0
	}
	return globalInodeMap.AddFile(fullPath, hash, index)
}

func GetInodeMapStats() (files, dirs, hits, misses int64) {
	if globalInodeMap == nil {
		return 0, 0, 0, 0
	}
	return globalInodeMap.Stats()
}

func GetInodeMapSavePath() string {
	return filepath.Join(GetStateDir(), "inode_map.json")
}

func EnsureInodeMapDir() error {
	dir := filepath.Dir(GetInodeMapSavePath())
	return os.MkdirAll(dir, 0755)
}
