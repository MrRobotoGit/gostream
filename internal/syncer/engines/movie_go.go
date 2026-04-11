package engines

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"gostream/internal/catalog"
	"gostream/internal/catalog/tmdb"
	"gostream/internal/catalog/torrentio"
	"gostream/internal/prowlarr"
)

// MovieGoEngine is the pure Go implementation of movie sync.
type MovieGoEngine struct {
	gostorm   *GoStormClient
	tmdb      *tmdb.Client
	torrentio *torrentio.Client
	prowlarr  *prowlarr.Client
	plexURL   string
	plexToken string
	plexLib   int
	moviesDir string
	stateDir  string
	limiter   *rate.Limiter
	logger    *log.Logger

	// Negative caches
	noMKVCache     map[string]CacheEntry
	noStreamsCache map[string]CacheEntry
	recheckCache   map[string]CacheEntry
	addFailCache   map[string]CacheEntry
	imdbCache      map[string]IMDBCacheEntry
	noMKVCFile     string
	noStreamsCFile string
	recheckCFile   string
	addFailCFile   string
	imdbCFile      string

	blacklist     BlacklistData
	blacklistFile string
}

// CacheEntry is a generic cache entry with timestamp.
type CacheEntry struct {
	Reason string `json:"reason,omitempty"`
	Title  string `json:"title,omitempty"`
	TS     int64  `json:"ts"`
}

// IMDBCacheEntry caches TMDB→IMDB mapping.
type IMDBCacheEntry struct {
	IMDBID string `json:"imdb_id"`
	Title  string `json:"title"`
	TS     int64  `json:"ts"`
}

// BlacklistData holds blocked hashes and titles.
type BlacklistData struct {
	Hashes map[string]string `json:"hashes,omitempty"`
	Titles []string          `json:"titles,omitempty"`
}

// MovieEngineConfig holds config for the movie engine.
type MovieEngineConfig struct {
	GoStormURL   string
	TMDBAPIKey   string
	TorrentioURL string
	PlexURL      string
	PlexToken    string
	PlexLib      int
	MoviesDir    string
	StateDir     string
	LogsDir      string
	ProwlarrCfg  prowlarr.ConfigProwlarr
}

// Movie thresholds
const (
	mMovie4KBase         = 1000
	mMovie1080pBase      = 200
	mMovieHDRBonus       = 60
	mMovieDVBonus        = 100
	mMovieAtmosBonus     = 50
	mMovie51Bonus        = 25
	mMovieStereoPenalty  = -50
	mMovieRemuxBonus     = 30
	mMovieITABonus       = 60
	mMovieUnknownPenalty = -5
	mMovieMinSeeders     = 15
	mMovie4KMinGB        = 10
	mMovie4KMaxGB        = 60
	mMovie1080PMinGB     = 4
	mMovie1080PMaxGB     = 20
	mMovieUpgradePct     = 1.1
	mMovieProcessSleep   = 1 * time.Second
	mMovieMetadataWait   = 12
	mMovie4KMetadataWait = 45
	noMKVCacheTTL        = 12 * time.Hour
	noStreamsCacheTTL    = 24 * time.Hour
	recheckCacheTTL      = 48 * time.Hour
	addFailCacheTTL      = 168 * time.Hour
)

var (
	reM4K        = regexp.MustCompile(`(?i)2160p|4[kK]|uhd`)
	reM1080p     = regexp.MustCompile(`(?i)1080p|1080i|fhd`)
	reM720p      = regexp.MustCompile(`(?i)720p|720i`)
	reMHDR       = regexp.MustCompile(`(?i)\bhdr\b|hdr10\+?`)
	reMDV        = regexp.MustCompile(`(?i)\bdv\b|dovi|dolby.?vision`)
	reMAtmos     = regexp.MustCompile(`(?i)atmos`)
	reM51        = regexp.MustCompile(`(?i)5\.1|dts|ddp5|ddp|dd\+|eac3|ac3`)
	reMStereo    = regexp.MustCompile(`(?i)stereo|aac|mp3|2\.0`)
	reMRemux     = regexp.MustCompile(`(?i)\bremux\b`)
	reMITA       = regexp.MustCompile(`(?i)\bita\b|🇮🇹`)
	reMExclLang  = regexp.MustCompile(`🇪🇸|🇫🇷|🇩🇪|🇷🇺|🇨🇳|🇯🇵|🇰🇷|🇹🇭|🇵🇹|🇧🇷`)
	reMGarbage   = regexp.MustCompile(`(?i)camrip|hdcam|hdts|telesync|\bts\b|telecine|\btc\b|\bscr\b|screener|webscreener`)
	reMSeeders   = regexp.MustCompile(`👤\s*(\d+)`)
	reMHashURL   = regexp.MustCompile(`link=([a-f0-9]{40})`)
	reMMKVHash8  = regexp.MustCompile(`_([a-f0-9]{8})\.mkv$`)
	reMYear      = regexp.MustCompile(`[._]((?:19|20)\d{2})[._]`)
	reMNonWord   = regexp.MustCompile(`[^a-z0-9]`)
	reMQuality   = regexp.MustCompile(`(?i)(2160p|1080p|720p|4k|uhd)`)
	reMTitleYear = regexp.MustCompile(`(.+?)[._\s]\(?((?:19|20)\d{2})\)?`)
)

// NewMovieGoEngine creates a new Go movie sync engine.
func NewMovieGoEngine(cfg MovieEngineConfig) *MovieGoEngine {
	var prowlarrClient *prowlarr.Client
	if cfg.ProwlarrCfg.Enabled {
		prowlarrClient = prowlarr.NewClient(cfg.ProwlarrCfg)
	}

	logPath := filepath.Join(cfg.LogsDir, "movies-sync.log")
	logFile, _ := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	logger := log.New(io.MultiWriter(os.Stdout, logFile), "[MovieSync] ", log.LstdFlags)

	e := &MovieGoEngine{
		gostorm:   NewGoStormClient(cfg.GoStormURL),
		tmdb:      tmdb.NewClient(cfg.TMDBAPIKey),
		torrentio: torrentio.NewClient(cfg.TorrentioURL, "sort=qualitysize|qualityfilter=480p,720p,scr,cam"),
		prowlarr:  prowlarrClient,
		plexURL:   cfg.PlexURL,
		plexToken: cfg.PlexToken,
		plexLib:   cfg.PlexLib,
		moviesDir: cfg.MoviesDir,
		stateDir:  cfg.StateDir,
		limiter:   rate.NewLimiter(rate.Every(250*time.Millisecond), 1),
		logger:    logger,

		noMKVCFile:     filepath.Join(cfg.StateDir, "no_mkv_hashes.json"),
		noStreamsCFile: filepath.Join(cfg.StateDir, "movie_no_streams_cache.json"),
		recheckCFile:   filepath.Join(cfg.StateDir, "movie_recheck_cache.json"),
		addFailCFile:   filepath.Join(cfg.StateDir, "movie_add_fail_cache.json"),
		imdbCFile:      filepath.Join(cfg.StateDir, "movie_imdb_cache.json"),
		blacklistFile:  filepath.Join(cfg.StateDir, "blacklist.json"),
	}

	e.noMKVCache = e.loadCache(e.noMKVCFile)
	e.noStreamsCache = e.loadCache(e.noStreamsCFile)
	e.recheckCache = e.loadCache(e.recheckCFile)
	e.addFailCache = e.loadCache(e.addFailCFile)
	e.imdbCache = e.loadIMDBCache(e.imdbCFile)
	e.blacklist = e.loadBlacklist()

	e.pruneExpiredCaches()

	return e
}

func (e *MovieGoEngine) Name() string { return "movies" }

func (e *MovieGoEngine) Run(ctx context.Context) error {
	e.logger.Printf("[MovieSync] Starting discovery...")
	movies, err := e.discoverMovies(ctx)
	if err != nil {
		return fmt.Errorf("discover movies: %w", err)
	}
	e.logger.Printf("[MovieSync] Discovered %d movies", len(movies))

	existingIndex, diskHashes := e.buildExistingMovieIndex()
	e.logger.Printf("[MovieSync] Existing index: %d movies, %d hashes on disk", len(existingIndex), len(diskHashes))

	created := 0
	for i, m := range movies {
		select {
		case <-ctx.Done():
			e.logger.Printf("[MovieSync] Stopped after %d/%d movies (%d created)", i, len(movies), created)
			return ctx.Err()
		default:
		}

		if e.processMovie(ctx, m, existingIndex, diskHashes) {
			created++
		}
		time.Sleep(mMovieProcessSleep)
	}

	e.logger.Printf("[MovieSync] Processing complete: %d created out of %d discovered", created, len(movies))
	e.saveAllCaches()
	e.rehydrateMissingTorrents(ctx)
	e.cleanupOrphanedFiles(ctx)

	if e.plexLib > 0 && e.plexURL != "" && e.plexToken != "" {
		url := fmt.Sprintf("%s/library/sections/%d/refresh?X-Plex-Token=%s", e.plexURL, e.plexLib, e.plexToken)
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		client := catalog.NewClient(10 * time.Second)
		resp, err := catalog.Do(context.Background(), client, req)
		if err == nil {
			resp.Body.Close()
		}
	}

	return nil
}

func (e *MovieGoEngine) discoverMovies(ctx context.Context) ([]tmdb.Movie, error) {
	cutoff := time.Now().AddDate(0, -6, 0).Format("2006-01-02")
	currentYear := time.Now().Year() + 1
	dateLTE := fmt.Sprintf("%d-12-31", currentYear)

	var all []tmdb.Movie
	seen := make(map[int]bool)

	endpoints := []func(context.Context) ([]tmdb.Movie, error){
		func(ctx context.Context) ([]tmdb.Movie, error) {
			return e.tmdb.DiscoverMovies(ctx, "en", cutoff, dateLTE, 12)
		},
		func(ctx context.Context) ([]tmdb.Movie, error) {
			return e.tmdb.DiscoverMovies(ctx, "it", cutoff, dateLTE, 3)
		},
		func(ctx context.Context) ([]tmdb.Movie, error) {
			return e.tmdb.DiscoverMoviesByRegion(ctx, "/movie/now_playing", "US", 1)
		},
		func(ctx context.Context) ([]tmdb.Movie, error) {
			return e.tmdb.DiscoverMoviesByRegion(ctx, "/movie/now_playing", "GB", 1)
		},
		func(ctx context.Context) ([]tmdb.Movie, error) {
			return e.tmdb.DiscoverMoviesByRegion(ctx, "/movie/popular", "US", 3)
		},
		func(ctx context.Context) ([]tmdb.Movie, error) {
			return e.tmdb.TrendingMovies(ctx, 1)
		},
	}

	for _, fn := range endpoints {
		movies, err := fn(ctx)
		if err != nil {
			continue
		}
		for _, m := range movies {
			if !seen[m.ID] {
				seen[m.ID] = true
				all = append(all, m)
			}
		}
	}

	return all, nil
}

type movieFile struct {
	path  string
	imdb  string
	score int
}

func (e *MovieGoEngine) buildExistingMovieIndex() (map[string]movieFile, map[string]bool) {
	index := make(map[string]movieFile)
	diskHashes := make(map[string]bool)
	if _, err := os.Stat(e.moviesDir); err != nil {
		return index, diskHashes
	}

	filepath.Walk(e.moviesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !strings.HasSuffix(strings.ToLower(path), ".mkv") {
			return nil
		}
		// Collect hash8 from filename (last 8 hex chars before .mkv)
		if m := reMMKVHash8.FindStringSubmatch(info.Name()); len(m) >= 2 {
			diskHashes[strings.ToLower(m[1])] = true
		}
		data, err := os.ReadFile(path)
		if err != nil || len(data) > 10240 {
			return nil
		}

		var imdb string
		content := strings.TrimSpace(string(data))

		// Try JSON format first (new Go format)
		if strings.HasPrefix(content, "{") {
			var obj map[string]interface{}
			if err := json.Unmarshal([]byte(content), &obj); err == nil {
				imdb, _ = obj["imdb"].(string)
			}
		} else {
			// Text format (old Python format): line 4 = IMDB ID
			lines := strings.SplitN(content, "\n", 4)
			if len(lines) >= 4 {
				imdb = strings.TrimSpace(lines[3])
			}
		}

		if imdb == "" {
			return nil
		}
		score := e.calculateMovieScore(info.Name(), 0, 0, reM4K.MatchString(info.Name()))
		if existing, ok := index[imdb]; !ok || score > existing.score {
			index[imdb] = movieFile{path: path, imdb: imdb, score: score}
		}
		return nil
	})

	return index, diskHashes
}

func (e *MovieGoEngine) processMovie(ctx context.Context, movie tmdb.Movie, existingIndex map[string]movieFile, diskHashes map[string]bool) bool {
	title := movie.Title
	if title == "" {
		title = movie.OriginalTitle
	}
	if title == "" {
		return false
	}

	// Blacklist check
	if e.isBlacklisted(title) {
		return false
	}

	// Resolve IMDB
	imdbID := e.resolveIMDB(ctx, movie.ID, title)
	if imdbID == "" {
		return false
	}

	// Check negative caches
	if e.isInCache(e.noStreamsCache, imdbID, noStreamsCacheTTL) {
		return false
	}
	if e.isInCache(e.recheckCache, imdbID, recheckCacheTTL) {
		return false
	}
	if e.isInCache(e.addFailCache, imdbID, addFailCacheTTL) {
		return false
	}

	// Get streams
	e.logger.Printf("[MovieSync] Processing: %s (%s)", title, imdbID)
	streams, err := e.getMovieStreams(ctx, imdbID, title)
	if err != nil || len(streams) == 0 {
		e.setCache(e.noStreamsCache, imdbID, CacheEntry{Title: title, TS: time.Now().Unix()})
		return false
	}
	delete(e.noStreamsCache, imdbID)

	// Filter: 4K first, then 1080p fallback
	candidates := e.filterMovieStreams(streams)
	if len(candidates) == 0 {
		e.setCache(e.recheckCache, imdbID, CacheEntry{Title: title, Reason: "no_valid_stream", TS: time.Now().Unix()})
		return false
	}

	// Check if we already have this movie
	existing := existingIndex[imdbID]
	existingPath := existing.path
	existingScore := existing.score

	// Try candidates
	for _, c := range candidates {
		if existingPath != "" && float64(c.QualityScore) <= float64(existingScore)*mMovieUpgradePct {
			e.setCache(e.recheckCache, imdbID, CacheEntry{Title: title, Reason: "no_better_stream", TS: time.Now().Unix()})
			return false
		}

		if e.isInCache(e.noMKVCache, c.Hash, noMKVCacheTTL) {
			continue
		}

		if diskHashes[c.Hash[len(c.Hash)-8:]] {
			continue
		}

		magnet := BuildMagnet(c.Hash, title, DefaultTrackers())
		hash, err := e.gostorm.AddTorrent(ctx, magnet, title)
		if err != nil || hash == "" {
			e.setCache(e.addFailCache, imdbID, CacheEntry{Title: title, Reason: "add_failed", TS: time.Now().Unix()})
			continue
		}
		delete(e.addFailCache, imdbID)

		maxWait := mMovieMetadataWait
		if c.Is4K {
			maxWait = mMovie4KMetadataWait
		}

		info, err := e.gostorm.GetTorrentInfo(ctx, hash, maxWait)
		if err != nil {
			e.setCache(e.noMKVCache, hash, CacheEntry{Reason: "metadata_timeout", TS: time.Now().Unix()})
			e.gostorm.RemoveTorrent(ctx, hash)
			continue
		}

		videoFiles := e.filterVideoFiles(info.FileStats, c.Is4K)
		if len(videoFiles) == 0 {
			e.setCache(e.noMKVCache, hash, CacheEntry{Reason: "no_valid_files", TS: time.Now().Unix()})
			e.gostorm.RemoveTorrent(ctx, hash)
			continue
		}

		// Take largest
		sort.Slice(videoFiles, func(i, j int) bool {
			return videoFiles[i].Length > videoFiles[j].Length
		})
		bestFile := videoFiles[0]

		// Remove existing if upgrading
		if existingPath != "" {
			e.logger.Printf("[MovieSync] Upgrade: removing %s", filepath.Base(existingPath))
			os.Remove(existingPath)
		}

		filename := e.buildMovieFilename(title, movie.ReleaseDate, c)
		mkvPath := filepath.Join(e.moviesDir, filename)
		streamURL := fmt.Sprintf("%s/stream?link=%s&index=%d&play", e.gostorm.baseURL, hash, bestFile.ID)

		if e.createMKV(mkvPath, streamURL, bestFile.Length, magnet, imdbID) {
			res := "4K"
			if !c.Is4K {
				res = "1080p"
			}
			e.logger.Printf("[MovieSync] Created: %s (%s, %.1fGB, score:%d)", filename, res, float64(bestFile.Length)/1024/1024/1024, c.QualityScore)
			e.setCache(e.recheckCache, imdbID, CacheEntry{Title: title, Reason: "processed", TS: time.Now().Unix()})
			return true
		}

		e.gostorm.RemoveTorrent(ctx, hash)
	}

	e.setCache(e.recheckCache, imdbID, CacheEntry{Title: title, Reason: "no_better_stream", TS: time.Now().Unix()})
	return false
}

type MovieStream struct {
	Title        string
	Hash         string
	Is4K         bool
	QualityScore int
	Seeders      int
	SizeGB       float64
}

func (e *MovieGoEngine) getMovieStreams(ctx context.Context, imdbID, title string) ([]prowlarr.Stream, error) {
	// Prowlarr first
	if e.prowlarr != nil {
		streams := e.prowlarr.FetchTorrents(imdbID, "movie", title)
		if len(e.filterMovieStreams(streams)) > 0 {
			return streams, nil
		}
	}

	// Torrentio fallback
	tioStreams, err := e.torrentio.FetchMovieStreams(ctx, imdbID)
	if err != nil {
		return nil, err
	}

	var streams []prowlarr.Stream
	for _, s := range tioStreams {
		streams = append(streams, prowlarr.Stream{
			Name:     s.Name,
			Title:    s.Title,
			InfoHash: s.InfoHash,
		})
	}

	return streams, nil
}

func (e *MovieGoEngine) filterMovieStreams(streams []prowlarr.Stream) []MovieStream {
	// Pass 1: 4K
	var pass4K []MovieStream
	for _, s := range streams {
		c := e.classifyMovieStream(s)
		if c == nil || !c.Is4K {
			continue
		}
		pass4K = append(pass4K, *c)
	}
	if len(pass4K) > 0 {
		sort.Slice(pass4K, func(i, j int) bool {
			return pass4K[i].QualityScore > pass4K[j].QualityScore
		})
		return pass4K
	}

	// Pass 2: 1080p
	var pass1080 []MovieStream
	for _, s := range streams {
		c := e.classifyMovieStream(s)
		if c == nil || c.Is4K {
			continue
		}
		pass1080 = append(pass1080, *c)
	}
	sort.Slice(pass1080, func(i, j int) bool {
		return pass1080[i].QualityScore > pass1080[j].QualityScore
	})
	return pass1080
}

func (e *MovieGoEngine) classifyMovieStream(s prowlarr.Stream) *MovieStream {
	title := s.Title
	fullText := title + " " + s.Name

	if reMGarbage.MatchString(fullText) {
		return nil
	}
	if reMExclLang.MatchString(title) {
		return nil
	}
	if e.isBlacklisted(title) {
		return nil
	}
	if _, ok := e.blacklist.Hashes[strings.ToLower(s.InfoHash)]; ok {
		return nil
	}

	seeders := e.extractMovieSeeders(title)
	if seeders > 0 && seeders < mMovieMinSeeders {
		return nil
	}

	is4K := reM4K.MatchString(fullText)
	is1080p := reM1080p.MatchString(fullText) && !reM720p.MatchString(fullText)

	if !is4K && !is1080p {
		return nil
	}

	sizeGB := s.SizeGB

	// 4K: accept unknown size with penalty; 1080p: reject unknown
	if is4K {
		if sizeGB != 0 && (sizeGB < mMovie4KMinGB || sizeGB > mMovie4KMaxGB) {
			return nil
		}
	} else {
		if sizeGB == 0 || sizeGB < mMovie1080PMinGB || sizeGB > mMovie1080PMaxGB {
			return nil
		}
	}

	score := e.calculateMovieScore(fullText, seeders, sizeGB, is4K)
	if score <= 0 {
		return nil
	}

	return &MovieStream{
		Title:        title,
		Hash:         strings.ToLower(s.InfoHash),
		Is4K:         is4K,
		QualityScore: score,
		Seeders:      seeders,
		SizeGB:       sizeGB,
	}
}

func (e *MovieGoEngine) calculateMovieScore(text string, seeders int, sizeGB float64, is4K bool) int {
	score := 0

	if is4K {
		score += mMovie4KBase
	} else {
		score += mMovie1080pBase
	}

	if reMDV.MatchString(text) {
		score += mMovieDVBonus
	} else if reMHDR.MatchString(text) {
		score += mMovieHDRBonus
	}

	if reMAtmos.MatchString(text) {
		score += mMovieAtmosBonus
	} else if reM51.MatchString(text) {
		score += mMovie51Bonus
	} else if reMStereo.MatchString(text) {
		score += mMovieStereoPenalty
	} else {
		score += 5
	}

	if reMRemux.MatchString(text) {
		score += mMovieRemuxBonus
	}

	if reMITA.MatchString(text) {
		score += mMovieITABonus
	}

	if sizeGB == 0 && is4K {
		score += mMovieUnknownPenalty
	}

	seederBonus := seeders
	if seederBonus > 50 {
		seederBonus = 50
	}
	score += seederBonus

	return score
}

func (e *MovieGoEngine) extractMovieSeeders(title string) int {
	m := reMSeeders.FindStringSubmatch(title)
	if len(m) > 1 {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

func (e *MovieGoEngine) filterVideoFiles(files []FileStat, is4K bool) []FileStat {
	var valid []FileStat
	for _, f := range files {
		ext := strings.ToLower(filepath.Ext(f.Path))
		if ext != ".mkv" && ext != ".mp4" && ext != ".avi" && ext != ".mov" && ext != ".m4v" {
			continue
		}
		minSize := int64(mMovie4KMinGB * 1024 * 1024 * 1024)
		maxSize := int64(mMovie4KMaxGB * 1024 * 1024 * 1024)
		if !is4K {
			minSize = int64(mMovie1080PMinGB * 1024 * 1024 * 1024)
			maxSize = int64(mMovie1080PMaxGB * 1024 * 1024 * 1024)
		}
		if f.Length >= minSize && f.Length <= maxSize {
			valid = append(valid, f)
		}
	}
	return valid
}

func (e *MovieGoEngine) buildMovieFilename(title, releaseDate string, stream MovieStream) string {
	year := ""
	if len(releaseDate) >= 4 {
		year = releaseDate[:4]
	} else if m := reMTitleYear.FindStringSubmatch(title); len(m) > 2 {
		year = m[2]
	}

	base := e.sanitizeMovieFilename(title)
	if year != "" {
		base = fmt.Sprintf("%s_%s", base, year)
	}

	if stream.Is4K {
		base += "_2160p"
	} else {
		base += "_1080p"
	}

	if reMDV.MatchString(stream.Title) {
		base += "_DV"
	} else if reMHDR.MatchString(stream.Title) {
		base += "_HDR"
	}

	if reMAtmos.MatchString(stream.Title) {
		base += "_Atmos"
	} else if reM51.MatchString(stream.Title) {
		base += "_5.1"
	}

	if reMRemux.MatchString(stream.Title) {
		base += "_REMUX"
	}

	return fmt.Sprintf("%s_%s.mkv", base, stream.Hash[len(stream.Hash)-8:])
}

func (e *MovieGoEngine) sanitizeMovieFilename(s string) string {
	s = regexp.MustCompile(`[^a-zA-Z0-9._-]`).ReplaceAllString(s, "_")
	s = regexp.MustCompile(`_+`).ReplaceAllString(s, "_")
	return strings.Trim(s, "_")
}

func (e *MovieGoEngine) resolveIMDB(ctx context.Context, tmdbID int, title string) string {
	// Check cache
	if entry, ok := e.imdbCache[strconv.Itoa(tmdbID)]; ok {
		if entry.IMDBID != "" {
			return entry.IMDBID
		}
	}

	imdbID, err := e.tmdb.ExternalIDs(ctx, tmdbID)
	if err != nil || imdbID == "" {
		return ""
	}

	e.imdbCache[strconv.Itoa(tmdbID)] = IMDBCacheEntry{
		IMDBID: imdbID,
		Title:  title,
		TS:     time.Now().Unix(),
	}

	return imdbID
}

func (e *MovieGoEngine) isBlacklisted(title string) bool {
	t := strings.ToLower(title)
	t = reMYear.ReplaceAllString(t, "")
	t = reMQuality.ReplaceAllString(t, "")
	normalized := reMNonWord.ReplaceAllString(t, "")

	for _, bt := range e.blacklist.Titles {
		if bt == normalized {
			return true
		}
	}

	return false
}

func (e *MovieGoEngine) rehydrateMissingTorrents(ctx context.Context) {
	torrents, err := e.gostorm.ListTorrents(ctx)
	if err != nil {
		return
	}
	activeHashes := make(map[string]bool)
	for _, t := range torrents {
		activeHashes[t.Hash] = true
	}

	filepath.Walk(e.moviesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !strings.HasSuffix(strings.ToLower(path), ".mkv") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var url, magnet string
		var size float64
		content := strings.TrimSpace(string(data))

		if strings.HasPrefix(content, "{") {
			var obj map[string]interface{}
			if err := json.Unmarshal(data, &obj); err != nil {
				return nil
			}
			url, _ = obj["url"].(string)
			magnet, _ = obj["magnet"].(string)
			size, _ = obj["size"].(float64)
		} else {
			lines := strings.SplitN(content, "\n", 4)
			if len(lines) < 3 {
				return nil
			}
			url = strings.TrimSpace(lines[0])
			magnet = strings.TrimSpace(lines[2])
			if len(lines) > 1 {
				size, _ = strconv.ParseFloat(strings.TrimSpace(lines[1]), 64)
			}
		}

		m := reMHashURL.FindStringSubmatch(url)
		if len(m) < 2 {
			return nil
		}
		hash := m[1]

		if activeHashes[hash] {
			return nil
		}

		if strings.HasPrefix(magnet, "magnet:?") {
			displayTitle := TitleFromFilename(info.Name())
			freshMagnet := BuildMagnet(hash, displayTitle, DefaultTrackers())
			if _, err := e.gostorm.AddTorrent(ctx, freshMagnet, displayTitle); err == nil {
				e.createMKV(path, url, int64(size), freshMagnet, "")
			}
		}

		return nil
	})
}

func (e *MovieGoEngine) cleanupOrphanedFiles(ctx context.Context) {
	torrents, err := e.gostorm.ListTorrents(ctx)
	if err != nil {
		return
	}
	activeHashes := make(map[string]bool)
	for _, t := range torrents {
		activeHashes[t.Hash] = true
	}

	filepath.Walk(e.moviesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !strings.HasSuffix(strings.ToLower(path), ".mkv") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var url string
		content := strings.TrimSpace(string(data))
		if strings.HasPrefix(content, "{") {
			var obj map[string]interface{}
			if err := json.Unmarshal(data, &obj); err != nil {
				return nil
			}
			url, _ = obj["url"].(string)
		} else {
			lines := strings.SplitN(content, "\n", 2)
			if len(lines) < 1 {
				return nil
			}
			url = strings.TrimSpace(lines[0])
		}

		m := reMHashURL.FindStringSubmatch(url)
		if len(m) < 2 {
			return nil
		}
		if !activeHashes[m[1]] {
			os.Remove(path)
		}
		return nil
	})
}

// Cache helpers
func (e *MovieGoEngine) loadCache(file string) map[string]CacheEntry {
	data, err := os.ReadFile(file)
	if err != nil {
		return make(map[string]CacheEntry)
	}
	var m map[string]CacheEntry
	json.Unmarshal(data, &m)
	return m
}

func (e *MovieGoEngine) loadIMDBCache(file string) map[string]IMDBCacheEntry {
	data, err := os.ReadFile(file)
	if err != nil {
		return make(map[string]IMDBCacheEntry)
	}
	var m map[string]IMDBCacheEntry
	json.Unmarshal(data, &m)
	return m
}

func (e *MovieGoEngine) loadBlacklist() BlacklistData {
	data, err := os.ReadFile(e.blacklistFile)
	if err != nil {
		return BlacklistData{Hashes: make(map[string]string), Titles: []string{}}
	}
	var bl BlacklistData
	json.Unmarshal(data, &bl)
	if bl.Hashes == nil {
		bl.Hashes = make(map[string]string)
	}
	return bl
}

func (e *MovieGoEngine) isInCache(cache map[string]CacheEntry, key string, ttl time.Duration) bool {
	entry, ok := cache[key]
	if !ok {
		return false
	}
	if time.Since(time.Unix(entry.TS, 0)) > ttl {
		delete(cache, key)
		return false
	}
	return true
}

func (e *MovieGoEngine) setCache(cache map[string]CacheEntry, key string, entry CacheEntry) {
	cache[key] = entry
}

func (e *MovieGoEngine) pruneExpiredCaches() {
	now := time.Now()

	for k, v := range e.noMKVCache {
		if now.Sub(time.Unix(v.TS, 0)) > noMKVCacheTTL {
			delete(e.noMKVCache, k)
		}
	}
	for k, v := range e.noStreamsCache {
		if now.Sub(time.Unix(v.TS, 0)) > noStreamsCacheTTL {
			delete(e.noStreamsCache, k)
		}
	}
	for k, v := range e.recheckCache {
		if now.Sub(time.Unix(v.TS, 0)) > recheckCacheTTL {
			delete(e.recheckCache, k)
		}
	}
	for k, v := range e.addFailCache {
		if now.Sub(time.Unix(v.TS, 0)) > addFailCacheTTL {
			delete(e.addFailCache, k)
		}
	}
}

func (e *MovieGoEngine) saveAllCaches() {
	// no_mkv_hashes.json is managed by SQLite after migration — skip if .migrated exists
	// to avoid triggering the DB crash-recovery wipe on next restart.
	if !isMigratedFile(e.noMKVCFile) {
		e.saveCache(e.noMKVCFile, e.noMKVCache)
	}
	e.saveCache(e.noStreamsCFile, e.noStreamsCache)
	e.saveCache(e.recheckCFile, e.recheckCache)
	e.saveCache(e.addFailCFile, e.addFailCache)
	e.saveIMDBCache(e.imdbCFile, e.imdbCache)
}

// isMigratedFile returns true if path+".migrated" exists, indicating the file
// has been ingested into SQLite and the raw JSON should no longer be written.
func isMigratedFile(path string) bool {
	_, err := os.Stat(path + ".migrated")
	return err == nil
}

func (e *MovieGoEngine) saveCache(file string, data interface{}) {
	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return
	}
	tmp := file + ".tmp"
	os.WriteFile(tmp, jsonData, 0644)
	os.Rename(tmp, file)
}

func (e *MovieGoEngine) saveIMDBCache(file string, data map[string]IMDBCacheEntry) {
	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return
	}
	tmp := file + ".tmp"
	os.WriteFile(tmp, jsonData, 0644)
	os.Rename(tmp, file)
}

func (e *MovieGoEngine) createMKV(path, streamURL string, fileSize int64, magnet, imdbID string) bool {
	data := map[string]interface{}{
		"url":    streamURL,
		"size":   fileSize,
		"magnet": magnet,
		"imdb":   imdbID,
	}
	jsonData, err := json.Marshal(data)
	if err != nil {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return false
	}
	return os.WriteFile(path, jsonData, 0644) == nil
}
