package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"gostream/internal/gostorm/torr"
)

var lastConns = 30
var lastTimeout = 30
var metricsHistory []string
var lastKnownTotalSpeed float64
var CurrentLimit int32 // V1.6.1: Exported for main package consumption

type AITweak struct {
	ConnectionsLimit int    `json:"connections_limit"`
	PeerTimeout      int    `json:"peer_timeout"`
}

func (t *AITweak) Sanitize() {
	if t.ConnectionsLimit < 15 { t.ConnectionsLimit = 15 }
	if t.ConnectionsLimit > 60 { t.ConnectionsLimit = 60 }
	if t.PeerTimeout < 10 { t.PeerTimeout = 10 }
	if t.PeerTimeout > 60 { t.PeerTimeout = 60 }
}

func StartAITuner(ctx context.Context, aiURL string) {
	if aiURL == "" { aiURL = "http://localhost:8085" }
	log.Printf("[AI-Pilot] Neural optimizer starting... (Targeting 4K Fiber Stability)")
	ticker := time.NewTicker(60 * time.Second)
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

func runTuningCycle(aiURL string) {
	activeTorrents := torr.ListActiveTorrent()
	if len(activeTorrents) == 0 { 
		lastKnownTotalSpeed = 0
		return 
	}

	var activeT *torr.Torrent
	var totalSpeed float64
	realActiveCount := 0
	maxSpeed := float64(-1)
	
	for _, t := range activeTorrents {
		if t.Torrent == nil { continue }
		st := t.StatHighFreq()
		realActiveCount++
		totalSpeed += st.DownloadSpeed
		if st.DownloadSpeed > maxSpeed {
			maxSpeed = st.DownloadSpeed
			activeT = t
		}
	}
	if activeT == nil { 
		lastKnownTotalSpeed = 0
		return 
	}

	lastKnownTotalSpeed = totalSpeed / (1024 * 1024)
	st := activeT.StatHighFreq()
	cpu := getCPUUsage()
	buffer := 100
	isStaleBuffer := (st.DownloadSpeed / (1024 * 1024)) < 0.1

	if activeT.GetCache() != nil {
		cs := activeT.GetCache().GetState()
		if cs.Capacity > 0 { buffer = int(cs.Filled * 100 / cs.Capacity) }
	}

	fSize := activeT.Size
	if fSize == 0 { fSize = activeT.Torrent.Length() }
	fileSizeGB := float64(fSize) / (1024 * 1024 * 1024)
	
	currentSnap := fmt.Sprintf("[CPU:%d%%, Buf:%d%%, Peers:%d, Speed:%.1fMB/s]", 
		cpu, buffer, st.ActivePeers, st.DownloadSpeed/(1024*1024))
	
	metricsHistory = append(metricsHistory, currentSnap)
	if len(metricsHistory) > 3 { metricsHistory = metricsHistory[1:] }
	historyStr := strings.Join(metricsHistory, " -> ")

	bufferStatus := "FRESH"
	if isStaleBuffer { bufferStatus = "STALE (Ignore it)" }

	contextStr := fmt.Sprintf("Fiber Optic, 4K Stream, File:%.1fGB, ActiveTorrents:%d, TotalDLSpeed:%.1fMB/s, Buffer:%s", 
		fileSizeGB, realActiveCount, lastKnownTotalSpeed, bufferStatus)

	prompt := fmt.Sprintf("<|im_start|>system\nYou are a BitTorrent Tuning unit for Raspberry Pi 4.\nContext: %s\nIMPORTANT: If Speed is < 1.2MB/s and CPU is Low, set connections_limit=60, peer_timeout=15.\nObjective: 100%% Buffer, Fast Performance, Stable CPU.\nRespond ONLY compact JSON: {\"connections_limit\": 25, \"peer_timeout\": 30}<|im_end|>\n<|im_start|>user\nAnalyze trend and context. DECIDE.<|im_end|>\n<|im_start|>assistant\n", 
		contextStr, historyStr)

	tweak, err := fetchAIJSON[AITweak](aiURL, prompt)
	if err != nil {
		log.Printf("[AI-Pilot] AI Communication Delay: %v", err)
		return
	}

	tweak.Sanitize()

	if activeT.Torrent != nil {
		oldConns := activeT.Torrent.MaxEstablishedConns()
		oldTimeout := lastTimeout
		
		activeT.Torrent.SetMaxEstablishedConns(tweak.ConnectionsLimit)
		
		// V1.6.1: Sync with exported variable for main package
		atomic.StoreInt32(&CurrentLimit, int32(tweak.ConnectionsLimit))
		
		activeT.AddExpiredTime(time.Duration(tweak.PeerTimeout) * time.Second)
		lastConns = tweak.ConnectionsLimit
		lastTimeout = tweak.PeerTimeout

		log.Printf("[AI-Pilot] Optimizer applying change: Conns(%d->%d) Timeout(%ds->%ds) [Metrics: %s] [Ctx: %s]", 
			oldConns, lastConns, oldTimeout, lastTimeout, currentSnap, contextStr)
	}
}

func fetchAIJSON[T any](url string, prompt string) (*T, error) {
	reqBody, _ := json.Marshal(map[string]interface{}{
		"prompt": prompt, "n_predict": 64, "temperature": 0.0,
		"stop": []string{"<|im_end|>", "}"},
	})
	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Post(url+"/completion", "application/json", bytes.NewBuffer(reqBody))
	if err != nil { return nil, err }
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK { return nil, fmt.Errorf("Status %d", resp.StatusCode) }
	var aiResp struct { Content string `json:"content"` }
	json.NewDecoder(resp.Body).Decode(&aiResp)
	
	content := strings.TrimSpace(aiResp.Content)
	if !strings.HasPrefix(content, "{") { content = "{" + content }
	if !strings.HasSuffix(content, "}") { content = content + "}" }
	content = strings.ReplaceAll(content, "%", "")
	content = strings.ReplaceAll(content, "s", "")

	var result T
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return nil, fmt.Errorf("JSON parse error | Raw: %s", content)
	}
	return &result, nil
}

func getCPUUsage() int {
	t1Total, t1Idle := readCPUSample()
	time.Sleep(300 * time.Millisecond)
	t2Total, t2Idle := readCPUSample()
	totalDiff := t2Total - t1Total
	idleDiff := t2Idle - t1Idle
	if totalDiff == 0 { return 0 }
	return int(100 * (totalDiff - idleDiff) / totalDiff)
}

func readCPUSample() (uint64, uint64) {
	data, _ := os.ReadFile("/proc/stat")
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 { return 0, 0 }
	fields := strings.Fields(lines[0])
	if len(fields) < 5 { return 0, 0 }
	var total uint64
	for i := 1; i < len(fields); i++ {
		val, _ := strconv.ParseUint(fields[i], 10, 64)
		total += val
	}
	idle, _ := strconv.ParseUint(fields[4], 10, 64)
	return total, idle
}
