package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"gostream/internal/gostorm/settings"
	"gostream/internal/gostorm/torr"
	"gostream/internal/gostorm/torr/state"
)

var aiDisabled atomic.Bool

var lastConns = 0
var lastTimeout = 0
var defaultConns = 0
var defaultTimeout = 0
var metricsHistory []string
var CurrentLimit int32

// Rolling buffers (300s window, 60 samples every 5s)
var torrentSpeedAvg []float64
var cpuUsageAvg []float64
var cycleCounter int
var pulseCounter int
var peakCPUCycle float64

const normalCycle = 60 // 300s
const crisisCycle = 12 // 60s

// Keep-Alive client for llama.cpp local
var aiClient = &http.Client{
	Timeout: 180 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:      10,
		IdleConnTimeout:   90 * time.Second,
		DisableKeepAlives: false,
	},
}

type AITweak struct {
	ConnectionsLimit float64 `json:"connections_limit"`
	PeerTimeout      float64 `json:"peer_timeout_seconds"`
}

func resetLlamaCache(aiURL string) {
	base := strings.TrimSuffix(aiURL, "/completion")
	url := base + "/slots/0?action=erase"
	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		log.Printf("[AI-Pilot] KV cache reset skipped: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[AI-Pilot] KV cache reset skipped: HTTP %d", resp.StatusCode)
		return
	}
	log.Printf("[AI-Pilot] KV cache reset OK (slot 0 erased)")
}

func (t *AITweak) Sanitize() {
	if t.ConnectionsLimit < 10 {
		t.ConnectionsLimit = 10
	}
	if t.ConnectionsLimit > 60 {
		t.ConnectionsLimit = 60
	}
	if t.PeerTimeout < 10 {
		t.PeerTimeout = 10
	}
}

func crisisActive() bool {
	return getAverage(torrentSpeedAvg) < 3.0 && len(torrentSpeedAvg) > 10
}

func getAverage(samples []float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, v := range samples {
		sum += v
	}
	return sum / float64(len(samples))
}

func StartAITuner(ctx context.Context, aiURL string) {
	if aiURL == "" {
		aiURL = "http://127.0.0.1:8085"
	}
	// Pulizia URL
	aiURL = strings.ReplaceAll(aiURL, " -d", "")
	aiURL = strings.TrimSuffix(aiURL, "/completion")
	aiURL = strings.TrimSuffix(aiURL, "/")

	log.Printf("[AI-Pilot] Initializing llama.cpp Bridge (%s)... waiting for system settings.", aiURL)
	for i := 0; i < 30; i++ {
		if settings.BTsets != nil && settings.BTsets.TorrentDisconnectTimeout > 0 {
			break
		}
		time.Sleep(1 * time.Second)
	}

	if settings.BTsets != nil {
		lastConns = settings.BTsets.ConnectionsLimit
		lastTimeout = settings.BTsets.TorrentDisconnectTimeout
		defaultConns = lastConns
		defaultTimeout = lastTimeout
	}
	log.Printf("[AI-Pilot] Neural optimizer starting... (Stats: 5s, AI: 300s) baseline conns=%d timeout=%d", lastConns, lastTimeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			runTuningCycle(aiURL)
		case <-ctx.Done():
			return
		}
	}
}

var lastActiveHash string
var lastAnnounceAt time.Time

func runTuningCycle(aiURL string) {
	if aiDisabled.Load() {
		return
	}
	activeTorrents := torr.ListActiveTorrent()
	count := len(activeTorrents)

	if count == 0 {
		torrentSpeedAvg = nil
		cpuUsageAvg = nil
		cycleCounter = 0
		peakCPUCycle = 0
		if lastActiveHash != "" {
			go resetLlamaCache(aiURL)
		}
		lastActiveHash = ""
		return
	}

	// Multi-stream protection logic
	if count > 1 {
		// Check if exactly one priority stream exists (others are Plex scan noise)
		var priorityList []*torr.Torrent
		for _, t := range activeTorrents {
			if t.IsPriority {
				priorityList = append(priorityList, t)
			}
		}
		if len(priorityList) == 1 {
			activeTorrents = priorityList
		} else {
			// Multiple real streams or no priority → safety reset
			if lastConns != defaultConns || lastTimeout != defaultTimeout {
				log.Printf("[AI-Pilot] Multiple streams detected (%d). Resetting to safety defaults (%d:%d).", count, defaultConns, defaultTimeout)
				for _, t := range activeTorrents {
					if t.Torrent != nil {
						t.Torrent.SetMaxEstablishedConns(defaultConns)
						t.AddExpiredTime(time.Duration(defaultTimeout) * time.Second)
					}
				}
				atomic.StoreInt32(&CurrentLimit, int32(defaultConns))
				lastConns = defaultConns
				lastTimeout = defaultTimeout
				metricsHistory = nil
				torrentSpeedAvg = nil
				cpuUsageAvg = nil
				cycleCounter = 0
				peakCPUCycle = 0
				go resetLlamaCache(aiURL)
			}
			return
		}
	}

	var activeT *torr.Torrent
	var activeStats *state.TorrentStatus

	for _, t := range activeTorrents {
		if t.Torrent == nil {
			continue
		}
		activeStats = t.StatHighFreq()
		activeT = t
	}

	if activeT == nil || activeStats == nil {
		return
	}

	currentHash := activeT.Hash().String()
	if lastActiveHash != "" && currentHash != lastActiveHash {
		log.Printf("[AI-Pilot] Context Change Detected: Resetting history for new torrent.")
		metricsHistory = nil
		torrentSpeedAvg = nil
		cpuUsageAvg = nil
		cycleCounter = 0
		lastConns = defaultConns
		lastTimeout = defaultTimeout
		pulseCounter = 0
		peakCPUCycle = 0
		lastAnnounceAt = time.Time{}
		go resetLlamaCache(aiURL)
	}
	lastActiveHash = currentHash

	// COLLECT SAMPLES (5s)
	currSpeedMBs := activeStats.DownloadSpeed / (1024 * 1024)
	currentCPU := float64(getCPUUsage())

	if currentCPU > peakCPUCycle {
		peakCPUCycle = currentCPU
	}

	torrentSpeedAvg = append(torrentSpeedAvg, currSpeedMBs)
	if len(torrentSpeedAvg) > 60 {
		torrentSpeedAvg = torrentSpeedAvg[1:]
	}

	cpuUsageAvg = append(cpuUsageAvg, currentCPU)
	if len(cpuUsageAvg) > 60 {
		cpuUsageAvg = cpuUsageAvg[1:]
	}

	// AI CYCLE: adaptive — 300s normal, 60s in crisis (avg speed < 1MB/s)
	cycleCounter++
	threshold := normalCycle
	if crisisActive() {
		threshold = crisisCycle
	}
	if cycleCounter < threshold {
		return
	}
	cycleCounter = 0

	// --- SMART CONTEXT GENERATION (Every 5m) ---
	avgTorrentSpeed := getAverage(torrentSpeedAvg)
	avgCPU := getAverage(cpuUsageAvg)

	speedTrendStr := "STABLE"
	if len(torrentSpeedAvg) >= 60 {
		diff := currSpeedMBs - torrentSpeedAvg[0]
		if diff > 1.0 {
			speedTrendStr = fmt.Sprintf("UP (+%.1fMB/s)", diff)
		} else if diff < -1.0 {
			speedTrendStr = fmt.Sprintf("DOWN (%.1fMB/s)", diff)
		}
	}

	buffer := 100
	if activeT.GetCache() != nil {
		cs := activeT.GetCache().GetState()
		if cs.Capacity > 0 {
			buffer = int(cs.Filled * 100 / cs.Capacity)
		}
	}

	// IDLE GUARD: buffer full + no download → nothing to optimize
	if buffer > 95 && currSpeedMBs == 0 {
		log.Printf("[AI-Pilot] Idle guard: buffer=%d%% speed=0 — skipping AI call.", buffer)
		return
	}

	currentSnap := sanitizeStr(fmt.Sprintf("[CPU:%d%% (Peak:%d%%), Buf:%d%%, Peers:%d, Speed:%.1fMB/s (%s)]",
		int(avgCPU), int(peakCPUCycle), buffer, activeStats.ActivePeers, currSpeedMBs, speedTrendStr))

	metricsHistory = append(metricsHistory, currentSnap)
	if len(metricsHistory) > 4 {
		metricsHistory = metricsHistory[1:]
	}
	historyStr := strings.Join(metricsHistory, " -> ")

	fSize := activeT.Size
	if fSize == 0 {
		fSize = activeT.Torrent.Length()
	}
	fileSizeGB := float64(fSize) / (1024 * 1024 * 1024)

	// Clean Context Format
	contextStr := sanitizeStr(fmt.Sprintf("V:%.1fMB/s (AVG 5m: %.1fMB/s) | CPU:%d%% (Peak 5m: %d%%) | Peers:%d | Buffer:%d%%",
		currSpeedMBs, avgTorrentSpeed, int(currentCPU), int(peakCPUCycle), activeStats.ActivePeers, buffer))

	// Re-zero peak for next 5m cycle
	peakCPUCycle = 0

	// Qwen3 ChatML template
	historyPrefix := ""
	if len(metricsHistory) > 0 {
		historyPrefix = "history=" + historyStr + " "
	}
	prompt := fmt.Sprintf(
		"<|im_start|>system\nTune BitTorrent parms for performance 4K Movie streaming. connections_limit MUST be between 10-60. Set peer_timeout_seconds according to the health of the swarm. Output JSON: {\"connections_limit\":N,\"peer_timeout_seconds\":M}<|im_end|>\n<|im_start|>user\nActive peers in swarm:%d, file size:%.1fGB - %sspeed=%.0fMB/s cpu=%d%% buf=%d%% peers=%d trend=%s<|im_end|>\n<|im_start|>assistant\n",
		activeStats.TotalPeers, fileSizeGB, historyPrefix, currSpeedMBs, int(currentCPU), buffer, activeStats.ActivePeers, speedTrendStr,
	)
	tweak, err := fetchAIJSON[AITweak](aiURL, prompt)
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			if !aiDisabled.Swap(true) {
				log.Printf("[AI-Pilot] LLM not reachable (%s) — auto-disabled. Restart gostream to re-enable.", aiURL)
			}
			return
		}
		log.Printf("[AI-Pilot] Communication Delay: %v", err)
		return
	}

	tweak.Sanitize()

	if activeT.Torrent != nil {
		oldConns := activeT.Torrent.MaxEstablishedConns()
		oldTimeout := lastTimeout

		newConns := int(tweak.ConnectionsLimit)
		newTimeout := int(tweak.PeerTimeout)

		if newConns == lastConns && newTimeout == lastTimeout {
			pulseCounter++
			if pulseCounter >= 5 {
				log.Printf("[AI-Pilot] Pulse: Optimizer active, values stable at Conns(%d) Timeout(%ds). Metrics: %s",
					lastConns, lastTimeout, currentSnap)
				pulseCounter = 0
			}
			return
		}
		pulseCounter = 0

		activeT.Torrent.SetMaxEstablishedConns(newConns)
		atomic.StoreInt32(&CurrentLimit, int32(newConns))
		activeT.AddExpiredTime(time.Duration(newTimeout) * time.Second)
		lastConns = newConns
		lastTimeout = newTimeout

		log.Printf("[AI-Pilot] Optimizer applying change: Conns(%d->%d) Timeout(%ds->%ds) [Metrics: %s] [Ctx: %s] [File: %.1fGB]",
			oldConns, lastConns, oldTimeout, lastTimeout, currentSnap, contextStr, fileSizeGB)
	}

	// Discovery Boost: deterministic re-announce when swarm is weak
	healthyThresholdMBs := fileSizeGB * 0.15
	swarmWeak := activeStats.ConnectedSeeders < 2 && currSpeedMBs < healthyThresholdMBs
	if swarmWeak && time.Since(lastAnnounceAt) > 120*time.Second {
		lastAnnounceAt = time.Now()
		activeT.Torrent.Announce()
		log.Printf("[AI-Pilot] DiscoveryBoost: weak swarm (seeds=%d speed=%.1fMB/s threshold=%.1fMB/s) → tracker re-announce triggered",
			activeStats.ConnectedSeeders, currSpeedMBs, healthyThresholdMBs)
	}

}

func fetchAIJSON[T any](url string, prompt string) (*T, error) {
	start := time.Now()

	grammar := `root ::= "{\"connections_limit\":" number ",\"peer_timeout_seconds\":" number "}"
number ::= [1-9] [0-9]?`

	reqBody, _ := json.Marshal(map[string]interface{}{
		"prompt":       prompt,
		"n_predict":    32,
		"temperature":  0.1,
		"stop":         []string{"<|im_end|>"},
		"cache_prompt": true,
		"grammar":      grammar,
		"keep_alive":   -1,
	})

	endpoint := url + "/completion"
	resp, err := aiClient.Post(endpoint, "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Status %d | Body: %s", resp.StatusCode, string(body))
	}

	var aiResp struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&aiResp); err != nil {
		return nil, fmt.Errorf("AI decode error: %v", err)
	}

	trimmed := strings.TrimSpace(aiResp.Content)
	if trimmed == "" {
		return nil, fmt.Errorf("empty AI response")
	}

	log.Printf("[AI-Pilot] RAW: %q | Latency: %v", trimmed, time.Since(start))

	var result T
	if err := json.Unmarshal([]byte(trimmed), &result); err != nil {
		return nil, fmt.Errorf("JSON unmarshal error: %v", err)
	}
	return &result, nil
}

func getCPUUsage() int {
	t1Total, t1Idle := readCPUSample()
	time.Sleep(500 * time.Millisecond)
	t2Total, t2Idle := readCPUSample()
	totalDiff := t2Total - t1Total
	idleDiff := t2Idle - t1Idle
	if totalDiff == 0 {
		return 0
	}
	return int(100 * (totalDiff - idleDiff) / totalDiff)
}

func readCPUSample() (uint64, uint64) {
	data, _ := os.ReadFile("/proc/stat")
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 {
		return 0, 0
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 5 {
		return 0, 0
	}
	var total uint64
	for i := 1; i < len(fields); i++ {
		val, _ := strconv.ParseUint(fields[i], 10, 64)
		total += val
	}
	idle, _ := strconv.ParseUint(fields[4], 10, 64)
	return total, idle
}

func sanitizeStr(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r > 31 && r < 127 {
			b.WriteRune(r)
		}
	}
	return b.String()
}
