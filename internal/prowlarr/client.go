package prowlarr

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Client queries the Prowlarr API and returns results in Stremio/Torrentio format.
// Thread-safe: all methods are safe for concurrent use.
type Client struct {
	cfg        ConfigProwlarr
	httpClient *http.Client
	searchURL  string
}

// NewClient creates a Prowlarr client from the given configuration.
// Returns nil if Prowlarr is not enabled.
func NewClient(cfg ConfigProwlarr) *Client {
	if !cfg.Enabled {
		return nil
	}
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 5,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		searchURL: cfg.URL + "/api/v1/search",
	}
}

// FetchTorrents queries Prowlarr and returns Stremio-format streams.
// contentType is "movie" or "series". title is the show/movie name.
// seasons is optional and used for series keyword search (e.g. "title s01").
// Returns an empty slice (never nil) if disabled or on error.
func (c *Client) FetchTorrents(imdbID, contentType, title string, seasons ...int) []Stream {
	if c == nil {
		return []Stream{}
	}
	results := c.fetchFromProwlarr(imdbID, contentType, title, seasons...)
	return c.mapToStremioFormat(results)
}

// fetchFromProwlarr executes an API query using the IMDb ID and merges results by infoHash.
// If contentType is "series" and seasons are provided, it also executes keyword searches
// (e.g., "Show Name s01") in parallel to maximize discovery of 4K releases.
func (c *Client) fetchFromProwlarr(imdbID, contentType, title string, seasons ...int) []ProwlarrResult {
	prowlarrType := "movie"
	if contentType == "series" {
		prowlarrType = "tvsearch"
	}

	baseParams := map[string]string{
		"apikey":     c.cfg.APIKey,
		"type":       prowlarrType,
		"indexerIds": "-2",
		"limit":      "100", // V1.7.4: Explicit limit to cover more releases in season searches
	}

	type result struct {
		items []ProwlarrResult
		idx   int
	}

	var queries []map[string]string
	// Primary query: IMDb ID
	queries = append(queries, mergeParams(baseParams, map[string]string{
		"query": imdbID,
	}))

	// Secondary queries: Title + Season keywords (Series only)
	if contentType == "series" && len(seasons) > 0 {
		// Clean title for keyword search (remove colons)
		cleanTitle := strings.ReplaceAll(title, ":", "")
		for _, s := range seasons {
			queries = append(queries, mergeParams(baseParams, map[string]string{
				"query": fmt.Sprintf("%s s%02d", cleanTitle, s),
			}))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	ch := make(chan result, len(queries))
	for i, params := range queries {
		i, params := i, params
		go func() {
			ch <- result{items: c.queryCtx(ctx, params), idx: i}
		}()
	}

	// Collect results preserving q1-first order for dedup
	collected := make([][]ProwlarrResult, len(queries))
	for range queries {
		r := <-ch
		collected[r.idx] = r.items
	}

	// Merge deduplicating by infoHash when available, or by guid for no-hash results.
	// No-hash results with a downloadUrl are kept for later hash resolution.
	var all []ProwlarrResult
	for _, items := range collected {
		all = append(all, items...)
	}
	seen := make(map[string]bool, len(all))
	merged := make([]ProwlarrResult, 0, len(all))
	for _, r := range all {
		var key string
		if r.InfoHash != "" {
			key = strings.ToLower(r.InfoHash)
		} else if r.DownloadUrl != "" {
			key = r.Guid
		} else {
			continue
		}
		if !seen[key] {
			seen[key] = true
			merged = append(merged, r)
		}
	}
	return merged
}

// queryCtx executes a single Prowlarr API GET request, respecting context cancellation.
func (c *Client) queryCtx(ctx context.Context, params map[string]string) []ProwlarrResult {
	req, err := http.NewRequestWithContext(ctx, "GET", c.searchURL, nil)
	if err != nil {
		log.Printf("[Prowlarr] Error building request: %v", err)
		return nil
	}

	q := req.URL.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	req.URL.RawQuery = q.Encode()

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		log.Printf("[Prowlarr] Error fetching from API: %v", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[Prowlarr] API returned status %d", resp.StatusCode)
		return nil
	}

	var results []ProwlarrResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		log.Printf("[Prowlarr] Error decoding response: %v", err)
		return nil
	}
	return results
}

// mapToStremioFormat converts raw Prowlarr results to Stremio/Torrentio stream format.
// Results without infoHash but with a downloadUrl have their hash resolved via a
// lightweight GET request that follows Prowlarr's 301→magnet redirect.
// Resolution is performed concurrently (up to 5 goroutines).
func (c *Client) mapToStremioFormat(results []ProwlarrResult) []Stream {
	if len(results) == 0 {
		return []Stream{}
	}

	// V1.7.3: Sort by size descending immediately. This ensures that high-quality
	// 4K releases (usually the largest) are at the top and get their infoHashes
	// resolved first if missing.
	sort.Slice(results, func(i, j int) bool {
		return results[i].Size > results[j].Size
	})

	// Separate results: those with hash are ready, those without need resolution.
	type indexed struct {
		idx int
		res ProwlarrResult
	}
	ready := make([]ProwlarrResult, 0, len(results))
	needsResolution := make([]indexed, 0)

	for i, res := range results {
		if garbageRe.MatchString(res.Title) {
			continue
		}
		if res.InfoHash != "" {
			ready = append(ready, res)
		} else if res.DownloadUrl != "" {
			needsResolution = append(needsResolution, indexed{i, res})
		}
	}

	// Resolve missing hashes concurrently (max 5 workers).
	// With the new sort, the first workers will focus on the largest files.
	if len(needsResolution) > 0 {
		sem := make(chan struct{}, 5)
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, item := range needsResolution {
			wg.Add(1)
			item := item
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				hash := c.resolveHashFromDownloadURL(item.res.DownloadUrl)
				if hash != "" {
					item.res.InfoHash = hash
					mu.Lock()
					ready = append(ready, item.res)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
	}

	streams := make([]Stream, 0, len(ready))
	for _, res := range ready {
		resTag := resolveResolution(res.Quality.Quality.Resolution, res.Title)
		sizeGB := float64(res.Size) / (1024 * 1024 * 1024)
		formattedTitle := fmt.Sprintf("%s\n👤 %d ⬇️ %d\n💾 %.2fGB",
			res.Title, res.Seeders, res.Leechers, sizeGB)
		streams = append(streams, Stream{
			Name:     fmt.Sprintf("Torrentio\n%s", resTag),
			Title:    formattedTitle,
			InfoHash: res.InfoHash,
			SizeGB:   sizeGB,
			BehaviorHints: BehaviorHints{
				BingeGroup: fmt.Sprintf("prowlarr-%s", resTag),
			},
		})
	}
	return streams
}

// resolveHashFromDownloadURL follows the Prowlarr download proxy URL, which issues a
// 301 redirect to a magnet link containing the infohash in the Location header.
// Returns the uppercase hex infohash, or empty string on failure.
func (c *Client) resolveHashFromDownloadURL(downloadURL string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	noRedirectClient := &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return ""
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusMovedPermanently && resp.StatusCode != http.StatusFound {
		return ""
	}

	location := resp.Header.Get("Location")
	m := reBtih.FindStringSubmatch(location)
	if len(m) < 2 {
		return ""
	}
	return strings.ToUpper(m[1])
}

// resolveResolution determines the resolution tag from the API value or falls back to regex.
func resolveResolution(resVal int, title string) string {
	switch resVal {
	case 2160:
		return "4k"
	case 1080:
		return "1080p"
	case 720:
		return "720p"
	}

	// Fallback: regex on title
	if res4kRe.MatchString(title) {
		return "4k"
	}
	if res1080Re.MatchString(title) {
		return "1080p"
	}
	if res720Re.MatchString(title) {
		return "720p"
	}
	return ""
}

// mergeParams combines two parameter maps. Second map overrides first on conflicts.
func mergeParams(base, extra map[string]string) map[string]string {
	result := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range extra {
		result[k] = v
	}
	return result
}
