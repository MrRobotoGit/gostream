#!/usr/bin/env python3
"""
Streaming Health Monitor Dashboard
====================================
Modern web dashboard for monitoring GoStorm, FUSE, and system health.

Port: 8095
No authentication, no HTTPS (local network only)

Updated: January 9, 2026 - Now monitoring Go FUSE proxy (mkv-proxy-go)
"""

import json
import logging
import os
import re
import socket
import subprocess
import threading
import time
from collections import deque
from datetime import datetime
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple
from concurrent.futures import ThreadPoolExecutor

import psutil
import requests
from fastapi import FastAPI, Query, Request, File, Form
from fastapi.responses import HTMLResponse, JSONResponse


def _load_gostream_config() -> dict:
    """Load config.json from the GoStream install directory."""
    config_path = os.environ.get(
        'MKV_PROXY_CONFIG_PATH',
        os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', 'config.json')
    )
    try:
        with open(config_path, encoding='utf-8') as f:
            cfg = json.load(f)
    except Exception:
        cfg = {}
    config_dir = os.path.dirname(os.path.abspath(config_path))
    base_dir = os.path.dirname(config_dir)
    cfg.setdefault('_state_dir', os.path.join(base_dir, 'STATE'))
    cfg.setdefault('_log_dir', os.path.join(base_dir, 'logs'))
    return cfg


_cfg = _load_gostream_config()
_scripts_dir = os.path.dirname(os.path.abspath(__file__))

# Configuration
GOSTORM_URL = _cfg.get('gostorm_url', 'http://127.0.0.1:8090')
PRELOAD_SIZE_MB = 128  # MB to preload
FUSE_METRICS_URL = f"http://127.0.0.1:{_cfg.get('metrics_port', 8096)}/metrics"
PLEX_URL     = _cfg.get('plex', {}).get('url', 'http://127.0.0.1:32400')
PLEX_TOKEN   = _cfg.get('plex', {}).get('token', '')
FUSE_MOUNT = _cfg.get('fuse_mount_path', '/mnt/torrserver-go')
MOVIES_DIR = os.path.join(_cfg.get('physical_source_path', '/mnt/torrserver'), 'movies')
TV_DIR = os.path.join(_cfg.get('physical_source_path', '/mnt/torrserver'), 'tv')
LOGS_DIR = _cfg.get('_log_dir', '/home/pi/logs')
STATE_DIR = _cfg.get('_state_dir', '/home/pi/STATE')
SYNC_SCRIPT = os.path.join(_scripts_dir, 'gostorm-sync-complete.py')
SYNC_LOG = os.path.join(LOGS_DIR, 'gostorm-debug.log')
TV_SYNC_SCRIPT = os.path.join(_scripts_dir, 'gostorm-tv-sync.py')
TV_SYNC_LOG = os.path.join(LOGS_DIR, 'gostorm-tv-sync.log')

# TMDB API for movie info
TMDB_API_KEY = _cfg.get('tmdb_api_key', '')
TMDB_BASE_URL = "https://api.themoviedb.org/3"
TMDB_IMAGE_BASE = "https://image.tmdb.org/t/p/w92"  # Small poster

COLLECT_INTERVAL = 5  # seconds
GOSTORM_HEALTH_INTERVAL = 15  # seconds between health checks
GOSTORM_MAX_FAILURES = 3      # failures before auto-restart
RESTART_COOLDOWN = 30  # seconds between restarts
SPEED_HISTORY_SIZE = 180  # 15 minutes of data at 5s intervals
ACTIVE_STICKY_SECONDS = 120  # keep torrent "active" for 15s after speed drops

PORT = 8095

# Logging setup
logging.basicConfig(
    level=logging.INFO,
    format='[%(asctime)s] [%(levelname)s] %(message)s',
    datefmt='%Y-%m-%d %H:%M:%S'
)
logger = logging.getLogger(__name__)

# =============================================================================
# Global State
# =============================================================================

# Auto-Restart tracking
gostorm_fail_count = 0
last_gostorm_health_check = 0

health_data: Dict[str, Any] = {
    "gostorm": {"status": "unknown", "latency_ms": 0, "error": None},
    "fuse": {"status": "unknown", "mounted": False, "files": 0},
    "natpmp": {"status": "unknown", "port": 0},
    "vpn": {"status": "unknown", "ip": "--", "is_up": False},
    "plex": {"status": "unknown", "version": None},
    "system": {"cpu": 0, "ram": 0, "ram_used_gb": 0, "ram_total_gb": 0, "disk_free_gb": 0, "disk_total_gb": 0},
    "torrents": {"total": 0, "downloading": 0, "speed_mbps": 0, "peers": 0, "seeders": 0, "fuse_buffer_active_percent": 0, "fuse_buffer_stale_percent": 0},
    "active_torrents": [],  # List of active torrents for ACTIVE DOWNLOADS panel
    "preload": {"title": None, "status": "idle", "timestamp": None, "poster": None}, # Smart Preload tracking
    "last_update": None
}

speed_history: deque = deque(maxlen=SPEED_HISTORY_SIZE)
last_restart: Dict[str, float] = {}  # service -> timestamp
active_torrents_sticky: Dict[str, float] = {}  # hash -> last_active_timestamp (sticky for 15s)
torrent_speed_history: Dict[str, deque] = {}  # hash -> deque of recent speeds for avg calculation
TORRENT_SPEED_SAMPLES = 12  # 12 samples * 5s = 60 seconds average (was 60 samples/5 min)

# Track the currently streaming torrent (last one with speed > 0)
current_streaming_hash: str = ""
current_streaming_timestamp: float = 0

# Performance: ThreadPool for parallel API calls
executor = ThreadPoolExecutor(max_workers=10)

# Sync state (Movies)
sync_state: Dict[str, Any] = {
    "running": False,
    "pid": None,
    "started_at": None,
    "last_run": None,
    "last_status": None,  # "success", "error", None
    "process": None
}

# TV Sync state
tv_sync_state: Dict[str, Any] = {
    "running": False,
    "pid": None,
    "started_at": None,
    "last_run": None,
    "last_status": None,
    "process": None
}

# TMDB cache: hash -> {title, poster, year}
tmdb_cache: Dict[str, Dict[str, Any]] = {}

# V238: Plex Webhook Metadata Cache (hash_suffix -> {title, poster, year})
# This stores metadata received directly from Plex webhooks for instant matching.
plex_webhook_cache: Dict[str, Dict[str, Any]] = {}

# VPN Public IP cache
vpn_public_ip_cache = {"ip": "--", "timestamp": 0}

# =============================================================================
# TMDB Helper Functions
# =============================================================================

def parse_torrent_title(raw_title: str) -> tuple:
    """Extract clean title and year from torrent name."""
    # Remove common tags and clean up
    clean = raw_title.replace(".", " ").replace("_", " ")

    # V238: Strip Season/Episode patterns (S01E01, 1x01, etc.)
    # This is crucial for TV Show posters on TMDB
    clean = re.sub(r'\bS\d+E\d+\b', '', clean, flags=re.IGNORECASE)
    clean = re.sub(r'\b\d+x\d+\b', '', clean, flags=re.IGNORECASE)
    clean = re.sub(r'\bSeason\s*\d+\b', '', clean, flags=re.IGNORECASE)

    # Extract year (4 digits between 1900-2099)
    year_match = re.search(r'\b(19\d{2}|20\d{2})\b', clean)
    year = int(year_match.group(1)) if year_match else None

    # Remove everything after year or quality tags
    stop_patterns = [
        r'\b(19\d{2}|20\d{2})\b.*',  # Year and after
        r'\b(2160p|1080p|720p|480p)\b.*',  # Resolution
        r'\b(WEB-DL|BluRay|BDRip|HDRip|HDTV|WEBRip)\b.*',  # Source
        r'\b(REMUX|x264|x265|H\.?264|H\.?265|HEVC)\b.*',  # Codec
        r'\b(Hybrid|Multi|Dual|iTA|ENG)\b.*',  # V238: Additional tags
    ]

    title = clean
    for pattern in stop_patterns:
        title = re.split(pattern, title, flags=re.IGNORECASE)[0]

    title = title.strip()
    return title, year


def detect_quality_badges(raw_title: str) -> Dict[str, Any]:
    """Detect quality attributes from torrent title."""
    title_lower = raw_title.lower()

    # Detect audio format
    audio = None
    if "atmos" in title_lower:
        audio = "ATMOS"
    elif "truehd" in title_lower:
        audio = "TrueHD"
    elif "dts-hd" in title_lower or "dts hd" in title_lower:
        audio = "DTS-HD"
    elif "dts" in title_lower:
        audio = "DTS"
    elif "ddp" in title_lower or "dd+" in title_lower or "eac3" in title_lower:
        audio = "DD+"
    elif "dd5" in title_lower or "ac3" in title_lower or "dd 5" in title_lower:
        audio = "DD5.1"
    elif "aac" in title_lower:
        audio = "AAC"

    # Detect channel configuration
    channels = None
    if "7.1" in title_lower:
        channels = "7.1"
    elif "5.1" in title_lower:
        channels = "5.1"
    elif "2.0" in title_lower:
        channels = "2.0"

    return {
        "is_4k": any(x in title_lower for x in ["2160p", "4k", "uhd"]),
        "is_dv": any(x in title_lower for x in [".dv.", "dovi", "dolby vision", "dolbyvision", ".dv ", " dv.", "dv."]),
        "is_hdr": any(x in title_lower for x in ["hdr10", "hdr.", ".hdr", "hdr+"]) and "dv" not in title_lower,
        "is_1080p": "1080p" in title_lower and "2160p" not in title_lower,
        "audio": audio,
        "channels": channels,
    }


# Cache for Plex session audio info
plex_audio_cache: Dict[str, Dict[str, Any]] = {}
plex_audio_cache_time: float = 0


def get_plex_session_info() -> Dict[str, Dict[str, Any]]:
    """Get full info from active Plex sessions. Returns dict: filename -> {title, year, poster, audio, channels}"""
    global plex_audio_cache, plex_audio_cache_time

    # Cache for 15 seconds to avoid hammering Plex API (Optimization)
    if time.time() - plex_audio_cache_time < 15:
        return plex_audio_cache

    try:
        url = f"{PLEX_URL}/status/sessions?X-Plex-Token={PLEX_TOKEN}"
        response = requests.get(url, timeout=10)
        if response.status_code != 200:
            return plex_audio_cache

        # Parse XML response
        import xml.etree.ElementTree as ET
        root = ET.fromstring(response.text)

        result = {}
        for video in root.findall('.//Video'):
            # Get the media file path
            part = video.find('.//Part')
            if part is not None:
                file_path = part.get('file', '')
                filename = os.path.basename(file_path) if file_path else ''

                # Get title, year, poster from Video element
                title = video.get('title', '')
                year = video.get('year', '')
                thumb = video.get('thumb', '')
                # Build full poster URL with token
                poster = f"{PLEX_URL}{thumb}?X-Plex-Token={PLEX_TOKEN}" if thumb else None

                # Get video info from Media element
                media = video.find('.//Media')
                video_resolution = media.get('videoResolution', '') if media is not None else ''
                video_codec = media.get('videoCodec', '').lower() if media is not None else ''

                # Get video stream for HDR/DV detection
                video_stream = video.find('.//Stream[@streamType="1"]')
                is_4k = video_resolution in ('4k', '2160')
                is_1080p = video_resolution == '1080'
                is_hdr = False
                is_dv = False

                if video_stream is not None:
                    color_trc = video_stream.get('colorTrc', '').lower()
                    color_space = video_stream.get('colorSpace', '').lower()
                    display_title = video_stream.get('displayTitle', '').lower()
                    dovi_profile = video_stream.get('DOVIProfile', '')

                    # HDR detection
                    if 'smpte2084' in color_trc or 'pq' in color_trc or 'hdr' in display_title or 'bt2020' in color_space:
                        is_hdr = True

                    # Dolby Vision detection
                    if dovi_profile or 'dv' in display_title or 'dolby vision' in display_title:
                        is_dv = True

                # Get audio stream info
                audio_stream = video.find('.//Stream[@streamType="2"]')
                channels = None
                audio = None
                if audio_stream is not None:
                    channels_num = audio_stream.get('channels', '')
                    codec = audio_stream.get('codec', '').lower()
                    display_title = audio_stream.get('displayTitle', '').lower()

                    # Determine audio format (only premium formats, skip basic AC3/AAC)
                    if 'atmos' in display_title or 'atmos' in codec:
                        audio = 'ATMOS'
                    elif codec == 'truehd':
                        audio = 'TrueHD'
                    elif 'dts' in codec and ('hd' in codec or 'ma' in display_title):
                        audio = 'DTS-HD'
                    elif 'dts' in codec:
                        audio = 'DTS'
                    elif codec in ('eac3', 'ec3'):
                        audio = 'DD+'

                    # Determine channels
                    if channels_num:
                        ch = int(channels_num)
                        if ch >= 8:
                            channels = '7.1'
                        elif ch >= 6:
                            channels = '5.1'
                        elif ch == 2:
                            channels = '2.0'

                if filename:
                    result[filename] = {
                        'title': title,
                        'year': year,
                        'poster': poster,
                        'is_4k': is_4k,
                        'is_1080p': is_1080p,
                        'is_hdr': is_hdr,
                        'is_dv': is_dv,
                        'audio': audio,
                        'channels': channels
                    }

        plex_audio_cache = result
        plex_audio_cache_time = time.time()
        return result

    except Exception as e:
        logger.debug(f"Plex session audio fetch failed: {e}")
        return plex_audio_cache


# Cache for MKV title lookups (hash -> clean title)
mkv_title_cache: Dict[str, str] = {}
last_inode_map_load: float = 0


def get_clean_title_from_mkv(torrent_hash: str) -> Optional[str]:
    """Find title by searching the Inode Map JSON instead of scanning the disk.
    This is much faster and doesn't impact FUSE/Disk performance.
    """
    global mkv_title_cache, last_inode_map_load
    
    # 1. Check RAM cache first
    if torrent_hash in mkv_title_cache:
        return mkv_title_cache[torrent_hash]

    hash_suffix = torrent_hash[-8:].lower()
    inode_map_path = os.path.join(STATE_DIR, "inode_map.json")

    # 2. Reload inode map if not loaded or if it's been more than 60s
    if time.time() - last_inode_map_load > 60:
        try:
            if os.path.exists(inode_map_path):
                with open(inode_map_path, 'r') as f:
                    data = json.load(f)
                    files = data.get("Files", {})
                    for path, url in files.items():
                        match = re.search(r'link=([a-f0-9]+)', url)
                        if match:
                            f_hash = match.group(1)
                            filename = os.path.basename(path)
                            # V238: Support both .mkv and .mp4 ( Disney+ / Ben The Men style)
                            clean = re.sub(rf'_[a-f0-9]{{8}}\.(mkv|mp4)$', '', filename, flags=re.IGNORECASE)
                            mkv_title_cache[f_hash] = clean
                
                last_inode_map_load = time.time()
                logger.debug(f"V238: Inode Map loaded, {len(mkv_title_cache)} titles cached.")
        except Exception as e:
            logger.error(f"Failed to load Inode Map: {e}")

    return mkv_title_cache.get(torrent_hash)


def search_plex_by_filename(filename: str) -> Optional[Dict[str, Any]]:
    """Search Plex library for a specific filename to get metadata (V238: TV Support)."""
    try:
        url = f"{PLEX_URL}/library/sections/all/search?filename={filename}&X-Plex-Token={PLEX_TOKEN}"
        response = requests.get(url, timeout=5)
        if response.status_code == 200:
            import xml.etree.ElementTree as ET
            root = ET.fromstring(response.text)
            video = root.find('.//Video')
            if video is not None:
                # Use grandparentThumb for TV Shows, thumb for Movies
                thumb = video.get('grandparentThumb') or video.get('thumb')
                title = video.get('title')
                
                # If it's an episode, construct a better display title
                if video.get('type') == 'episode':
                    show = video.get('grandparentTitle', '')
                    s = video.get('parentIndex', '')
                    e = video.get('index', '')
                    if show:
                        title = f"{show} S{s}E{e}"
                
                return {
                    'title': title,
                    'year': video.get('year'),
                    'poster': f"{PLEX_URL}{thumb}?X-Plex-Token={PLEX_TOKEN}" if thumb else None
                }
    except Exception as e:
        logger.debug(f"Plex filename search failed for {filename}: {e}")
    return None


def get_torrent_files(torrent_hash: str) -> List[str]:
    """Fetch file list for a torrent from GoStorm API."""
    try:
        response = requests.post(
            f"{GOSTORM_URL}/torrents",
            json={"action": "get", "hash": torrent_hash},
            timeout=2
        )
        if response.status_code == 200:
            data = response.json()
            # Support multiple video formats
            video_extensions = ('.mkv', '.mp4', '.avi', '.ts', '.m4v')
            return [f.get("path", "") for f in data.get("file_stats", []) if f.get("path", "").lower().endswith(video_extensions)]
    except Exception:
        pass
    return []


def search_plex_by_filename(filename: str) -> Optional[Dict[str, Any]]:
    """Search Plex library for a specific filename to get metadata (V238: TV Support)."""
    try:
        # V238: Removed type=1 to allow searching both movies and episodes
        url = f"{PLEX_URL}/library/sections/all/search?filename={filename}&X-Plex-Token={PLEX_TOKEN}"
        response = requests.get(url, timeout=5)
        if response.status_code == 200:
            import xml.etree.ElementTree as ET
            root = ET.fromstring(response.text)
            
            # Find first Video element (can be Movie or Episode)
            video = root.find('.//Video')
            if video is not None:
                # Use grandparentThumb for TV Shows, thumb for Movies
                thumb = video.get('grandparentThumb') or video.get('thumb')
                title = video.get('title')
                
                # If it's an episode, construct a better display title
                if video.get('type') == 'episode':
                    show = video.get('grandparentTitle', '')
                    s = video.get('parentIndex', '')
                    e = video.get('index', '')
                    if show:
                        title = f"{show} S{s}E{e}"
                
                return {
                    'title': title,
                    'year': video.get('year'),
                    'poster': f"{PLEX_URL}{thumb}?X-Plex-Token={PLEX_TOKEN}" if thumb else None
                }
    except Exception as e:
        logger.debug(f"Plex filename search failed for {filename}: {e}")
    return None


def get_tmdb_info(torrent_hash: str, raw_title: str) -> Dict[str, Any]:
    """Get movie/TV info from TMDB with caching."""
    # Check cache first
    if torrent_hash in tmdb_cache:
        return tmdb_cache[torrent_hash]

    # Try to get clean title from mkv filename (preferred - no site prefixes)
    clean_mkv_title = get_clean_title_from_mkv(torrent_hash)
    if clean_mkv_title:
        title, year = parse_torrent_title(clean_mkv_title)
    else:
        title, year = parse_torrent_title(raw_title)

    if not title:
        return {"title": raw_title[:40], "poster": None, "year": None}

    try:
        # 1. Try Movie Search
        params = {
            "api_key": TMDB_API_KEY,
            "query": title,
            "language": "it-IT"
        }
        if year:
            params["year"] = year

        response = requests.get(f"{TMDB_BASE_URL}/search/movie", params=params, timeout=3)
        if response.status_code == 200:
            results = response.json().get("results", [])
            if results:
                movie = results[0]
                poster_path = movie.get("poster_path")
                info = {
                    "title": movie.get("title", title),
                    "poster": f"{TMDB_IMAGE_BASE}{poster_path}" if poster_path else None,
                    "year": movie.get("release_date", "")[:4] if movie.get("release_date") else year
                }
                tmdb_cache[torrent_hash] = info
                return info

        # 2. Try TV Show Search (if movie failed)
        if year:
            del params["year"]
            params["first_air_date_year"] = year
        
        response = requests.get(f"{TMDB_BASE_URL}/search/tv", params=params, timeout=3)
        if response.status_code == 200:
            results = response.json().get("results", [])
            if results:
                show = results[0]
                poster_path = show.get("poster_path")
                info = {
                    "title": show.get("name", title),
                    "poster": f"{TMDB_IMAGE_BASE}{poster_path}" if poster_path else None,
                    "year": show.get("first_air_date", "")[:4] if show.get("first_air_date") else year
                }
                tmdb_cache[torrent_hash] = info
                return info

        # Fallback: return cleaned title without poster
        info = {"title": title, "poster": None, "year": year}
        tmdb_cache[torrent_hash] = info
        return info

    except Exception as e:
        logger.debug(f"TMDB lookup failed for {title}: {e}")
        return {"title": title or raw_title[:40], "poster": None, "year": year}


def format_size(size_bytes: int) -> str:
    """Format file size to human readable."""
    if size_bytes >= 1024**3:
        return f"{size_bytes / 1024**3:.1f} GB"
    elif size_bytes >= 1024**2:
        return f"{size_bytes / 1024**2:.0f} MB"
    else:
        return f"{size_bytes / 1024:.0f} KB"


def extract_hash_suffix(filename: str) -> Optional[str]:
    """Extract 8-char hash suffix from filename (e.g. Movie_8ca427f0.mkv)."""
    # Remove extension
    base = os.path.splitext(filename)[0]
    # Find last underscore
    idx = base.rfind('_')
    if idx != -1 and len(base) - idx - 1 == 8:
        # Check if it looks like hex
        suffix = base[idx+1:]
        if all(c in '0123456789abcdefABCDEF' for c in suffix):
            return suffix
    return None


# =============================================================================
# Buffer/Cache Query Functions
# =============================================================================

# Cache for FUSE metrics (refreshed every 5s with collector)
_fuse_metrics_cache: Dict[str, Any] = {}
_fuse_metrics_timestamp: float = 0
FUSE_METRICS_TTL = 5  # seconds


def get_torrent_cache_percent(torrent_hash: str) -> float:
    """Get cache fill percentage for a specific torrent from GoStorm /cache endpoint."""
    try:
        response = requests.post(
            f"{GOSTORM_URL}/cache",
            json={"action": "get", "hash": torrent_hash},
            timeout=1
        )
        if response.status_code == 200:
            data = response.json()
            filled = data.get("Filled", 0)
            capacity = data.get("Capacity", 0)
            if capacity > 0:
                percent = (filled / capacity) * 100
                return round(min(percent, 100), 1)  # Cap at 100%
    except Exception as e:
        logger.debug(f"Cache query failed for {torrent_hash[:8]}: {e}")
    return 0.0


def get_fuse_metrics() -> Dict[str, Any]:
    """Get FUSE proxy buffer metrics. Cached briefly to reduce load.

    Maps Go metrics format to Python metrics format for dashboard compatibility.
    Go metrics: cache_size_mb, cache_entries
    Python metrics: read_ahead_active_percent, read_ahead_stale_percent, read_ahead_active_bytes
    """
    global _fuse_metrics_cache, _fuse_metrics_timestamp

    now = time.time()
    if now - _fuse_metrics_timestamp < FUSE_METRICS_TTL and _fuse_metrics_cache:
        return _fuse_metrics_cache

    try:
        response = requests.get(FUSE_METRICS_URL, timeout=1)
        if response.status_code == 200:
            go_metrics = response.json()

            # Go now provides read_ahead metrics directly (V82 update)
            # read_ahead_active_percent: % of budget for recent buffers (<30s old)
            # read_ahead_stale_percent: % of budget for old buffers (>30s old)
            _fuse_metrics_cache = {
                "read_ahead_active_bytes": go_metrics.get("read_ahead_active_bytes", 0),
                "read_ahead_active_percent": go_metrics.get("read_ahead_active_percent", 0),
                "read_ahead_stale_percent": go_metrics.get("read_ahead_stale_percent", 0),
                "read_ahead_total_bytes": go_metrics.get("read_ahead_total_bytes", 0),
                "read_ahead_entries": go_metrics.get("read_ahead_entries", 0),
                "cache_entries": go_metrics.get("cache_entries", 0),
                "cache_size_mb": go_metrics.get("cache_size_mb", 0),
                "preload_entries": go_metrics.get("preload_entries", 0),
                "preload_size_mb": go_metrics.get("preload_size_mb", 0),
                "preload_hits": go_metrics.get("preload_hits", 0),
                "version": go_metrics.get("version", "unknown")
            }
            _fuse_metrics_timestamp = now
            return _fuse_metrics_cache
    except Exception as e:
        logger.debug(f"FUSE metrics query failed: {e}")

    return {"read_ahead_active_percent": 0, "read_ahead_stale_percent": 0, "read_ahead_active_bytes": 0, "cache_entries": 0}


# =============================================================================
# Health Check Functions
# =============================================================================

def check_gostorm() -> None:
    """Check GoStorm API health and get torrent statistics."""
    global gostorm_fail_count, last_gostorm_health_check
    
    # Auto-Restart Logic: Check health every 15 seconds independently from collect interval
    now = time.time()
    if now - last_gostorm_health_check >= GOSTORM_HEALTH_INTERVAL:
        last_gostorm_health_check = now
        try:
            # Lightweight heartbeat
            requests.get(f"{GOSTORM_URL}/echo", timeout=10)
            gostorm_fail_count = 0  # Reset on success
        except Exception as e:
            gostorm_fail_count += 1
            logger.warning(f"GoStorm heartbeat failed ({gostorm_fail_count}/{GOSTORM_MAX_FAILURES}): {e}")
            
            if gostorm_fail_count >= GOSTORM_MAX_FAILURES:
                logger.error(f"GoStorm persistent failure detected ({GOSTORM_MAX_FAILURES} attempts @ {GOSTORM_HEALTH_INTERVAL}s). Auto-restarting...")
                try:
                    subprocess.run(["sudo", "systemctl", "restart", "gostream"], timeout=10)
                    gostorm_fail_count = 0
                    last_restart["gostorm"] = time.time()
                except Exception as re:
                    logger.error(f"Auto-restart failed: {re}")

    try:
        start = time.time()
        response = requests.post(
            f"{GOSTORM_URL}/torrents",
            json={"action": "active"},
            timeout=5
        )
        latency = int((time.time() - start) * 1000)

        if response.status_code == 200:
            torrents = response.json() or []
            total = len(torrents)
            now = time.time()

            # Update sticky state: mark torrents with speed > 0 or is_priority as active
            global current_streaming_hash, current_streaming_timestamp
            for t in torrents:
                h = t.get("hash", "")
                is_prio = t.get("is_priority", False)
                speed = t.get("download_speed", 0)
                
                if speed > 50000 or is_prio:  # Min 50 KB/s threshold or Priority Mode
                    active_torrents_sticky[h] = now
                    # Track this as the current streaming torrent
                    if speed > 50000:
                        current_streaming_hash = h
                        current_streaming_timestamp = now
                    elif is_prio and not current_streaming_hash:
                        current_streaming_hash = h
                        current_streaming_timestamp = now

            # Check FUSE for active reads - if Plex is actively reading, extend sticky
            try:
                fuse_check = get_fuse_metrics()
                # Only extend if there are ACTIVE reads (not just stale entries)
                fuse_active_bytes = fuse_check.get("read_ahead_active_bytes", 0)
                fuse_has_active_reads = fuse_active_bytes > 0 or fuse_check.get("read_ahead_active_percent", 0) > 0
                # Only refresh the current streaming torrent (not all)
                if fuse_has_active_reads and current_streaming_hash in active_torrents_sticky:
                    active_torrents_sticky[current_streaming_hash] = now
            except:
                fuse_has_active_reads = False

            # Clean expired sticky entries (older than ACTIVE_STICKY_SECONDS)
            expired = [h for h, ts in active_torrents_sticky.items() if now - ts > ACTIVE_STICKY_SECONDS]
            for h in expired:
                del active_torrents_sticky[h]
                # Also clean up speed history for expired torrents
                if h in torrent_speed_history:
                    del torrent_speed_history[h]

            # Count active using sticky state (prevents flicker)
            downloading = sum(1 for t in torrents if t.get("hash", "") in active_torrents_sticky)
            speed_bps = sum(t.get("download_speed", 0) for t in torrents)
            speed_mbps = round(speed_bps / 1024 / 1024 * 8, 2)
            peers = sum(t.get("active_peers", 0) for t in torrents)
            seeders = sum(t.get("connected_seeders", 0) for t in torrents)

            # Build active torrents list (same data used by both panels)
            active_list = []

            # Get FUSE metrics once (shared across all torrents)
            fuse_metrics = get_fuse_metrics()
            fuse_active_percent = fuse_metrics.get("read_ahead_active_percent", 0)
            fuse_stale_percent = fuse_metrics.get("read_ahead_stale_percent", 0)

            # Step 1: Pre-filter torrents and launch parallel cache requests
            torrents_to_process = [t for t in torrents if t.get("hash", "") in active_torrents_sticky]
            cache_futures = {}
            for t in torrents_to_process:
                h = t.get("hash", "")
                is_prio = t.get("is_priority", False)
                # Only query cache if streaming or priority
                if is_prio or (t.get("download_speed", 0) > 0):
                   cache_futures[h] = executor.submit(get_torrent_cache_percent, h)

            # Step 2: Build the active list with parallel data
            for t in torrents_to_process:
                torrent_hash = t.get("hash", "")
                t_speed = t.get("download_speed", 0)
                total_size = t.get("torrent_size", 0) or t.get("total_size", 0)
                loaded_size = t.get("loaded_size", 0)
                progress = round(loaded_size / total_size * 100, 1) if total_size > 0 else 0
                t_speed_mbps = round(t_speed / 1024 / 1024 * 8, 2)

                # Track speed history for this torrent
                if torrent_hash not in torrent_speed_history:
                    torrent_speed_history[torrent_hash] = deque(maxlen=TORRENT_SPEED_SAMPLES)
                torrent_speed_history[torrent_hash].append(t_speed_mbps)

                speed_samples = list(torrent_speed_history[torrent_hash])
                avg_speed_mbps = round(sum(speed_samples) / len(speed_samples), 2) if speed_samples else t_speed_mbps

                if t_speed > 0 and progress < 5:
                    display_progress = -1
                else:
                    display_progress = progress

                # Get the parallelized cache result
                cache_percent = 0.0
                if torrent_hash in cache_futures:
                    try:
                        cache_percent = cache_futures[torrent_hash].result(timeout=1)
                    except:
                        pass

                # V238-Fix: Metadata enrichment moved OUTSIDE cache_futures block
                
                # 1. Get raw title from GoStorm
                raw_title = t.get("title", "")
                # Deep Resolution - if title is empty, look at internal files
                if not raw_title or raw_title.lower() == "unknown":
                    files = get_torrent_files(torrent_hash)
                    if files:
                        # Use the first video filename as the new raw_title
                        raw_title = os.path.basename(files[0])
                    else:
                        raw_title = "Unknown Torrent"

                # 2. Try to get a better title from the Inode Map (RAM cache)
                hash_suffix = torrent_hash[-8:].lower()
                disk_title_clean = get_clean_title_from_mkv(torrent_hash)
                
                # 3. Resolve Display Title via Plex First
                plex_title = None
                plex_year = None
                plex_poster = None
                
                # Try Plex Session Info first (Active playback)
                plex_sessions = get_plex_session_info()
                for filename, info in plex_sessions.items():
                    f_suffix = extract_hash_suffix(filename)
                    if f_suffix and f_suffix.lower() == hash_suffix:
                        plex_title = info.get('title')
                        plex_year = info.get('year')
                        plex_poster = info.get('poster')
                        break

                # 4. Try Deep Plex Search by Filename (if session missed)
                if not plex_title and disk_title_clean:
                    # Try common extensions for the filename match (V238: Support MP4)
                    for ext in ['.mkv', '.mp4']:
                        full_disk_filename = f"{disk_title_clean}_{hash_suffix}{ext}"
                        plex_file_info = search_plex_by_filename(full_disk_filename)
                        if plex_file_info:
                            plex_title = plex_file_info.get('title')
                            plex_year = plex_file_info.get('year')
                            plex_poster = plex_file_info.get('poster')
                            break

                # 5. Resolve via TMDB as last resort
                tmdb_info = {}
                if not plex_title:
                    search_query = disk_title_clean if disk_title_clean else raw_title
                    tmdb_info = get_tmdb_info(torrent_hash, search_query)

                # Detect quality badges
                badges = detect_quality_badges(raw_title)
                if disk_title_clean:
                    badges.update(detect_quality_badges(disk_title_clean))

                # Final Title Logic: Plex > TMDB > Disk Filename > Raw Title
                final_title = plex_title or tmdb_info.get("title")
                if not final_title or final_title == "Unknown":
                     final_title = disk_title_clean if disk_title_clean else raw_title[:50]

                active_list.append({
                    "hash": torrent_hash[:8],
                    "full_hash": torrent_hash,
                    "fuse_streaming": fuse_has_active_reads and (t_speed > 0 or torrent_hash == current_streaming_hash),
                    "raw_title": raw_title[:60],
                    "title": final_title,
                    "year": plex_year or tmdb_info.get("year"),
                    "poster": plex_poster or tmdb_info.get("poster"),
                    "size": format_size(total_size),
                    "progress": display_progress,
                    "speed_mbps": t_speed_mbps,  # Current speed (for DOWNLOAD SPEED panel)
                    "avg_speed_mbps": avg_speed_mbps,  # Average speed (for ACTIVE DOWNLOADS panel)
                    "peers": t.get("active_peers", 0),
                    "seeders": t.get("connected_seeders", 0),
                    "cache_percent": cache_percent,  # GoStorm cache fill % (per-torrent)
                    "ra_active_percent": fuse_active_percent if (torrent_hash == current_streaming_hash) else 0,
                    "ra_stale_percent": fuse_stale_percent if (torrent_hash == current_streaming_hash) else 0,
                    "is_priority": t.get("is_priority", False), # V200 Priority Mode
                    **badges  # is_4k, is_dv, is_hdr, is_atmos, is_1080p
                })

            # Sort by speed descending
            active_list.sort(key=lambda x: -x["speed_mbps"])

            health_data["gostorm"] = {
                "status": "ok" if latency < 500 else "warn",
                "latency_ms": latency,
                "error": None
            }
            health_data["torrents"] = {
                "total": total,
                "downloading": downloading,
                "speed_mbps": speed_mbps,
                "peers": peers,
                "seeders": seeders,
                "fuse_buffer_active_percent": fuse_active_percent,  # Active FUSE buffer (current streams)
                "fuse_buffer_stale_percent": fuse_stale_percent     # Stale FUSE buffer (cached from previous streams)
            }
            health_data["active_torrents"] = active_list[:10]  # Top 10
            speed_history.append({"time": datetime.now().isoformat(), "speed": speed_mbps})
        else:
            health_data["gostorm"] = {
                "status": "error",
                "latency_ms": latency,
                "error": f"HTTP {response.status_code}"
            }
    except requests.exceptions.Timeout:
        health_data["gostorm"] = {"status": "error", "latency_ms": 5000, "error": "Timeout"}
    except requests.exceptions.ConnectionError as e:
        health_data["gostorm"] = {"status": "error", "latency_ms": 0, "error": "Connection refused"}
    except Exception as e:
        health_data["gostorm"] = {"status": "error", "latency_ms": 0, "error": str(e)[:50]}


def check_fuse() -> None:
    """Check FUSE mount status and file count."""
    try:
        mounted = os.path.ismount(FUSE_MOUNT)
        files = 0

        if mounted:
            # Count .mkv files
            movies = list(Path(MOVIES_DIR).glob("*.mkv")) if os.path.exists(MOVIES_DIR) else []
            tv_episodes = list(Path(TV_DIR).rglob("*.mkv")) if os.path.exists(TV_DIR) else []
            files = len(movies) + len(tv_episodes)

        health_data["fuse"] = {
            "status": "ok" if mounted and files > 0 else ("warn" if mounted else "error"),
            "mounted": mounted,
            "files": files
        }
    except Exception as e:
        health_data["fuse"] = {"status": "error", "mounted": False, "files": 0, "error": str(e)[:50]}


def check_system() -> None:
    """Check system resources (CPU, RAM, Disk)."""
    try:
        cpu = psutil.cpu_percent(interval=1)
        mem = psutil.virtual_memory()
        disk = psutil.disk_usage("/")

        health_data["system"] = {
            "cpu": round(cpu, 1),
            "ram": round(mem.percent, 1),
            "ram_used_gb": round(mem.used / 1024**3, 1),
            "ram_total_gb": round(mem.total / 1024**3, 1),
            "disk_free_gb": round(disk.free / 1024**3, 1),
            "disk_total_gb": round(disk.total / 1024**3, 1)
        }
    except Exception as e:
        logger.error(f"System check failed: {e}")


def check_vpn() -> None:
    """Check WireGuard (wg0) status and Public IP."""
    global vpn_public_ip_cache
    try:
        stats = psutil.net_if_stats()
        addrs = psutil.net_if_addrs()
        
        if "wg0" in stats:
            is_up = stats["wg0"].isup
            internal_ip = "unknown"
            if "wg0" in addrs:
                for addr in addrs["wg0"]:
                    if addr.family == socket.AF_INET:
                        internal_ip = addr.address
                        break
            
            # Get Public IP (Cached 60s)
            public_ip = vpn_public_ip_cache["ip"]
            if is_up and (time.time() - vpn_public_ip_cache["timestamp"] > 60):
                try:
                    res = requests.get("https://api.ipify.org", timeout=2)
                    if res.status_code == 200:
                        public_ip = res.text
                        vpn_public_ip_cache = {"ip": public_ip, "timestamp": time.time()}
                except Exception as e:
                    logger.debug(f"Public IP check failed: {e}")

            health_data["vpn"] = {
                "status": "ok" if is_up else "error",
                "ip": internal_ip,
                "public_ip": public_ip,
                "is_up": is_up
            }
        else:
             health_data["vpn"] = {"status": "error", "ip": "down", "public_ip": "--", "is_up": False}
    except Exception as e:
        health_data["vpn"] = {"status": "error", "ip": "error", "public_ip": "error", "error": str(e)}


def check_natpmp() -> None:
    """Check NAT-PMP status from GoStream metrics (V229: Zero-Overhead check)."""
    try:
        # Read from GoStream metrics (memory) instead of GoStorm API (lock)
        response = requests.get(FUSE_METRICS_URL, timeout=2)
        if response.status_code == 200:
            data = response.json()
            port = data.get("natpmp_port", 0)
            status = "ok" if port > 0 else "unknown"
            health_data["natpmp"] = {"status": status, "port": port}
        else:
            health_data["natpmp"] = {"status": "error", "port": 0, "error": f"HTTP {response.status_code}"}
    except Exception as e:
        health_data["natpmp"] = {"status": "error", "port": 0, "error": str(e)[:50]}


def check_plex() -> None:
    """Check Plex server status."""
    try:
        response = requests.get(f"{PLEX_URL}/identity", timeout=5)
        if response.status_code == 200:
            # Parse XML to extract version from MediaContainer
            version_match = re.search(r'MediaContainer.*?version="([^"]+)"', response.text)
            version = version_match.group(1) if version_match else "unknown"
            health_data["plex"] = {"status": "ok", "version": version}
        else:
            health_data["plex"] = {"status": "error", "version": None}
    except Exception as e:
        health_data["plex"] = {"status": "error", "version": None}


def collector_loop() -> None:
    """Background thread to collect metrics periodically."""
    logger.info("Collector thread started")
    while True:
        try:
            check_gostorm()
            check_fuse()
            check_system()
            check_vpn()
            check_natpmp()
            check_plex()
            check_sync_status()
            check_tv_sync_status()
            health_data["last_update"] = datetime.now().isoformat()
        except Exception as e:
            logger.error(f"Collector error: {e}")
        time.sleep(COLLECT_INTERVAL)


# =============================================================================
# Sync Management Functions
# =============================================================================

def check_sync_status() -> None:
    """Check if sync process is still running."""
    if sync_state["process"] is not None:
        poll = sync_state["process"].poll()
        if poll is not None:
            # Process finished
            sync_state["running"] = False
            sync_state["last_run"] = datetime.now().isoformat()
            sync_state["last_status"] = "success" if poll == 0 else "error"
            sync_state["process"] = None
            sync_state["pid"] = None
            sync_state["started_at"] = None
            logger.info(f"Sync finished with status: {sync_state['last_status']}")


# =============================================================================
# Smart Preloader Logic
# =============================================================================

def trigger_preload(file_path: Path) -> bool:
    """
    Trigger preload via Proxy Go API instead of physical read.
    """
    try:
        # Reduced Safety Delay: 2 seconds is enough
        logger.info(f"Smart Preload: Waiting 2s safety margin...")
        time.sleep(2)
        
        start_time = time.time()
        logger.info(f"Smart Preload: Triggering API preload of {PRELOAD_SIZE_MB}MB for {file_path}")
        
        # Call Proxy Go /preload endpoint
        # Example: GET http://127.0.0.1:8096/preload?path=/mnt/torrserver-go/...&size=32MB
        url = f"http://127.0.0.1:8096/preload"
        params = {
            "path": str(file_path),
            "size": f"{PRELOAD_SIZE_MB}MB"
        }
        
        response = requests.get(url, params=params, timeout=10)
        
        duration = time.time() - start_time
        if response.status_code == 200:
            data = response.json()
            status = data.get("status")
            logger.info(f"Smart Preload: API accepted request (status={status}) in {duration:.2f}s")
            return True
        else:
            logger.error(f"Smart Preload: API call failed (HTTP {response.status_code}): {response.text}")
            return False
            
    except Exception as e:
        logger.error(f"Smart Preload: Failed to trigger via API: {e}")
        return False

def handle_plex_event(payload: Dict) -> None:
    """Handle Plex webhook event."""
    event = payload.get('event')
    metadata = payload.get('Metadata', {})
    
    # 1. Reset Preload panel when user starts watching something new
    if event in ('media.play', 'media.resume'):
        logger.info(f"Plex Event: {event} - Resetting Preload panel")
        health_data["preload"] = {"title": None, "status": "idle", "timestamp": None, "poster": None}
        return

    # 2. Trigger Preload only on 'media.scrobble' (watched > 90%)
    if event != 'media.scrobble':
        return

    if metadata.get('type') != 'episode':
        return
        
    show_title = metadata.get('grandparentTitle')
    season = metadata.get('parentIndex')
    episode = metadata.get('index')
    
    if not (show_title and season and episode):
        return
        
    logger.info(f"Smart Preload: Detected finished episode: {show_title} S{season:02d}E{episode:02d}")
    
    # Extract file path for deterministic search
    current_file_path = None
    try:
        media = payload.get('Metadata', {}).get('Media', [])
        if media and len(media) > 0:
            parts = media[0].get('Part', [])
            if parts and len(parts) > 0:
                current_file_path = parts[0].get('file')
                logger.info(f"Smart Preload: Source file path: {current_file_path}")
    except Exception:
        pass
    
    # Use existing TMDB function for consistent poster
    tmdb_info = get_tmdb_info(f"preload_{show_title}", show_title)
    
    # Update status in dashboard: Searching
    health_data["preload"] = {
        "title": f"{show_title} S{season:02d}E{episode+1:02d}",
        "status": "searching",
        "timestamp": datetime.now().isoformat(),
        "poster": tmdb_info.get("poster") # Using TMDB poster URL
    }
    
    # Find next episode
    result = find_next_episode(show_title, int(season), int(episode), current_file_path)
    
    if result:
        next_file, formatted_name = result
        
        # Update status: Preloading
        health_data["preload"]["title"] = formatted_name
        health_data["preload"]["status"] = "preloading"
        
        if trigger_preload(next_file):
            health_data["preload"]["status"] = "ready"
            logger.info(f"Smart Preload: 🚀 Preloaded {formatted_name}")
        else:
            health_data["preload"]["status"] = "error"
    else:
        health_data["preload"]["status"] = "not_found"
        logger.info(f"Smart Preload: No next episode found for {show_title}")


def start_sync() -> Dict[str, Any]:
    """Start the sync script in background."""
    if sync_state["running"]:
        return {"status": "error", "message": "Sync already running"}

    if not os.path.exists(SYNC_SCRIPT):
        return {"status": "error", "message": "Sync script not found"}

    try:
        # Open log file for appending
        log_file = open(SYNC_LOG, "a")

        # Start process in background
        process = subprocess.Popen(
            ["python3", SYNC_SCRIPT],
            stdout=log_file,
            stderr=subprocess.STDOUT,
            start_new_session=True  # Detach from parent
        )

        sync_state["running"] = True
        sync_state["pid"] = process.pid
        sync_state["started_at"] = datetime.now().isoformat()
        sync_state["process"] = process

        logger.info(f"Sync started with PID {process.pid}")

        return {
            "status": "ok",
            "message": "Sync started",
            "pid": process.pid
        }
    except Exception as e:
        logger.error(f"Failed to start sync: {e}")
        return {"status": "error", "message": str(e)}


def stop_sync() -> Dict[str, Any]:
    """Stop the running sync process."""
    if not sync_state["running"] or sync_state["process"] is None:
        return {"status": "error", "message": "No sync running"}

    try:
        sync_state["process"].terminate()
        sync_state["process"].wait(timeout=5)
        sync_state["running"] = False
        sync_state["process"] = None
        sync_state["pid"] = None
        sync_state["started_at"] = None
        sync_state["last_status"] = "stopped"
        logger.info("Sync stopped by user")
        return {"status": "ok", "message": "Sync stopped"}
    except subprocess.TimeoutExpired:
        sync_state["process"].kill()
        return {"status": "ok", "message": "Sync killed"}
    except Exception as e:
        return {"status": "error", "message": str(e)}


# TV Sync Management Functions

def check_tv_sync_status() -> None:
    """Check if TV sync process is still running."""
    if tv_sync_state["process"] is not None:
        poll = tv_sync_state["process"].poll()
        if poll is not None:
            tv_sync_state["running"] = False
            tv_sync_state["last_run"] = datetime.now().isoformat()
            tv_sync_state["last_status"] = "success" if poll == 0 else "error"
            tv_sync_state["process"] = None
            tv_sync_state["pid"] = None
            tv_sync_state["started_at"] = None
            logger.info(f"TV Sync finished with status: {tv_sync_state['last_status']}")


def start_tv_sync() -> Dict[str, Any]:
    """Start the TV sync script in background."""
    if tv_sync_state["running"]:
        return {"status": "error", "message": "TV Sync already running"}

    if not os.path.exists(TV_SYNC_SCRIPT):
        return {"status": "error", "message": "TV Sync script not found"}

    try:
        log_file = open(TV_SYNC_LOG, "a")
        process = subprocess.Popen(
            ["python3", TV_SYNC_SCRIPT],
            stdout=log_file,
            stderr=subprocess.STDOUT,
            start_new_session=True
        )

        tv_sync_state["running"] = True
        tv_sync_state["pid"] = process.pid
        tv_sync_state["started_at"] = datetime.now().isoformat()
        tv_sync_state["process"] = process

        logger.info(f"TV Sync started with PID {process.pid}")

        return {
            "status": "ok",
            "message": "TV Sync started",
            "pid": process.pid
        }
    except Exception as e:
        logger.error(f"Failed to start TV sync: {e}")
        return {"status": "error", "message": str(e)}


def stop_tv_sync() -> Dict[str, Any]:
    """Stop the running TV sync process."""
    if not tv_sync_state["running"] or tv_sync_state["process"] is None:
        return {"status": "error", "message": "No TV sync running"}

    try:
        tv_sync_state["process"].terminate()
        tv_sync_state["process"].wait(timeout=5)
        tv_sync_state["running"] = False
        tv_sync_state["process"] = None
        tv_sync_state["pid"] = None
        tv_sync_state["started_at"] = None
        tv_sync_state["last_status"] = "stopped"
        logger.info("TV Sync stopped by user")
        return {"status": "ok", "message": "TV Sync stopped"}
    except subprocess.TimeoutExpired:
        tv_sync_state["process"].kill()
        return {"status": "ok", "message": "TV Sync killed"}
    except Exception as e:
        return {"status": "error", "message": str(e)}


# =============================================================================
# FastAPI Application
# =============================================================================

app = FastAPI(title="Streaming Health Monitor", version="1.0.0")


@app.get("/favicon.ico")
async def favicon():
    """Serve favicon - clapperboard emoji."""
    from fastapi.responses import Response
    svg = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><text y=".9em" font-size="90">🎬</text></svg>'
    return Response(content=svg, media_type="image/svg+xml")


@app.post("/webhook")
async def plex_webhook(request: Request) -> JSONResponse:
    """
    Handle Plex Webhooks.
    Payload is multipart/form-data.
    'payload' field contains the JSON.
    """
    try:
        # Plex sends multipart form data
        form = await request.form()
        payload_str = form.get('payload')
        
        if not payload_str:
            return JSONResponse({"status": "ignored", "reason": "no payload"})
            
        return JSONResponse({"status": "ok"})
    except Exception as e:
        logger.error(f"Webhook error: {e}")
        return JSONResponse({"status": "error", "message": str(e)}, status_code=500)


@app.get("/api/health")
async def api_health() -> JSONResponse:
    """Get complete health status."""
    overall = "healthy"
    if health_data["gostorm"]["status"] == "error":
        overall = "unhealthy"
    elif health_data["fuse"]["status"] == "error":
        overall = "unhealthy"
    elif health_data["gostorm"]["status"] == "warn" or health_data["fuse"]["status"] == "warn":
        overall = "degraded"

    # Enrich preload state with real metrics from Go Proxy
    fuse_metrics = get_fuse_metrics()
    preload_data = health_data["preload"].copy()
    preload_data.update({
        "real_size_mb": fuse_metrics.get("preload_size_mb", 0),
        "real_hits": fuse_metrics.get("preload_hits", 0),
        "real_entries": fuse_metrics.get("preload_entries", 0)
    })

    return JSONResponse({
        "status": overall,
        "timestamp": health_data["last_update"],
        "services": {
            "gostorm": health_data["gostorm"],
            "fuse": health_data["fuse"],
            "natpmp": health_data["natpmp"],
            "vpn": health_data["vpn"],
            "plex": health_data["plex"]
        },
        "system": health_data["system"],
        "torrents": health_data["torrents"],
        "active_torrents": health_data["active_torrents"],
        "preload": preload_data
    })


@app.get("/api/torrents")
async def api_torrents(active: bool = False) -> JSONResponse:
    """Get torrent list using the lightweight 'active' action."""
    try:
        response = requests.post(
            f"{GOSTORM_URL}/torrents",
            json={"action": "active"},
            timeout=5
        )
        if response.status_code != 200:
            return JSONResponse({"error": "GoStorm unavailable"}, status_code=503)

        torrents = response.json() or []

        result = []
        for t in torrents:
            torrent_hash = t.get("hash", "")
            speed = t.get("download_speed", 0)
            is_priority = t.get("is_priority", False)
            
            # Use sticky state, speed, or priority to define active
            is_active = torrent_hash in active_torrents_sticky or speed > 50000 or is_priority

            if active and not is_active:
                continue

            total_size = t.get("torrent_size", 0) or t.get("total_size", 0)
            loaded_size = t.get("loaded_size", 0)
            progress = round(loaded_size / total_size * 100, 1) if total_size > 0 else 0
            speed_mbps = round(t.get("download_speed", 0) / 1024 / 1024 * 8, 2)

            # For streaming, show "streaming" status instead of misleading 0%
            if is_active and progress < 5:
                display_progress = -1  # Flag for "streaming"
            else:
                display_progress = progress

            # TMDB and quality badges (for the list view)
            raw_title = t.get("title", "Unknown")
            badges = detect_quality_badges(raw_title)

            result.append({
                "hash": t.get("hash", "")[:8],
                "title": raw_title[:60],
                "progress": display_progress,
                "speed_mbps": speed_mbps,
                "peers": t.get("active_peers", 0),
                "seeders": t.get("connected_seeders", 0),
                "status": "active" if is_active else "idle",
                **badges
            })

        # Sort by speed (active first, then by speed)
        result.sort(key=lambda x: (x["status"] != "active", -x["speed_mbps"]))

        return JSONResponse({
            "torrents": result[:50],  # Limit to 50
            "total": len(torrents)
        })
    except Exception as e:
        return JSONResponse({"error": str(e)}, status_code=500)


@app.get("/api/speed-history")
async def api_speed_history() -> JSONResponse:
    """Get speed history for chart."""
    return JSONResponse({"history": list(speed_history)})


@app.get("/api/logs")
async def api_logs(
    file: str = "gostorm-debug",
    lines: int = Query(default=50, le=200)
) -> JSONResponse:
    """Get recent log lines."""
    allowed_files = {
        "gostorm-debug": "gostorm-debug.log",
        "gostorm-tv-sync": "gostorm-tv-sync.log",
        "gostream": "gostream.log",
    }

    if file not in allowed_files:
        return JSONResponse({"error": "Invalid log file"}, status_code=400)

    log_path = os.path.join(LOGS_DIR, allowed_files[file])

    try:
        if not os.path.exists(log_path):
            return JSONResponse({"lines": [], "error": "Log file not found"})

        # Efficient tail using subprocess (V303 optimization)
        result = subprocess.run(["tail", "-n", str(lines), log_path], capture_output=True, text=True, errors="ignore")
        log_content = result.stdout

        parsed_lines = []
        for line in log_content.splitlines():
            # Try format 1: [2025-12-29 10:30:00] [LEVEL] message (GoStorm/Sync)
            match1 = re.match(r'\[(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})\] \[(\w+)\] (.+)', line)
            if match1:
                parsed_lines.append({
                    "time": match1.group(1)[11:],
                    "level": match1.group(2),
                    "msg": match1.group(3)[:200]
                })
                continue
            
            # Try format 2: [2026-01-31 11:00:00 +0100 LEVEL ...] message (GoStream)
            match2 = re.match(r'\[(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}) \+\d{4} (\w+)', line)
            if match2:
                # Find message after the closing bracket of the header
                msg_part = line[line.find(']')+1:].strip()
                if not msg_part: msg_part = "Log header" # Header lines might not have message
                parsed_lines.append({
                    "time": match2.group(1)[11:],
                    "level": match2.group(2),
                    "msg": msg_part[:200]
                })
                continue

            parsed_lines.append({"time": "", "level": "INFO", "msg": line[:200]})

        return JSONResponse({"lines": parsed_lines})
    except Exception as e:
        return JSONResponse({"error": str(e)}, status_code=500)


@app.post("/api/restart/{service}")
async def api_restart(service: str) -> JSONResponse:
    """Restart a service with cooldown protection."""
    services = {
        "gostorm": "gostorm",
        "fuse": "gostream",
        "natpmp": "gostream",
        "plex": "plexmediaserver"  # Remote service — host from config plex.url
    }

    if service not in services:
        return JSONResponse({"status": "error", "message": "Unknown service"}, status_code=400)

    # Check cooldown
    now = time.time()
    last = last_restart.get(service, 0)
    if now - last < RESTART_COOLDOWN:
        remaining = int(RESTART_COOLDOWN - (now - last))
        return JSONResponse({
            "status": "cooldown",
            "message": f"Wait {remaining} more seconds",
            "retry_after": remaining
        }, status_code=429)

    # Execute restart
    try:
        logger.info(f"Restarting service: {services[service]}")

        # Plex is on a different machine, use SSH (host extracted from config plex.url)
        if service == "plex":
            from urllib.parse import urlparse as _urlparse
            _plex_host = _urlparse(PLEX_URL).hostname or "127.0.0.1"
            result = subprocess.run(
                ["ssh", f"pi@{_plex_host}", "sudo", "systemctl", "restart", services[service]],
                capture_output=True, text=True, timeout=30
            )
        elif service == "fuse":
            # Use improved watchdog script for clean FUSE restart (unmounts correctly)
            result = subprocess.run(
                ["sudo", "/home/pi/scripts/fuse-watchdog-improved.sh", "restart"],
                capture_output=True, text=True, timeout=120
            )
        else:
            result = subprocess.run(
                ["sudo", "systemctl", "restart", services[service]],
                capture_output=True, text=True, timeout=30
            )

        if result.returncode == 0:
            last_restart[service] = now
            return JSONResponse({
                "status": "ok",
                "message": f"{service} restarted successfully",
                "timestamp": datetime.now().isoformat()
            })
        else:
            return JSONResponse({
                "status": "error",
                "message": f"Restart failed: {result.stderr[:100]}"
            }, status_code=500)
    except subprocess.TimeoutExpired:
        return JSONResponse({"status": "error", "message": "Restart timed out"}, status_code=500)
    except Exception as e:
        return JSONResponse({"status": "error", "message": str(e)}, status_code=500)


@app.post("/api/kill-stream/{torrent_hash}")
async def api_kill_stream(torrent_hash: str) -> JSONResponse:
    """Kill/drop cache for a specific torrent stream."""
    logger.info(f"Kill stream request received for hash: {torrent_hash}")
    try:
        response = requests.post(
            f"{GOSTORM_URL}/torrents",
            json={"action": "drop", "hash": torrent_hash},
            timeout=5
        )
        logger.info(f"GoStorm response: {response.status_code}")
        if response.status_code == 200:
            # Remove from sticky tracking so it disappears immediately
            if torrent_hash in active_torrents_sticky:
                del active_torrents_sticky[torrent_hash]
            # Clear speed history so average doesn't show old data
            if torrent_hash in torrent_speed_history:
                del torrent_speed_history[torrent_hash]
            logger.info(f"Killed stream for torrent: {torrent_hash[:8]}...")
            return JSONResponse({"status": "ok", "message": "Stream killed"})
        else:
            logger.error(f"GoStorm returned: {response.status_code} - {response.text}")
            return JSONResponse({"status": "error", "message": f"HTTP {response.status_code}"}, status_code=500)
    except Exception as e:
        import traceback
        logger.error(f"Kill stream error: {e}\n{traceback.format_exc()}")
        return JSONResponse({"status": "error", "message": str(e)}, status_code=500)


@app.get("/api/sync")
async def api_sync_status() -> JSONResponse:
    """Get sync status."""
    return JSONResponse({
        "running": sync_state["running"],
        "pid": sync_state["pid"],
        "started_at": sync_state["started_at"],
        "last_run": sync_state["last_run"],
        "last_status": sync_state["last_status"]
    })


@app.post("/api/sync/start")
async def api_sync_start() -> JSONResponse:
    """Start sync script."""
    result = start_sync()
    status_code = 200 if result["status"] == "ok" else 400
    return JSONResponse(result, status_code=status_code)


@app.post("/api/sync/stop")
async def api_sync_stop() -> JSONResponse:
    """Stop sync script."""
    result = stop_sync()
    status_code = 200 if result["status"] == "ok" else 400
    return JSONResponse(result, status_code=status_code)


@app.get("/api/tv-sync")
async def api_tv_sync_status() -> JSONResponse:
    """Get TV sync status."""
    return JSONResponse({
        "running": tv_sync_state["running"],
        "pid": tv_sync_state["pid"],
        "started_at": tv_sync_state["started_at"],
        "last_run": tv_sync_state["last_run"],
        "last_status": tv_sync_state["last_status"]
    })


@app.post("/api/tv-sync/start")
async def api_tv_sync_start() -> JSONResponse:
    """Start TV sync script."""
    result = start_tv_sync()
    status_code = 200 if result["status"] == "ok" else 400
    return JSONResponse(result, status_code=status_code)


@app.post("/api/tv-sync/stop")
async def api_tv_sync_stop() -> JSONResponse:
    """Stop TV sync script."""
    result = stop_tv_sync()
    status_code = 200 if result["status"] == "ok" else 400
    return JSONResponse(result, status_code=status_code)


@app.get("/", response_class=HTMLResponse)
async def dashboard() -> HTMLResponse:
    """Serve the main dashboard HTML with injected config."""
    html = DASHBOARD_HTML.replace("{PLEX_URL}", PLEX_URL) \
                         .replace("{PLEX_TOKEN}", PLEX_TOKEN) \
                         .replace("{PRELOAD_SIZE_MB}", str(PRELOAD_SIZE_MB))
    return HTMLResponse(html)


# =============================================================================
# Dashboard HTML (Tailwind CSS + Chart.js)
# =============================================================================

DASHBOARD_HTML = """
<!DOCTYPE html>
<html lang="en" class="dark">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Streaming Health Monitor</title>
    <link rel="icon" href="/favicon.ico" type="image/svg+xml">
    <script src="https://cdn.tailwindcss.com"></script>
    <script src="https://cdn.jsdelivr.net/npm/chart.js"></script>
    <style>
        body { background: linear-gradient(135deg, #0f172a 0%, #1e293b 100%); min-height: 100vh; }
        .glass { background: rgba(30, 41, 59, 0.8); backdrop-filter: blur(10px); border: 1px solid rgba(255,255,255,0.1); }
        .status-ok { color: #22c55e; }
        .status-warn { color: #eab308; }
        .status-error { color: #ef4444; }
        .status-unknown { color: #6b7280; }
        .pulse-ok { animation: pulse-green 2s infinite; }
        .pulse-error { animation: pulse-red 1s infinite; }
        @keyframes pulse-green { 0%, 100% { opacity: 1; } 50% { opacity: 0.7; } }
        @keyframes pulse-red { 0%, 100% { opacity: 1; } 50% { opacity: 0.5; } }
        .progress-bar { transition: width 0.5s ease-in-out; }
    </style>
</head>
<body class="text-slate-100 p-4 md:p-8">
    <div class="max-w-7xl mx-auto">
        <!-- Header -->
        <div class="flex items-center justify-between mb-8">
            <h1 class="text-2xl md:text-3xl font-bold">
                Streaming Health Monitor
            </h1>
            <div class="text-sm text-slate-400">
                Last update: <span id="lastUpdate">--:--:--</span>
            </div>
        </div>

        <!-- Speed & Torrents Row -->
        <div class="grid md:grid-cols-2 gap-4 mb-8">
            <!-- Speed Chart -->
            <div class="glass rounded-xl p-4">
                <div class="flex items-center justify-between mb-4">
                    <div class="text-xs text-slate-400 uppercase tracking-wider">Download Speed</div>
                    <div class="text-2xl font-bold"><span id="currentSpeed">0</span> Mbps</div>
                </div>
                <div style="height: 150px; position: relative;">
                    <canvas id="speedChart"></canvas>
                </div>
            </div>

            <!-- Torrent Stats -->
            <div class="glass rounded-xl p-4">
                <div class="flex items-center justify-between mb-4">
                    <div class="text-xs text-slate-400 uppercase tracking-wider">Torrents</div>
                    <div class="text-sm text-slate-400"><span id="totalTorrents">0</span> total</div>
                </div>
                <div class="grid grid-cols-3 gap-4 text-center mb-4">
                    <div>
                        <div class="text-3xl font-bold text-green-400" id="downloadingCount">0</div>
                        <div class="text-xs text-slate-400">Active</div>
                    </div>
                    <div>
                        <div class="text-3xl font-bold text-blue-400" id="peersCount">0</div>
                        <div class="text-xs text-slate-400">Peers</div>
                    </div>
                    <div>
                        <div class="text-3xl font-bold text-purple-400" id="seedersCount">0</div>
                        <div class="text-xs text-slate-400">Seeders</div>
                    </div>
                </div>
                <!-- FUSE Buffer Bar -->
                <div class="pt-4 border-t border-slate-700 flex flex-col justify-center" style="min-height: 70px;">
                    <div class="flex items-center justify-between mb-2">
                        <span class="text-xs text-slate-400">FUSE Buffer</span>
                    </div>
                    <div class="h-2 bg-slate-900 rounded-full overflow-hidden flex">
                        <div id="fuseActiveBar" class="h-full bg-blue-500 transition-all duration-500" style="width: 0%"></div>
                        <div id="fuseStaleBar" class="h-full bg-slate-500 transition-all duration-500" style="width: 0%"></div>
                    </div>
                    <div class="flex items-center gap-4 mt-1 text-xs text-slate-500">
                        <span><span class="inline-block w-2 h-2 bg-blue-500 rounded mr-1"></span>Attivo <span class="text-blue-400" id="fuseActivePercent">0%</span></span>
                        <span><span class="inline-block w-2 h-2 bg-slate-500 rounded mr-1"></span>Latente <span id="fuseStalePercent">0%</span></span>
                    </div>
                </div>
            </div>
        </div>

        <!-- Active Stream -->
        <div class="glass rounded-xl p-4 mb-8">
            <div class="text-xs text-slate-400 uppercase tracking-wider mb-4">Active Stream</div>
            <div id="activeDownloads" class="space-y-3 max-h-80 overflow-y-auto">
                <div class="text-slate-500 text-sm">No active streams</div>
            </div>
        </div>

        <!-- Status Cards -->
        <div class="grid grid-cols-2 md:grid-cols-5 gap-4 mb-8">
            <!-- GoStorm -->
            <div class="glass rounded-xl p-4" id="card-gostorm">
                <div class="text-xs text-slate-400 uppercase tracking-wider mb-2">GoStorm</div>
                <div class="flex items-center gap-2 mb-2">
                    <span id="status-gostorm" class="text-3xl">●</span>
                    <span id="latency-gostorm" class="text-xl font-bold">--ms</span>
                </div>
                <button id="restart-gostorm" class="mt-2 w-full bg-slate-600 hover:bg-red-600 text-white text-xs py-1.5 px-2 rounded transition" onclick="restartService('gostorm')">
                    🔄 Restart
                </button>
            </div>

            <!-- FUSE -->
            <div class="glass rounded-xl p-4" id="card-fuse">
                <div class="text-xs text-slate-400 uppercase tracking-wider mb-2">FUSE Mount</div>
                <div class="flex items-center gap-2 mb-2">
                    <span id="status-fuse" class="text-3xl">●</span>
                    <span id="files-fuse" class="text-xl font-bold">-- files</span>
                </div>
                <button id="restart-fuse" class="mt-2 w-full bg-slate-600 hover:bg-red-600 text-white text-xs py-1.5 px-2 rounded transition" onclick="restartService('fuse')">
                    🔄 Restart
                </button>
            </div>

            <!-- VPN -->
            <div class="glass rounded-xl p-4" id="card-vpn">
                <div class="text-xs text-slate-400 uppercase tracking-wider mb-2">VPN (wg0)</div>
                <div class="flex items-center gap-2 mb-2">
                    <span id="status-vpn" class="text-3xl">●</span>
                    <span id="ip-vpn" class="text-xl font-bold">--</span>
                </div>
            </div>

            <!-- NAT-PMP -->
            <div class="glass rounded-xl p-4" id="card-natpmp">
                <div class="text-xs text-slate-400 uppercase tracking-wider mb-2">NAT-PMP</div>
                <div class="flex items-center gap-2 mb-2">
                    <span id="status-natpmp" class="text-3xl">●</span>
                    <span id="port-natpmp" class="text-xl font-bold">:--</span>
                </div>
                <button id="restart-natpmp" class="mt-2 w-full bg-slate-600 hover:bg-red-600 text-white text-xs py-1.5 px-2 rounded transition" onclick="restartService('natpmp')">
                    🔄 Restart
                </button>
            </div>

            <!-- Plex -->
            <div class="glass rounded-xl p-4" id="card-plex">
                <div class="text-xs text-slate-400 uppercase tracking-wider mb-2">PLEX</div>
                <div class="flex items-center gap-2 mb-2">
                    <span id="status-plex" class="text-3xl">●</span>
                    <span id="version-plex" class="text-xl font-bold">--</span>
                </div>
                <button id="restart-plex" class="mt-2 w-full bg-slate-600 hover:bg-red-600 text-white text-xs py-1.5 px-2 rounded transition" onclick="restartService('plex')">
                    🔄 Restart
                </button>
            </div>

            <!-- System -->
            <div class="glass rounded-xl p-4">
                <div class="text-xs text-slate-400 uppercase tracking-wider mb-2">System</div>
                <div class="space-y-1 text-sm">
                    <div class="flex justify-between">
                        <span>CPU</span>
                        <span id="cpu" class="font-mono">--%</span>
                    </div>
                    <div class="flex justify-between">
                        <span>RAM</span>
                        <span id="ram" class="font-mono">--%</span>
                    </div>
                    <div class="flex justify-between">
                        <span>Disk</span>
                        <span id="disk" class="font-mono">-- GB</span>
                    </div>
                </div>
            </div>
        </div>

        <!-- Sync Control -->
        <div class="grid md:grid-cols-2 gap-4 mb-8">
            <!-- Movies Sync -->
            <div class="glass rounded-xl p-4">
                <div class="flex items-center justify-between">
                    <div class="flex items-center gap-3">
                        <div>
                            <div class="text-xs text-slate-400 uppercase tracking-wider mb-1">🎬 Movies Sync</div>
                            <div class="flex items-center gap-2">
                                <span id="sync-status-icon" class="text-xl">⏸️</span>
                                <span id="sync-status-text" class="text-sm">Idle</span>
                            </div>
                        </div>
                        <div id="sync-info" class="text-xs text-slate-500 hidden">
                            <div>Started: <span id="sync-started">--</span></div>
                            <div>Last: <span id="sync-lastrun">--</span></div>
                        </div>
                    </div>
                    <div class="flex gap-2">
                        <button id="sync-start-btn" onclick="startSync()" class="bg-green-600 hover:bg-green-700 text-white px-3 py-1.5 rounded-lg transition flex items-center gap-1 text-sm">
                            <span>▶️</span> Start
                        </button>
                        <button id="sync-stop-btn" onclick="stopSync()" class="bg-red-600 hover:bg-red-700 text-white px-3 py-1.5 rounded-lg transition flex items-center gap-1 text-sm hidden">
                            <span>⏹️</span> Stop
                        </button>
                    </div>
                </div>
            </div>
            <!-- TV Sync -->
            <div class="glass rounded-xl p-4">
                <div class="flex items-center justify-between">
                    <div class="flex items-center gap-3">
                        <div>
                            <div class="text-xs text-slate-400 uppercase tracking-wider mb-1">📺 TV Sync</div>
                            <div class="flex items-center gap-2">
                                <span id="tv-sync-status-icon" class="text-xl">⏸️</span>
                                <span id="tv-sync-status-text" class="text-sm">Idle</span>
                            </div>
                        </div>
                        <div id="tv-sync-info" class="text-xs text-slate-500 hidden">
                            <div>Started: <span id="tv-sync-started">--</span></div>
                            <div>Last: <span id="tv-sync-lastrun">--</span></div>
                        </div>
                    </div>
                    <div class="flex gap-2">
                        <button id="tv-sync-start-btn" onclick="startTvSync()" class="bg-green-600 hover:bg-green-700 text-white px-3 py-1.5 rounded-lg transition flex items-center gap-1 text-sm">
                            <span>▶️</span> Start
                        </button>
                        <button id="tv-sync-stop-btn" onclick="stopTvSync()" class="bg-red-600 hover:bg-red-700 text-white px-3 py-1.5 rounded-lg transition flex items-center gap-1 text-sm hidden">
                            <span>⏹️</span> Stop
                        </button>
                    </div>
                </div>
            </div>
        </div>

        <!-- Logs -->
        <div class="glass rounded-xl p-4">
            <div class="flex items-center justify-between mb-4">
                <div class="text-xs text-slate-400 uppercase tracking-wider">Recent Logs</div>
                <select id="logSelect" class="bg-slate-700 text-sm rounded px-2 py-1 border-none" onchange="loadLogs()">
                    <option value="gostorm-debug">gostorm-debug</option>
                    <option value="gostorm-tv-sync">gostorm-tv-sync</option>
                    <option value="gostream">GoStream</option>
                </select>
            </div>
            <div id="logContainer" class="font-mono text-xs space-y-1 max-h-60 overflow-y-auto bg-slate-900/50 rounded p-3">
                <div class="text-slate-500">Loading logs...</div>
            </div>
        </div>
    </div>

    <!-- Generic Confirmation Modal -->
    <div id="confirmModal" class="fixed inset-0 bg-black/50 backdrop-blur-sm hidden items-center justify-center z-50">
        <div class="glass rounded-xl p-6 max-w-sm mx-4">
            <h3 id="confirmTitle" class="text-lg font-bold mb-4">Confirm Action</h3>
            <p id="confirmMessage" class="text-slate-300 mb-6"></p>
            <div class="flex gap-3">
                <button onclick="hideConfirmModal()" class="flex-1 bg-slate-600 hover:bg-slate-700 py-2 rounded transition">Cancel</button>
                <button id="confirmActionBtn" class="flex-1 bg-red-600 hover:bg-red-700 py-2 rounded transition">Confirm</button>
            </div>
        </div>
    </div>

    <script>
        let speedChart;
        let pendingAction = null;  // {type: 'restart'|'kill'|'sync', data: any, callback: fn}
        
        // PLEX CONFIG (Injected from Python)
        const PLEX_URL = "{PLEX_URL}";
        const PLEX_TOKEN = "{PLEX_TOKEN}";
        const PRELOAD_SIZE_MB = {PRELOAD_SIZE_MB};

        // Initialize chart
        function initChart() {
            const ctx = document.getElementById('speedChart').getContext('2d');
            speedChart = new Chart(ctx, {
                type: 'line',
                data: {
                    labels: [],
                    datasets: [{
                        label: 'Speed (Mbps)',
                        data: [],
                        borderColor: '#3b82f6',
                        backgroundColor: 'rgba(59, 130, 246, 0.1)',
                        fill: true,
                        tension: 0.3,
                        pointRadius: 0,
                        borderWidth: 2
                    }]
                },
                options: {
                    responsive: true,
                    maintainAspectRatio: false,
                    animation: {
                        duration: 750,
                        easing: 'easeInOutQuart'
                    },
                    plugins: { legend: { display: false } },
                    scales: {
                        x: {
                            display: true,
                            grid: { display: false },
                            ticks: {
                                color: '#64748b',
                                maxTicksLimit: 6,
                                font: { size: 10 }
                            }
                        },
                        y: {
                            beginAtZero: true,
                            grid: { color: 'rgba(255,255,255,0.05)' },
                            ticks: {
                                color: '#64748b',
                                font: { size: 10 }
                            }
                        }
                    }
                }
            });
        }

        // Update status indicator
        function updateStatus(id, status) {
            const el = document.getElementById(`status-${id}`);
            const btn = document.getElementById(`restart-${id}`);

            el.className = 'text-3xl status-' + status;
            if (status === 'ok') {
                el.classList.add('pulse-ok');
            } else if (status === 'error') {
                el.classList.add('pulse-error');
            }

            // Highlight restart button on error
            if (btn) {
                if (status === 'error') {
                    btn.classList.remove('bg-slate-600');
                    btn.classList.add('bg-red-600', 'animate-pulse');
                } else {
                    btn.classList.add('bg-slate-600');
                    btn.classList.remove('bg-red-600', 'animate-pulse');
                }
            }
        }

        // Fetch health data (unified source for all panels)
        async function fetchHealth() {
            try {
                const res = await fetch('/api/health');
                const data = await res.json();

                // Update timestamp
                if (data.timestamp) {
                    const time = data.timestamp.split('T')[1].split('.')[0];
                    document.getElementById('lastUpdate').textContent = time;
                }

                // GoStorm
                updateStatus('gostorm', data.services.gostorm.status);
                document.getElementById('latency-gostorm').textContent = data.services.gostorm.latency_ms + 'ms';

                // FUSE
                updateStatus('fuse', data.services.fuse.status);
                document.getElementById('files-fuse').textContent = data.services.fuse.files + ' files';

                // VPN
                updateStatus('vpn', data.services.vpn.status);
                document.getElementById('ip-vpn').innerHTML = `
                    <div class="flex flex-col leading-tight text-right">
                        <span class="text-xs text-slate-500 font-normal">${data.services.vpn.ip}</span>
                        <span class="text-base">${data.services.vpn.public_ip || '--'}</span>
                    </div>`;

                // NAT-PMP
                updateStatus('natpmp', data.services.natpmp.status);
                document.getElementById('port-natpmp').textContent = ':' + (data.services.natpmp.port || '--');

                // Plex
                updateStatus('plex', data.services.plex.status);
                if (data.services.plex.status === 'ok' && data.services.plex.version) {
                    const v = data.services.plex.version.split('.').slice(0, 2).join('.');
                    document.getElementById('version-plex').textContent = 'v' + v;
                } else {
                    document.getElementById('version-plex').textContent = 'Offline';
                }

                // System
                document.getElementById('cpu').textContent = data.system.cpu + '%';
                document.getElementById('ram').textContent = data.system.ram + '%';
                document.getElementById('disk').textContent = data.system.disk_free_gb + ' GB free';

                // Torrents stats
                document.getElementById('currentSpeed').textContent = data.torrents.speed_mbps;
                document.getElementById('totalTorrents').textContent = data.torrents.total;
                document.getElementById('downloadingCount').textContent = data.torrents.downloading;
                document.getElementById('peersCount').textContent = data.torrents.peers;
                document.getElementById('seedersCount').textContent = data.torrents.seeders;

                // FUSE Buffer bar (global) - Active (blue) + Stale (gray)
                let fuseActive = data.torrents.fuse_buffer_active_percent || 0;
                let fuseStale = data.torrents.fuse_buffer_stale_percent || 0;
                
                // Clamp sum to 100% to avoid layout breaking
                if (fuseActive + fuseStale > 100) {
                    const ratio = 100 / (fuseActive + fuseStale);
                    fuseActive *= ratio;
                    fuseStale *= ratio;
                }

                document.getElementById('fuseActivePercent').textContent = fuseActive.toFixed(1) + '%';
                document.getElementById('fuseStalePercent').textContent = fuseStale.toFixed(1) + '%';
                document.getElementById('fuseActiveBar').style.width = Math.min(fuseActive, 100) + '%';
                document.getElementById('fuseStaleBar').style.width = Math.min(fuseStale, 100) + '%';

                // Active Downloads (from same data source - synchronized!)
                updateActiveDownloads(data.active_torrents || []);

            } catch (e) {
                console.error('Health fetch error:', e);
            }
        }

        // Update Active Downloads panel
        function updateActiveDownloads(torrents) {
            const container = document.getElementById('activeDownloads');
            // ... (keep existing code)

            if (!torrents || torrents.length === 0) {
                container.innerHTML = '<div class="text-slate-500 text-sm">No active streams</div>';
                return;
            }

            container.innerHTML = torrents.map(t => {
                const hasLiveFlag = (t.progress === -1 || t.progress < 0);
                const hasSpeed = t.speed_mbps > 0;
                const isStreaming = t.fuse_streaming || (hasLiveFlag && hasSpeed);  // LIVE if speed OR FUSE active
                const progressText = hasLiveFlag ? 'LIVE' : (t.progress + '%');
                const progressWidth = hasLiveFlag ? 100 : t.progress;
                const barClass = isStreaming
                    ? 'bg-gradient-to-r from-green-500 to-emerald-400 animate-pulse'
                    : 'bg-slate-600';
                const avgSpeed = t.avg_speed_mbps || t.speed_mbps;

                // Build quality badges (monochrome)
                const badgeClass = 'bg-white/10 text-white px-1.5 py-0.5 rounded text-xs font-medium border border-white/20';
                let badges = '';
                if (t.is_priority) badges += `<span class="${badgeClass} border-blue-500/50 text-blue-400 bg-blue-500/10">PRIORITY</span> `;
                if (t.is_4k) badges += `<span class="${badgeClass}">4K</span> `;
                if (t.is_1080p && !t.is_4k) badges += `<span class="${badgeClass}">1080p</span> `;
                if (t.is_dv) badges += `<span class="${badgeClass}">DV</span> `;
                if (t.is_hdr && !t.is_dv) badges += `<span class="${badgeClass}">HDR</span> `;
                // Audio badge: combine codec + channels (e.g., "DD+ 5.1"), but ATMOS stands alone
                const audioLabel = t.audio === 'ATMOS' ? 'ATMOS' : [t.audio, t.channels].filter(Boolean).join(' ');
                if (audioLabel) badges += `<span class="${badgeClass}">${audioLabel}</span> `;

                // Poster or placeholder
                const posterHtml = t.poster
                    ? `<img src="${t.poster}" alt="" class="w-14 h-20 object-cover rounded shadow-lg flex-shrink-0">`
                    : `<div class="w-14 h-20 bg-slate-700 rounded flex items-center justify-center text-2xl flex-shrink-0">🎬</div>`;

                // Title with year and size
                const titleLine = t.year
                    ? `${t.title} <span class="text-slate-500">(${t.year})</span> <span class="text-slate-500">- ${t.size}</span>`
                    : `${t.title} <span class="text-slate-500">- ${t.size}</span>`;

                // Apply grayscale filter to non-streaming items
                const cardStyle = isStreaming ? '' : 'opacity: 0.5; filter: grayscale(70%);';

                // Cache bar (per-torrent GoStorm cache only)
                const cachePercent = t.cache_percent || 0;

                // Choose bar style based on streaming status
                let barHtml;
                // V226: Render Proxy RAM only (FUSE raCache)
                if ((isStreaming || t.is_priority) && (t.ra_active_percent + t.ra_stale_percent) > 0) {
                    const raActive = t.ra_active_percent || 0;
                    const raStale = t.ra_stale_percent || 0;
                    
                    // Add brightness boost if idle but priority to cut through the 0.5 opacity
                    const vibrancyClass = !isStreaming && t.is_priority ? 'brightness-150 saturate-150' : '';
                    barHtml = `
                        <div class="flex-1 h-2 bg-slate-700 rounded-full overflow-hidden flex">
                            <!-- FUSE raCache (Blue/Gray) -->
                            <div class="h-full bg-blue-500 transition-all ${vibrancyClass} border-r border-white/10" style="width: ${raActive}%" title="Proxy Active ${raActive.toFixed(1)}%"></div>
                            <div class="h-full bg-slate-500 transition-all ${vibrancyClass}" style="width: ${raStale}%" title="Proxy Stale ${raStale.toFixed(1)}%"></div>
                        </div>`;
                } else {
                    // Simple progress bar for non-streaming
                    barHtml = `
                        <div class="flex-1 h-2 bg-slate-700 rounded-full overflow-hidden">
                            <div class="h-full ${barClass} progress-bar" style="width: ${progressWidth}%"></div>
                        </div>`;
                }

                // Cache legend + peers/seeders (only show for streaming)
                const bufferLegend = isStreaming
                    ? `<div class="flex items-center justify-between text-[10px] text-slate-500 mt-1">
                        <div class="flex gap-2">
                            ${(t.ra_active_percent + t.ra_stale_percent) > 0 ? `<span><span class="inline-block w-1.5 h-1.5 bg-blue-500 rounded-sm mr-1"></span>Proxy RAM</span>` : ''}
                        </div>
                        <span class="text-slate-400"><span class="text-blue-400">${t.peers}</span> peers · <span class="text-purple-400">${t.seeders}</span> seeders</span>
                       </div>`
                    : '';

                return `
                <div class="flex gap-3 bg-slate-800/50 rounded-lg p-3 relative" style="${cardStyle}">
                    <button onclick="killStream('${t.full_hash}')"
                            class="absolute top-2 right-2 text-slate-500 hover:text-red-400 hover:bg-red-900/30 rounded p-1 transition-colors"
                            title="Kill stream">
                        <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12"></path>
                        </svg>
                    </button>
                    ${posterHtml}
                    <div class="flex-1 min-w-0 flex flex-col justify-between">
                        <div>
                            <div class="text-sm font-medium truncate pr-6">${titleLine}</div>
                            <div class="flex items-center gap-1 mt-1 flex-wrap">${badges}</div>
                        </div>
                        <div class="mt-2">
                            <div class="flex items-center gap-3">
                                ${barHtml}
                                <span class="text-xs font-bold ${isStreaming ? 'text-green-400' : 'text-slate-400'} w-10">${progressText}</span>
                                <div class="text-right flex-shrink-0">
                                    <span class="text-lg font-bold ${isStreaming ? 'text-blue-400' : 'text-slate-400'}">${avgSpeed}</span>
                                    <span class="text-xs text-slate-500 ml-1">Mbps/avg</span>
                                </div>
                            </div>
                            ${bufferLegend}
                        </div>
                    </div>
                </div>`;
            }).join('');
        }

        // Fetch speed history
        async function fetchSpeedHistory() {
            try {
                const res = await fetch('/api/speed-history');
                const data = await res.json();

                // Show time labels every ~2.5 minutes (30 data points)
                const labels = data.history.map((h, i) => {
                    if (i % 30 === 0 || i === data.history.length - 1) {
                        const time = h.time.split('T')[1].split('.')[0];
                        return time.substring(0, 5);
                    }
                    return '';
                });
                const speeds = data.history.map(h => h.speed);

                speedChart.data.labels = labels;
                speedChart.data.datasets[0].data = speeds;
                speedChart.update();  // Smooth animation
            } catch (e) {
                console.error('Speed history error:', e);
            }
        }

        // Load logs
        async function loadLogs() {
            const file = document.getElementById('logSelect').value;
            const container = document.getElementById('logContainer');

            try {
                const res = await fetch(`/api/logs?file=${file}&lines=50`);
                const data = await res.json();

                if (!data.lines || data.lines.length === 0) {
                    container.innerHTML = '<div class="text-slate-500">No logs available</div>';
                    return;
                }

                container.innerHTML = data.lines.map(l => {
                    const levelClass = l.level === 'ERROR' ? 'text-red-400' :
                                       l.level === 'WARN' ? 'text-yellow-400' :
                                       'text-slate-400';
                    return `<div><span class="text-slate-500">${l.time}</span> <span class="${levelClass}">${l.level}</span> ${l.msg}</div>`;
                }).join('');

                container.scrollTop = container.scrollHeight;
            } catch (e) {
                container.innerHTML = '<div class="text-red-400">Error loading logs</div>';
            }
        }

        // Generic confirmation modal functions
        function showConfirmModal(title, message, buttonText, buttonClass, callback) {
            document.getElementById('confirmTitle').textContent = title;
            document.getElementById('confirmMessage').textContent = message;
            const btn = document.getElementById('confirmActionBtn');
            btn.textContent = buttonText;
            btn.className = `flex-1 ${buttonClass} py-2 rounded transition`;
            pendingAction = callback;
            document.getElementById('confirmModal').classList.remove('hidden');
            document.getElementById('confirmModal').classList.add('flex');
        }

        function hideConfirmModal() {
            document.getElementById('confirmModal').classList.add('hidden');
            document.getElementById('confirmModal').classList.remove('flex');
            pendingAction = null;
        }

        function executeConfirmedAction() {
            if (pendingAction) {
                const callback = pendingAction;
                hideConfirmModal();
                callback();
            }
        }

        // Update confirm button to call executeConfirmedAction
        document.getElementById('confirmActionBtn').onclick = executeConfirmedAction;

        // Restart service
        function restartService(service) {
            const serviceNames = {
                'torrserver': 'GoStorm',
                'fuse': 'FUSE Mount',
                'natpmp': 'NAT-PMP',
                'plex': 'Plex'
            };
            showConfirmModal(
                'Confirm Restart',
                `Are you sure you want to restart ${serviceNames[service] || service}?`,
                'Restart',
                'bg-red-600 hover:bg-red-700',
                () => doRestart(service)
            );
        }

        async function doRestart(service) {
            try {
                const res = await fetch(`/api/restart/${service}`, { method: 'POST' });
                const data = await res.json();

                if (data.status === 'ok') {
                    showNotification(`${service} restarted successfully!`, 'success');
                } else if (data.status === 'cooldown') {
                    showNotification(data.message, 'warning');
                } else {
                    showNotification(`Restart failed: ${data.message}`, 'error');
                }
            } catch (e) {
                showNotification(`Error: ${e.message}`, 'error');
            }
        }

        // Kill stream (drop cache)
        function killStream(hash) {
            showConfirmModal(
                'Kill Stream',
                'Terminate this stream? The playback will stop.',
                'Kill Stream',
                'bg-red-600 hover:bg-red-700',
                () => doKillStream(hash)
            );
        }

        async function doKillStream(hash) {
            try {
                const res = await fetch(`/api/kill-stream/${hash}`, { method: 'POST' });
                const data = await res.json();

                if (data.status === 'ok') {
                    fetchHealth();  // Refresh immediately
                    showNotification('Stream terminated', 'success');
                } else {
                    showNotification('Error: ' + (data.message || 'Kill failed'), 'error');
                }
            } catch (e) {
                showNotification('Connection error', 'error');
            }
        }

        // Notification toast
        function showNotification(message, type = 'info') {
            const colors = {
                success: 'bg-green-600',
                error: 'bg-red-600',
                warning: 'bg-yellow-600',
                info: 'bg-blue-600'
            };
            const toast = document.createElement('div');
            toast.className = `fixed bottom-4 right-4 ${colors[type]} text-white px-4 py-2 rounded-lg shadow-lg z-50 animate-fade-in`;
            toast.textContent = message;
            document.body.appendChild(toast);
            setTimeout(() => {
                toast.classList.add('opacity-0', 'transition-opacity');
                setTimeout(() => toast.remove(), 300);
            }, 3000);
        }

        // Sync management
        async function fetchSyncStatus() {
            try {
                const res = await fetch('/api/sync');
                const data = await res.json();
                updateSyncUI(data);
            } catch (e) {
                console.error('Sync status error:', e);
            }
        }

        function updateSyncUI(data) {
            const icon = document.getElementById('sync-status-icon');
            const text = document.getElementById('sync-status-text');
            const info = document.getElementById('sync-info');
            const startBtn = document.getElementById('sync-start-btn');
            const stopBtn = document.getElementById('sync-stop-btn');
            const started = document.getElementById('sync-started');
            const lastrun = document.getElementById('sync-lastrun');

            if (data.running) {
                icon.textContent = '🔄';
                icon.classList.add('animate-spin');
                text.textContent = 'Running...';
                text.classList.add('text-green-400');
                text.classList.remove('text-slate-400');
                startBtn.classList.add('hidden');
                stopBtn.classList.remove('hidden');
                info.classList.remove('hidden');
                if (data.started_at) {
                    started.textContent = data.started_at.split('T')[1].split('.')[0];
                }
            } else {
                icon.textContent = '⏸️';
                icon.classList.remove('animate-spin');
                text.classList.remove('text-green-400');
                text.classList.add('text-slate-400');
                startBtn.classList.remove('hidden');
                stopBtn.classList.add('hidden');

                if (data.last_status === 'success') {
                    text.textContent = 'Last: Success ✓';
                } else if (data.last_status === 'error') {
                    text.textContent = 'Last: Error ✗';
                    text.classList.add('text-red-400');
                } else if (data.last_status === 'stopped') {
                    text.textContent = 'Stopped';
                } else {
                    text.textContent = 'Idle';
                }

                if (data.last_run) {
                    info.classList.remove('hidden');
                    lastrun.textContent = data.last_run.split('T')[1].split('.')[0];
                }
            }
        }

        function startSync() {
            showConfirmModal(
                'Start Movies Sync',
                'Start library sync? This may take several minutes.',
                'Start',
                'bg-green-600 hover:bg-green-700',
                doStartSync
            );
        }

        async function doStartSync() {
            try {
                const res = await fetch('/api/sync/start', { method: 'POST' });
                const data = await res.json();

                if (data.status === 'ok') {
                    fetchSyncStatus();
                    showNotification('Movies sync started', 'success');
                } else {
                    showNotification(`Error: ${data.message}`, 'error');
                }
            } catch (e) {
                showNotification(`Error: ${e.message}`, 'error');
            }
        }

        function stopSync() {
            showConfirmModal(
                'Stop Movies Sync',
                'Stop the running sync process?',
                'Stop',
                'bg-red-600 hover:bg-red-700',
                doStopSync
            );
        }

        async function doStopSync() {
            try {
                const res = await fetch('/api/sync/stop', { method: 'POST' });
                const data = await res.json();

                if (data.status === 'ok') {
                    fetchSyncStatus();
                    showNotification('Movies sync stopped', 'success');
                } else {
                    showNotification(`Error: ${data.message}`, 'error');
                }
            } catch (e) {
                showNotification(`Error: ${e.message}`, 'error');
            }
        }

        // TV Sync management
        async function fetchTvSyncStatus() {
            try {
                const res = await fetch('/api/tv-sync');
                const data = await res.json();
                updateTvSyncUI(data);
            } catch (e) {
                console.error('TV Sync status error:', e);
            }
        }

        function updateTvSyncUI(data) {
            const icon = document.getElementById('tv-sync-status-icon');
            const text = document.getElementById('tv-sync-status-text');
            const info = document.getElementById('tv-sync-info');
            const startBtn = document.getElementById('tv-sync-start-btn');
            const stopBtn = document.getElementById('tv-sync-stop-btn');
            const started = document.getElementById('tv-sync-started');
            const lastrun = document.getElementById('tv-sync-lastrun');

            if (data.running) {
                icon.textContent = '🔄';
                icon.classList.add('animate-spin');
                text.textContent = 'Running...';
                text.classList.add('text-green-400');
                text.classList.remove('text-slate-400');
                startBtn.classList.add('hidden');
                stopBtn.classList.remove('hidden');
                info.classList.remove('hidden');
                if (data.started_at) {
                    started.textContent = data.started_at.split('T')[1].split('.')[0];
                }
            } else {
                icon.textContent = '⏸️';
                icon.classList.remove('animate-spin');
                text.classList.remove('text-green-400');
                text.classList.add('text-slate-400');
                startBtn.classList.remove('hidden');
                stopBtn.classList.add('hidden');

                if (data.last_status === 'success') {
                    text.textContent = 'Last: Success ✓';
                } else if (data.last_status === 'error') {
                    text.textContent = 'Last: Error ✗';
                    text.classList.add('text-red-400');
                } else if (data.last_status === 'stopped') {
                    text.textContent = 'Stopped';
                } else {
                    text.textContent = 'Idle';
                }

                if (data.last_run) {
                    info.classList.remove('hidden');
                    lastrun.textContent = data.last_run.split('T')[1].split('.')[0];
                }
            }
        }

        function startTvSync() {
            showConfirmModal(
                'Start TV Sync',
                'Start TV series sync? This may take several minutes.',
                'Start',
                'bg-green-600 hover:bg-green-700',
                doStartTvSync
            );
        }

        async function doStartTvSync() {
            try {
                const res = await fetch('/api/tv-sync/start', { method: 'POST' });
                const data = await res.json();

                if (data.status === 'ok') {
                    fetchTvSyncStatus();
                    showNotification('TV sync started', 'success');
                } else {
                    showNotification(`Error: ${data.message}`, 'error');
                }
            } catch (e) {
                showNotification(`Error: ${e.message}`, 'error');
            }
        }

        function stopTvSync() {
            showConfirmModal(
                'Stop TV Sync',
                'Stop the running TV sync process?',
                'Stop',
                'bg-red-600 hover:bg-red-700',
                doStopTvSync
            );
        }

        async function doStopTvSync() {
            try {
                const res = await fetch('/api/tv-sync/stop', { method: 'POST' });
                const data = await res.json();

                if (data.status === 'ok') {
                    fetchTvSyncStatus();
                    showNotification('TV sync stopped', 'success');
                } else {
                    showNotification(`Error: ${data.message}`, 'error');
                }
            } catch (e) {
                showNotification(`Error: ${e.message}`, 'error');
            }
        }

        // Initialize
        initChart();
        fetchHealth();
        fetchSpeedHistory();
        fetchSyncStatus();
        fetchTvSyncStatus();
        loadLogs();

        // Auto-refresh (fetchHealth now updates all panels)
        setInterval(fetchHealth, 5000);
        setInterval(fetchSpeedHistory, 5000);
        setInterval(fetchSyncStatus, 5000);
        setInterval(fetchTvSyncStatus, 5000);
        setInterval(loadLogs, 10000);
    </script>
</body>
</html>
"""


# =============================================================================
# Main Entry Point
# =============================================================================

if __name__ == "__main__":
    import uvicorn

    # Start collector thread
    collector_thread = threading.Thread(target=collector_loop, daemon=True, name="collector")
    collector_thread.start()

    logger.info(f"Starting Health Monitor on port {PORT}")
    logger.info(f"Dashboard: http://0.0.0.0:{PORT}")

    uvicorn.run(app, host="0.0.0.0", port=PORT, log_level="warning")
