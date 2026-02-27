# GoStream

A unified Go binary that combines a stripped BitTorrent engine (GoStorm) with a FUSE virtual filesystem proxy, exposing torrents as seekable `.mkv` files over Samba to a Plex media server. Designed for 24/7 operation on Raspberry Pi 4 (arm64).

---

## Architecture

```
GoStream Unified Engine
  (:8080 Proxy / :8090 GoStorm API / :8096 Metrics+Dashboard)
       |
  Native Bridge (Zero-Network Metadata + Data) -- In-Memory Pipe
       |
  /mnt/gostream-mkv-virtual/  <-- FUSE virtual filesystem
       |
  Samba share
       |
  Synology CIFS mount --> Plex libraries
```

GoStream runs as a single process. The FUSE proxy layer translates Plex read requests into streaming reads against the GoStorm BitTorrent engine via an in-process native bridge — no HTTP round-trips on the data path.

---

## Key Ports

| Port | Purpose |
|------|---------|
| `:8080` | FUSE Proxy — main HTTP endpoint |
| `:8090` | GoStorm API — JSON torrent management |
| `:8096` | Metrics + Control Panel (`/metrics`, `/control`) |
| `:8095` | Health Monitor Dashboard (Python, separate process) |

---

## Requirements

- **Go 1.24+** (arm64 cross-compilation target for Pi 4)
- **Python 3.9+** with `requests`, `psutil`, `fastapi`, `uvicorn`
- **FUSE 3** (`fuse3`, `libfuse3-dev`)
- **systemd** (service management)
- **Samba** (`smbd`) — for exposing the FUSE mount to Plex via CIFS
- **Plex Media Server** (optional — scripts integrate with Plex API)
- **TMDB API key** (free at [themoviedb.org](https://www.themoviedb.org/settings/api) — optional, used by sync scripts)

---

## Quick Install

```bash
git clone <repo-url> GoStream_Src
cd GoStream_Src

# Interactive installer — configures paths, Plex, TMDB, and installs services
chmod +x install.sh
./install.sh
```

The installer generates `config.json` from `config.json.example`, installs Python dependencies, creates required directories, and enables systemd services. After installation, copy the compiled binary:

```bash
cp gostream /home/pi/GoStream/gostream
sudo systemctl start gostream health-monitor
```

---

## Build from Source

Cross-compile on Pi 4 (native arm64):

```bash
cd /home/pi/GoStream_Src
/usr/local/go/bin/go clean -cache
/usr/local/go/bin/go mod tidy
GOARCH=arm64 CGO_ENABLED=1 /usr/local/go/bin/go build -pgo=auto -o gostream .
```

Go 1.24 is required. Verify the installed toolchain is 64-bit:

```bash
/usr/local/go/bin/go version
# must show: linux/arm64
```

### Profile-Guided Optimization (PGO)

The `-pgo=auto` flag uses `default.pgo` if present in the build directory. Regenerate periodically after significant code changes:

```bash
# Capture profile during real workload (streaming + sync active)
curl -o /home/pi/GoStream_Src/default.pgo \
  "http://127.0.0.1:8096/debug/pprof/profile?seconds=120"

# Rebuild with updated profile
GOARCH=arm64 CGO_ENABLED=1 /usr/local/go/bin/go build -pgo=auto -o gostream .
```

---

## Configuration

The binary resolves `config.json` relative to its own path — no CLI arguments needed at runtime. `config.json` is not tracked by git (it contains credentials). Use `config.json.example` as the starting template:

```bash
cp config.json.example /home/pi/GoStream/config.json
nano /home/pi/GoStream/config.json
```

Or run `./install.sh` which generates `config.json` interactively.

### Key config.json Fields

| Field | Description |
|-------|-------------|
| `physical_source_path` | Directory where virtual `.mkv` stub files are created (Samba source root) |
| `fuse_mount_path` | FUSE mount point — seekable virtual files served from here |
| `gostorm_url` | GoStorm internal API endpoint (usually `http://127.0.0.1:8090`) |
| `proxy_listen_port` | FUSE proxy HTTP port (default: `8080`) |
| `metrics_port` | Metrics + Control Panel port (default: `8096`) |
| `read_ahead_budget_mb` | In-memory read-ahead budget in MB (default: `256`) |
| `disk_warmup_quota_gb` | SSD warmup cache quota in GB (default: `32`, ~150 films) |
| `warmup_head_size_mb` | Per-file warmup size in MB (default: `64`) |
| `master_concurrency_limit` | Max concurrent data operations (default: `25`) |
| `blocklist_url` | URL of a gzipped IP blocklist (BGP/country ranges) |
| `plex.url` | Plex server URL (used by sync scripts) |
| `plex.token` | Plex authentication token |
| `tmdb_api_key` | TMDB API key (used by sync scripts) |
| `natpmp.enabled` | Enable NAT-PMP port mapping (for VPN/WireGuard setups) |

Apply changes via the embedded Control Panel at `http://<your-pi-ip>:8096/control`, or restart the service after editing the file directly.

---

## Scripts

All Python scripts live in `scripts/` and read `config.json` from the parent directory automatically. Override with the `MKV_PROXY_CONFIG_PATH` environment variable.

| Script | Description |
|--------|-------------|
| `gostorm-sync-complete.py` | Primary daily sync: queries TMDB popular/trending movies, finds torrents via Torrentio, adds to GoStorm, creates virtual `.mkv` stubs. Run via cron or manually. |
| `gostorm-tv-sync.py` | TV series sync (weekly): fullpack-first approach with automatic quality upgrades. Organizes files as `Show/Season.01/Show_S01E01_<hash>.mkv`. |
| `plex-watchlist-sync.py` | Watchlist sync (hourly cron): reads Plex cloud watchlist, finds movies not yet in the library, adds them. Supports `--dry-run --verbose` for testing. |
| `health-monitor.py` | Web dashboard on `:8095`: real-time metrics for GoStorm, FUSE, system resources, and active torrents. Requires `fastapi`, `uvicorn`, `psutil`. |

Install Python dependencies:

```bash
pip3 install requests psutil fastapi uvicorn
```

---

## Service Management

```bash
# Start / stop / restart
sudo systemctl start gostream
sudo systemctl stop gostream
sudo systemctl restart gostream

# Status and logs
sudo systemctl status gostream
tail -f /home/pi/logs/gostream.log

# Health monitor
sudo systemctl start health-monitor
sudo systemctl status health-monitor

# Check GoStorm API
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"action":"list"}' http://<your-pi-ip>:8090/torrents | jq length

# Check metrics
curl -s http://<your-pi-ip>:8096/metrics | jq '{version, uptime}'
```

The service templates (`gostream.service`, `health-monitor.service`) are included in the repository. Edit `User`, `WorkingDirectory`, and path values to match your install before copying to `/etc/systemd/system/`.

---

## Runtime Memory Tuning

Set via the systemd service environment (no recompile needed):

```
GOMEMLIMIT=2200MiB   # hard Go memory limit — tune to available RAM
GOGC=100             # standard GC pressure
```

For Pi 4 with 4 GB RAM and other services running, `GOMEMLIMIT=2200MiB` leaves headroom for the OS, Samba, and Python scripts.

---

## License

MIT
