package collector

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const speedHistorySize = 60
const shieldEventWindow = 10 * time.Minute

// ShieldEvent represents an AdaptiveShield log event.
// Type: "corruption" (single bad piece), "strict" (STRICT mode started), "restore" (FAST mode restored).
type ShieldEvent struct {
	Type string `json:"type"`
	Time int64  `json:"t"` // UnixMilli
}

// HealthStatus holds the current system health snapshot.
type HealthStatus struct {
	Timestamp   time.Time     `json:"timestamp"`
	Uptime      string        `json:"uptime"`
	GoRoutines  int           `json:"go_routines"`
	MemAllocMB  float64       `json:"mem_alloc_mb"`
	MemSysMB    float64       `json:"mem_sys_mb"`
	CPU         float64       `json:"cpu_pct"`
	RAMPct      float64       `json:"ram_pct"`
	RAMUsedGB   float64       `json:"ram_used_gb"`
	RAMTotalGB  float64       `json:"ram_total_gb"`
	DiskUsedPct float64       `json:"disk_used_pct"`
	DiskFreeGB  float64       `json:"disk_free_gb"`
	DiskTotalGB float64       `json:"disk_total_gb"`
	GoStorm     ServiceStatus `json:"gostorm"`
	FUSE        ServiceStatus `json:"fuse"`
	VPN         ServiceStatus `json:"vpn"`
	NATPMP      ServiceStatus `json:"natpmp"`
	Plex        ServiceStatus `json:"plex"`

	TotalTorrents int     `json:"total_torrents"`
	ActiveCount   int     `json:"active_count"`
	TotalPeers    int     `json:"total_peers"`
	TotalSeeders  int     `json:"total_seeders"`
	DownloadMbps  float64 `json:"download_mbps"`

	FUSEActivePct float64 `json:"fuse_active_pct"`
	FUSEStalePct  float64 `json:"fuse_stale_pct"`
	FUSEBudgetMB  float64 `json:"fuse_budget_mb"`
	FUSEActiveMB  float64 `json:"fuse_active_mb"`
	FUSEStaleMB   float64 `json:"fuse_stale_mb"`
}

// ServiceStatus tracks a single service's health.
type ServiceStatus struct {
	OK      bool   `json:"ok"`
	Latency int    `json:"latency_ms"`
	Message string `json:"message,omitempty"`
}

// TorrentInfo holds torrent data for the dashboard.
type TorrentInfo struct {
	Hash       string  `json:"hash"`
	Title      string  `json:"title"`
	CleanTitle string  `json:"clean_title"`
	Year       string  `json:"year,omitempty"`
	Poster     string  `json:"poster,omitempty"`
	SpeedMBs   float64 `json:"speed_mbs"`
	Peers      int     `json:"peers"`
	Seeders    int     `json:"seeders"`
	Size       int64   `json:"size"`
	BytesRead  int64   `json:"bytes_read"`
	IsPriority bool    `json:"is_priority"`
	Status     string  `json:"status"`
	Is4K       bool    `json:"is_4k"`
	IsDV       bool    `json:"is_dv"`
	IsHDR      bool    `json:"is_hdr"`
	Is1080p    bool    `json:"is_1080p"`
	Audio      string  `json:"audio,omitempty"`
	Channels   string  `json:"channels,omitempty"`
}

// SpeedPoint is a timestamped speed measurement.
type SpeedPoint struct {
	Time  int64   `json:"t"`
	Speed float64 `json:"v"`
}

const (
	fuseFileCountTTL = 4 * time.Hour
	publicIPTTL      = 60 * time.Second
)

// Collector polls system services on a ticker.
type Collector struct {
	gostormURL string
	metricsURL string
	fusePath   string   // FUSE mount point (for mount status check)
	sourcePath string   // physical_source_path (for file counting)
	vpnIface   string
	plexURL    string
	plexToken  string
	natpmpPort int

	logsDir string

	mu           sync.RWMutex
	status       HealthStatus
	torrents     []TorrentInfo
	speedHistory []SpeedPoint
	shieldEvents []ShieldEvent
	start        time.Time
	httpClient   *http.Client

	prevCPUIdle  uint64
	prevCPUTotal uint64

	fuseFileCount     int
	fuseFileCountTime time.Time

	publicIP     string
	publicIPTime time.Time
}

// New creates a Collector.
func New(gostormURL, fusePath, sourcePath, vpnIface, plexURL, plexToken string, natpmpPort, metricsPort int, logsDir string) *Collector {
	return &Collector{
		gostormURL:   gostormURL,
		metricsURL:   fmt.Sprintf("http://127.0.0.1:%d/metrics", metricsPort),
		fusePath:     fusePath,
		sourcePath:   sourcePath,
		vpnIface:     vpnIface,
		plexURL:      plexURL,
		plexToken:    plexToken,
		natpmpPort:   natpmpPort,
		logsDir:      logsDir,
		start:        time.Now(),
		speedHistory: make([]SpeedPoint, 0, speedHistorySize),
		httpClient:   &http.Client{Timeout: 5 * time.Second},
	}
}

// Run starts the collection loop. Blocks until stop is closed.
func (c *Collector) Run(stop <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	c.collect()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			c.collect()
		}
	}
}

// Status returns the latest health snapshot.
func (c *Collector) Status() HealthStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status
}

// Torrents returns the current torrent list.
func (c *Collector) Torrents() []TorrentInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]TorrentInfo, len(c.torrents))
	copy(out, c.torrents)
	return out
}

// SpeedHistory returns the speed history ring buffer.
func (c *Collector) SpeedHistory() []SpeedPoint {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]SpeedPoint, len(c.speedHistory))
	copy(out, c.speedHistory)
	return out
}

// ShieldEvents returns the recent AdaptiveShield event list.
func (c *Collector) ShieldEvents() []ShieldEvent {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ShieldEvent, len(c.shieldEvents))
	copy(out, c.shieldEvents)
	return out
}

var (
	reShieldTS      = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2})`)
	reShieldStrict  = regexp.MustCompile(`\[AdaptiveShield\].*Force STRICT mode`)
	reShieldRestore = regexp.MustCompile(`\[AdaptiveShield\].*Restoring FAST mode`)
	reShieldCorr    = regexp.MustCompile(`\[AdaptiveShield\] (?:Single|Persistent) corruption`)
)

// parseShieldEvents reads the last portion of gostream.log and extracts
// AdaptiveShield events within the last shieldEventWindow.
func (c *Collector) parseShieldEvents() []ShieldEvent {
	if c.logsDir == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(c.logsDir, "gostream.log"))
	if err != nil {
		return nil
	}
	if len(data) > 256*1024 {
		data = data[len(data)-256*1024:]
	}
	cutoff := time.Now().Add(-shieldEventWindow)
	var events []ShieldEvent
	for _, line := range strings.Split(string(data), "\n") {
		m := reShieldTS.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		t, err := time.ParseInLocation("2006/01/02 15:04:05", m[1], time.Local)
		if err != nil || t.Before(cutoff) {
			continue
		}
		ms := t.UnixMilli()
		switch {
		case reShieldStrict.MatchString(line):
			events = append(events, ShieldEvent{Type: "strict", Time: ms})
		case reShieldRestore.MatchString(line):
			events = append(events, ShieldEvent{Type: "restore", Time: ms})
		case reShieldCorr.MatchString(line):
			events = append(events, ShieldEvent{Type: "corruption", Time: ms})
		}
	}
	return events
}

// PlexURL returns the configured Plex base URL (for server-side proxy use only).
func (c *Collector) PlexURL() string { return c.plexURL }

// PlexToken returns the configured Plex token (for server-side proxy use only).
func (c *Collector) PlexToken() string { return c.plexToken }

// GostormURL returns the GoStorm API base URL.
func (c *Collector) GostormURL() string { return c.gostormURL }

func (c *Collector) collect() {
	s := HealthStatus{
		Timestamp:  time.Now(),
		Uptime:     time.Since(c.start).Round(time.Second).String(),
		GoRoutines: runtime.NumGoroutine(),
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	s.MemAllocMB = float64(mem.Alloc) / 1024 / 1024
	s.MemSysMB = float64(mem.Sys) / 1024 / 1024

	s.CPU = c.readCPU()
	s.RAMPct, s.RAMUsedGB, s.RAMTotalGB = readRAM()
	s.DiskUsedPct, s.DiskFreeGB, s.DiskTotalGB = diskUsage("/")

	s.GoStorm = c.checkHTTP(c.gostormURL, "/echo", 3*time.Second)
	s.FUSE = c.checkFUSE()
	s.VPN = c.checkVPN()
	s.NATPMP = c.checkNATPMP()
	s.Plex = c.checkHTTP(c.plexURL, "/", 5*time.Second)

	// FUSE buffer from /metrics
	c.fetchFUSEBuffer(&s)

	// Torrents from GoStorm + enrich with Plex sessions and badges
	torrents := c.fetchTorrents()
	c.enrichTorrents(torrents)
	var totalPeers, totalSeeders, activeCount int
	var totalSpeedMB float64
	for _, t := range torrents {
		totalPeers += t.Peers
		totalSeeders += t.Seeders
		totalSpeedMB += t.SpeedMBs
		if t.Status != "idle" {
			activeCount++
		}
	}
	s.TotalTorrents = len(torrents)
	s.ActiveCount = activeCount
	s.TotalPeers = totalPeers
	s.TotalSeeders = totalSeeders
	s.DownloadMbps = totalSpeedMB * 8

	point := SpeedPoint{Time: time.Now().UnixMilli(), Speed: s.DownloadMbps}
	shieldEvts := c.parseShieldEvents()

	c.mu.Lock()
	c.status = s
	c.torrents = torrents
	c.speedHistory = append(c.speedHistory, point)
	if len(c.speedHistory) > speedHistorySize {
		c.speedHistory = c.speedHistory[len(c.speedHistory)-speedHistorySize:]
	}
	c.shieldEvents = shieldEvts
	c.mu.Unlock()
}

func (c *Collector) fetchTorrents() []TorrentInfo {
	resp, err := c.httpClient.Post(c.gostormURL+"/torrents", "application/json",
		strings.NewReader(`{"action":"active"}`))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	data, _ := io.ReadAll(resp.Body)

	var raw []struct {
		Hash             string  `json:"hash"`
		Title            string  `json:"title"`
		DownloadSpeed    float64 `json:"download_speed"`
		UploadSpeed      float64 `json:"upload_speed"`
		ActivePeers      int     `json:"active_peers"`
		ConnectedSeeders int     `json:"connected_seeders"`
		TorrentSize      int64   `json:"torrent_size"`
		BytesRead        int64   `json:"bytes_read"`
		IsPriority       bool    `json:"is_priority"`
		StatString       string  `json:"stat_string"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}

	out := make([]TorrentInfo, 0, len(raw))
	for _, r := range raw {
		speedMB := r.DownloadSpeed / 1024 / 1024
		// A torrent is "active" if it has peers, priority flag, or download speed
		isActive := r.ActivePeers > 0 || r.IsPriority || speedMB > 0.01
		status := "idle"
		if isActive {
			if speedMB > 0.01 {
				status = "downloading"
			} else {
				status = "streaming"
			}
		}
		out = append(out, TorrentInfo{
			Hash:       r.Hash,
			Title:      r.Title,
			SpeedMBs:   speedMB,
			Peers:      r.ActivePeers,
			Seeders:    r.ConnectedSeeders,
			Size:       r.TorrentSize,
			BytesRead:  r.BytesRead,
			IsPriority: r.IsPriority,
			Status:     status,
		})
	}
	return out
}

func (c *Collector) fetchFUSEBuffer(s *HealthStatus) {
	resp, err := c.httpClient.Get(c.metricsURL)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}

	active, _ := m["read_ahead_active_bytes"].(float64)
	stale := jsonFloat(m, "read_ahead_stale_bytes")
	budget := jsonFloat(m, "read_ahead_budget")

	if budget > 0 {
		s.FUSEBudgetMB = budget / 1024 / 1024
		s.FUSEActiveMB = active / 1024 / 1024
		s.FUSEStaleMB = stale / 1024 / 1024
		s.FUSEActivePct = active / budget * 100
		s.FUSEStalePct = stale / budget * 100
	}
}

func jsonFloat(m map[string]interface{}, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
}

func (c *Collector) readCPU() float64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	lines := strings.SplitN(string(data), "\n", 2)
	if len(lines) == 0 {
		return 0
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0
	}
	var total, idle uint64
	for i := 1; i < len(fields); i++ {
		v, _ := strconv.ParseUint(fields[i], 10, 64)
		total += v
		if i == 4 {
			idle = v
		}
	}
	var pct float64
	if c.prevCPUTotal > 0 {
		td := total - c.prevCPUTotal
		id := idle - c.prevCPUIdle
		if td > 0 {
			pct = float64(td-id) / float64(td) * 100
		}
	}
	c.prevCPUIdle = idle
	c.prevCPUTotal = total
	return pct
}

func readRAM() (pct, usedGB, totalGB float64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return
	}
	var memTotal, memAvail uint64
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			memTotal = v
		case "MemAvailable:":
			memAvail = v
		}
	}
	if memTotal == 0 {
		return
	}
	totalGB = float64(memTotal) / 1024 / 1024
	usedGB = float64(memTotal-memAvail) / 1024 / 1024
	pct = usedGB / totalGB * 100
	return
}

func (c *Collector) checkHTTP(url, path string, timeout time.Duration) ServiceStatus {
	if url == "" {
		return ServiceStatus{OK: false, Message: "not configured"}
	}
	start := time.Now()
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url + path)
	if err != nil {
		return ServiceStatus{OK: false, Message: err.Error()}
	}
	defer resp.Body.Close()
	latency := int(time.Since(start).Milliseconds())
	if resp.StatusCode < 500 {
		return ServiceStatus{OK: true, Latency: latency}
	}
	return ServiceStatus{OK: false, Latency: latency, Message: resp.Status}
}

func (c *Collector) checkFUSE() ServiceStatus {
	if c.fusePath == "" {
		return ServiceStatus{OK: false, Message: "not configured"}
	}
	start := time.Now()
	// Verify FUSE mount is accessible
	if _, err := os.Stat(c.fusePath); err != nil {
		return ServiceStatus{OK: false, Latency: int(time.Since(start).Milliseconds()), Message: err.Error()}
	}
	// Count .mkv files from physical_source_path (same as Python health-monitor):
	//   movies/   → flat glob  *.mkv
	//   tv/       → recursive  **/*.mkv
	// Cached every 4h to avoid hammering disk on every dashboard refresh.
	if time.Since(c.fuseFileCountTime) > fuseFileCountTTL || c.fuseFileCountTime.IsZero() {
		base := c.sourcePath
		if base == "" {
			base = c.fusePath
		}
		count := 0
		// movies/ — flat (Python: Path(MOVIES_DIR).glob("*.mkv"))
		moviesDir := filepath.Join(base, "movies")
		if entries, err := os.ReadDir(moviesDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".mkv") {
					count++
				}
			}
		}
		// tv/ — recursive (Python: Path(TV_DIR).rglob("*.mkv"))
		tvDir := filepath.Join(base, "tv")
		if _, err := os.Stat(tvDir); err == nil {
			filepath.WalkDir(tvDir, func(_ string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), ".mkv") {
					count++
				}
				return nil
			})
		}
		c.fuseFileCount = count
		c.fuseFileCountTime = time.Now()
	}
	latency := int(time.Since(start).Milliseconds())
	return ServiceStatus{OK: true, Latency: latency, Message: fmt.Sprintf("%d files", c.fuseFileCount)}
}

func (c *Collector) checkVPN() ServiceStatus {
	if c.vpnIface == "" {
		return ServiceStatus{OK: false, Message: "not configured"}
	}
	// Check interface is up
	flagsData, err := os.ReadFile(fmt.Sprintf("/sys/class/net/%s/flags", c.vpnIface))
	if err != nil {
		return ServiceStatus{OK: false, Message: c.vpnIface + " not found"}
	}
	flags, _ := strconv.ParseUint(strings.TrimSpace(string(flagsData)), 0, 32)
	const iffUp = 0x1
	if flags&iffUp == 0 {
		return ServiceStatus{OK: false, Message: c.vpnIface + " down"}
	}
	// Public IP via api.ipify.org — cached 60s (same as Python health-monitor)
	if time.Since(c.publicIPTime) > publicIPTTL || c.publicIPTime.IsZero() {
		cl := &http.Client{Timeout: 2 * time.Second}
		if resp, err := cl.Get("https://api.ipify.org"); err == nil {
			if body, err := io.ReadAll(resp.Body); err == nil && len(body) > 0 {
				c.publicIP = strings.TrimSpace(string(body))
			}
			resp.Body.Close()
		}
		c.publicIPTime = time.Now()
	}
	if c.publicIP != "" {
		return ServiceStatus{OK: true, Message: c.publicIP}
	}
	// Fallback: internal interface IP
	iface, err := net.InterfaceByName(c.vpnIface)
	if err != nil {
		return ServiceStatus{OK: true, Message: c.vpnIface + " up"}
	}
	addrs, _ := iface.Addrs()
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil {
			return ServiceStatus{OK: true, Message: ipnet.IP.String()}
		}
	}
	return ServiceStatus{OK: true, Message: c.vpnIface + " up"}
}

func (c *Collector) checkNATPMP() ServiceStatus {
	if c.natpmpPort == 0 {
		return ServiceStatus{OK: false, Message: "not configured"}
	}
	return ServiceStatus{OK: true, Message: "port " + strconv.Itoa(c.natpmpPort)}
}

func diskUsage(path string) (usedPct, freeGB, totalGB float64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return
	}
	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bavail * uint64(stat.Bsize)
	used := total - free
	if total > 0 {
		usedPct = float64(used) / float64(total) * 100
	}
	freeGB = float64(free) / 1024 / 1024 / 1024
	totalGB = float64(total) / 1024 / 1024 / 1024
	return
}

// --- Torrent enrichment: Plex sessions, badges, clean title ---

var (
	reSitePrefix  = regexp.MustCompile(`^(?:www\.\S+\s*[-–]\s*)+`)
	reCJKBrackets = regexp.MustCompile(`【[^】]*】`)
	reBrackets    = regexp.MustCompile(`\[[^\]]*\]`)
	reGroupTag    = regexp.MustCompile(`-[A-Za-z0-9]+$`)
	reYearTitle   = regexp.MustCompile(`[.\s]((?:19|20)\d{2})[.\s]`)
	reBadge4K     = regexp.MustCompile(`(?i)2160p|4[kK]|uhd`)
	reBadgeDV    = regexp.MustCompile(`(?i)(?:^|[._\s-])dv(?:$|[._\s-])|dovi|dolby.?vision`)
	reBadgeHDR   = regexp.MustCompile(`(?i)hdr10\+?|(?:^|[._\s])hdr(?:$|[._\s])`)
	reBadge1080  = regexp.MustCompile(`(?i)1080p`)
	reBadgeAtmos = regexp.MustCompile(`(?i)atmos`)
	reBadgeTrueHD = regexp.MustCompile(`(?i)truehd`)
	reBadgeDTSHD = regexp.MustCompile(`(?i)dts[- ]?hd|dts[- ]?ma`)
	reBadgeDTS   = regexp.MustCompile(`(?i)\bdts\b`)
	reBadgeDDP   = regexp.MustCompile(`(?i)ddp|dd\+|eac3`)
	reBadgeDD51  = regexp.MustCompile(`(?i)dd5|ac3`)
	reBadge71    = regexp.MustCompile(`7\.1`)
	reBadge51    = regexp.MustCompile(`5\.1`)
	reBadge20    = regexp.MustCompile(`2\.0`)
	reQualityTail = regexp.MustCompile(`(?i)[.\s](2160p|1080p|720p|4k|uhd|hdr|dv|dovi|web|bluray|remux)\b.*`)
)

func (c *Collector) enrichTorrents(torrents []TorrentInfo) {
	sessions := c.fetchPlexSessions()

	for i := range torrents {
		t := &torrents[i]
		// Badges from raw title
		tl := strings.ToLower(t.Title)
		t.Is4K = reBadge4K.MatchString(tl)
		t.Is1080p = reBadge1080.MatchString(tl) && !t.Is4K
		t.IsDV = reBadgeDV.MatchString(tl)
		t.IsHDR = reBadgeHDR.MatchString(tl) && !t.IsDV

		if reBadgeAtmos.MatchString(tl) {
			t.Audio = "ATMOS"
		} else if reBadgeTrueHD.MatchString(tl) {
			t.Audio = "TrueHD"
		} else if reBadgeDTSHD.MatchString(tl) {
			t.Audio = "DTS-HD"
		} else if reBadgeDTS.MatchString(tl) {
			t.Audio = "DTS"
		} else if reBadgeDDP.MatchString(tl) {
			t.Audio = "DD+"
		} else if reBadgeDD51.MatchString(tl) {
			t.Audio = "DD5.1"
		}

		if reBadge71.MatchString(tl) {
			t.Channels = "7.1"
		} else if reBadge51.MatchString(tl) {
			t.Channels = "5.1"
		} else if reBadge20.MatchString(tl) {
			t.Channels = "2.0"
		}

		// Try Plex session match: movie filenames use hash[len-8:], TV filenames use hash[:8].
		// Try both to cover all cases.
		var sess plexSession
		var ok bool
		if len(t.Hash) >= 8 {
			sess, ok = sessions[t.Hash[len(t.Hash)-8:]]
			if !ok {
				sess, ok = sessions[t.Hash[:8]]
			}
		}
		if ok {
			t.CleanTitle = sess.Title
			t.Year = sess.Year
			t.Poster = sess.Poster
			// Override quality badges with authoritative Plex media info
			if sess.VideoResolution != "" {
				res := sess.VideoResolution
				t.Is4K = res == "4k" || res == "2160"
				t.Is1080p = res == "1080" && !t.Is4K
				// Note: DV/HDR not available from Plex sessions; keep title-based detection
			}
			if sess.AudioCodec != "" {
				switch {
				case strings.Contains(sess.AudioCodec, "truehd"):
					if t.Audio == "" { t.Audio = "TrueHD" }
				case sess.AudioCodec == "eac3":
					t.Audio = "DD+"
				case strings.Contains(sess.AudioCodec, "dca"): // DTS family
					t.Audio = "DTS"
				case sess.AudioCodec == "ac3":
					t.Audio = "DD5.1"
				}
			}
			if sess.AudioChannels > 0 && t.Channels == "" {
				switch sess.AudioChannels {
				case 8:
					t.Channels = "7.1"
				case 6:
					t.Channels = "5.1"
				case 2:
					t.Channels = "2.0"
				}
			}
		}

		// Fallback: clean title from raw torrent name
		if t.CleanTitle == "" {
			t.CleanTitle = cleanTorrentTitle(t.Title)
		}
	}
}

type plexSession struct {
	Title           string
	Year            string
	Poster          string
	VideoResolution string // "4k", "1080", "720", …
	AudioCodec      string // "eac3", "truehd", "dts", "ac3", …
	AudioChannels   int    // 8=7.1, 6=5.1, 2=2.0
}

type plexMediaContainer struct {
	XMLName xml.Name    `xml:"MediaContainer"`
	Videos  []plexVideo `xml:"Video"`
}

type plexVideo struct {
	Type              string `xml:"type,attr"`            // "movie" or "episode"
	Title             string `xml:"title,attr"`           // episode title (or movie title)
	GrandparentTitle  string `xml:"grandparentTitle,attr"` // series title (episodes only)
	GrandparentYear   string `xml:"grandparentYear,attr"`  // series year (episodes only)
	Year              string `xml:"year,attr"`
	Thumb             string `xml:"thumb,attr"`
	GrandparentThumb  string `xml:"grandparentThumb,attr"` // series poster (episodes only)
	ParentIndex       int    `xml:"parentIndex,attr"`      // season number
	Index             int    `xml:"index,attr"`            // episode number
	Media []struct {
		VideoResolution string `xml:"videoResolution,attr"`
		AudioCodec      string `xml:"audioCodec,attr"`
		AudioChannels   int    `xml:"audioChannels,attr"`
		Parts           []struct {
			File string `xml:"file,attr"`
		} `xml:"Part"`
	} `xml:"Media"`
}

func (c *Collector) fetchPlexSessions() map[string]plexSession {
	result := make(map[string]plexSession)
	if c.plexURL == "" || c.plexToken == "" {
		return result
	}

	url := fmt.Sprintf("%s/status/sessions?X-Plex-Token=%s", c.plexURL, c.plexToken)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return result
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return result
	}
	data, _ := io.ReadAll(resp.Body)

	var container plexMediaContainer
	if err := xml.Unmarshal(data, &container); err != nil {
		return result
	}

	reHash8 := regexp.MustCompile(`_([a-f0-9]{8})\.mkv$`)
	for _, v := range container.Videos {
		// For episodes: use series title + series poster; for movies: use title + thumb
		title := v.Title
		year := v.Year
		thumbPath := v.Thumb
		if v.Type == "episode" && v.GrandparentTitle != "" {
			title = v.GrandparentTitle
			if v.GrandparentYear != "" {
				year = v.GrandparentYear
			}
			if v.GrandparentThumb != "" {
				thumbPath = v.GrandparentThumb
			}
		}
		poster := ""
		if thumbPath != "" {
			// Store as proxy path — browser fetches via /api/plex-thumb?p=...
			// so the Plex URL (often 127.0.0.1) never leaks to the client.
			poster = "/api/plex-thumb?p=" + thumbPath
		}
		for _, media := range v.Media {
			sess := plexSession{
				Title:           title,
				Year:            year,
				Poster:          poster,
				VideoResolution: strings.ToLower(media.VideoResolution),
				AudioCodec:      strings.ToLower(media.AudioCodec),
				AudioChannels:   media.AudioChannels,
			}
			for _, p := range media.Parts {
				m := reHash8.FindStringSubmatch(p.File)
				if len(m) >= 2 {
					result[m[1]] = sess
				}
			}
		}
	}
	return result
}

var (
	reVideoExt  = regexp.MustCompile(`(?i)\.(mkv|mp4|avi|mov|ts|m2ts)$`)
	reHexHash8  = regexp.MustCompile(`[_.\s][a-f0-9]{8}$`)
)

func cleanTorrentTitle(raw string) string {
	s := raw
	// Strip video file extension (.mkv, .mp4, …) before any other processing
	s = reVideoExt.ReplaceAllString(s, "")
	// Remove 8-char hex hash suffix (e.g. _dfcbca0b or .dfcbca0b)
	s = reHexHash8.ReplaceAllString(s, "")
	// Remove CJK bracket blocks: 【...】
	s = reCJKBrackets.ReplaceAllString(s, "")
	// Remove square bracket blocks: [...]
	s = reBrackets.ReplaceAllString(s, "")
	// Remove site prefix: "www.xxx.org - "
	s = reSitePrefix.ReplaceAllString(s, "")
	// Remove quality tags and everything after
	s = reQualityTail.ReplaceAllString(s, "")
	// Remove group tag at end: -ETHEL
	s = reGroupTag.ReplaceAllString(s, "")
	// Replace dots with spaces
	s = strings.ReplaceAll(s, ".", " ")
	s = strings.ReplaceAll(s, "_", " ")
	// Remove CJK characters (keep Latin + digits + basic punctuation)
	cleaned := make([]rune, 0, len(s))
	for _, r := range s {
		if r < 0x2E80 || r > 0x9FFF { // skip CJK ranges
			cleaned = append(cleaned, r)
		}
	}
	s = strings.TrimSpace(string(cleaned))
	// Collapse multiple spaces
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}
