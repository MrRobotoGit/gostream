# GoStream

**GoStream** is a unified Go binary that combines a stripped, heavily-patched BitTorrent engine (GoStorm) with a FUSE virtual filesystem, exposing active torrents as seekable `.mkv` files over Samba to Plex Media Server. Built for 24/7 production on a Raspberry Pi 4 (arm64, 4 GB RAM), it achieves warm-start playback in **0.1–0.5 seconds** with CPU usage under 23% of one core during 4K HDR streaming.

The project is not a media center or a download manager. It is a **streaming filesystem engine**: torrents are never fully downloaded. Instead, GoStream reads exactly the bytes Plex requests, in the order they are requested, using a multi-layer cache and a zero-network in-process data path.

---

## Table of Contents

- [Architecture](#architecture)
- [Unique Technical Features](#unique-technical-features)
- [Performance Numbers](#performance-numbers)
- [Requirements](#requirements)
- [Quick Install](#quick-install)
- [How-To Guide](#how-to-guide)
- [GoStream Control Panel](#gostream-control-panel)
- [Health Monitor Dashboard](#health-monitor-dashboard)
- [Configuration Reference](#configuration-reference)
- [Sync Scripts](#sync-scripts)
- [Plex and Samba Setup](#plex-and-samba-setup)
- [Build from Source](#build-from-source)
- [API Quick Reference](#api-quick-reference)
- [Troubleshooting](#troubleshooting)
- [License](#license)

---

## Architecture

```
BitTorrent Peers ←→ GoStorm Engine (:8090)
                         │
              Native Bridge (In-Memory Pipe)
              Zero-Network, Zero-Copy Hot Path
                         │
         ┌───────────────────────────────────┐
         │        FUSE Proxy Layer           │
         │  L1: Read-Ahead Cache (256 MB)    │
         │  L2: SSD Warmup Head (64 MB/file) │
         │  L3: SSD Warmup Tail (16 MB/file) │
         └───────────────────────────────────┘
                         │
         /mnt/gostream-mkv-virtual/*.mkv  (FUSE mount)
                         │
         Samba share (smbd, oplocks=no, vfs objects=fileid)
                         │
         Synology CIFS mount (serverino, vers=3.0)
                         │
         Plex Media Server libraries
```

GoStream runs as a **single process**. The GoStorm BitTorrent engine and the FUSE proxy layer are compiled into one binary and communicate via an in-process `io.Pipe()` — no TCP sockets, no HTTP stack, no serialization on the data path. Metadata operations are direct Go function calls.

### Port Map

| Port | Purpose |
|------|---------|
| `:8080` | FUSE Proxy HTTP endpoint |
| `:8090` | GoStorm API — JSON torrent management |
| `:8096` | Metrics, Control Panel, Plex Webhook |
| `:8095` | Health Monitor Dashboard (Python, separate process) |

---

## Unique Technical Features

### 1. Zero-Network Native Bridge (V226+)

The FUSE proxy and GoStorm engine run in the same OS process. When Plex reads a `.mkv` byte range, the FUSE layer calls directly into the GoStorm engine via an in-memory `io.Pipe()`. There is no TCP round-trip, no HTTP header parsing, no serialization. This eliminates the network RTT that causes stuttering in HTTP-based torrent streaming proxies on constrained hardware.

### 2. Two-Layer SSD Warmup Cache (V256)

- **Head cache**: The first 64 MB of each file is written to SSD on first play. Repeat time-to-first-frame (TTFF): **less than 0.01 seconds** (SSD at 150–200 MB/s, versus 2–4 seconds for torrent activation from cold).
- **Tail cache**: The last 16 MB of each file is cached separately. MKV files store their Cues (seek index) near the end. Without tail cache, Plex probes the end of the file before confirming playback and the seek bar renders as unavailable.
- **Quota**: 32 GB default, LRU eviction by write-time mtime (~150 films cached simultaneously).
- **Auto-population**: Plex library scans read the first 1 MB of every file. This is enough to populate the warmup head cache for every title in the library automatically — no manual warming step needed.
- **Storage**: `{install_dir}/STATE/warmup/`

### 3. Plex Webhook Integration and Smart Streaming (V253+, V281)

GoStream embeds a webhook receiver at `:8096/plex-webhook`. When Plex sends a `media.play` event:

1. GoStream extracts the IMDB ID from the raw JSON payload using `imdb://(tt\d+)` regex **before** `json.Unmarshal`. This is intentional: Plex uses a non-standard `Guid` array format that causes a silent `UnmarshalTypeError` when decoded normally. Raw-string extraction is the only reliable method.
2. GoStream enters **Priority Mode** — GoStorm is instructed to aggressively prioritize pieces covering the exact byte range being played.
3. The tail warmup is frozen: the MKV Cues segment is not evicted while the film is playing.
4. On `media.stop`, GoStream triggers a fast-drop: the torrent retention window shrinks from 60 seconds to 10 seconds, freeing peers immediately.

The IMDB-ID matching approach (V281) also solves a localization problem: Plex sends titles in the user's display language (e.g., `"den stygge stesøsteren"` instead of `"The Ugly Stepsister"`). Fuzzy title matching fails on translated titles. IMDB ID matching is language-independent.

**Configure the webhook in Plex**: Settings → Webhooks → Add Webhook:
```
http://<your-pi-ip>:8096/plex-webhook
```

### 4. Adaptive Responsive Shield (V302)

GoStream reads in two modes:

- **Responsive Mode** (default): Data is served before SHA1 piece verification. Playback starts instantly; pieces are verified in parallel.
- **Strict Mode**: Only SHA1-verified pieces are served. Zero risk of corruption, but playback is gated on verification speed.

If a corrupt piece is detected at any point (`MarkNotComplete()`), the Adaptive Shield activates Strict Mode for **60 seconds**, then automatically restores Responsive Mode. The transition is tracked via an atomic boolean — no mutex contention on the hot read path.

### 5. Seek-Master Architecture (V284–V288, V560)

Accurate, low-latency seeking in large 4K files required four coordinated fixes:

| Fix | What it does |
|-----|-------------|
| **V285** | Updates `lastOff` before the cache check, so the pump goroutine sees the seek target on the same `Read()` call, eliminating a one-round lag |
| **V286** | `Interrupt()` closes the pipe reader atomically when the player jumps more than 256 MB. The pump goroutine is unblocked instantly from `io.ReadFull` — no polling, no sleep |
| **V284** | Pump reactive jump: if the player is more than 256 MB ahead of the pump's current position, the pump snaps to `(playerOff / chunkSize) * chunkSize` |
| **V288** | Pump survives `ErrInterrupted` by sleeping 200 ms and continuing the read loop rather than exiting, preventing premature goroutine termination |
| **V560** | Tail probe detection: Plex probes the last N% of the file for MKV Cues before confirming playback. These reads are identified by byte range and served from the SSD tail cache without steering the pump to the end of the file |

### 6. 32-Shard Read-Ahead Cache

The 256 MB read-ahead budget is distributed across 32 independent shards, keyed by a hash of file path and offset. Each shard has its own LRU and mutex. This eliminates global lock contention when multiple Plex sessions or scanner threads read concurrently. All pool operations use **defensive copies** on both write (Put) and read (Get) to prevent use-after-free races that were present in earlier channel-pool implementations.

### 7. Automated Content Ecosystem

Three Python sync scripts form a self-maintaining library. All scripts read credentials from `config.json` automatically:

| Script | Trigger | What it does |
|--------|---------|-------------|
| `gostorm-sync-complete.py` | Daily cron | TMDB Discover + Popular → Torrentio → GoStorm → virtual `.mkv` |
| `gostorm-tv-sync.py` | Weekly cron | TV series with fullpack-first approach, season pack structure |
| `plex-watchlist-sync.py` | Hourly cron | Plex cloud watchlist → IMDB → Torrentio → GoStorm |
| `health-monitor.py` | Persistent service | Real-time dashboard at `:8095` |

Quality selection ladder (movies): `4K DV > 4K HDR10+ > 4K HDR > 4K > 1080p REMUX > 1080p`

Minimum seeders: 20 (main sync), 10 (watchlist sync, for older films).

### 8. NAT-PMP Native VPN Port Forwarding (V228/V229)

Integrated as a sidecar goroutine. On startup (and periodically), GoStream requests a TCP+UDP port mapping from the WireGuard VPN gateway using the NAT-PMP protocol, installs `iptables PREROUTING REDIRECT` rules, and updates GoStorm's `PeersListenPort` when the external port changes — all without a service restart.

The effect on download speed is substantial:

| Configuration | Speed | Peers |
|--------------|-------|-------|
| WireGuard only | 3–6 Mbps | 8–10 |
| WireGuard + NAT-PMP | 15–21 Mbps | 19–20 |
| AMZN WEB-DL torrents | 140–198 Mbps | 23–25 |

### 9. IP Blocklist — ~700k Ranges (V298)

GoStream auto-downloads and periodically refreshes a gzipped BGP/country range blocklist. The blocklist is injected directly into anacrolix/torrent's IP filter, blocking known-bad actors before any connection attempt. Refresh interval: 24 hours.

### 10. Profile-Guided Optimization (PGO)

The binary is compiled with `-pgo=auto`. Go 1.24 reads `default.pgo` from the build directory to inline hot paths and optimize branch prediction based on real production workload data. On Pi 4 Cortex-A72 (no hardware AES/SHA1 extensions): approximately **5–7% CPU reduction** from PGO alone. The profile is regenerated from live streaming + sync workloads.

### 11. GoStorm Engine — Fork Modifications

GoStorm is a fork of anacrolix/torrent v1.55 + TorrServer, with substantial modifications:

- **O(1) `AddTorrent` DB write**: The original implementation rewrote all torrents on every add (O(N) BoltDB fsync). With 520 torrents, each add caused 520 fsync operations (~1–2 seconds on SSD), which saturated the disk and caused `smbd` D-state during Plex scans. Fixed to a single `tdb.Set()` per modified torrent.
- **O(1) `GetTorrentDB`**: The original called `ListTorrent()` (520 reads + 520 unmarshals) to find one torrent by hash. Fixed to direct key lookup.
- **InfoBytes + PeerAddrs caching**: `TorrentSpec.InfoBytes` was never persisted to the database. Re-activation required a full metadata fetch. Now both InfoBytes and a snapshot of peer addresses are saved on `Wake()` — re-activation connects to known peers instantly with no metadata fetch.
- **Request rebuild debounce (V278)**: The anacrolix torrent layer rebuilt its full piece request state on every received chunk — approximately 300 rebuilds/second. Debouncing to 60 rebuilds/second reduced CPU by 5x.
- **O(1) `clearPriority` (V279)**: Originally iterated all ~512 cached pieces, acquiring the global lock per piece. Replaced with a `localPriority` map tracking only the ~25 currently-prioritized pieces.
- **4 MB MemPiece buffer zeroing**: Channel-based buffer pools reuse memory without zeroing. When a piece is evicted and its buffer returned to the pool, the next piece to claim that buffer would contain stale data from a different file — causing forward-jump corruption in Responsive Mode. Fixed with `clear(p.buffer)` on pool reuse.
- **raCache defensive copies**: The read-ahead cache's `Get()` previously returned a sub-slice of the pooled buffer. When the pool evicted and recycled the buffer, Plex received overwritten data. Fixed with defensive copies on both `Put()` and `Get()`.
- **`cleanTrigger` panic fix (V280)**: `Cache.Close()` closed the `cleanTrigger` channel while background goroutines could still send to it, causing panics during peer upload. Fixed with a separate `cleanStop` channel.
- **`PeekTorrent` discipline**: Read-only/monitoring endpoints that called `GetTorrent()` caused silent torrent activation loops. `health-monitor.py`'s `/cache` polling endpoint was activating and timing out 520 torrents in a loop. All monitoring paths now use `PeekTorrent()`.
- **V302 InodeMap GC fix**: The inode cleanup routine built its `validFiles` set from active torrents only, which caused it to prune virtual MKV stubs every 5 minutes (529 files + directories). Each prune rebuilt all inodes, causing `smbd` D-state. Fixed to use `WalkDir(physicalSourcePath)` as the primary source.
- **8 additional race condition fixes**: Concurrent map writes, torn reads, nil pointer dereferences across `requesting.go`, `piece.go`, `cache.go`, and `apihelper.go`.

---

## Performance Numbers

| Metric | Value |
|--------|-------|
| Cold start (first play, no warmup) | 2–4 s |
| Warm start (SSD warmup HIT) | **0.1–0.5 s** |
| Seek latency (cached position) | **< 0.01 s** |
| CPU at 4K HDR streaming | **20–23%** of one Pi 4 core |
| CPU reduction vs. baseline | **−87%** (161% → 20%) |
| Binary size | **33 MB** (60% smaller than legacy builds) |
| Memory footprint (read-ahead) | Deterministic 256 MB |
| GOMEMLIMIT | 2200 MiB |
| Peak throughput (NAT-PMP + fast seeder) | 200+ Mbps |
| Plex scan peer count | ~6 total (was ~15,000 before V272 fix) |
| Inode shard count | 32 (collision-protected) |
| Warmup cache capacity | ~150 films at 64 MB each (32 GB quota) |

---

## Requirements

- **Raspberry Pi 4** with arm64 OS (4 GB RAM recommended for 256 MB read-ahead + OS headroom)
- **Go 1.24+** — must be the `linux/arm64` toolchain, not `linux/arm` (32-bit)
- **Python 3.9+** with pip3
- **FUSE 3** — `sudo apt install fuse3 libfuse3-dev`
- **systemd**
- **Samba** — `sudo apt install samba`
- **Plex Media Server** (on Synology or any network-accessible host)
- **TMDB API key** — free at [themoviedb.org/settings/api](https://www.themoviedb.org/settings/api)
- **Plex authentication token** — Settings → Account → XML API

---

## Quick Install

```bash
git clone https://github.com/MrRobotoGit/gostream GoStream_Src
cd GoStream_Src
chmod +x install.sh
./install.sh
```

The interactive installer:
1. Prompts for all required paths, Plex credentials, TMDB key, and NAT-PMP settings
2. Generates `config.json` from `config.json.example`
3. Installs Python dependencies from `requirements.txt`
4. Creates `STATE/`, `logs/`, and FUSE mount point directories
5. Writes and enables systemd services for `gostream` and `health-monitor`
6. Optionally configures cron jobs for sync scripts

After installation, build the binary (see [Build from Source](#build-from-source)) and deploy:

```bash
cp gostream /home/pi/GoStream/gostream
sudo systemctl start gostream health-monitor
```

---

## How-To Guide

### Step 1 — Configure the Plex Webhook

In Plex Web: **Settings → Webhooks → Add Webhook**:

```
http://192.168.1.2:8096/plex-webhook
```

This is required for Priority Mode (bitrate boost during playback), fast-drop on stop, and unambiguous IMDB-ID-based file matching.

Test connectivity:
```bash
curl -X POST http://192.168.1.2:8096/plex-webhook \
  -H 'Content-Type: application/json' \
  -d '{"event":"media.play"}'
```

### Step 2 — Configure the Plex Library

Add a Movies library in Plex pointing to the Samba share:
```
smb://192.168.1.2/gostream-mkv-virtual/movies
```
Or, if using Synology, point Plex to the CIFS mount: `/volume1/GoStream/movies`.

Run a library scan after adding the library. Plex reads the first megabyte of every `.mkv` file during the scan — this automatically populates the SSD warmup head cache for every title. Subsequent plays on those titles will start in under 0.5 seconds.

### Step 3 — Add Your First Movie

**Manually via API:**
```bash
curl -X POST http://127.0.0.1:8090/torrents \
  -H "Content-Type: application/json" \
  -d '{"action":"add","link":"magnet:?xt=urn:btih:...","title":"Interstellar (2014)"}'
```

**Via sync scripts (recommended):**
```bash
python3 /home/pi/GoStream/scripts/gostorm-sync-complete.py
```
The script fetches popular films from TMDB, finds the best available torrent for each via Torrentio, adds them to GoStorm, and writes virtual `.mkv` stub files to the physical source path.

### Step 4 — Watch a Film (What Happens Internally)

1. Plex requests `/mnt/gostream-mkv-virtual/movies/Interstellar.mkv`
2. FUSE `Open()` triggers `Wake()` — GoStorm activates the torrent (instant with warmup HIT)
3. Plex metadata probes → served from SSD head warmup cache (< 0.01 s)
4. Plex probes the MKV Cues near the end of the file → served from SSD tail cache
5. Plex sends `media.play` webhook → GoStream activates Priority Mode
6. Streaming reads arrive → served from Read-Ahead Cache or Native Bridge pump
7. Playback begins in **0.1–0.5 s**

### Step 5 — Seek in 4K

When Plex seeks to a new timestamp:

1. `Read()` is called at the new offset — `lastOff` is updated immediately (V285)
2. If the jump exceeds 256 MB: `Interrupt()` closes the in-memory pipe (V286) — the pump goroutine unblocks from `io.ReadFull` atomically
3. Pump detects that `lastOff` is more than 256 MB ahead of its current position — snaps to `(playerOff / chunkSize) * chunkSize` (V284)
4. Pump restarts the stream via `startStream(newOff)` — GoStorm repositions its torrent reader
5. Data arrives at the new position within seconds from peers or SSD cache

The pump goroutine survives `ErrInterrupted` (V288) — it sleeps 200 ms and continues the read loop rather than exiting, so no goroutine restart overhead.

### Step 6 — Add from Your Plex Watchlist

Add any movie to your Plex cloud watchlist (desktop or mobile app). Within one hour (hourly cron):

```bash
python3 /home/pi/GoStream/scripts/plex-watchlist-sync.py
```

The script:
1. Queries `discover.provider.plex.tv` for your watchlist
2. Resolves each entry to an IMDB ID (or falls back to TMDB)
3. Queries Torrentio for the best available stream (minimum 10 seeders)
4. Adds the torrent to GoStorm
5. Writes a virtual `.mkv` stub

On the next Plex library scan, the film appears in your library. Test without making changes:

```bash
python3 /home/pi/GoStream/scripts/plex-watchlist-sync.py --dry-run --verbose
```

### Step 7 — Monitor in Real Time

```bash
# Control Panel — GoStream + GoStorm settings, paths, restart button
open http://192.168.1.2:8096/control

# Health Dashboard — speed graph, torrent stats, active stream, log viewer
open http://192.168.1.2:8095

# Raw metrics (JSON)
curl -s http://192.168.1.2:8096/metrics | python3 -m json.tool

# Live log — key events only
ssh pi@192.168.1.2 "tail -f /home/pi/logs/gostream.log | grep -E '(OPEN|NATIVE|V286|V284|DiskWarmup|Emergency)'"

# Active torrents in RAM with speed
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"action":"active"}' http://192.168.1.2:8090/torrents | \
  jq '.[] | {title: .title[:60], speed_mbps: ((.download_speed//0)/1048576|round), peers: (.active_peers//0)}'
```

### Step 8 — Set Up Cron Jobs

```bash
crontab -e
```

Add:

```cron
# Plex Watchlist sync — every hour
0 * * * * /usr/bin/python3 /home/pi/GoStream/scripts/plex-watchlist-sync.py >> /home/pi/logs/watchlist-sync.log 2>&1

# Full movie sync — daily at 3 AM
0 3 * * * /usr/bin/python3 /home/pi/GoStream/scripts/gostorm-sync-complete.py >> /home/pi/logs/gostorm-debug.log 2>&1

# TV sync — every Sunday at 4 AM
0 4 * * 0 /usr/bin/python3 /home/pi/GoStream/scripts/gostorm-tv-sync.py >> /home/pi/logs/gostorm-tv-sync.log 2>&1
```

### Step 9 — Tune GoStorm Settings

Via the Control Panel at `:8096/control`, or via API:

```bash
curl -X POST http://127.0.0.1:8090/settings \
  -H "Content-Type: application/json" \
  -d '{
    "action": "set",
    "sets": {
      "CacheSize": 67108864,
      "ReaderReadAHead": 95,
      "PreloadCache": 0,
      "ConnectionsLimit": 25,
      "TorrentDisconnectTimeout": 10,
      "UseDisk": true,
      "ResponsiveMode": true
    }
  }'
```

| Setting | Value | Rationale |
|---------|-------|-----------|
| `CacheSize` | 64 MB | Lean engine strategy — passes data up to the FUSE 256 MB buffer; smaller heap = lower GC pressure |
| `ConnectionsLimit` | 25 | Matches the FUSE master semaphore; prevents Samba thread exhaustion |
| `ResponsiveMode` | `true` | Serves unverified data for instant start; Adaptive Shield (V302) corrects corruption automatically |
| `UseDisk` | `true` | Enables SSD warmup cache |
| `TorrentDisconnectTimeout` | 10 s | Fast peer cleanup to control RAM footprint |

### Step 10 — Regenerate the PGO Profile

Capture a CPU profile during real streaming workload:

```bash
# Single profile (120 seconds while streaming a 4K film)
curl -o /home/pi/GoStream_Src/default.pgo \
  "http://127.0.0.1:8096/debug/pprof/profile?seconds=120"

# Or merge multiple workloads for better coverage
curl -o /tmp/pgo-stream.pprof "http://127.0.0.1:8096/debug/pprof/profile?seconds=120"
curl -o /tmp/pgo-sync.pprof   "http://127.0.0.1:8096/debug/pprof/profile?seconds=120"
go tool pprof -proto /tmp/pgo-stream.pprof /tmp/pgo-sync.pprof > /home/pi/GoStream_Src/default.pgo

# Rebuild — Go detects the changed profile and re-optimizes automatically
cd /home/pi/GoStream_Src
GOARCH=arm64 CGO_ENABLED=1 /usr/local/go/bin/go build -pgo=auto -o gostream .
```

Regenerate the profile after significant code changes or when CPU usage changes noticeably. On Pi 4 Cortex-A72, `sha1.blockGeneric` appearing in the profile is expected — the A72 has no hardware SHA1 extensions.

---

## GoStream Control Panel

The Control Panel is a web UI **embedded in the GoStream binary** — no additional server, no React build step, no external dependencies. It is served directly at `:8096/control` alongside the metrics and webhook endpoints.

```
http://<your-pi-ip>:8096/control
```

### Simple / Advanced Mode

A toggle in the top-right corner switches between two views:

- **Simple**: Shows the most frequently changed settings — read-ahead budget, concurrency, cache size, paths, and NAT-PMP toggle.
- **Advanced**: Exposes all tunable parameters, split into labelled groups across two panels.

### GoStream FUSE Panel (left)

Settings in this panel are written to `config.json` and require a **service restart** to take effect. The **Save Engine Config** button persists the values; the **Restart** button in the header triggers an immediate service restart.

| Group | Settings |
|-------|----------|
| **Core & Streaming** | ReadAhead Budget (MB) — slider for the 256 MB in-memory pre-read buffer; Master Concurrency — global request slots (matches Samba thread ceiling); Max Streaming Slots — reserved slots for active playback; Streaming Threshold (KB) — min request size to activate stream mode |
| **Paths** | Physical Source Path — directory containing the real `.mkv` stub files (Samba source root); FUSE Mount Path — the virtual filesystem mount point exposed to Samba and Plex |
| **FUSE Timing & Buffers** | Read Buffer (KB) — OS-level read buffer size (1024 KB = Samba Turbo); FUSE Block Size (bytes) — 1 MB block aligns FUSE with Samba transfer units; Attr Timeout / Entry Timeout (s) — FUSE kernel cache validity for file attributes and directory entries |
| **Cache Management** | Metadata Cache (MB) — LRU cache size for file/torrent metadata; Max Cache Entries — file count cap for the metadata cache; Cleanup Interval (min) — frequency of the inode GC pass |
| **Connectivity & Rescue** | GoStorm URL — internal API address; Rescue Grace (s) — seconds before a non-responsive GoStorm triggers a rescue restart (240 s default); Rescue Cooldown (h) — minimum interval between rescues (24 h); Metrics Port; Log Level (INFO/DEBUG); Proxy Port; IP BlockList URL |

### GoStorm Engine Panel (right)

Settings in this panel are pushed to GoStorm **live via API** — no restart needed. The **Apply All Core Settings** button sends the current values to GoStorm immediately.

| Group | Settings |
|-------|----------|
| **Cache & Data** | Cache Size (MB) — GoStorm piece buffer that feeds the FUSE read-ahead cache; Readahead Cache (%) — percentage of CacheSize reserved for piece pre-read (75%); Preload Cache (%) — pre-fill before playback starts (0% recommended — warmup cache makes this redundant) |
| **Warmup & SSD** | Use Warmup Cache — enable/disable the two-layer SSD warmup; Warmup files path — where the `.warmup` binary files are stored; SSD Quota (GB) — LRU eviction cap (32 GB ≈ 150 films); Head Warmup (MB) — per-file head cache size (64 MB) |
| **Swarm Limits** | Connections Limit — max simultaneous peers per torrent (30); DL Rate / UP Rate (KB/s) — 0 = unlimited; Disconnect Timeout (s) — idle peer expiry (30 s) |
| **Network & Protocol** | Listen Port — peer inbound port (shown in orange if managed by NAT-PMP; do not edit manually when NAT-PMP is active); Retrackers Mode (Add/Replace/None); Enable IPv6 / DHT / PEX / TCP / uTP / Upload / Force Encrypt |
| **NAT-PMP (WireGuard)** | Enable toggle; Gateway IP (e.g., `10.2.0.1`); VPN Interface (`wg0`); Refresh (s) — how often to renew the port mapping (45 s); Lifetime (s) — requested mapping duration (60 s); Local Port — GoStorm listen port on the Pi (8091) |
| **Behaviors** | Smart Responsive Mode — enables Adaptive Shield (serves unverified pieces instantly; auto-reverts to Strict on corruption); Debug Log — enables verbose server-side logging |

---

## Health Monitor Dashboard

The Health Monitor is a standalone Python service (`health-monitor.py`) running on port **`:8095`**. It provides a live operational view of the entire stack.

```
http://<your-pi-ip>:8095
```

### Status Grid

Six cards in a 2×3 grid give an at-a-glance view of every layer:

| Card | What it shows |
|------|--------------|
| **GOSTORM** | API ping latency in milliseconds (green = responding). Restart button triggers GoStorm restart. |
| **FUSE MOUNT** | Number of virtual `.mkv` files currently exposed via the FUSE mount. Confirms the filesystem is mounted and populated. Restart button remounts FUSE. |
| **VPN (WG0)** | WireGuard interface status: current VPN IP and gateway address. Red if the interface is down. |
| **NAT-PMP** | Active external port currently assigned by the VPN gateway (e.g., `:44012`). Updates automatically when the port is renewed. Restart button re-triggers port mapping. |
| **PLEX** | Plex Media Server version and reachability. Restart button restarts the Plex service. |
| **SYSTEM** | CPU %, RAM %, and free disk space — live readings via `psutil`. |

### Download Speed Graph

A **15-minute rolling chart** of GoStorm download speed in Mbps. The chart samples every few seconds and scrolls automatically. Peaks reflect burst behaviour during initial piece discovery; the plateau reflects steady-state streaming speed (194 Mbps in the example above with an AMZN WEB-DL torrent).

### Torrents Panel

Shows the current torrent swarm state:

- **Active** — torrents currently in RAM with active peers (not just in the database)
- **Total** — all torrents in the GoStorm database
- **Peers / Seeders** — aggregate peer counts across all active torrents
- **FUSE Buffer bar** — Active % (read-ahead buffer currently in use) vs. Latent % (allocated but idle). A fully-blue bar (100% Active) means the entire 256 MB budget is committed to active reads — normal during fast-seeder playback.

### Active Stream Panel

Appears automatically when a file is being streamed. Shows:

- **Movie poster** fetched from TMDB
- **Title + year + file size** (e.g., *The Long Walk (2025) — 19.7 GB*)
- **Quality badges**: `PRIORITY` (Plex webhook received, priority mode active), `4K`, `DV` (Dolby Vision), `ATMOS`, `HDR10+`
- **LIVE** indicator + **5-minute average speed** (e.g., 207.3 Mbps/avg)
- **Source indicator**: `Proxy RAM` (data served from in-memory read-ahead cache) or `Warmup SSD` (served from disk cache)
- **Peer and seeder count** for the active torrent

### Sync Controls

Two panels at the bottom allow manual sync execution without SSH:

- **MOVIES SYNC** — triggers `gostorm-sync-complete.py`. Status shows `Idle` when not running, `Running` with a live log stream when active. Start button is disabled during an active run to prevent concurrent execution.
- **TV SYNC** — triggers `gostorm-tv-sync.py`. Same Start/Idle behaviour.

Live script output is streamed directly to the dashboard via server-sent events (SSE) — no need to tail a log file manually.

---

## Configuration Reference

`config.json` is always resolved relative to the binary's own path (`os.Executable()`). No path argument is needed at runtime. The file is not tracked by git (it contains credentials). Use `config.json.example` as the starting template:

```bash
cp config.json.example /home/pi/GoStream/config.json
nano /home/pi/GoStream/config.json
```

Apply changes via the Control Panel at `:8096/control`. GoStorm settings take effect live; FUSE settings require a service restart.

### Full Field Reference

| Field | Default | Description |
|-------|---------|-------------|
| `physical_source_path` | — | Directory where virtual `.mkv` stub files are created (the Samba source root) |
| `fuse_mount_path` | — | FUSE mount point — seekable virtual files are served from here |
| `read_ahead_budget_mb` | `256` | Total in-memory read-ahead budget across all active files |
| `disk_warmup_quota_gb` | `32` | SSD cache quota for warmup data (~150 films at 64 MB each) |
| `warmup_head_size_mb` | `64` | Per-file SSD warmup size (first N MB of each file) |
| `master_concurrency_limit` | `25` | Max concurrent data slots (caps Samba thread usage) |
| `gostorm_url` | `http://127.0.0.1:8090` | GoStorm internal API URL |
| `proxy_listen_port` | `8080` | FUSE proxy HTTP port |
| `metrics_port` | `8096` | Metrics, Control Panel, and Plex Webhook port |
| `blocklist_url` | *(BT_BlockLists)* | Gzipped IP blocklist URL (auto-downloaded, 24 h refresh) |
| `plex.url` | — | Plex server URL |
| `plex.token` | — | Plex authentication token |
| `plex.library_id` | — | Plex movie library section ID |
| `tmdb_api_key` | — | TMDB API key (free tier sufficient) |
| `natpmp.enabled` | `false` | Enable NAT-PMP VPN port forwarding |
| `natpmp.gateway` | — | VPN gateway IP (e.g., `10.2.0.1` for ProtonVPN WireGuard) |
| `natpmp.vpn_interface` | `wg0` | WireGuard interface name |

### Runtime Environment Variables

Set in the systemd service (no recompile needed):

```ini
Environment="GOMEMLIMIT=2200MiB"
Environment="GOGC=100"
```

`GOMEMLIMIT=2200MiB` leaves headroom for the OS, Samba, and Python scripts on a 4 GB Pi 4.

---

## Sync Scripts

All scripts in `scripts/` resolve `config.json` from the parent directory automatically. Override with the `MKV_PROXY_CONFIG_PATH` environment variable.

### gostorm-sync-complete.py — Daily Movie Sync

Queries TMDB Discover and Popular lists (Italian + English, region IT+US), evaluates Torrentio results for each film, and adds the best torrent to GoStorm.

```bash
python3 scripts/gostorm-sync-complete.py
```

- Quality ladder: `4K DV > 4K HDR10+ > 4K HDR > 4K > 1080p REMUX > 1080p`
- Minimum seeders: 20
- Minimum file size: 10 GB (4K), 3 GB (1080p)
- Skips films already in the library (by TMDB ID)
- Upgrades existing lower-quality entries when a better source is found

### gostorm-tv-sync.py — Weekly TV Sync

```bash
python3 scripts/gostorm-tv-sync.py
```

Fullpack-first approach: prefers complete season packs over individual episodes. Organizes files in a Plex-compatible directory structure:

```
Show Name/
  Season.01/
    Show.Name_S01E01_<hash>.mkv
    Show.Name_S01E02_<hash>.mkv
```

### plex-watchlist-sync.py — Hourly Watchlist Sync

```bash
python3 scripts/plex-watchlist-sync.py [--dry-run] [--verbose]
```

Reads your Plex cloud watchlist, resolves each title to an IMDB ID via `discover.provider.plex.tv`, queries Torrentio (minimum 10 seeders for better coverage of older films), and adds to GoStorm. Films appear in Plex on next library scan.

### health-monitor.py — Dashboard

```bash
python3 scripts/health-monitor.py
# or: sudo systemctl start health-monitor
```

Real-time operational dashboard at `http://<pi-ip>:8095`. See [Health Monitor Dashboard](#health-monitor-dashboard) for the full description with panel-by-panel breakdown.

---

## Plex and Samba Setup

### Samba Configuration

Critical parameters in `/etc/samba/smb.conf`. These prevent FUSE deadlocks during Plex library scans:

```ini
[gostream-mkv-virtual]
   path = /mnt/gostream-mkv-virtual
   browseable = yes
   read only = yes
   oplocks = no           # CRITICAL: prevents kernel exclusive locks on FUSE virtual files
   aio read size = 1      # CRITICAL: forces async I/O, prevents smbd threads from hanging
   deadtime = 15          # cleans inactive SMB connections every 15 minutes
   vfs objects = fileid   # CRITICAL: transmits 64-bit inodes to Synology/Plex
```

`oplocks = no` is non-negotiable. With oplocks enabled, the kernel requests exclusive locks on FUSE-backed files before serving them to Samba clients. FUSE cannot grant these locks, which causes `smbd` threads to enter D-state (uninterruptible sleep) indefinitely during concurrent Plex scans.

`vfs objects = fileid` ensures inode numbers are transmitted as 64-bit values. Without it, Synology receives 32-bit truncated inodes, which causes Plex to misidentify files after library scans.

### Synology CIFS Mount

```
Source:  //pi-ip/gostream-mkv-virtual
Target:  /volume1/GoStream
Options: serverino,vers=3.0,uid=1024,gid=100,file_mode=0777,dir_mode=0777
```

`serverino` must remain active. Synology automatically disables it after network timeout events. Configure a Task Scheduler job on the Synology NAS (every 5 minutes) to verify the mount is active with `serverino` and remount if not.

---

## Build from Source

Compile natively on Pi 4 (arm64). Do not cross-compile from another architecture — the binary must be built on `linux/arm64` for the PGO profile to be valid.

```bash
ssh pi@192.168.1.2
cd /home/pi/GoStream_Src

/usr/local/go/bin/go clean -cache
/usr/local/go/bin/go mod tidy
GOARCH=arm64 CGO_ENABLED=1 /usr/local/go/bin/go build -pgo=auto -o gostream .

# Deploy
sudo systemctl stop gostream
cp gostream /home/pi/GoStream/gostream
sudo systemctl start gostream
```

**Verify the toolchain is 64-bit before building:**

```bash
/usr/local/go/bin/go version
# Required: go version go1.24.x linux/arm64
# Wrong:    go version go1.24.x linux/arm   <-- 32-bit, do not use
```

**Install Go 1.24 if needed:**

```bash
wget https://go.dev/dl/go1.24.0.linux-arm64.tar.gz
sudo tar -C /usr/local -xzf go1.24.0.linux-arm64.tar.gz
```

---

## API Quick Reference

GoStorm API at `:8090`:

```bash
# List all torrents (count)
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"action":"list"}' http://127.0.0.1:8090/torrents | jq length

# Add a torrent
curl -X POST -H 'Content-Type: application/json' \
  -d '{"action":"add","link":"magnet:?xt=urn:btih:...","title":"Film Title (Year)"}' \
  http://127.0.0.1:8090/torrents

# Active torrents (in RAM only, not DB)
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"action":"active"}' http://127.0.0.1:8090/torrents | \
  jq '.[] | {title: .title[:50], speed_mbps: ((.download_speed//0)/1048576|round), peers: (.active_peers//0)}'

# Read GoStorm settings
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"action":"get"}' http://127.0.0.1:8090/settings | jq

# Remove a torrent
curl -X POST -H 'Content-Type: application/json' \
  -d '{"action":"rem","hash":"<infohash>"}' http://127.0.0.1:8090/torrents
```

GoStream Metrics API at `:8096`:

```bash
# Full metrics
curl -s http://127.0.0.1:8096/metrics | jq

# Key fields
curl -s http://127.0.0.1:8096/metrics | \
  jq '{version, uptime, read_ahead_active_bytes, config_source}'
```

---

## Troubleshooting

### Plex shows buffering or "Playback Error"

Check whether the warmup cache is populated:
```bash
curl -s http://127.0.0.1:8096/metrics | jq '.read_ahead_active_bytes'
```

If the warmup cache is empty, force a Plex library scan. The scan reads the first MB of each file, which populates the SSD head warmup for every title. Subsequent plays will start in under 0.5 seconds.

Check active torrent status:
```bash
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"action":"active"}' http://127.0.0.1:8090/torrents | \
  jq '.[] | {title: .title[:50], speed_mbps: ((.download_speed//0)/1048576|round), peers: (.active_peers//0)}'
```

If peer count is below 3, check NAT-PMP configuration (see below).

### smbd D-state or Samba hangs during Plex scan

This is almost always one of three causes:

1. **`oplocks = no` missing from `smb.conf`**: The kernel acquires exclusive locks on FUSE files, smbd threads block indefinitely. Verify and restart Samba.
2. **`vfs objects = fileid` missing**: Synology receives truncated 32-bit inodes, causing Plex to misidentify files and re-scan them repeatedly.
3. **`serverino` dropped on Synology CIFS mount**: After a network timeout, Synology may remount without `serverino`. Check: `mount | grep gostream`. If `serverino` is absent, remount with the correct options.

Check for D-state processes:
```bash
ps aux | grep -E 'smbd|gostream|fuse' | awk '$8 == "D"'
```

### Few seeders or slow downloads

Enable NAT-PMP in `config.json`:
```json
"natpmp": {
  "enabled": true,
  "gateway": "10.2.0.1",
  "vpn_interface": "wg0"
}
```

Restart gostream. Without an open inbound port, peers cannot initiate connections to the Pi — the engine must rely solely on outbound connections, which limits peer discovery substantially.

### Service fails to start

```bash
sudo systemctl status gostream
tail -30 /home/pi/logs/gostream.log
```

If the FUSE mount point is in a stale mounted state:
```bash
fusermount3 -uz /mnt/gostream-mkv-virtual
sudo systemctl start gostream
```

Ensure the mount point directory exists and is owned by the service user:
```bash
sudo mkdir -p /mnt/gostream-mkv-virtual
sudo chown pi:pi /mnt/gostream-mkv-virtual
```

### Plex webhook not triggering Priority Mode

Verify Plex can reach the Pi on port 8096. Test from the Pi:
```bash
curl -v http://127.0.0.1:8096/plex-webhook \
  -X POST -H 'Content-Type: application/json' \
  -d '{"event":"media.play","Metadata":{"guid":"plex://movie/..."}}'
```

Check the GoStream log for `[Webhook]` entries:
```bash
grep -i webhook /home/pi/logs/gostream.log | tail -20
```

If the webhook fires but IMDB matching fails (common with non-English Plex installations), verify the raw payload contains an `imdb://tt\d+` Guid. GoStream uses regex on the raw JSON string before unmarshaling, so `Guid` field format variations do not affect matching.

### High CPU usage

Profile the live binary:
```bash
go tool pprof -top "http://127.0.0.1:8096/debug/pprof/profile?seconds=30"
```

Expected hot paths: `sha1.blockGeneric` (hardware limitation on Pi 4 Cortex-A72, no crypto extensions), `io.ReadFull`, `sync.(*Mutex).Lock`. If `requesting.go` or `clearPriority` appear high, verify the V278/V279 patches are applied.

Regenerating the PGO profile with a fresh live workload typically reduces CPU 5–7% additional.

---

## Key File Locations (Production)

| Path | Purpose |
|------|---------|
| `/home/pi/GoStream/gostream` | Production binary |
| `/home/pi/GoStream/config.json` | Live configuration |
| `/home/pi/GoStream/scripts/` | Python sync and monitor scripts |
| `/home/pi/GoStream_Src/` | Go source code |
| `/home/pi/GoStream_Src/default.pgo` | PGO profile for next build |
| `/home/pi/STATE/` | Inode map, warmup cache |
| `/home/pi/logs/gostream.log` | Main service log |
| `/mnt/gostream-mkv-virtual/` | FUSE mount point |
| `/etc/systemd/system/gostream.service` | Service definition |

---

## License

MIT
