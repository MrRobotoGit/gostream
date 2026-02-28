#!/usr/bin/env bash
# ==============================================================================
# GoStream Installer v1.4.4
# GoStream + GoStorm (Unified Engine)
# Target: auto-detected at install time
# ==============================================================================
set -e

# ------------------------------------------------------------------------------
# Color output (graceful fallback when not running in a terminal)
# ------------------------------------------------------------------------------
if [ -t 1 ] && command -v tput >/dev/null 2>&1; then
    RED=$(tput setaf 1)
    GREEN=$(tput setaf 2)
    YELLOW=$(tput setaf 3)
    BLUE=$(tput setaf 4)
    BOLD=$(tput bold)
    NC=$(tput sgr0)
else
    RED='\033[0;31m'
    GREEN='\033[0;32m'
    YELLOW='\033[1;33m'
    BLUE='\033[0;34m'
    BOLD='\033[1m'
    NC='\033[0m'
fi

# ------------------------------------------------------------------------------
# Helper: print a colored section header
# ------------------------------------------------------------------------------
print_header() {
    echo ""
    echo "${BOLD}${BLUE}=== $1 ===${NC}"
    echo ""
}

print_ok()   { echo "  ${GREEN}✓${NC} $1"; }
print_warn() { echo "  ${YELLOW}⚠${NC}  $1"; }
print_err()  { echo "  ${RED}✗${NC} $1"; }
print_info() { echo "  ${BLUE}→${NC} $1"; }

# ------------------------------------------------------------------------------
# Helper: ask "Prompt" "default" VAR_NAME
#   Displays [default] hint; if user presses Enter, uses default.
# ------------------------------------------------------------------------------
ask() {
    local prompt="$1"
    local default="$2"
    local var_name="$3"
    local user_input

    if [ -n "$default" ]; then
        printf "  %s [%s]: " "$prompt" "$default"
    else
        printf "  %s: " "$prompt"
    fi

    read -r user_input
    if [ -z "$user_input" ]; then
        user_input="$default"
    fi
    # Assign to the caller's variable by name
    printf -v "$var_name" '%s' "$user_input"
}

# ------------------------------------------------------------------------------
# Helper: ask_secret "Prompt" VAR_NAME
#   Hidden input (no echo); no default shown for security.
# ------------------------------------------------------------------------------
ask_secret() {
    local prompt="$1"
    local var_name="$2"
    local user_input

    printf "  %s: " "$prompt"
    read -rs user_input
    echo ""  # newline after hidden input
    printf -v "$var_name" '%s' "$user_input"
}

# ------------------------------------------------------------------------------
# Helper: ask_yn "Question" [default_yn]
#   Returns 0 for yes, 1 for no.
#   default_yn should be "y" or "n" (case-insensitive).
# ------------------------------------------------------------------------------
ask_yn() {
    local question="$1"
    local default="${2:-n}"
    local hint user_input

    if [ "${default,,}" = "y" ]; then
        hint="Y/n"
    else
        hint="y/N"
    fi

    printf "  %s [%s]: " "$question" "$hint"
    read -r user_input

    if [ -z "$user_input" ]; then
        user_input="$default"
    fi

    case "${user_input,,}" in
        y|yes) return 0 ;;
        *)     return 1 ;;
    esac
}

# ------------------------------------------------------------------------------
# ASCII Banner
# ------------------------------------------------------------------------------
show_banner() {
    echo ""
    echo "${BOLD}${BLUE}╔══════════════════════════════════════╗${NC}"
    echo "${BOLD}${BLUE}║     GoStream Installer v1.4.4        ║${NC}"
    echo "${BOLD}${BLUE}╚══════════════════════════════════════╝${NC}"
    echo ""
    echo "  GoStream + GoStorm — Unified BitTorrent + FUSE Streaming Engine"
    echo "  Target:         $(uname -m) / $(uname -s), 24/7 production"
    echo "  Includes:       cron setup, sudoers, Plex webhook guide"
    echo ""
}

# ==============================================================================
# [0a] Auto-install system dependencies via apt
# ==============================================================================
install_system_deps() {
    # Only run on Debian/Ubuntu-based systems
    if ! command -v apt-get >/dev/null 2>&1; then
        print_warn "apt-get not found — skipping automatic dependency installation."
        print_warn "Please install manually: git python3-pip fuse3 curl samba"
        return 0
    fi

    # Map: package name → apt package to install
    local -A needed=()

    command -v git         >/dev/null 2>&1 || needed["git"]="git"
    command -v pip3        >/dev/null 2>&1 || needed["pip3"]="python3-pip"
    command -v fusermount3 >/dev/null 2>&1 || needed["fusermount3"]="fuse3"
    command -v curl        >/dev/null 2>&1 || needed["curl"]="curl"
    command -v samba       >/dev/null 2>&1 || needed["samba"]="samba"
    # libfuse3-dev is required for CGO_ENABLED=1 compilation (provides fuse.h)
    dpkg -s libfuse3-dev   >/dev/null 2>&1 || needed["libfuse3-dev"]="libfuse3-dev"
    # gcc is required for CGO
    command -v gcc         >/dev/null 2>&1 || needed["gcc"]="gcc"

    if [ "${#needed[@]}" -eq 0 ]; then
        print_ok "All system dependencies already installed."
        return 0
    fi

    print_header "Installing System Dependencies"
    print_info "Missing packages: ${needed[*]}"
    print_info "Running: sudo apt-get update && sudo apt-get install -y ${needed[*]}"
    echo ""

    sudo apt-get update -qq
    sudo apt-get install -y "${needed[@]}"

    echo ""
    print_ok "System dependencies installed."
}

# ==============================================================================
# [0] Prerequisite checks
# ==============================================================================
check_prerequisites() {
    print_header "Checking Prerequisites"

    local all_ok=true

    # python3 >= 3.9
    if command -v python3 >/dev/null 2>&1; then
        local py_ver
        py_ver=$(python3 -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")')
        local py_major py_minor
        py_major=$(echo "$py_ver" | cut -d. -f1)
        py_minor=$(echo "$py_ver" | cut -d. -f2)
        if [ "$py_major" -gt 3 ] || { [ "$py_major" -eq 3 ] && [ "$py_minor" -ge 9 ]; }; then
            print_ok "python3 ($py_ver)"
        else
            print_err "python3 found but version $py_ver < 3.9 (required)"
            all_ok=false
        fi
    else
        print_err "python3 not found (required)"
        all_ok=false
    fi

    # pip3
    if command -v pip3 >/dev/null 2>&1; then
        print_ok "pip3"
    else
        print_err "pip3 not found (required for Python dependencies)"
        all_ok=false
    fi

    # fusermount3 or fusermount (FUSE userspace tool)
    if command -v fusermount3 >/dev/null 2>&1; then
        print_ok "fusermount3"
        FUSERMOUNT_CMD="fusermount3"
    elif command -v fusermount >/dev/null 2>&1; then
        print_ok "fusermount (fusermount3 preferred but fusermount found)"
        FUSERMOUNT_CMD="fusermount"
    else
        print_err "fusermount3/fusermount not found (install: sudo apt install fuse3)"
        all_ok=false
    fi

    # systemctl
    if command -v systemctl >/dev/null 2>&1; then
        print_ok "systemctl"
    else
        print_err "systemctl not found (systemd required for service management)"
        all_ok=false
    fi

    # curl (used for Plex auto-discovery and health-check — not strictly fatal)
    if command -v curl >/dev/null 2>&1; then
        print_ok "curl"
    else
        print_warn "curl not found — Plex library auto-discovery will be skipped"
    fi

    echo ""
    if [ "$all_ok" = false ]; then
        print_err "One or more required prerequisites are missing. Please install them and re-run."
        exit 1
    fi
}

# ==============================================================================
# [1/5] System Paths
# ==============================================================================
collect_paths() {
    print_header "[1/5] System Paths"

    ask "GoStream install directory" "/home/pi/GoStream" INSTALL_DIR
    ask "Physical MKV source path   (physical_source_path)" "/mnt/gostream-mkv-real" STORAGE_PATH
    ask "FUSE virtual mount path     (fuse_mount_path)"     "/mnt/gostream-mkv-virtual" FUSE_MOUNT
    ask "System user that owns GoStream" "pi" SYSTEM_USER

    # Derive BASE_DIR as the parent of INSTALL_DIR (holds logs/ and STATE/)
    BASE_DIR="$(dirname "${INSTALL_DIR}")"

    echo ""
    print_info "Derived base directory : ${BASE_DIR}"
    print_info "Logs directory         : ${BASE_DIR}/logs/"
    print_info "State directory        : ${BASE_DIR}/STATE/"
}

# ==============================================================================
# [2/5] Plex Configuration
# ==============================================================================
collect_plex() {
    print_header "[2/5] Plex Configuration"

    ask       "Plex server URL"  "http://127.0.0.1:32400" PLEX_URL
    ask_secret "Plex token (hidden)" PLEX_TOKEN

    PLEX_LIBRARY_ID=0

    # Auto-discover library sections if we have curl and a non-empty token
    if [ -n "$PLEX_TOKEN" ] && command -v curl >/dev/null 2>&1; then
        echo ""
        print_info "Querying Plex for library sections..."
        local sections_xml
        if sections_xml=$(curl -sf --max-time 8 \
                "${PLEX_URL}/library/sections?X-Plex-Token=${PLEX_TOKEN}" 2>/dev/null); then
            # Parse XML with python3: extract (key, title) pairs for video libraries
            local parsed
            parsed=$(python3 - <<'PYEOF'
import sys, xml.etree.ElementTree as ET

xml_text = sys.stdin.read()
try:
    root = ET.fromstring(xml_text)
except ET.ParseError as e:
    print(f"XML_PARSE_ERROR:{e}", file=sys.stderr)
    sys.exit(1)

sections = []
for directory in root.findall('Directory'):
    lib_type = directory.get('type', '')
    if lib_type in ('movie', 'show'):
        key = directory.get('key', '')
        title = directory.get('title', '(unknown)')
        sections.append(f"{key}:{title}:{lib_type}")

print('\n'.join(sections))
PYEOF
                <<< "$sections_xml") || true

            if [ -n "$parsed" ]; then
                echo ""
                echo "  Available Plex libraries:"
                local i=1
                local -a lib_keys lib_titles
                while IFS= read -r line; do
                    local key title lib_type
                    key="${line%%:*}"
                    rest="${line#*:}"
                    lib_type="${rest##*:}"
                    title="${rest%:*}"
                    lib_keys+=("$key")
                    lib_titles+=("$title ($lib_type)")
                    printf "    %d) [%s] %s\n" "$i" "$key" "$title ($lib_type)"
                    (( i++ )) || true
                done <<< "$parsed"

                echo ""
                ask "Select library number (0 to enter manually)" "0" _lib_choice

                if [ "$_lib_choice" -gt 0 ] 2>/dev/null && \
                   [ "$_lib_choice" -le "${#lib_keys[@]}" ] 2>/dev/null; then
                    local idx=$(( _lib_choice - 1 ))
                    PLEX_LIBRARY_ID="${lib_keys[$idx]}"
                    print_ok "Selected: ${lib_titles[$idx]} (ID: ${PLEX_LIBRARY_ID})"
                else
                    ask "Plex library ID (numeric)" "1" PLEX_LIBRARY_ID
                fi
            else
                print_warn "No movie/TV libraries found — entering manually."
                ask "Plex library ID (numeric)" "1" PLEX_LIBRARY_ID
            fi
        else
            print_warn "Could not reach Plex at ${PLEX_URL} — entering library ID manually."
            ask "Plex library ID (numeric)" "1" PLEX_LIBRARY_ID
        fi
    else
        if [ -z "$PLEX_TOKEN" ]; then
            print_warn "No Plex token provided — skipping auto-discovery."
        fi
        ask "Plex library ID (numeric)" "1" PLEX_LIBRARY_ID
    fi
}

# ==============================================================================
# [3/5] External APIs
# ==============================================================================
collect_apis() {
    print_header "[3/5] External APIs"

    ask "TMDB API key (optional — leave empty to skip movie sync)" "" TMDB_API_KEY
    ask "Torrentio base URL" "https://torrentio.strem.fun" TORRENTIO_URL

    if [ -z "$TMDB_API_KEY" ]; then
        print_warn "No TMDB key entered. Movie sync (gostorm-sync-complete.py) will not function."
    else
        print_ok "TMDB API key set."
    fi
}

# ==============================================================================
# [4/5] Hardware & Network
# ==============================================================================
collect_hardware() {
    print_header "[4/5] Hardware & Network"

    ask "GOMEMLIMIT (MiB)  — 2200 is optimal for Pi 4 / 4GB" "2200" GOMEMLIMIT_MB
    ask "Disk warmup quota (GB)" "3" DISK_WARMUP_GB
    ask "Proxy listen port        (proxy_listen_port)" "8080" PROXY_PORT
    ask "Metrics/dashboard port   (metrics_port)"      "8096" METRICS_PORT
    ask "Health monitor port      (health-monitor.py)" "8095" DASHBOARD_PORT

    # NAT-PMP
    echo ""
    NATPMP_ENABLED=false
    NATPMP_GATEWAY=""
    VPN_INTERFACE="wg0"

    if ask_yn "Enable NAT-PMP (WireGuard port forwarding)?" "n"; then
        NATPMP_ENABLED=true
        ask "NAT-PMP gateway IP" "" NATPMP_GATEWAY
        ask "VPN interface" "wg0" VPN_INTERFACE
        print_ok "NAT-PMP enabled (gateway: ${NATPMP_GATEWAY}, interface: ${VPN_INTERFACE})"
    else
        print_info "NAT-PMP disabled."
    fi
}

# ==============================================================================
# [5/5] Installing — step implementations
# ==============================================================================

# ------------------------------------------------------------------------------
# 5a. Generate config.json from config.json.example
# ------------------------------------------------------------------------------
generate_config_json() {
    local output_path="${INSTALL_DIR}/config.json"

    # Ensure INSTALL_DIR exists before writing anything
    mkdir -p "${INSTALL_DIR}"

    # Look for config.json.example: first in the repo (SCRIPT_DIR), then in INSTALL_DIR
    local example_path
    if [ -f "${SCRIPT_DIR}/config.json.example" ]; then
        example_path="${SCRIPT_DIR}/config.json.example"
    else
        example_path="${INSTALL_DIR}/config.json.example"
    fi

    print_info "Generating config.json..."

    if [ ! -f "$example_path" ]; then
        print_warn "config.json.example not found — using built-in template."
        example_path="${INSTALL_DIR}/config.json.example"
        mkdir -p "${INSTALL_DIR}"
        # Write the embedded template so the python script below can update it
        cat > "$example_path" <<'TEMPLATE_EOF'
{
  "master_concurrency_limit": 25,
  "read_ahead_budget_mb": 256,
  "metadata_cache_size_mb": 50,
  "write_buffer_size_kb": 64,
  "read_buffer_size_kb": 1024,
  "fuse_block_size_bytes": 1048576,
  "streaming_threshold_kb": 128,
  "max_concurrent_streaming": 25,
  "log_level": "INFO",
  "attr_timeout_seconds": 1,
  "entry_timeout_seconds": 1,
  "negative_timeout_seconds": 0,
  "http_client_timeout_seconds": 30,
  "max_retry_attempts": 6,
  "retry_delay_ms": 500,
  "rescue_grace_period_seconds": 240,
  "rescue_cooldown_hours": 24,
  "preload_workers_count": 4,
  "preload_initial_delay_ms": 1000,
  "warm_start_idle_seconds": 6,
  "max_concurrent_prefetch": 3,
  "cache_cleanup_interval_min": 5,
  "max_cache_entries": 10000,
  "gostorm_url": "http://127.0.0.1:8090",
  "proxy_listen_port": 8080,
  "metrics_port": 8096,
  "blocklist_url": "https://github.com/Naunter/BT_BlockLists/raw/master/bt_blocklists.gz",
  "physical_source_path": "/mnt/gostream-mkv-real",
  "fuse_mount_path": "/mnt/gostream-mkv-virtual",
  "disk_warmup_quota_gb": 3,
  "warmup_head_size_mb": 64,
  "natpmp": {
    "enabled": false,
    "gateway": "",
    "local_port": 8091,
    "vpn_interface": "wg0",
    "lifetime": 60,
    "refresh": 45
  },
  "plex": {
    "url": "http://127.0.0.1:32400",
    "token": "",
    "library_id": 0
  },
  "tmdb_api_key": ""
}
TEMPLATE_EOF
    fi

    # Use python3 to safely read JSON, update fields, and write output
    python3 - <<PYEOF
import json, sys

example_path = "${example_path}"
output_path  = "${output_path}"

with open(example_path, 'r') as fh:
    cfg = json.load(fh)

# --- Paths ---
cfg['physical_source_path'] = "${STORAGE_PATH}"
cfg['fuse_mount_path']       = "${FUSE_MOUNT}"

# --- Network ---
cfg['proxy_listen_port'] = int("${PROXY_PORT}")
cfg['metrics_port']      = int("${METRICS_PORT}")

# --- Hardware ---
cfg['disk_warmup_quota_gb'] = int("${DISK_WARMUP_GB}")

# --- External APIs ---
if "${TMDB_API_KEY}":
    cfg['tmdb_api_key'] = "${TMDB_API_KEY}"

# --- Plex block ---
if 'plex' not in cfg or not isinstance(cfg.get('plex'), dict):
    cfg['plex'] = {}
cfg['plex']['url']        = "${PLEX_URL}"
cfg['plex']['token']      = "${PLEX_TOKEN}"
try:
    cfg['plex']['library_id'] = int("${PLEX_LIBRARY_ID}")
except ValueError:
    cfg['plex']['library_id'] = 0

# --- NAT-PMP block ---
if 'natpmp' not in cfg or not isinstance(cfg.get('natpmp'), dict):
    cfg['natpmp'] = {
        "local_port": 8091,
        "lifetime": 60,
        "refresh": 45
    }
natpmp_enabled_str = "${NATPMP_ENABLED}"
cfg['natpmp']['enabled']       = natpmp_enabled_str.lower() == 'true'
cfg['natpmp']['gateway']       = "${NATPMP_GATEWAY}"
cfg['natpmp']['vpn_interface'] = "${VPN_INTERFACE}"

with open(output_path, 'w') as fh:
    json.dump(cfg, fh, indent=2)
    fh.write('\n')

print(f"  Written: {output_path}")
PYEOF

    print_ok "config.json written to ${output_path}"
}

# ------------------------------------------------------------------------------
# 5a2. Deploy files from repo to INSTALL_DIR
# ------------------------------------------------------------------------------
deploy_files() {
    print_info "Deploying files to ${INSTALL_DIR}..."

    mkdir -p "${INSTALL_DIR}/scripts"

    # Copy scripts
    if [ -d "${SCRIPT_DIR}/scripts" ]; then
        cp -r "${SCRIPT_DIR}/scripts/." "${INSTALL_DIR}/scripts/"
        print_ok "Scripts deployed to ${INSTALL_DIR}/scripts/"
    else
        print_warn "scripts/ directory not found in ${SCRIPT_DIR} — skipping."
    fi

    # Copy config.json.example (useful reference for the user)
    if [ -f "${SCRIPT_DIR}/config.json.example" ]; then
        cp "${SCRIPT_DIR}/config.json.example" "${INSTALL_DIR}/config.json.example"
        print_ok "config.json.example deployed to ${INSTALL_DIR}/"
    fi

    # Copy requirements.txt
    if [ -f "${SCRIPT_DIR}/requirements.txt" ]; then
        cp "${SCRIPT_DIR}/requirements.txt" "${INSTALL_DIR}/requirements.txt"
        print_ok "requirements.txt deployed to ${INSTALL_DIR}/"
    fi
}

# ------------------------------------------------------------------------------
# 5b. Install Python dependencies
# ------------------------------------------------------------------------------
install_python_deps() {
    local req_file="${INSTALL_DIR}/requirements.txt"

    print_info "Installing Python dependencies..."

    # --break-system-packages is required on Debian 12+ / Raspberry Pi OS Bookworm
    # This is intentional: the Pi is a single-purpose server, not a shared system.
    local pip_flags="--quiet --break-system-packages --no-warn-script-location"

    if [ -f "$req_file" ]; then
        pip3 install -r "$req_file" $pip_flags
        print_ok "Python dependencies installed from requirements.txt"
    else
        # Install the known runtime dependencies directly
        print_warn "requirements.txt not found — installing known dependencies."
        pip3 install $pip_flags \
            requests \
            psutil \
            "fastapi>=0.100.0" \
            "uvicorn[standard]"
        print_ok "Core Python packages installed (requests, psutil, fastapi, uvicorn)"
    fi
}

# ------------------------------------------------------------------------------
# 5c. Create directories
# ------------------------------------------------------------------------------
create_directories() {
    print_info "Creating required directories..."

    # Directories that belong to the system user (no sudo needed if running as that user)
    local user_dirs=(
        "${BASE_DIR}/STATE"
        "${BASE_DIR}/logs"
    )

    for d in "${user_dirs[@]}"; do
        if mkdir -p "$d" 2>/dev/null; then
            print_ok "Created: $d"
        elif sudo mkdir -p "$d"; then
            sudo chown "${SYSTEM_USER}:${SYSTEM_USER}" "$d"
            print_ok "Created (sudo): $d"
        else
            print_err "Failed to create: $d"
            exit 1
        fi
    done

    # Directories under /mnt may require sudo
    local root_dirs=(
        "${STORAGE_PATH}/movies"
        "${STORAGE_PATH}/tv"
        "${FUSE_MOUNT}"
    )

    for d in "${root_dirs[@]}"; do
        if mkdir -p "$d" 2>/dev/null; then
            chown "${SYSTEM_USER}:${SYSTEM_USER}" "$d" 2>/dev/null || true
            print_ok "Created: $d"
        else
            print_info "Creating ${d} requires sudo..."
            sudo mkdir -p "$d"
            sudo chown "${SYSTEM_USER}:${SYSTEM_USER}" "$d"
            print_ok "Created (sudo): $d"
        fi
    done
}

# ------------------------------------------------------------------------------
# 5d. Install systemd service files
# ------------------------------------------------------------------------------
install_services() {
    print_info "Installing systemd service files (requires sudo)..."

    # -- gostream.service --
    sudo tee /etc/systemd/system/gostream.service > /dev/null <<SERVICE_EOF
[Unit]
Description=GoStream + GoStorm (Unified Streaming Engine)
After=network-online.target systemd-resolved.service nss-lookup.target local-fs.target remote-fs.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
# Memory tuning — GOMEMLIMIT=${GOMEMLIMIT_MB}MiB is optimal for Pi 4 / 4GB
Environment=GOMEMLIMIT=${GOMEMLIMIT_MB}MiB
Environment=GOGC=100

Type=simple
User=${SYSTEM_USER}
Group=${SYSTEM_USER}

WorkingDirectory=${INSTALL_DIR}

# Wait for DNS before starting (required for tracker + blocklist resolution)
ExecStartPre=/bin/sh -c 'for i in 1 2 3 4 5; do nslookup www.google.com >/dev/null 2>&1 && exit 0 || sleep 2; done; exit 1'

# FUSE mount cleanup and creation
ExecStartPre=-/usr/bin/${FUSERMOUNT_CMD} -uz ${FUSE_MOUNT}
ExecStartPre=/bin/mkdir -p ${FUSE_MOUNT}

# Main binary — no CLI args needed:
#   --path defaults to WorkingDirectory (${INSTALL_DIR})
#   physical_source_path and fuse_mount_path are read from config.json
ExecStart=${INSTALL_DIR}/gostream

# Allow gostream to stabilize, then restart Samba so it sees the FUSE mount
ExecStartPost=/bin/sleep 2
ExecStartPost=/usr/bin/sudo /bin/systemctl restart smbd

Restart=always
RestartSec=10
LimitNOFILE=65536
LimitNPROC=4096

StandardOutput=append:${BASE_DIR}/logs/gostream.log
StandardError=append:${BASE_DIR}/logs/gostream.log

# Cleanly unmount FUSE on stop
ExecStop=/usr/bin/${FUSERMOUNT_CMD} -uz ${FUSE_MOUNT}

[Install]
WantedBy=multi-user.target
SERVICE_EOF

    print_ok "Wrote /etc/systemd/system/gostream.service"

    # -- health-monitor.service --
    sudo tee /etc/systemd/system/health-monitor.service > /dev/null <<HEALTHSVC_EOF
[Unit]
Description=GoStorm Health Monitor Dashboard
After=network.target gostream.service

[Service]
Type=simple
User=${SYSTEM_USER}
WorkingDirectory=${INSTALL_DIR}
ExecStart=/usr/bin/python3 ${INSTALL_DIR}/scripts/health-monitor.py
Restart=always
RestartSec=5
TimeoutStopSec=5
Environment=PYTHONUNBUFFERED=1

[Install]
WantedBy=multi-user.target
HEALTHSVC_EOF

    print_ok "Wrote /etc/systemd/system/health-monitor.service"
}

# ------------------------------------------------------------------------------
# 5e. Enable services via systemd
# ------------------------------------------------------------------------------
enable_services() {
    print_info "Reloading systemd and enabling services..."

    sudo systemctl daemon-reload
    sudo systemctl enable gostream health-monitor

    print_ok "Services enabled: gostream, health-monitor"
}

# ------------------------------------------------------------------------------
# 5f. Verify installation (non-fatal — binary may not be deployed yet)
# ------------------------------------------------------------------------------
verify_install() {
    print_info "Verifying installation (checking metrics endpoint)..."

    local url="http://127.0.0.1:${METRICS_PORT}/metrics"
    if command -v curl >/dev/null 2>&1; then
        if curl -sf --max-time 5 "$url" >/dev/null 2>&1; then
            print_ok "GoStream metrics endpoint is reachable at ${url}"
        else
            print_warn "GoStream is not running yet (metrics endpoint not reachable)."
            print_warn "This is expected if the binary has not been copied and started."
        fi
    else
        print_warn "curl not available — skipping endpoint verification."
    fi
}

# ------------------------------------------------------------------------------
# 5f2. Install Go toolchain if missing or wrong architecture
# ------------------------------------------------------------------------------
GO_BIN=""
GO_ARCH=""
GO_OS=""

detect_go_arch() {
    local machine
    machine="$(uname -m)"
    GO_OS="$(uname -s | tr '[:upper:]' '[:lower:]')"

    case "$machine" in
        aarch64|arm64)    GO_ARCH="arm64"   ;;
        x86_64|amd64)     GO_ARCH="amd64"   ;;
        armv7l|armv7)     GO_ARCH="arm"     ;;
        armv6l|armv6)     GO_ARCH="armv6l"  ;;
        i686|i386)        GO_ARCH="386"     ;;
        *)
            print_err "Unsupported architecture: ${machine}"
            exit 1
            ;;
    esac

    print_info "Detected platform: ${GO_OS}/${GO_ARCH}"
}

ensure_go() {
    local go_install_dir="/usr/local/go"

    detect_go_arch

    # Find an existing Go binary that matches the detected OS/arch
    local candidates=("${go_install_dir}/bin/go" "$(command -v go 2>/dev/null)")
    for candidate in "${candidates[@]}"; do
        if [ -x "$candidate" ]; then
            local info
            info=$("$candidate" version 2>/dev/null)
            if echo "$info" | grep -q "${GO_OS}/${GO_ARCH}"; then
                GO_BIN="$candidate"
                print_ok "Go found: $info"
                return 0
            fi
        fi
    done

    # Fetch the latest stable Go version number from go.dev
    local go_version
    go_version=$(curl -fsSL "https://go.dev/VERSION?m=text" | head -1)
    if [ -z "$go_version" ]; then
        go_version="go1.24.0"   # fallback if network unavailable
    fi

    print_info "${go_version} (${GO_OS}/${GO_ARCH}) not found — downloading..."

    local tarball="${go_version}.${GO_OS}-${GO_ARCH}.tar.gz"
    local url="https://go.dev/dl/${tarball}"
    local tmp="/tmp/${tarball}"

    curl -fL --progress-bar -o "$tmp" "$url"
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf "$tmp"
    rm -f "$tmp"

    GO_BIN="${go_install_dir}/bin/go"
    print_ok "Go installed: $($GO_BIN version)"
}

# ------------------------------------------------------------------------------
# 5f3. Compile the GoStream binary from source
# ------------------------------------------------------------------------------
ensure_swap() {
    # Go compilation can OOM on Pi with little/no swap — ensure at least 1GB
    local swap_total
    swap_total=$(free -m | awk '/^Swap:/ {print $2}')
    if [ "${swap_total:-0}" -lt 1024 ]; then
        print_info "Swap < 1 GB detected (${swap_total} MB) — creating 1 GB swapfile for compilation..."
        if [ ! -f /swapfile ]; then
            sudo fallocate -l 1G /swapfile 2>/dev/null || sudo dd if=/dev/zero of=/swapfile bs=1M count=1024 status=none
            sudo chmod 600 /swapfile
            sudo mkswap /swapfile >/dev/null
        fi
        sudo swapon /swapfile 2>/dev/null || true
        print_ok "Swapfile active ($(free -m | awk '/^Swap:/ {print $2}') MB total swap)"
    fi
}

compile_binary() {
    print_info "Compiling GoStream binary (this takes a few minutes on Pi 4)..."

    ensure_go
    ensure_swap

    local src_dir="${SCRIPT_DIR}"
    local out_bin="${INSTALL_DIR}/gostream"

    # Verify we have Go source files in the expected location
    if [ ! -f "${src_dir}/main.go" ]; then
        print_err "main.go not found in ${src_dir} — cannot compile."
        exit 1
    fi

    cd "${src_dir}"

    print_info "Running go mod tidy..."
    GOTOOLCHAIN=local "$GO_BIN" mod tidy

    # Use -pgo=off if no default.pgo present (fresh install)
    local pgo_flag="-pgo=off"
    if [ -f "${src_dir}/default.pgo" ]; then
        pgo_flag="-pgo=auto"
        print_info "PGO profile found — building with -pgo=auto"
    else
        print_info "No PGO profile — building with -pgo=off (regenerate later for 5-7% CPU gain)"
    fi

    # -p 2 limits parallel jobs to keep peak RAM under control on Pi 4
    # GOTMPDIR on the main FS avoids OOM on small /tmp tmpfs during linking
    local go_tmp="${HOME}/go-tmp"
    mkdir -p "${go_tmp}"
    print_info "Building binary (GOARCH=${GO_ARCH}, -p 2)..."
    GOTOOLCHAIN=local GOARCH="${GO_ARCH}" CGO_ENABLED=1 GOTMPDIR="${go_tmp}" \
        "$GO_BIN" build ${pgo_flag} -p 2 -o "${out_bin}" .
    rm -rf "${go_tmp}"

    chmod +x "${out_bin}"
    print_ok "Binary compiled and deployed: ${out_bin}"

    cd - >/dev/null
}

# ------------------------------------------------------------------------------
# 5g. Sudoers entry so gostream.service can restart smbd without a password
# ------------------------------------------------------------------------------
setup_sudoers() {
    print_info "Configuring sudoers for smbd restart..."

    local sudoers_file="/etc/sudoers.d/gostream-smbd"
    local sudoers_line="${SYSTEM_USER} ALL=(ALL) NOPASSWD: /bin/systemctl restart smbd"

    # Check whether the entry already exists anywhere in sudoers
    if sudo grep -qF "$sudoers_line" /etc/sudoers /etc/sudoers.d/* 2>/dev/null; then
        print_ok "Sudoers entry already present — no change needed."
        return 0
    fi

    if sudo sh -c "echo '${sudoers_line}' | tee ${sudoers_file} > /dev/null"; then
        sudo chmod 440 "${sudoers_file}"
        print_ok "Sudoers entry written: ${sudoers_file}"
    else
        print_warn "Could not write sudoers entry (sudo unavailable or permission denied)."
        print_warn "To add manually, run:"
        print_warn "  echo '${sudoers_line}' | sudo tee ${sudoers_file} && sudo chmod 440 ${sudoers_file}"
    fi
}

# ------------------------------------------------------------------------------
# 5h. Cron jobs for sync scripts (optional)
# ------------------------------------------------------------------------------
setup_cron_jobs() {
    print_info "Setting up cron jobs for sync scripts..."

    if ! ask_yn "Set up cron jobs for sync scripts?" "y"; then
        print_info "Skipping cron job setup."
        return 0
    fi

    local logs_dir="${BASE_DIR}/logs"

    # Each entry: "schedule script logfile"
    local -a cron_entries=(
        "0 * * * * /usr/bin/python3 ${INSTALL_DIR}/scripts/plex-watchlist-sync.py >> ${logs_dir}/watchlist-sync.log 2>&1"
        "0 3 * * * /usr/bin/python3 ${INSTALL_DIR}/scripts/gostorm-sync-complete.py >> ${logs_dir}/gostorm-debug.log 2>&1"
        "0 4 * * 0 /usr/bin/python3 ${INSTALL_DIR}/scripts/gostorm-tv-sync.py >> ${logs_dir}/gostorm-tv-sync.log 2>&1"
    )

    # Script name substrings used for deduplication checks
    local -a cron_markers=(
        "plex-watchlist-sync.py"
        "gostorm-sync-complete.py"
        "gostorm-tv-sync.py"
    )

    # Decide whether to edit root's crontab (via sudo -u SYSTEM_USER) or current user
    local crontab_cmd
    if [ "$(id -u)" -eq 0 ]; then
        crontab_cmd="crontab -u ${SYSTEM_USER}"
    else
        crontab_cmd="crontab"
    fi

    # Load existing crontab (gracefully handle empty/missing crontab)
    local existing_crontab
    existing_crontab=$(${crontab_cmd} -l 2>/dev/null || true)

    local new_crontab="$existing_crontab"
    local added=0

    for i in "${!cron_entries[@]}"; do
        local entry="${cron_entries[$i]}"
        local marker="${cron_markers[$i]}"

        if echo "$existing_crontab" | grep -qF "$marker"; then
            print_warn "Cron entry for ${marker} already exists — skipping."
        else
            # Append a newline before the new entry if crontab is not empty
            if [ -n "$new_crontab" ]; then
                new_crontab="${new_crontab}"$'\n'"${entry}"
            else
                new_crontab="${entry}"
            fi
            print_ok "Cron added: ${entry}"
            (( added++ )) || true
        fi
    done

    if [ "$added" -gt 0 ]; then
        echo "$new_crontab" | ${crontab_cmd} -
        print_ok "${added} cron job(s) installed."
    else
        print_info "No new cron entries were needed."
    fi
}

# ==============================================================================
# Final summary
# ==============================================================================
show_summary() {
    echo ""
    echo "${BOLD}${GREEN}╔══════════════════════════════════════╗${NC}"
    echo "${BOLD}${GREEN}║  Installation Complete!              ║${NC}"
    echo "${BOLD}${GREEN}╚══════════════════════════════════════╝${NC}"
    echo ""
    echo "  Configuration written to:"
    echo "    ${BOLD}${INSTALL_DIR}/config.json${NC}"
    echo ""
    echo "  Service files installed:"
    echo "    /etc/systemd/system/gostream.service"
    echo "    /etc/systemd/system/health-monitor.service"
    echo ""
    echo "${BOLD}Next steps:${NC}"
    echo ""
    echo "  1. Start services:"
    echo "     ${YELLOW}sudo systemctl start gostream health-monitor${NC}"
    echo ""
    echo "  2. Configure Plex Webhook:"
    echo "     Open Plex → Settings → Webhooks → Add:"
    echo "     ${BOLD}http://<your-pi-ip>:${METRICS_PORT}/plex-webhook${NC}"
    echo ""
    echo "  3. Configure Samba (critical for stability):"
    echo "     Edit /etc/samba/smb.conf and ensure your share has:"
    echo "       oplocks = no"
    echo "       aio read size = 1"
    echo "       deadtime = 15"
    echo "       vfs objects = fileid"
    echo "     Then: ${YELLOW}sudo systemctl restart smbd${NC}"
    echo ""
    echo "  4. Check status:"
    echo "     ${YELLOW}sudo systemctl status gostream${NC}"
    echo "     ${YELLOW}curl http://127.0.0.1:${METRICS_PORT}/metrics${NC}"
    echo ""
    echo "  5. Dashboards:"
    echo "     ${BOLD}http://<your-ip>:${DASHBOARD_PORT}${NC}  (Health Monitor)"
    echo "     ${BOLD}http://<your-ip>:${METRICS_PORT}/control${NC}  (GoStream Control Panel)"
    echo ""
    echo "  6. Sync scripts (run manually or via cron):"
    echo "     ${YELLOW}python3 ${INSTALL_DIR}/scripts/gostorm-sync-complete.py${NC}  # Movies"
    echo "     ${YELLOW}python3 ${INSTALL_DIR}/scripts/gostorm-tv-sync.py${NC}         # TV"
    echo "     ${YELLOW}python3 ${INSTALL_DIR}/scripts/plex-watchlist-sync.py${NC}     # Watchlist"
    echo ""
    echo "  7. Logs:"
    echo "     ${YELLOW}tail -f ${BASE_DIR}/logs/gostream.log${NC}"
    echo ""
}

# ==============================================================================
# Main
# ==============================================================================
main() {
    show_banner

    # Directory containing this script (= cloned repo root)
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

    # Note: FUSERMOUNT_CMD is set inside check_prerequisites
    FUSERMOUNT_CMD="fusermount3"

    install_system_deps
    check_prerequisites

    collect_paths
    collect_plex
    collect_apis
    collect_hardware

    print_header "[5/5] Installing"

    deploy_files
    echo ""
    generate_config_json
    echo ""
    install_python_deps
    echo ""
    create_directories
    echo ""
    install_services
    echo ""
    enable_services
    echo ""
    setup_sudoers
    echo ""
    setup_cron_jobs
    echo ""
    compile_binary
    echo ""
    verify_install

    show_summary
}

main "$@"
