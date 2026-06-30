package dashboard

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"gostream/internal/monitor/collector"
)

//go:embed dashboard.html
var dashboardHTML []byte

// Handler serves the dashboard and API endpoints.
type Handler struct {
	collector *collector.Collector
	logsDir   string
}

// New creates a dashboard handler.
func New(c *collector.Collector, logsDir string) *Handler {
	return &Handler{collector: c, logsDir: logsDir}
}

// Dashboard serves the HTML page.
func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(dashboardHTML)
}

// Health serves the /api/health JSON endpoint.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(h.collector.Status())
}

// Torrents serves the /api/torrents JSON endpoint.
func (h *Handler) Torrents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	t := h.collector.Torrents()
	if t == nil {
		t = []collector.TorrentInfo{}
	}
	json.NewEncoder(w).Encode(t)
}

// SpeedHistory serves the /api/speed-history JSON endpoint.
func (h *Handler) SpeedHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s := h.collector.SpeedHistory()
	if s == nil {
		s = []collector.SpeedPoint{}
	}
	json.NewEncoder(w).Encode(s)
}

// ShieldEvents serves the /api/shield-events JSON endpoint.
func (h *Handler) ShieldEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	evts := h.collector.ShieldEvents()
	if evts == nil {
		evts = []collector.ShieldEvent{}
	}
	json.NewEncoder(w).Encode(evts)
}

// Logs serves the /api/logs endpoint (tail of log files).
func (h *Handler) Logs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	allowed := map[string]string{
		"gostream":       "gostream.log",
		"movies-sync":    "movies-sync.log",
		"tv-sync":        "tv-sync.log",
		"watchlist-sync": "watchlist-sync.log",
	}

	file := r.URL.Query().Get("file")
	if file == "" {
		file = "gostream"
	}
	logName, ok := allowed[file]
	if !ok {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "invalid log file"})
		return
	}

	lines := 80
	if v := r.URL.Query().Get("lines"); v != "" {
		if n := atoi(v); n > 0 && n <= 200 {
			lines = n
		}
	}

	logPath := filepath.Join(h.logsDir, logName)
	result := tailFile(logPath, lines)
	json.NewEncoder(w).Encode(map[string]interface{}{"lines": result})
}

func tailFile(path string, n int) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return []string{}
	}
	// For large files, only use last 128KB
	if len(data) > 128*1024 {
		data = data[len(data)-128*1024:]
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}

// KillStream forwards a drop request to GoStorm for the given torrent hash.
func (h *Handler) KillStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte(`{"status":"error","message":"method not allowed"}`))
		return
	}
	hash := strings.TrimPrefix(r.URL.Path, "/api/kill-stream/")
	if hash == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"status":"error","message":"missing hash"}`))
		return
	}
	body := fmt.Sprintf(`{"action":"drop","hash":%q}`, hash)
	resp, err := http.Post(h.collector.GostormURL()+"/torrents", "application/json", strings.NewReader(body))
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `{"status":"error","message":%q}`, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		w.Write([]byte(`{"status":"ok"}`))
	} else {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `{"status":"error","message":"gostorm returned %s"}`, resp.Status)
	}
}

// PlexThumb proxies a Plex thumbnail through gostream.
// This avoids exposing the Plex token in client-side HTML/JS and decouples
// the browser from the configured plex_url (which may not be directly reachable
// from the client network).
func (h *Handler) PlexThumb(w http.ResponseWriter, r *http.Request) {
	thumbPath := r.URL.Query().Get("p")
	if thumbPath == "" || h.collector.PlexURL() == "" {
		http.NotFound(w, r)
		return
	}
	url := h.collector.PlexURL() + thumbPath + "?X-Plex-Token=" + h.collector.PlexToken()
	resp, err := http.Get(url) //nolint:noctx
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		http.NotFound(w, r)
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	io.Copy(w, resp.Body) //nolint:errcheck
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		} else {
			break
		}
	}
	return n
}
