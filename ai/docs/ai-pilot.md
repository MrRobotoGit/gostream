# AI GoStream Pilot Overview [Experimental]

> ### Author's Note – Why This Is Interesting
>
> This project explores a non-traditional application of AI: using a locally quantized Large Language Model (LLM) as a real-time policy engine to dynamically optimize BitTorrent runtime parameters during media streaming.
>
> Traditional torrent clients rely on static configuration or deterministic heuristics to manage peer connections, timeouts, and bandwidth behavior. In contrast, GoStream's AI Pilot periodically analyzes live system metrics (CPU load, buffer state, peer count, throughput, contextual usage scenario) and adapts operational parameters in real time.
>
> What makes this approach interesting is not the use of AI for content generation, but the use of a lightweight LLM as a decision layer inside a constrained edge environment (e.g., Raspberry Pi), controlling a distributed P2P network workload under streaming conditions (including 4K media). The model operates with a reduced context window and low resource footprint, demonstrating that adaptive AI-driven control loops can function on minimal hardware without cloud dependency.
>
> This experiment reframes an LLM from a conversational tool into a runtime optimization component effectively acting as a soft, contextual control system layered on top of a torrent engine.
>
> While experimental, this approach opens discussion around AI-assisted network tuning, adaptive peer management, and lightweight autonomous optimization in decentralized systems.
>
> Author: **Matteo Rancilio**
>
> .



**AI GoStream Pilot** is an **optional** autonomous optimization engine designed for GoStream on Raspberry Pi 4. It leverages a tiny local LLM (**Llama-3.2-1B-Instruct-Q3_K_M**) to dynamically tune BitTorrent parameters, achieving two critical goals:

## Optional Activation
The system is designed to be plug-and-play and entirely decoupled:
*   **Auto-Detection**: GoStream automatically attempts to connect to the AI Server on port `8085`.
*   **Auto-Disable**: If the server is unreachable (`connection refused`), the AI Pilot disables itself after the first failed attempt and logs a single message. Subsequent cycles are silent. Re-enable by restarting GoStream.
*   **Silent Fallback**: If the server returns errors (non-connection issues), GoStream logs a `Communication Delay` and maintains current settings without affecting playback.
*   **Zero Impact**: The streaming pipeline does not wait for AI responses; if there's a communication delay, the current settings are maintained without affecting playback.
1.  **4K Stabilization**: It protects the system from CPU spikes and thermal stress by scaling down resources when performance is optimal.
2.  **Discovery Boost**: It actively attempts to improve connectivity for "difficult" or low-peer torrents by experimenting with higher connection limits and aggressive timeouts to discover faster seeders.


## Core Architecture

1.  **AI Server**: A background service (`ai-server.service`) running `llama.cpp`. It hosts the quantized model and provides a local API on port `8085`. Configured with a context window of **512 tokens** and `Nice=15` (low CPU priority).
2.  **AI Tuner**: A background goroutine within GoStream that samples system metrics every **5 seconds** and invokes the AI for decision-making every **300 seconds (5 minutes)**. This "High-Fidelity Sampling / Low-Frequency Inference" approach minimizes CPU overhead.

## Model

| Parameter | Value |
|-----------|-------|
| Model | `Llama-3.2-1B-Instruct-Q3_K_M.gguf` |
| Context window | 512 tokens (`-c 512`) |
| Threads | 2 (`-t 2`) |
| Inference latency | ~9–20s on Pi 4 Cortex-A72 |
| RAM usage | ~735 MB |
| Prompt template | Llama-3.2 Instruct (`<\|start_header_id\|>`) |

The model is prompted without current-value anchors (no explicit `conns=N` in the user message) to prevent statistical echo of input values. Grammar-constrained output via GBNF forces valid JSON regardless of model confidence.

## Operational Logic

The AI acts as a "Pilot" observing trends through a moving average window:
*   **Context Change Detection**: Automatically detects when a new torrent is played (via InfoHash) and resets all history, averages, and baseline values to ensure decisions are based only on the current film.
*   **History Management**: Maintains **4 snapshots** of previous metrics (20-minute window) to provide temporal context.
*   **Baseline from Config**: `connections_limit` and `peer_timeout` baselines are read from `config.json` at startup, not hardcoded.
*   **Surgical Sanitization**: All prompt data is stripped of non-ASCII characters to prevent backend errors.
*   **KV Cache**: Uses `cache_prompt: true` — the static system message prefix is cached between cycles, reducing latency on successive requests.

## Real-Time Adjustments

*   **Connections Limit**: Scaled between **15 and 60** peers. The AI prioritizes stability when CPU is high or streaming is smooth, and explores higher limits when peers are scarce or speed is declining.
*   **Peer Timeout**: Higher values reduce peer churn (keep good peers longer); lower values cycle through bad peers faster on struggling torrents.
*   **Hysteresis & Pulse**: Changes are only applied and logged when parameters actually change. A **Pulse log** is emitted every 5 stable cycles to confirm the optimizer is active.
*   **Multi-Stream Safety**: If more than one torrent is active simultaneously, the AI is bypassed and all torrents are reset to config default values.

## Installation & Setup

1.  **Deploy AI Directory**:
    ```bash
    rsync -avz GoStream/ai/ pi@192.168.1.2:/home/pi/GoStream/ai/
    ```

2.  **Run Setup Script**:
    ```bash
    ssh pi@192.168.1.2 "cd /home/pi/GoStream/ai && chmod +x setup_pi.sh && ./setup_pi.sh"
    ```

3.  **Service Management** (start manually, does not auto-start on boot):
    ```bash
    sudo systemctl start ai-server
    # To enable auto-start:
    sudo systemctl enable ai-server
    ```

## Fail-Safe Design

If the AI Server is unreachable or returns malformed data, GoStream automatically maintains the last known good settings. Grammar-constrained generation (GBNF) ensures the model can only produce syntactically valid JSON. The `Sanitize()` method clamps values to safe ranges `[15–60]` before applying any change.

## Key Files
*   Logic: `GoStream/ai/ai_tuner.go`
*   Service: `GoStream/ai/ai-server.service`
*   Model: `GoStream/ai/models/Llama-3.2-1B-Instruct-Q3_K_M.gguf`
*   Metrics: `:8096/metrics` (includes `ai_current_limit`)

## Real-World Activity Logs

Below are examples of how the AI Pilot behaves during a typical streaming session (Pi 4, March 2026):

### 1. Startup
```text
2026/03/09 21:45:35 [AI-Pilot] Neural optimizer starting... (Stats: 5s, AI: 300s) baseline conns=25 timeout=15
```

### 2. New Torrent Detection (History Reset)
```text
2026/03/09 21:45:40 [AI-Pilot] Context Change Detected: Resetting history for new torrent.
```

### 3. Dynamic Optimization
```text
// Stabilize peer pool without adding connections (buffer full, speed rising)
2026/03/09 21:51:05 [AI-Pilot] Optimizer applying change: Conns(25->25) Timeout(15s->48s) [Metrics: [CPU:51% (Peak:78%), Buf:100%, Peers:23, Speed:25.5MB/s (UP (+24.3MB/s))]]

// Expand peer pool aggressively (speed rising fast, buffer full)
2026/03/09 21:17:17 [AI-Pilot] Optimizer applying change: Conns(25->50) Timeout(15s->60s) [Metrics: [CPU:60% (Peak:90%), Buf:99%, Peers:23, Speed:23.4MB/s (UP (+22.9MB/s))]]

// Consolidate after expansion (speed stable, CPU high)
2026/03/09 21:22:28 [AI-Pilot] Optimizer applying change: Conns(50->33) Timeout(60s->60s) [Metrics: [CPU:57% (Peak:82%), Buf:100%, Peers:47, Speed:32.5MB/s (UP (+17.2MB/s))]]

// Scale down when player stops consuming (speed=0, CPU=0)
2026/03/09 22:01:14 [AI-Pilot] Optimizer applying change: Conns(50->15) Timeout(60s->15s) [Metrics: [CPU:16% (Peak:55%), Buf:98%, Peers:18, Speed:0.0MB/s (DOWN (-13.4MB/s))]]
```

### 4. Auto-Disable (LLM not running)
```text
2026/03/09 20:37:03 [AI-Pilot] LLM not reachable (http://127.0.0.1:8085) — auto-disabled. Restart gostream to re-enable.
```

### 5. Stability Confirmation (Pulse)
```text
2026/03/04 11:18:28 [AI-Pilot] Pulse: Optimizer active, values stable at Conns(25) Timeout(48s). Metrics: [CPU:49%, Buf:102%, Peers:15, Speed:16.5MB/s]
```
