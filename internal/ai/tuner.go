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

// Circuit breaker
var consecutiveTimeouts int32
var cooldownUntil time.Time

const maxConsecutiveTimeouts = 3
const cooldownDuration = 2 * time.Minute

// Reset lock: prevents concurrent reset + completion requests to llama-server
var resetInProgress atomic.Bool

// skipAICycles: after a context reset, skip N AI cycles before calling llama-server
var skipAICycles int

var lastConns = 0
var lastTimeout = 0
var defaultConns = 0
var defaultTimeout = 0
var metricsHistory []string
var CurrentLimit int32

// Rolling buffers (180s window, 36 samples every 5s)
var torrentSpeedAvg []float64
var cpuUsageAvg []float64
var cycleCounter int
var pulseCounter int
var peakCPUCycle float64

const normalCycle = 36 // 180s
const crisisCycle = 12 // 60s

// AIProvider holds configuration for LLM backends
type AIProvider struct {
	URL          string     // Base URL (local: http://127.0.0.1:8085, openrouter: https://openrouter.ai/api/v1)
	APIKey       string     // API key for cloud providers (empty for local)
	Model        string     // Model ID for cloud providers (empty for local)
	IsLocal      bool       // true = llama.cpp, false = cloud API
	GetBufferPct  func() int // Returns FUSE buffer fill percentage (0-100)
	GetSaturation func() int // Returns number of active semaphore slots (concurrent FUSE readers)
}

// Keep-Alive client for llama.cpp local
var aiClient = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:      10,
		IdleConnTimeout:   90 * time.Second,
		DisableKeepAlives: false,
	},
}

// HTTP client for cloud providers (longer timeout for API latency)
var cloudAIClient = &http.Client{
	Timeout: 120 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:      10,
		IdleConnTimeout:   90 * time.Second,
		DisableKeepAlives: false,
	},
}

// Short-timeout client for KV cache reset (fire-and-forget)
var cacheResetClient = &http.Client{Timeout: 10 * time.Second}

type AITweak struct {
	ConnectionsLimit float64 `json:"connections_limit"`
	PeerTimeout      float64 `json:"peer_timeout_seconds"`
}

func resetLlamaCache(provider AIProvider) {
	if !provider.IsLocal {
		return
	}
	// Deduplicate: if a reset (or completion) is already in flight, skip.
	if !resetInProgress.CompareAndSwap(false, true) {
		log.Printf("[AI-Pilot] KV cache reset skipped: server busy")
		return
	}
	defer resetInProgress.Store(false)

	base := strings.TrimSuffix(provider.URL, "/completion")
	url := base + "/slots/0?action=erase"
	resp, err := cacheResetClient.Post(url, "application/json", nil)
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
	if t.ConnectionsLimit < 5 {
		t.ConnectionsLimit = 5
	}
	if t.ConnectionsLimit > 60 {
		t.ConnectionsLimit = 60
	}
	if t.PeerTimeout < 10 {
		t.PeerTimeout = 10
	}
	if t.PeerTimeout > 60 {
		t.PeerTimeout = 60
	}
}

func crisisActive(buffer int) bool {
	// Se il buffer è quasi pieno (>95%), non siamo in crisi anche se la velocità è 0
	if buffer > 95 {
		return false
	}
	return getAverage(torrentSpeedAvg) < 2.0 && len(torrentSpeedAvg) > 8
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

func StartAITuner(ctx context.Context, provider AIProvider) {
	if provider.URL == "" {
		provider.URL = "http://127.0.0.1:8085"
	}
	// Pulizia URL
	provider.URL = strings.ReplaceAll(provider.URL, " -d", "")
	provider.URL = strings.TrimSuffix(provider.URL, "/completion")
	provider.URL = strings.TrimSuffix(provider.URL, "/")

	// Auto-detect provider type if not explicitly set
	if provider.URL == "http://127.0.0.1:8085" || strings.Contains(provider.URL, "127.0.0.1") || strings.Contains(provider.URL, "localhost") {
		provider.IsLocal = true
	}

	if provider.IsLocal {
		log.Printf("[AI-Pilot] Initializing llama.cpp Bridge (%s)... waiting for system settings.", provider.URL)
	} else {
		log.Printf("[AI-Pilot] Initializing Cloud LLM Provider (%s, model: %s)... waiting for system settings.", provider.URL, provider.Model)
	}
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
	log.Printf("[AI-Pilot] Neural optimizer starting... (Stats: 5s, AI: 180s) baseline conns=%d timeout=%d", lastConns, lastTimeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			runTuningCycle(provider)
		case <-ctx.Done():
			return
		}
	}
}

var lastActiveHash string
var lastAnnounceAt time.Time
var wasIdle bool // true when count==0 in last cycle — triggers skipAICycles on next torrent

func runTuningCycle(provider AIProvider) {
	if aiDisabled.Load() {
		return
	}
	// Circuit breaker: skip during cooldown
	if !cooldownUntil.IsZero() && time.Now().Before(cooldownUntil) {
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
			go resetLlamaCache(provider)
			if lastConns != defaultConns || lastTimeout != defaultTimeout {
				log.Printf("[AI-Pilot] Playback ended — restoring defaults (Conns:%d Timeout:%ds)", defaultConns, defaultTimeout)
				lastConns = defaultConns
				lastTimeout = defaultTimeout
			}
		}
		wasIdle = true
		atomic.StoreInt32(&CurrentLimit, 0) // fall back to globalConfig.MasterConcurrencyLimit
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
				go resetLlamaCache(provider)
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
	contextChanged := lastActiveHash != "" && currentHash != lastActiveHash
	freshStart := wasIdle && lastActiveHash == ""
	wasIdle = false

	if contextChanged || freshStart {
		reason := "Context Change"
		if freshStart {
			reason = "Fresh Start"
		}
		log.Printf("[AI-Pilot] %s Detected: Applying defaults (Conns:%d Timeout:%ds) for new torrent.", reason, defaultConns, defaultTimeout)
		metricsHistory = nil
		torrentSpeedAvg = nil
		cpuUsageAvg = nil
		cycleCounter = 0
		lastConns = defaultConns
		lastTimeout = defaultTimeout
		pulseCounter = 0
		peakCPUCycle = 0
		lastAnnounceAt = time.Time{}
		skipAICycles = 1 // skip 1 AI cycle to allow peer discovery at full connections
		if activeT.Torrent != nil {
			activeT.Torrent.SetMaxEstablishedConns(defaultConns)
			activeT.AddExpiredTime(time.Duration(defaultTimeout) * time.Second)
		}
		atomic.StoreInt32(&CurrentLimit, int32(defaultConns))
		go resetLlamaCache(provider)
	}
	lastActiveHash = currentHash

	// COLLECT SAMPLES (5s)
	currSpeedMBs := activeStats.DownloadSpeed / (1024 * 1024)
	currentCPU := float64(getCPUUsage())

	if currentCPU > peakCPUCycle {
		peakCPUCycle = currentCPU
	}

	torrentSpeedAvg = append(torrentSpeedAvg, currSpeedMBs)
	if len(torrentSpeedAvg) > 36 {
		torrentSpeedAvg = torrentSpeedAvg[1:]
	}

	cpuUsageAvg = append(cpuUsageAvg, currentCPU)
	if len(cpuUsageAvg) > 36 {
		cpuUsageAvg = cpuUsageAvg[1:]
	}

	// AI CYCLE: adaptive — 180s normal, 60s in crisis (avg speed < 1MB/s)
	cycleCounter++
	threshold := normalCycle

	buffer := 100
	if provider.GetBufferPct != nil {
		buffer = provider.GetBufferPct()
	}

	if crisisActive(buffer) {
		threshold = crisisCycle
	}
	if cycleCounter < threshold {
		return
	}
	cycleCounter = 0

	// Saturation guard: Plex scan attivo sullo stesso pack → sospendi LLM e resetta a default.
	if provider.GetSaturation != nil && provider.GetSaturation() > 1 {
		log.Printf("[AI-Pilot] Multi-reader detected (saturation>1). Suspending LLM call — resetting to defaults (%d conns, %ds timeout).", defaultConns, defaultTimeout)
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
		peakCPUCycle = 0
		return
	}

	// Post-reset guard: skip AI call if we're still in the cooldown after context change
	if skipAICycles > 0 {
		skipAICycles--
		log.Printf("[AI-Pilot] Post-reset guard: skipping AI call (%d cycles remaining)", skipAICycles)
		return
	}

	// Server busy guard: skip if a reset is in flight
	if resetInProgress.Load() {
		log.Printf("[AI-Pilot] Server busy (reset in progress): skipping AI call.")
		return
	}

	// --- SMART CONTEXT GENERATION (Every 3m) ---
	avgTorrentSpeed := getAverage(torrentSpeedAvg)
	avgCPU := getAverage(cpuUsageAvg)

	speedTrendStr := "STABLE"
	if len(torrentSpeedAvg) >= 36 {
		diff := currSpeedMBs - torrentSpeedAvg[0]
		if diff > 1.0 {
			speedTrendStr = fmt.Sprintf("UP (+%.1fMB/s)", diff)
		} else if diff < -1.0 {
			speedTrendStr = fmt.Sprintf("DOWN (%.1fMB/s)", diff)
		}
	}

	// IDLE GUARD: buffer full + no download → restore defaults for next episode
	if buffer > 95 && currSpeedMBs == 0 {
		if lastConns != defaultConns || lastTimeout != defaultTimeout {
			log.Printf("[AI-Pilot] Idle guard: buffer=%d%% speed=0 — restoring defaults (Conns:%d Timeout:%ds).", buffer, defaultConns, defaultTimeout)
			if activeT.Torrent != nil {
				activeT.Torrent.SetMaxEstablishedConns(defaultConns)
				activeT.AddExpiredTime(time.Duration(defaultTimeout) * time.Second)
			}
			atomic.StoreInt32(&CurrentLimit, int32(defaultConns))
			lastConns = defaultConns
			lastTimeout = defaultTimeout
		} else {
			log.Printf("[AI-Pilot] Idle guard: buffer=%d%% speed=0 — skipping AI call.", buffer)
		}
		return
	}

	currentSnap := sanitizeStr(fmt.Sprintf("[CPU:%d%% (Peak:%d%%), Buf:%d%%, Peers:%d/%d, Speed:%.1fMB/s (%s)]",
		int(avgCPU), int(peakCPUCycle), buffer, activeStats.ActivePeers, activeStats.TotalPeers, currSpeedMBs, speedTrendStr))

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
	contextStr := sanitizeStr(fmt.Sprintf("V:%.1fMB/s (AVG 3m: %.1fMB/s) | CPU:%d%% (Peak 3m: %d%%) | Peers:%d | Buffer:%d%%",
		currSpeedMBs, avgTorrentSpeed, int(currentCPU), int(peakCPUCycle), activeStats.ActivePeers, buffer))

	// Re-zero peak for next 3m cycle
	peakCPUCycle = 0

	// Dynamic Optimizer Prompt for Minimax/Cloud Providers (Fine-tuned for RP4)
	prompt := fmt.Sprintf(
		"BitTorrent Optimizer for Raspberry Pi 4. Default settings: 25 connections, 30s peer timeout. "+
			"Output ONLY JSON: {\"connections_limit\": int, \"peer_timeout_seconds\": int}. "+
			"Perform dynamic fine-tuning for BOTH values independently: lower timeout (<30s) to prune dead peers during stalls, higher timeout (>30s) to stabilize slow but steady peers. Adjust connections to balance CPU load (target <60%). If speed is below 3MB/s increase connections. If file size is circa 20GB tune to get 5MB/s or more\n"+
			"User context:\n"+
			"Peers:%d, Total:%d, Size:%.1fGB, Speed:%.1fMB/s, CPU:%d%%, Buf:%d%%, History:%s, Trend:%s",
		activeStats.ActivePeers, activeStats.TotalPeers, fileSizeGB, currSpeedMBs, int(currentCPU), buffer, historyStr, speedTrendStr,
	)

	tweak, err := fetchAIJSON[AITweak](provider, prompt)
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			if !aiDisabled.Swap(true) {
				log.Printf("[AI-Pilot] LLM not reachable (%s) — auto-disabled. Restart gostream to re-enable.", provider.URL)
			}
			return
		}
		failures := atomic.AddInt32(&consecutiveTimeouts, 1)
		log.Printf("[AI-Pilot] Communication Delay (%d/%d): %v", failures, maxConsecutiveTimeouts, err)
		go resetLlamaCache(provider)
		if failures >= maxConsecutiveTimeouts {
			cooldownUntil = time.Now().Add(cooldownDuration)
			atomic.StoreInt32(&consecutiveTimeouts, 0)
			log.Printf("[AI-Pilot] Circuit breaker OPEN: %d consecutive timeouts — cooldown until %s",
				maxConsecutiveTimeouts, cooldownUntil.Format("15:04:05"))
		}
		return
	}

	// Success: reset circuit breaker
	if atomic.SwapInt32(&consecutiveTimeouts, 0) > 0 || !cooldownUntil.IsZero() {
		log.Printf("[AI-Pilot] Circuit breaker CLOSED: AI recovered successfully")
		cooldownUntil = time.Time{}
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

func fetchAIJSON[T any](provider AIProvider, prompt string) (*T, error) {
	if provider.IsLocal {
		return fetchLocalJSON[T](provider, prompt)
	}
	return fetchCloudJSON[T](provider, prompt)
}

func fetchLocalJSON[T any](provider AIProvider, prompt string) (*T, error) {
	if !resetInProgress.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("server busy (reset in progress)")
	}
	defer resetInProgress.Store(false)

	start := time.Now()

	grammar := "root ::= \"{\\\"connections_limit\\\":\" number \",\\\"peer_timeout_seconds\\\":\" number \"}\"\nnumber ::= [0-9]+"

	reqBody, _ := json.Marshal(map[string]interface{}{
		"prompt":       prompt,
		"n_predict":    32,
		"temperature":  0.1,
		"stop":         []string{"<|im_end|>"},
		"cache_prompt": true,
		"grammar":      grammar,
		"keep_alive":   -1,
	})

	endpoint := provider.URL + "/completion"
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

	// Pre-processing: Tronca i numeri lunghi (>2 cifre) alle prime due cifre
	// Esempio: "130000000" -> "13". Questo risolve le allucinazioni di zeri del modello 0.6B.
	cleaned := trimmed
	for _, key := range []string{"\"connections_limit\":", "\"peer_timeout_seconds\":"} {
		if idx := strings.Index(cleaned, key); idx != -1 {
			vStart := idx + len(key)
			for vStart < len(cleaned) && (cleaned[vStart] == ' ' || cleaned[vStart] == ':') {
				vStart++
			}
			vEnd := vStart
			for vEnd < len(cleaned) && (cleaned[vEnd] >= '0' && cleaned[vEnd] <= '9') {
				vEnd++
			}
			if vEnd-vStart > 2 {
				cleaned = cleaned[:vStart] + cleaned[vStart:vStart+2] + cleaned[vEnd:]
			}
		}
	}

	var result T
	if err := json.Unmarshal([]byte(cleaned), &result); err != nil {
		return nil, fmt.Errorf("AI decode error: %v (Original: %q)", err, trimmed)
	}

	return &result, nil
}

func fetchCloudJSON[T any](provider AIProvider, prompt string) (*T, error) {
	start := time.Now()

	reqBody, _ := json.Marshal(map[string]interface{}{
		"model": provider.Model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"temperature": 0.1,
		"max_tokens":  64,
	})

	req, err := http.NewRequest("POST", provider.URL+"/chat/completions", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	// OpenRouter headers
	req.Header.Set("HTTP-Referer", "https://gostream.workers.dev")
	req.Header.Set("X-Title", "GoStream AI Tuner")

	resp, err := cloudAIClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Status %d | Body: %s", resp.StatusCode, string(body))
	}

	var cloudResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cloudResp); err != nil {
		return nil, fmt.Errorf("AI decode error: %v", err)
	}

	if len(cloudResp.Choices) == 0 {
		return nil, fmt.Errorf("empty AI response (no choices)")
	}

	trimmed := strings.TrimSpace(cloudResp.Choices[0].Message.Content)
	if trimmed == "" {
		return nil, fmt.Errorf("empty AI response")
	}
	log.Printf("[AI-Pilot] RAW: %q | Latency: %v", trimmed, time.Since(start))

	// Extract JSON from markdown if wrapped
	if idx := strings.Index(trimmed, "```"); idx != -1 {
		// Find the JSON between ```json and ``` or just between ``` and ```
		startIdx := idx + 3
		if startIdx < len(trimmed) && trimmed[startIdx:startIdx+4] == "json" {
			startIdx += 4
		}
		endIdx := strings.Index(trimmed[startIdx:], "```")
		if endIdx != -1 {
			trimmed = strings.TrimSpace(trimmed[startIdx : startIdx+endIdx])
		}
	}

	var result T
	if err := json.Unmarshal([]byte(trimmed), &result); err != nil {
		return nil, fmt.Errorf("AI decode error: %v (Original: %q)", err, trimmed)
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
