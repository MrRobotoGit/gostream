#!/usr/bin/env python3
"""
GoStorm Movies Sync Script - Movies Only (TV Logic Disabled)
TMDB -> Stremio -> GoStorm integration for Movies
TV Series processing moved to separate script: gostorm-tv-sync.py
"""

import json
import logging
import os
import random
import re
import sys
import time
import requests
from pathlib import Path
from typing import Dict, List, Optional, Tuple, Any
import subprocess


def _load_gostream_config() -> dict:
    """Load config.json from the GoStream install directory.

    Resolution order:
      1. MKV_PROXY_CONFIG_PATH env var (absolute path to config.json)
      2. ../config.json relative to this script (standard co-located layout)
    Also derives _state_dir and _log_dir from the install directory layout.
    """
    config_path = os.environ.get(
        'MKV_PROXY_CONFIG_PATH',
        os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', 'config.json')
    )
    try:
        with open(config_path, encoding='utf-8') as f:
            cfg = json.load(f)
    except Exception:
        cfg = {}
    # Derive auxiliary dirs from install layout:
    # GoStream/config.json → parent = GoStream dir → parent = base dir
    config_dir = os.path.dirname(os.path.abspath(config_path))
    base_dir = os.path.dirname(config_dir)
    cfg.setdefault('_state_dir', os.path.join(base_dir, 'STATE'))
    cfg.setdefault('_log_dir', os.path.join(base_dir, 'logs'))
    return cfg


_cfg = _load_gostream_config()


class GoStormSync:
    """Port evoluto di gostorm-sync-complete.sh con miglioramenti prestazionali e di qualità"""
    
    def __init__(self):
        # === CONFIGURATION (read from config.json, with sensible defaults) ===
        self.TORRSERVER_URL = _cfg.get('gostorm_url', 'http://127.0.0.1:8090')
        self.MOUNT_DIR = _cfg.get('physical_source_path', '/mnt/torrserver')
        self.MOVIES_DIR = os.path.join(self.MOUNT_DIR, "movies")
        self.TV_DIR = os.path.join(self.MOUNT_DIR, "tv")
        # Persistent cache for processed TV fullpacks to ensure idempotence across runs
        self.STATE_DIR = _cfg.get('_state_dir', '/home/pi/STATE')
        self.FULLPACK_CACHE_FILE = os.path.join(self.STATE_DIR, "tv_fullpacks.json")
        # TV library persistence cache to prevent systematic deletion
        self.TV_LIBRARY_CACHE_FILE = os.path.join(self.STATE_DIR, "tv_series_library.json")
        # Negative cache per torrent hash senza .mkv valido (evita retry inutili di release solo mp4)
        self.NEGATIVE_CACHE_FILE = os.path.join(self.STATE_DIR, "no_mkv_hashes.json")
        self.NEGATIVE_CACHE_TTL_HOURS = 12  # configurabile
        try:
            os.makedirs(self.STATE_DIR, exist_ok=True)
        except OSError:
            pass
        self._fullpack_cache = self._load_fullpack_cache()
        self._tv_library_cache = self._load_tv_library_cache()
        self._negative_cache = self._load_negative_cache()
        self.BLACKLIST_FILE = os.path.join(self.STATE_DIR, "blacklist.json")
        self._blacklist = self._load_blacklist()
        
        # Prune expired negative cache entries at startup
        try:
            now = int(time.time())
            expired = []
            for h, rec in list(self._negative_cache.items()):
                ts = rec.get('ts')
                if not ts or (now - ts) > (self.NEGATIVE_CACHE_TTL_HOURS * 3600):
                    expired.append(h)
                    self._negative_cache.pop(h, None)
            if expired:
                self._save_negative_cache()
                # Evita logging prima di setup_logging
                if hasattr(self, "logger"):
                    self.log("DEBUG", f"Startup prune: removed {len(expired)} expired negative cache entries")
        except Exception:
            pass
        self.LOG_FILE = os.path.join(_cfg.get('_log_dir', '/home/pi/logs'), 'gostorm-debug.log')
        # TMDB / Torrentio
        self.TMDB_API_KEY = _cfg.get('tmdb_api_key', '')
        self.TMDB_BASE_URL = "https://api.themoviedb.org/3"
        self.TORRENTIO_BASE_URL = "https://torrentio.strem.fun"
        
        # Torrentio API Configuration (unified for movies and TV)
        self.TORRENTIO_SORT = os.getenv("TORRENTIO_SORT", "qualitysize")
        self.TORRENTIO_EXCLUDE_QUALITIES = os.getenv("TORRENTIO_EXCLUDE", "480p,720p,scr,cam")
        self.TORRENTIO_4K_FOCUS = os.getenv("TORRENTIO_4K_FOCUS", "0") == "1"
        self.TORRENTIO_PROVIDERS = os.getenv("TORRENTIO_PROVIDERS", "")  # empty = all providers
        # Sizes
        self.BYTES_PER_GB = 1024 * 1024 * 1024
        self.MOVIE_4K_MIN_GB = int(os.getenv("MOVIE_4K_MIN_GB", "10"))  # ora configurabile via ENV
        self.MOVIE_4K_MIN_SIZE = self.MOVIE_4K_MIN_GB * self.BYTES_PER_GB
        self.MOVIE_4K_MAX_GB = 60
        self.MOVIE_4K_MAX_SIZE = self.MOVIE_4K_MAX_GB * self.BYTES_PER_GB
        self.MOVIE_1080P_MIN_SIZE = 4 * self.BYTES_PER_GB
        self.MOVIE_1080P_MAX_SIZE = 20 * self.BYTES_PER_GB
        self.MOVIE_1080P_MIN_GB = 4
        self.MOVIE_1080P_MAX_GB = 20
        self.TV_SERIES_MIN_SIZE = 500 * 1024 * 1024
        self.TV_SERIES_MAX_SIZE = 25 * self.BYTES_PER_GB

        # File validation limits
        self.FILE_MIN_VALIDATION = 100 * 1024 * 1024          # 100MB
        self.FILE_MAX_VALIDATION = 100 * 1024 * 1024 * 1024   # 100GB

        # Video resolution patterns
        self.VIDEO_4K_PATTERNS = r"2160p|4K|4k|UHD"
        self.VIDEO_1080P_PATTERNS = r"1080p|1080i|FHD"
        self.VIDEO_720P_PATTERNS = r"720p|720i|HD"

        # Languages
        self.SUPPORTED_LANGUAGES = "en|it"
        self.EXCLUDED_STREAM_LANGUAGES = "🇪🇸|🇫🇷|🇩🇪|🇷🇺|🇨🇳|🇯🇵|🇰🇷|🇹🇭|🇵🇹|🇧🇷"

        # Quality feature patterns
        self.HDR_PATTERNS = r"HDR|HDR10\+|\bDV\b|DoVi|Dolby.?Vision"
        self.PREMIUM_SOURCE_PATTERNS = "bluray|web-dl|amazon|\\bweb\\b"

        # Premium IT Providers (TMDB IDs)
        # 350: Apple TV Plus, 8: Netflix, 337: Disney Plus, 119: Amazon Prime Video
        self.PREMIUM_PROVIDER_IDS = {350, 8, 337, 119}

        # Niche/curated streaming providers for arthouse discovery
        # 11: MUBI, 258: Criterion Channel, 529: ARROW
        self.NICHE_PROVIDER_IDS = "11|258|529"

        # Seeder requirements
        self.MIN_SEEDERS = 15

        # Runtime / polling config
        self.PROCESS_INTERVAL = int(os.getenv("PROCESS_INTERVAL", "1"))  # seconds sleep between items
        self.METADATA_MAX_WAIT = int(os.getenv("METADATA_MAX_WAIT", "12"))  # seconds total for torrent metadata
        self.METADATA_SLEEP_SEQ = [int(x) for x in os.getenv("METADATA_SLEEP_SEQ", "1,2,3,3,3").split(',')]
        # Attese metadata (base + estesa per 4K)
        self.METADATA_4K_MAX_WAIT = int(os.getenv("METADATA_4K_MAX_WAIT", "45"))  # secondi (default 45)
        
        # Coverage scan optimization controls
        self.FULL_SCAN_AFTER_CLEANUP = os.getenv("FULL_SCAN_AFTER_CLEANUP", "1") == "1"
        self.FAST_EXISTING_PASS = os.getenv("FAST_EXISTING_PASS", "1") == "1"
        
        # TV processing control - fullpack only mode
        self.TV_FULLPACK_ONLY = os.getenv("TV_FULLPACK_ONLY", "0") == "1"  # 1=scarica solo season/full pack, 0=anche episodi singoli
        # TV library preservation - prevent systematic deletion of existing shows
        self.TV_PRESERVE_LIBRARY = os.getenv("TV_PRESERVE_LIBRARY", "1") == "1"  # 1=preserve existing TV library, 0=only discover recent shows
        
        # Rate limiting and stability controls
        self.MIN_API_INTERVAL_SEC = float(os.getenv("MIN_API_INTERVAL_SEC", "1.0"))  # Min seconds between API calls
        self.MAX_RETRIES = int(os.getenv("MAX_RETRIES", "5"))
        self.INITIAL_BACKOFF_SEC = float(os.getenv("INITIAL_BACKOFF_SEC", "1.0"))
        self.MAX_BACKOFF_SEC = float(os.getenv("MAX_BACKOFF_SEC", "30.0"))
        self.CACHE_TTL_SEC = int(os.getenv("CACHE_TTL_SEC", "300"))  # 5 min cache for torrents list
        # REMOVED: MAX_ACTIVE_TORRENTS - no artificial limit on torrents
        
        # Rate limiting state
        self._last_api_call_time = 0
        self._torrents_cache = None
        self._torrents_cache_time = 0

        # Tracker list cache (fetched from GitHub)
        self.TRACKERS_URL = "https://raw.githubusercontent.com/ngosang/trackerslist/master/trackers_best.txt"
        self._trackers_cache: list = []
        self._trackers_cache_time = 0
        self.TRACKERS_CACHE_TTL = 3600  # 1 hour cache for trackers

        # Cache runtime: stagione già coperta da un fullpack (season_key -> hash)
        self._season_fullpack_hash: dict = {}

        # Init logging & HTTP session
        self.setup_logging()
        self.session = requests.Session()
        self.session.headers.update({'User-Agent': 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36'})
        
        # === CORE INDEX PERSISTENTI ===
        self.MOVIE_CORE_INDEX_FILE = os.path.join(self.STATE_DIR, "movie_core_index.json")
        self.TV_CORE_INDEX_FILE = os.path.join(self.STATE_DIR, "tv_core_index.json")
        self._movie_core_index = self._load_core_index(self.MOVIE_CORE_INDEX_FILE)
        self._tv_core_index = self._load_core_index(self.TV_CORE_INDEX_FILE)
        # Rebuild se file mancante / vuoto
        if not self._movie_core_index:
            self._rebuild_movie_core_index()
        if not self._tv_core_index:
            self._rebuild_tv_core_index()

        # Populate TV library cache from existing shows on startup
        if self.TV_PRESERVE_LIBRARY:
            self._populate_tv_library_cache_from_existing()

    # === Logging utilities ===
    def setup_logging(self):
        handlers = []
        # Solo aggiungere StreamHandler se stdout è un terminale interattivo
        # Evita log duplicati quando eseguito da cron con redirect >> file.log 2>&1
        if sys.stdout.isatty():
            handlers.append(logging.StreamHandler(sys.stdout))
        try:
            log_dir = os.path.dirname(self.LOG_FILE)
            if not os.path.exists(log_dir):
                os.makedirs(log_dir, exist_ok=True)
            handlers.append(logging.FileHandler(self.LOG_FILE, encoding='utf-8'))
        except (OSError, PermissionError):
            try:
                local_log = os.path.join(os.getcwd(), "gostorm-debug.log")
                handlers.append(logging.FileHandler(local_log, encoding='utf-8'))
            except Exception:
                pass
        # NEW: livello log da ENV (default INFO)
        level_name = os.getenv("LOG_LEVEL", "INFO").upper()
        level = getattr(logging, level_name, logging.INFO)
        logging.basicConfig(level=level,
                            format='[%(asctime)s] [%(levelname)s] %(message)s',
                            datefmt='%Y-%m-%d %H:%M:%S',
                            handlers=handlers)
        self.logger = logging.getLogger(__name__)

    def log(self, level: str, message: str):
        if level == "INFO":
            self.logger.info(message)
        elif level == "ERROR":
            self.logger.error(message)
        elif level == "WARN":
            self.logger.warning(message)
        elif level == "DEBUG":
            self.logger.debug(message)
        elif level == "SUCCESS":
            self.logger.info(f"\033[0;32m{message}\033[0m")

    def log_error(self, message: str):
        self.log("ERROR", f"\033[0;31m{message}\033[0m")

    def log_success(self, message: str):
        self.log("SUCCESS", message)

    def _fetch_trackers(self) -> list:
        """Fetch public trackers from GitHub, with caching"""
        now = time.time()
        if self._trackers_cache and (now - self._trackers_cache_time) < self.TRACKERS_CACHE_TTL:
            return self._trackers_cache

        try:
            resp = self.session.get(self.TRACKERS_URL, timeout=10)
            if resp.status_code == 200:
                # Parse trackers (one per line, skip empty lines)
                trackers = [line.strip() for line in resp.text.split('\n') if line.strip()]
                if trackers:
                    self._trackers_cache = trackers[:20]  # Limit to 20 best trackers
                    self._trackers_cache_time = now
                    self.log("DEBUG", f"Fetched {len(self._trackers_cache)} trackers from GitHub")
                    return self._trackers_cache
        except Exception as e:
            self.log("DEBUG", f"Failed to fetch trackers: {e}")

        # Fallback to hardcoded trackers if fetch fails
        fallback = [
            "udp://tracker.opentrackr.org:1337/announce",
            "udp://open.stealth.si:80/announce",
            "udp://tracker.torrent.eu.org:451/announce",
            "udp://exodus.desync.com:6969/announce",
            "udp://tracker.openbittorrent.com:6969/announce"
        ]
        if not self._trackers_cache:
            self._trackers_cache = fallback
            self._trackers_cache_time = now
        return self._trackers_cache

    def _build_magnet(self, info_hash: str, name: str = "") -> str:
        """Build magnet URL with trackers for faster resolution"""
        from urllib.parse import quote
        magnet = f"magnet:?xt=urn:btih:{info_hash}"
        if name:
            magnet += f"&dn={quote(name)}"
        trackers = self._fetch_trackers()
        for tr in trackers:
            magnet += f"&tr={quote(tr)}"
        return magnet

    def safe_curl(self, url: str, method: str = "GET", data: Dict = None, params: Dict = None, headers: Dict = None, timeout: int = 30) -> Optional[requests.Response]:
        """
        Enhanced safe_curl with rate limiting, exponential backoff + jitter, and improved error handling
        """
        
        from urllib.parse import urlparse

        # Rate limiting: enforce minimum interval between API calls (solo per host esterni)
        host = (urlparse(url).hostname or "").lower()
        is_local = host in ("localhost", "127.0.0.1") or host.startswith("192.168.") or host.startswith("10.") or host.startswith("172.")
        if not is_local:
            current_time = time.time()
            time_since_last_call = current_time - self._last_api_call_time
            if time_since_last_call < self.MIN_API_INTERVAL_SEC:
                sleep_time = self.MIN_API_INTERVAL_SEC - time_since_last_call
                time.sleep(sleep_time)
            self._last_api_call_time = time.time()
        else:
            # Per GoStorm in LAN non forziamo il gap minimo
            self._last_api_call_time = time.time()
        
        backoff = self.INITIAL_BACKOFF_SEC
        for attempt in range(self.MAX_RETRIES):
            try:
                if method == "POST":
                    response = self.session.post(url, json=data, headers=headers, timeout=timeout)
                else:
                    response = self.session.get(url, params=params, headers=headers, timeout=timeout)
                
                if response.status_code == 200:
                    return response
                elif response.status_code == 429:
                    self.log("WARN", f"Rate limited (429) for {url}, attempt {attempt + 1}/{self.MAX_RETRIES}")
                else:
                    self.log("WARN", f"HTTP {response.status_code} for {url}")
                    
            except requests.exceptions.ConnectionError as e:
                error_str = str(e).lower()
                if "connection refused" in error_str or "connection reset" in error_str:
                    self.log("WARN", f"GoStorm connection issue (attempt {attempt + 1}/{self.MAX_RETRIES}): {e}")
                else:
                    self.log("WARN", f"Connection error (attempt {attempt + 1}/{self.MAX_RETRIES}): {e}")
            except requests.exceptions.RequestException as e:
                self.log("WARN", f"HTTP request failed (attempt {attempt + 1}/{self.MAX_RETRIES}): {e}")
            
            if attempt < self.MAX_RETRIES - 1:
                # Exponential backoff with jitter
                jitter = random.uniform(0, backoff * 0.1)
                sleep_time = min(backoff + jitter, self.MAX_BACKOFF_SEC)
                self.log("DEBUG", f"Retrying in {sleep_time:.1f}s...")
                time.sleep(sleep_time)
                backoff = min(backoff * 2, self.MAX_BACKOFF_SEC)
                
        return None

    def _invalidate_torrents_cache(self):
        """Invalida cache torrents per forzare refresh al prossimo accesso"""
        self._torrents_cache = None
        self._torrents_cache_time = 0
        self.log("DEBUG", "Invalidated torrents cache")

    def _cached_torrents_list(self, force: bool = False) -> List[Dict]:
        """
        Cached torrents list with TTL to reduce API spam
        Supporta force=True per forzare refresh anche con cache valida
        """
        current_time = time.time()
        if (not force and self._torrents_cache is not None and 
            (current_time - self._torrents_cache_time) < self.CACHE_TTL_SEC):
            self.log("DEBUG", f"Using cached torrents list ({len(self._torrents_cache)} items)")
            return self._torrents_cache
        
        self.log("DEBUG", "Refreshing torrents list cache...")
        response = self.safe_curl(f"{self.TORRSERVER_URL}/torrents", method="POST", 
                                  data={"action": "list"})
        if response:
            try:
                self._torrents_cache = response.json()
                self._torrents_cache_time = current_time
                self.log("DEBUG", f"Cached {len(self._torrents_cache)} torrents")
                return self._torrents_cache
            except (json.JSONDecodeError, KeyError):
                pass
        
        self.log("WARN", "Failed to refresh torrents cache, returning empty list")
        return []

    def _prune_unreferenced_torrents(self):
        """
        Prune torrent non referenziati quando superiamo la soglia.
        Conserva:
          - quelli referenziati da almeno un .mkv (hash letto dal contenuto)
          - quelli più "recenti" (usa campi 'time' o 'added')
        Rimuove solo se età >= PRUNE_MIN_AGE_MIN.
        """
        try:
            min_age_min = int(os.getenv("PRUNE_MIN_AGE_MIN", "30"))
        except ValueError:
            min_age_min = 30
        min_age_sec = min_age_min * 60
        torrents = self._cached_torrents_list()
        total = len(torrents)
        # No artificial limit - proceed with pruning unreferenced torrents
        
        # Build/riusa cache hash -> file (.mkv virtuali). check_existing_mkv_for_hash() inizializza _hash_cache con un unico scan.
        try:
            _ = self.check_existing_mkv_for_hash("0" * 40)  # trigger build se non presente
        except Exception:
            pass
        referenced = set(getattr(self, "_hash_cache", {}).keys())
        
        # Candidati rimovibili
        now = time.time()
        removable = []
        for t in torrents:
            h = (t.get("hash") or "").lower()
            if not h or h in referenced:
                continue

            # Protect TV library torrents from pruning
            if self._is_tv_library_protected_torrent(h):
                self.log("DEBUG", f"Skipping pruning of protected TV library torrent: {h[:8]}...")
                continue
            raw_ts = t.get("time") or t.get("added") or t.get("timestamp") or 0
            try:
                ts = int(raw_ts)
            except Exception:
                ts = 0
            age = now - ts if ts else 0
            if age < min_age_sec:
                continue
            size = t.get("length") or t.get("size") or 0
            removable.append((age, size, h, ts))
        
        if not removable:
            self.log("INFO", f"Prune skip: total={total} referenced={len(referenced)} removable={len(removable)}")
            return

        # Ordina per età desc poi size desc
        removable.sort(key=lambda x: (-x[0], -x[1]))
        # Remove ALL unreferenced torrents (not just excess)
        to_remove = removable
        
        self.log("WARN", f"Pruning {len(to_remove)} unreferenced torrents (active={total} referenced={len(referenced)})")
        removed = 0
        for age, size, h, ts in to_remove:
            if self.remove_torrent_from_server(h):
                removed += 1
        
        self.log("INFO", f"Prune complete: removed={removed}, kept={total - removed}")
        
        # PATCH: Invalidate cache after pruning usando metodo coerente
        self._invalidate_torrents_cache()

    def extract_size_from_title(self, title: str) -> str:
        """
        Exact replica of bash extract_size_from_title() function
        Extract file size from torrent title (e.g., "15GB", "8.5GB", "1.2TB")
        Returns size in bytes as string, or empty string if no valid size found
        """
        import re
        
        # Match patterns like "15GB", "8.5GB", "1.2TB", etc.
        match = re.search(r'([0-9]+\.?[0-9]*)\s*(GB|TB)', title, re.IGNORECASE)
        
        if match:
            size_num = float(match.group(1))
            size_unit = match.group(2).upper()
            
            # Convert to bytes
            if size_unit == "GB":
                size_bytes = int(size_num * self.BYTES_PER_GB)
            elif size_unit == "TB":
                size_bytes = int(size_num * 1099511627776)  # 1024^4
            else:
                return ""
            
            # Validate result is reasonable (between 100MB and 100GB for movies/TV)
            if (self.FILE_MIN_VALIDATION <= size_bytes <= self.FILE_MAX_VALIDATION):
                return str(size_bytes)
        
        # Fallback: return empty if no valid size found
        return ""

    def _get_full_torrent_info(self, hash_suffix: str) -> dict:
        """
        Get full torrent information from GoStorm using hash suffix.
        V150-Optimization: Uses internal cache instead of direct API call.
        """
        try:
            # Use cached list to avoid "List Bomb" bug (DDoS on GoStorm)
            torrents = self._cached_torrents_list()
            if not isinstance(torrents, list):
                return {}
            
            # Find torrent by hash suffix match
            for torrent in torrents:
                full_hash = torrent.get('hash', '')
                if full_hash.endswith(hash_suffix):
                    return torrent
            
        except Exception as e:
            self.log("DEBUG", f"Error getting full torrent info for hash {hash_suffix}: {e}")
        
        return {}

    def check_existing_mkv_for_hash(self, hash_value: str) -> str:
        """
        Indexed hash lookup (costruzione on-demand semplice) - eliminates O(N) scans
        Now scans ONLY MOVIES_DIR to avoid touching TV files in movies-only script.
        """
        if not hasattr(self, "_hash_cache"):
            self._hash_cache = {}
            for base in (self.MOVIES_DIR,):
                if not os.path.isdir(base):
                    continue
                for root, _, files in os.walk(base):
                    for f in files:
                        if not f.lower().endswith('.mkv'):
                            continue
                        fp = os.path.join(root, f)
                        try:
                            with open(fp, 'r') as fh:
                                first = fh.readline().strip()
                            m = re.search(r'link=([a-f0-9]{40})', first, re.IGNORECASE)
                            if m:
                                self._hash_cache[m.group(1).lower()] = fp
                        except Exception:
                            continue
        hv = hash_value.lower()
        return self._hash_cache.get(hv, "")

    def create_mkv_with_metadata(self, mkv_file: str, stream_url: str, file_size_bytes: str = "", magnet_url: str = "", imdb_id: str = "") -> bool:
        """
        Create virtual .mkv file with embedded size metadata + magnet for rehydration
        Enhanced version with magnet persistence for automatic torrent recovery
        """
        try:
            # Create directory if it doesn't exist
            os.makedirs(os.path.dirname(mkv_file), exist_ok=True)
            
            with open(mkv_file, 'w') as f:
                f.write(stream_url + '\n')
                if file_size_bytes and file_size_bytes.isdigit() and int(file_size_bytes) > 0:
                    f.write(file_size_bytes + '\n')
                    size_gb = round(int(file_size_bytes) / self.BYTES_PER_GB, 1)
                    self.log("DEBUG", f"Created .mkv with actual size: {size_gb}GB for {os.path.basename(mkv_file)}")
                else:
                    f.write('0\n')
                    self.log("DEBUG", f"Created .mkv with default size for {os.path.basename(mkv_file)}")
                # Third line: magnet for rehydration
                if magnet_url:
                    f.write(magnet_url + '\n')
                else:
                    f.write('\n')
                
                # Fourth line: IMDB ID for auto-rescue logic
                if imdb_id:
                    f.write(imdb_id + '\n')
            
            # Update hash cache if initialized
            if hasattr(self, "_hash_cache"):
                try:
                    with open(mkv_file, 'r') as fh:
                        first = fh.readline().strip()
                    mh = re.search(r'link=([a-f0-9]{40})', first, re.IGNORECASE)
                    if mh:
                        self._hash_cache[mh.group(1).lower()] = mkv_file
                except Exception:
                    pass
            
            return True
            
        except IOError as e:
            self.log("ERROR", f"Failed to create mkv file {mkv_file}: {e}")
            return False

    def should_replace_mkv(self, mkv_file: str, new_title: str, new_size: str = "", new_name: str = "") -> bool:
        """
        Quality-based replacement decision using scoring system
        Decide if an existing .mkv should be replaced with a better quality one
        Returns True -> replace, False -> keep existing
        """
        # If file doesn't exist, we can obviously create
        if not os.path.exists(mkv_file):
            return True
        
        try:
            # Read first (url) and second line (size) of existing file
            with open(mkv_file, 'r') as f:
                lines = f.readlines()
                old_url = lines[0].strip() if len(lines) > 0 else ""
                old_size = lines[1].strip() if len(lines) > 1 else ""
        except (IOError, IndexError):
            return True  # If we can't read the file, replace it
        
        # Calculate quality scores for comparison
        existing_filename = os.path.basename(mkv_file)
        
        # FIX: Apply same logic as movies - get torrent title from hash for accurate scoring
        existing_hash_match = re.search(r'_([a-f0-9]{8})\.mkv$', existing_filename)
        if existing_hash_match:
            existing_hash = existing_hash_match.group(1)
            # Get full hash and torrent title for accurate scoring (like movies)
            existing_torrent_info = self._get_full_torrent_info(existing_hash)
            if existing_torrent_info:
                existing_title = existing_torrent_info.get('title', '')
                existing_score = self.calculate_quality_score(existing_title, seeders=0)
                self.log("DEBUG", f"TV existing score from torrent title: {existing_score} (title: {existing_title[:60]}...)")
            else:
                # Fallback to filename scoring
                existing_score = self.calculate_quality_score(old_url + " " + existing_filename, existing_filename)
                self.log("DEBUG", f"TV existing score from filename (fallback): {existing_score}")
        else:
            # No hash found, use old method
            existing_score = self.calculate_quality_score(old_url + " " + existing_filename, existing_filename)
            self.log("DEBUG", f"TV existing score from URL+filename: {existing_score}")
        
        # Include campo name di Torrentio per tags HDR/DV/4K
        new_score = self.calculate_quality_score(new_title + " " + new_name)
        
        # Exclude 720p content (score = 0)
        if new_score == 0:
            self.log("DEBUG", f"Skip 720p content: {new_title}")
            return False
        
        # Primary comparison: quality score
        if new_score > existing_score:
            self.log("INFO", f"Upgrading {os.path.basename(mkv_file)}: score {existing_score} → {new_score}")
            return True
        
        # Tiebreaker: if same quality score, check size upgrade (≥110%)
        if new_score == existing_score:
            if new_size and old_size and old_size.isdigit() and new_size.isdigit():
                old_size_int = int(old_size)
                new_size_int = int(new_size)
                threshold = old_size_int * 110 // 100
                
                if new_size_int >= threshold:
                    old_gb = round(old_size_int / self.BYTES_PER_GB, 1)
                    new_gb = round(new_size_int / self.BYTES_PER_GB, 1)
                    percent = (new_size_int - old_size_int) * 100 // old_size_int
                    self.log("INFO", f"Upgrading {os.path.basename(mkv_file)}: {old_gb}GB → {new_gb}GB (+{percent}%)")
                    return True
            else:
                self.log("DEBUG", f"Size upgrade rule skipped (size missing or invalid) for {os.path.basename(mkv_file)}")
        
        # Keep existing if new is not better
        if new_score < existing_score:
            self.log("INFO", f"Keep existing {os.path.basename(mkv_file)}: score {existing_score} > {new_score} (downgrade blocked)")
        else:
            self.log("INFO", f"Keep existing {os.path.basename(mkv_file)}: same score, no size upgrade")
        
        return False

    def should_replace_tv_series(self, existing_torrent: dict, new_torrent: dict, existing_dir: str) -> bool:
        """
        Quality-based TV series replacement decision using scoring system
        Compare two TV series fullpack torrents to determine if new should replace existing
        Returns True -> replace existing, False -> keep existing
        """
        existing_title = existing_torrent.get("title", "")
        new_title = new_torrent.get("title", "")
        
        self.log("DEBUG", f"TV series comparison: '{existing_title}' vs '{new_title}'")
        
        # Calculate quality scores for comparison
        existing_score = self.calculate_quality_score(existing_title)
        new_score = self.calculate_quality_score(new_title)
        
        # Exclude 720p content (score = 0)
        if new_score == 0:
            self.log("DEBUG", f"Skip 720p TV series: {new_title}")
            return False
        
        # Primary comparison: quality score
        if new_score > existing_score:
            self.log("INFO", f"TV series upgrade: {os.path.basename(existing_dir)} → score {existing_score} to {new_score}")
            return True
        
        # Tiebreaker: if same quality score, check size upgrade (≥110%)
        if new_score == existing_score:
            existing_size = existing_torrent.get("size", 0)
            new_size = new_torrent.get("size", 0)
            
            if existing_size and new_size and existing_size > 0:
                threshold = existing_size * 110 // 100
                if new_size >= threshold:
                    existing_gb = round(existing_size / self.BYTES_PER_GB, 1)
                    new_gb = round(new_size / self.BYTES_PER_GB, 1)
                    percent = (new_size - existing_size) * 100 // existing_size
                    self.log("INFO", f"TV series upgrade: {os.path.basename(existing_dir)} → {existing_gb}GB to {new_gb}GB (+{percent}%)")
                    return True
        
        # Keep existing if new is not better
        if new_score < existing_score:
            self.log("INFO", f"TV series keep existing: {os.path.basename(existing_dir)} → score {existing_score} > {new_score} (downgrade blocked)")
        else:
            self.log("INFO", f"TV series keep existing: {os.path.basename(existing_dir)} → same score, no size upgrade")
        
        return False

    def should_replace_tv_fullpack(self, existing_show_dir: str, new_stream_title: str, new_stream_name: str = "") -> bool:
        """
        Decide if existing TV fullpack should be replaced with a better quality one
        Checks all existing episodes in the show directory and compares quality scores
        Returns True -> replace entire show, False -> keep existing
        """
        if not os.path.exists(existing_show_dir):
            return True  # No existing show, can create
        
        # Get all existing season directories
        season_dirs = []
        for item in os.listdir(existing_show_dir):
            season_path = os.path.join(existing_show_dir, item)
            if os.path.isdir(season_path) and item.startswith('Season'):
                season_dirs.append(season_path)
        
        if not season_dirs:
            return True  # No existing seasons, can create
        
        # Sample a few existing episodes to get representative quality score
        existing_scores = []
        existing_files_checked = 0
        
        for season_dir in season_dirs[:2]:  # Check max 2 seasons
            for episode_file in os.listdir(season_dir):
                if not episode_file.lower().endswith('.mkv'):
                    continue
                
                episode_path = os.path.join(season_dir, episode_file)
                try:
                    with open(episode_path, 'r') as f:
                        old_url = f.readline().strip()
                    
                    # Calculate existing score using episode filename tokens
                    existing_score = self.calculate_quality_score(old_url + " " + episode_file)
                    existing_scores.append(existing_score)
                    existing_files_checked += 1
                    
                    if existing_files_checked >= 3:  # Sample 3 episodes max
                        break
                        
                except (IOError, IndexError):
                    continue
            
            if existing_files_checked >= 3:
                break
        
        if not existing_scores:
            return True  # Can't read existing files, replace
        
        # Get average existing quality score
        avg_existing_score = sum(existing_scores) / len(existing_scores)
        
        # Calculate new stream quality score (include Torrentio name field)
        new_score = self.calculate_quality_score(new_stream_title + " " + new_stream_name)
        
        # Exclude 720p content
        if new_score == 0:
            self.log("DEBUG", f"Skip 720p TV fullpack: {new_stream_title}")
            return False
        
        # Primary comparison: quality score (require significant improvement for fullpack replacement)
        score_improvement_threshold = 1.2  # Require 20% score improvement
        if new_score > (avg_existing_score * score_improvement_threshold):
            self.log("INFO", f"TV fullpack upgrade: {os.path.basename(existing_show_dir)} → score {avg_existing_score:.0f} to {new_score} ({((new_score/avg_existing_score-1)*100):.0f}% improvement)")
            return True
        
        # Log why we're keeping existing
        if new_score <= avg_existing_score:
            self.log("INFO", f"TV fullpack keep existing: {os.path.basename(existing_show_dir)} → score {new_score} ≤ {avg_existing_score:.0f} (no improvement)")
        else:
            self.log("INFO", f"TV fullpack keep existing: {os.path.basename(existing_show_dir)} → score {new_score} vs {avg_existing_score:.0f} (insufficient improvement <20%)")
        
        return False

    def extract_hash_from_magnet(self, magnet_url: str) -> str:
        """
        Extract hash from magnet URL in standardized format
        Also handles GoStorm stream URLs
        """
        import re
        
        # First try GoStorm stream URL format: link=40char_hash
        match = re.search(r'link=([a-fA-F0-9]{40})', magnet_url)
        if match:
            return match.group(1).lower()
        
        # Fallback: Extract hash from magnet URL using regex
        match = re.search(r'xt=urn:btih:([a-fA-F0-9]{32,40})', magnet_url)
        if match:
            hash_value = match.group(1).lower()
            # Standardize to 40 characters if it's 32
            if len(hash_value) == 32:
                # Convert base32 to hex if needed - for now just return as-is
                return hash_value
            return hash_value
        return ""

    def calculate_quality_score(self, title: str, filename: str = "", seeders: int = 0, debug: bool = False) -> int:
        """
        UNIFIED quality scoring system for stream selection AND upgrade logic
        Prevents inconsistencies between initial selection and upgrade decisions
        
        Args:
            title: Content title/description to analyze
            filename: Optional filename to analyze (for existing files)
            seeders: Seeder count for bonus calculation (0 = no bonus)
            debug: If True, return tuple (score, breakdown_dict)
        
        Returns:
            Quality score (higher = better quality) or tuple if debug=True
            0 = exclude (720p or invalid)
        """
        import re
        
        # Combine title and filename for analysis
        content = f"{title} {filename}".lower()
        
        # Base resolution score - MASSIVE 4K PRIORITY
        base_score = 0
        resolution = "unknown"
        if re.search(r'2160p|4[kK]|uhd', content, re.IGNORECASE):
            base_score = 1000  # 4K base - MASSIVE PRIORITY
            resolution = "4K"
        elif re.search(r'1080p', content, re.IGNORECASE):
            base_score = 200   # 1080p base
            resolution = "1080p"
        else:
            # FIX: Se non troviamo tag 4K o 1080p, scartiamo (no assumptions)
            if debug:
                return (0, {"resolution": "low_or_missing", "excluded": True})
            return 0  # Exclude SD/480p/720p/Unknown
        
        # HDR/DoVi bonus - DV is SUPERIOR to HDR/HDR10/HDR10+ (user requirement Dec 31 2025)
        hdr_bonus = 0
        hdr_type = "none"
        if re.search(r'(?:^|[._\s-])dv(?:$|[._\s-])|\bdv\b|dovi|dolby.?vision', content, re.IGNORECASE):
            hdr_bonus = 100  # DV is highest HDR format
            hdr_type = "DV"
        elif re.search(r'hdr10\+?|hdr', content, re.IGNORECASE):
            hdr_bonus = 60   # HDR/HDR10/HDR10+ are equivalent, inferior to DV (-40 vs DV)
            hdr_type = "HDR"
        
        # Audio bonus - UNIFIED scoring with stereo penalty
        audio_bonus = 0
        audio_type = "unknown"
        if re.search(r'atmos', content, re.IGNORECASE):
            audio_bonus = 50   # ATMOS bonus (consistent with upgrade system)
            audio_type = "ATMOS"
        elif re.search(r'5\.1|ddp?5\.?1|dd5\.?1|eac3|dts(?!.*hd)', content, re.IGNORECASE):
            audio_bonus = 25   # 5.1/DTS bonus  
            audio_type = "5.1/DTS"
        elif re.search(r'stereo|2\.0|\b2ch\b', content, re.IGNORECASE):
            audio_bonus = -50  # Stereo penalty (from original filter system)
            audio_type = "stereo"
        else:
            audio_bonus = 5    # Default audio bonus
            audio_type = "default"
        
        # Source quality bonus - Only REMUX gets bonus (Dec 31 2025)
        # All other sources (Blu-ray, WEB-DL, streaming services) equivalent at 0
        source_bonus = 0
        source_type = "other"
        if re.search(r'remux', content, re.IGNORECASE):
            source_bonus = 30  # REMUX is the only premium source
            source_type = "REMUX"
        
        # Lingua Bonus (ITA esplicito) - solo traccia italiana confermata
        language_bonus = 0
        language_type = "en"
        if re.search(r'\bita\b|🇮🇹', content, re.IGNORECASE):
            language_bonus = 60
            language_type = "ita"

        # Seeder bonus - linear, capped at 50 (never bridges resolution gap of 800)
        seeder_bonus = min(seeders, 50)
        
        total_score = base_score + hdr_bonus + audio_bonus + source_bonus + seeder_bonus + language_bonus
        
        # Return breakdown for debugging if requested
        if debug:
            breakdown = {
                "resolution": f"{resolution} (+{base_score})",
                "hdr": f"{hdr_type} (+{hdr_bonus})",
                "audio": f"{audio_type} ({'+' if audio_bonus >= 0 else ''}{audio_bonus})",
                "source": f"{source_type} (+{source_bonus})",
                "language": f"{language_type} (+{language_bonus})",
                "seeders": f"{seeders} (+{seeder_bonus})",
                "total": total_score
            }
            return (total_score, breakdown)
        
        return total_score

    def extract_quality_filename(self, stream_title: str, torrent_hash: str) -> str:
        """
        Exact replica of bash extract_quality_filename() function
        Extract filename with quality markers preserved and hash suffix for uniqueness
        """
        import re
        
        # Remove new-line/carriage-return to avoid multi-line filenames
        filename = stream_title.replace('\r', ' ').replace('\n', ' ')
        
        # Remove common extensions and group tags
        filename = re.sub(r'\.(mkv|mp4|avi)$', '', filename, flags=re.IGNORECASE)
        filename = re.sub(r'-[A-Za-z0-9]+$', '', filename)  # Remove group tags like -BTM, -NOGRP
        
        # DEBUG: Log input to understand extraction issues
        self.log("DEBUG", f"extract_quality_filename INPUT: '{stream_title}'")
        
        # Extract and preserve quality markers
        year_match = re.search(r'(19|20)\d{2}', filename)
        year = year_match.group(0) if year_match else ""
        
        resolution_match = re.search(r'(2160p|1080p|720p|4k|uhd)', filename, re.IGNORECASE)
        resolution = resolution_match.group(0) if resolution_match else ""
        
        hdr_match = re.search(r'(dv|dolby.vision|hdr10\+?|hdr)', filename, re.IGNORECASE)
        hdr = hdr_match.group(0).replace('HDR10+', 'HDR10+') if hdr_match else ""
        
        audio_match = re.search(r'(atmos|ddp?5\.?1|dd5\.?1|aac)', filename, re.IGNORECASE)
        audio = audio_match.group(0) if audio_match else ""
        
        source_match = re.search(r'(web[-_.]?dl|webrip|bluray|bdrip)', filename, re.IGNORECASE)
        source = source_match.group(0) if source_match else ""
        
        # Clean base title (remove quality markers for cleaner base)
        base_title = re.sub(r'[._-]?(19|20)\d{2}.*$', '', filename)[:60]
        base_title = re.sub(r'[^a-zA-Z0-9._-]', '_', base_title)
        
        # Build quality filename with preserved markers
        quality_filename = base_title
        if year:
            quality_filename += f"_{year}"
        if resolution:
            quality_filename += f"_{resolution}"
        if hdr:
            quality_filename += f"_{hdr}"
        if source:
            quality_filename += f"_{source}"
        if audio:
            quality_filename += f"_{audio}"
        
        # Final cleanup and length limit
        quality_filename = quality_filename.replace('\r', '_').replace('\n', '_')
        quality_filename = re.sub(r'[^a-zA-Z0-9._-]', '_', quality_filename)
        quality_filename = re.sub(r'_+', '_', quality_filename)  # Remove multiple underscores
        quality_filename = quality_filename.strip('_')
        quality_filename = quality_filename[:90]  # Length limit
        
        # Add torrent hash suffix for uniqueness (last 8 chars)
        if torrent_hash and len(torrent_hash) >= 8:
            hash_suffix = torrent_hash[-8:]  # Last 8 characters
            quality_filename += f"_{hash_suffix}"
        
        # DEBUG: Log extraction results
        self.log("DEBUG", f"extract_quality_filename RESULT: '{quality_filename}' (year:{year}, res:{resolution}, hdr:{hdr}, src:{source}, aud:{audio})")
        
        return quality_filename

    def build_original_pattern_filename(self, original_filename: str, torrent_hash: str) -> str:
        """Preserve original filename pattern from torrent file (bash-like behavior)"""
        name = original_filename.strip()
        if name.lower().endswith(('.mkv','.mp4','.avi')):
            name = name[:name.rfind('.')]
        name = name.replace(' ', '.')
        name = re.sub(r'[^A-Za-z0-9._\-]+', '.', name)
        name = re.sub(r'\.{2,}', '.', name).strip('.')
        if len(name) > 160:
            name = name[:160].rstrip('._-')
        if torrent_hash and len(torrent_hash) >= 8 and not re.search(r'_[a-f0-9]{8}$', name, re.IGNORECASE):
            name = f"{name}_{torrent_hash[-8:]}"
        return name

    def create_bash_style_mkv(self, title: str, hash_value: str, stream_title: str, expect_4k: bool, is_tv: bool = False, season_dir: str = None, file_id: str = "1") -> bool:
        """Create .mkv file using bash-style approach (immediate, index=1, estimated size)"""
        try:
            # Extract size from stream title (bash approach)
            size_match = re.search(r'💾\s*([0-9]+\.?[0-9]*)\s*(GB|MB)', stream_title, re.IGNORECASE)
            if not size_match:
                size_match = re.search(r'\b([0-9]+\.?[0-9]*)\s*(GB|MB)\b', stream_title, re.IGNORECASE)
            
            if size_match:
                size_num = float(size_match.group(1))
                unit = size_match.group(2).upper()
                file_size_bytes = str(int(size_num * (1024**3 if unit == "GB" else 1024**2)))
            else:
                # Fallback size estimates based on quality
                if expect_4k:
                    file_size_bytes = str(15 * 1024**3)  # 15GB default for 4K
                else:
                    file_size_bytes = str(8 * 1024**3)   # 8GB default for 1080p

            # Use stream title to derive filename (bash approach)
            clean_filename = self.build_original_pattern_filename(stream_title, hash_value)
            
            if is_tv and season_dir:
                mkv_file = os.path.join(season_dir, f"{clean_filename}.mkv")
                os.makedirs(season_dir, exist_ok=True)
            else:
                mkv_file = os.path.join(self.MOVIES_DIR, f"{clean_filename}.mkv")
            
            # Create bash-style URL - both movies and TV require index parameter
            if is_tv:
                stream_url = f"{self.TORRSERVER_URL}/stream?link={hash_value}&index={file_id}&play"
            else:
                # Movies: INDEX OBBLIGATORIO - default to 1 if not specified
                stream_url = f"{self.TORRSERVER_URL}/stream?link={hash_value}&index={file_id or '1'}&play"
            
            # Create the file - generate magnet URL
            magnet_url = self._build_magnet(hash_value)
            if self.create_mkv_with_metadata(mkv_file, stream_url, file_size_bytes, magnet_url):
                self.log_success(f"Created bash-style file: {os.path.basename(mkv_file)}")
                return True
            else:
                return False
                
        except Exception as e:
            self.log("ERROR", f"Failed to create bash-style mkv: {e}")
            return False

    def remove_existing_torrent_if_any(self, new_mkv: str) -> bool:
        """Safe cleanup: rimuove solo varianti realmente correlate (stesso base + hash diverso)."""
        if not os.path.isfile(new_mkv):
            return False
        base_new = os.path.basename(new_mkv)
        # Core senza markers qualità / anno / hash
        core_new = re.sub(r'_[a-f0-9]{8}\.mkv$', '', base_new, flags=re.IGNORECASE)
        # Fix: Handle all separators (. _ -) for years and quality markers
        core_new = re.sub(r'[._-](19|20)\d{2}.*', '', core_new)
        core_new = re.sub(r'[._-](2160p|1080p|720p|4[Kk]|UHD).*', '', core_new, flags=re.IGNORECASE)
        core_new = re.sub(r'[._-](DV|HDR10\+?|HDR|Atmos).*', '', core_new, flags=re.IGNORECASE)
        # Normalize all separators to spaces for comparison
        core_new = re.sub(r'[._-]+', ' ', core_new).strip()
        
        # Debug logging for core extraction
        self.log("DEBUG", f"Cleanup check for: {base_new} -> core: '{core_new}'")
        dir_path = os.path.dirname(new_mkv)
        removed = 0
        try:
            for f in os.listdir(dir_path):
                if not f.lower().endswith('.mkv') or f == base_new:
                    continue
                core_old = re.sub(r'_[a-f0-9]{8}\.mkv$', '', f, flags=re.IGNORECASE)
                # Fix: Handle all separators (. _ -) for years and quality markers
                core_old = re.sub(r'[._-](19|20)\d{2}.*', '', core_old)
                core_old = re.sub(r'[._-](2160p|1080p|720p|4[Kk]|UHD).*', '', core_old, flags=re.IGNORECASE)
                core_old = re.sub(r'[._-](DV|HDR10\+?|HDR|Atmos).*', '', core_old, flags=re.IGNORECASE)
                # Normalize all separators to spaces for comparison
                core_old = re.sub(r'[._-]+', ' ', core_old).strip()
                
                # Debug logging for comparison
                self.log("DEBUG", f"  Comparing with: {f} -> core: '{core_old}' (match: {core_old.lower() == core_new.lower()})")
                
                if core_old.lower() != core_new.lower():
                    continue
                old_path = os.path.join(dir_path, f)
                # Estrarre hash vecchio
                old_hash = ""
                try:
                    with open(old_path, 'r') as fh:
                        first = fh.readline().strip()
                    m = re.search(r'link=([a-f0-9]{40})', first, re.IGNORECASE)
                    if m:
                        old_hash = m.group(1).lower()
                except Exception:
                    pass
                # Rimuovi torrent se hash estratto
                if old_hash:
                    # Non eliminare se coincide con hash nuovo
                    try:
                        with open(new_mkv, 'r') as nf:
                            first_new = nf.readline().strip()
                        mnew = re.search(r'link=([a-f0-9]{40})', first_new, re.IGNORECASE)
                        new_hash = mnew.group(1).lower() if mnew else ""
                    except Exception:
                        new_hash = ""
                    if new_hash and new_hash == old_hash:
                        continue  # stessa risorsa, non rimuovere
                    # PATCH: usa remove_torrent_from_server per invalidare cache + housekeeping
                    self.remove_torrent_from_server(old_hash)
                try:
                    os.remove(old_path)
                    removed += 1
                    self.log_success(f"Removed old variant: {f}")
                    # PRUNE dal core index se file rimosso durante cleanup
                    core_removed = self._core_key(f, is_tv=('Season.' in dir_path))
                    self._prune_core_entry(core_removed, is_tv=('Season.' in dir_path))
                    # Update hash cache after removal
                    if hasattr(self, "_hash_cache") and old_hash:
                        self._hash_cache.pop(old_hash, None)
                except OSError:
                    self.log("WARN", f"Failed to remove old variant: {f}")
        except OSError:
            return removed > 0
        if removed:
            self.log("INFO", f"Variant cleanup complete: {removed} removed")
        return removed > 0

    # ================== NEW: FULLPACK CACHE HELPERS ==================
    def _load_fullpack_cache(self) -> dict:
        """Load processed fullpack cache from disk (show+season+hash)."""
        try:
            if os.path.isfile(self.FULLPACK_CACHE_FILE):
                with open(self.FULLPACK_CACHE_FILE, 'r') as f:
                    data = json.load(f)
                    if isinstance(data, dict):
                        return data
        except Exception:
            pass
        return {}

    def _save_fullpack_cache(self):
        """Persist fullpack cache to disk if modified (atomic)."""
        try:
            tmp = self.FULLPACK_CACHE_FILE + '.tmp'
            with open(tmp, 'w') as f:
                json.dump(self._fullpack_cache, f)
            os.replace(tmp, self.FULLPACK_CACHE_FILE)
        except Exception as e:
            self.log("WARN", f"Unable to save fullpack cache: {e}")

    def _fullpack_key(self, show_name: str, season_num: int, info_hash: str) -> str:
        return f"{show_name.lower()}_S{season_num:02d}_{info_hash.lower()}"

    # ================== TV LIBRARY PERSISTENCE CACHE ==================
    def _load_tv_library_cache(self) -> dict:
        """Load TV series library cache from disk to prevent systematic deletion."""
        try:
            if os.path.isfile(self.TV_LIBRARY_CACHE_FILE):
                with open(self.TV_LIBRARY_CACHE_FILE, 'r') as f:
                    data = json.load(f)
                    if isinstance(data, dict):
                        return data
        except Exception:
            pass
        return {}

    def _save_tv_library_cache(self):
        """Persist TV library cache to disk if modified (atomic)."""
        try:
            tmp = self.TV_LIBRARY_CACHE_FILE + '.tmp'
            with open(tmp, 'w') as f:
                json.dump(self._tv_library_cache, f, indent=2)
            os.replace(tmp, self.TV_LIBRARY_CACHE_FILE)
        except Exception as e:
            self.log("WARN", f"Unable to save TV library cache: {e}")

    def _add_to_tv_library_cache(self, tmdb_id: int, imdb_id: str, title: str):
        """Add a TV show to the library persistence cache."""
        if not self.TV_PRESERVE_LIBRARY:
            return

        cache_key = f"{tmdb_id}"
        now = int(time.time())
        self._tv_library_cache[cache_key] = {
            'tmdb_id': tmdb_id,
            'imdb_id': imdb_id,
            'title': title,
            'first_seen': self._tv_library_cache.get(cache_key, {}).get('first_seen', now),
            'last_seen': now
        }
        self._save_tv_library_cache()
        self.log("DEBUG", f"Added to TV library cache: {title} (TMDB: {tmdb_id})")

    def _get_cached_tv_shows(self) -> List[Dict]:
        """Get all cached TV shows for inclusion in TMDB results."""
        if not self.TV_PRESERVE_LIBRARY or not self._tv_library_cache:
            return []

        cached_shows = []
        for cache_entry in self._tv_library_cache.values():
            # Convert cache entry back to TMDB-like format
            cached_shows.append({
                'id': cache_entry['tmdb_id'],
                'name': cache_entry['title'],
                'original_name': cache_entry['title'],
                '_from_cache': True  # Mark as cached for debugging
            })

        return cached_shows

    def _is_tv_library_protected_torrent(self, torrent_hash: str) -> bool:
        """Check if a torrent belongs to a TV show in the library cache and should be protected."""
        if not self.TV_PRESERVE_LIBRARY or not self._tv_library_cache:
            return False

        # Find any .mkv files that reference this torrent hash
        if not hasattr(self, '_hash_cache') or torrent_hash not in getattr(self, '_hash_cache', {}):
            return False

        mkv_file_path = self._hash_cache.get(torrent_hash, '')
        if not mkv_file_path or not os.path.exists(mkv_file_path):
            return False

        # Check if this is a TV file (in TV directory)
        try:
            if self.TV_DIR in os.path.abspath(mkv_file_path):
                self.log("DEBUG", f"Protected TV library torrent: {torrent_hash[:8]}... -> {os.path.basename(mkv_file_path)}")
                return True
        except Exception:
            pass

        return False

    def _populate_tv_library_cache_from_existing(self):
        """Populate TV library cache from existing TV shows in the filesystem to protect them immediately."""
        if not os.path.exists(self.TV_DIR):
            return

        populated_count = 0
        for show_dir in os.listdir(self.TV_DIR):
            show_path = os.path.join(self.TV_DIR, show_dir)
            if not os.path.isdir(show_path):
                continue

            # Extract clean show title from directory name
            show_title = show_dir.replace('_', ' ').title()

            # Use a placeholder TMDB ID based on directory name hash for existing shows
            # This ensures existing shows are protected even if we can't get their real TMDB ID
            placeholder_tmdb_id = abs(hash(show_dir.lower())) % 1000000 + 900000  # Range 900000-999999
            placeholder_imdb_id = f"existing_{show_dir.lower()}"

            cache_key = f"{placeholder_tmdb_id}"
            if cache_key not in self._tv_library_cache:
                now = int(time.time())
                self._tv_library_cache[cache_key] = {
                    'tmdb_id': placeholder_tmdb_id,
                    'imdb_id': placeholder_imdb_id,
                    'title': show_title,
                    'first_seen': now,
                    'last_seen': now,
                    '_existing_show': True  # Mark as populated from filesystem
                }
                populated_count += 1

        if populated_count > 0:
            self._save_tv_library_cache()
            self.log("INFO", f"Populated TV library cache with {populated_count} existing shows for protection")

    # ================== NEW: NEGATIVE CACHE (NO MKV) ==================
    def _load_negative_cache(self) -> dict:
        """Load negative cache for hashes with no mkv files."""
        try:
            if os.path.isfile(self.NEGATIVE_CACHE_FILE):
                with open(self.NEGATIVE_CACHE_FILE, 'r') as f:
                    data = json.load(f)
                    if isinstance(data, dict):
                        return data
        except Exception:
            pass
        return {}

    def _save_negative_cache(self):
        """Persist negative cache to disk (atomic)."""
        try:
            tmp = self.NEGATIVE_CACHE_FILE + '.tmp'
            with open(tmp, 'w') as f:
                json.dump(self._negative_cache, f)
            os.replace(tmp, self.NEGATIVE_CACHE_FILE)
        except Exception as e:
            self.log("WARN", f"Unable to save negative cache: {e}")

    def _load_blacklist(self) -> dict:
        """Load blacklist from disk."""
        if not os.path.exists(self.BLACKLIST_FILE):
            return {"hashes": {}, "titles": []}
        try:
            with open(self.BLACKLIST_FILE, 'r') as f:
                data = json.load(f)
                if isinstance(data, dict):
                    return data
        except Exception:
            pass
        return {"hashes": {}, "titles": []}

    def _normalize_title(self, title: str) -> str:
        """Normalize title for fuzzy blacklist matching (Squeeze logic)"""
        if not title: return ""
        t = title.lower()
        t = re.sub(r'\(?\d{4}\)?', '', t)
        t = re.sub(r'\b(2160p|1080p|720p|4k|uhd|hdr|dv|dovi|web|bluray|remux)\b.*', '', t)
        # Remove ALL non-alphanumeric characters INCLUDING spaces
        t = re.sub(r'[^a-z0-9]', '', t)
        return t.strip()
    
    def _is_fullpack_title(self, title: str) -> bool:
        """
        Rileva se il titolo di uno stream TV rappresenta un pack (season/complete/multi-episodio)
        Criteri più conservativi:
          - contiene keywords: 'season', 'complete', 'full', 'pack' (con word boundaries)
          - contiene pattern range S01E01-E10 o S01E01-07
          - contiene pattern stagione: S01 COMPLETE, S01 PACK
          - contiene multipli episodi: S01E01 S01E02 nello stesso titolo
          - DEFAULT: False per casi ambigui (più conservativo)
        """
        import re
        if not title:
            return False
        t = title.lower()
        
        # Keywords forti con word boundaries per evitare false positive
        if re.search(r'\b(season|complete|full|pack)\b', t):
            return True
            
        # Pattern season + keywords: "S01 COMPLETE", "S01 PACK"
        if re.search(r's\d+\s+(complete|pack|full)', t, re.IGNORECASE):
            return True
            
        # Range episodi S01E01-E10, S01E01-07, S01E01-E07
        if re.search(r's\d+e\d+\s*-\s*e?\d+', t, re.IGNORECASE):
            return True
            
        # Pattern multi episodio nel titolo (due o più SxxEyy)
        episodes = re.findall(r's\d+e\d+', t, re.IGNORECASE)
        if len(episodes) >= 2:
            return True
            
        return False
    
    def _is_real_fullpack_from_files(self, video_files: list) -> bool:
        """
        Determina se un set di video files rappresenta davvero un fullpack:
          - almeno 3 episodi distinti (SxxEyy) OPPURE
          - almeno 4 video totali (per rarissimi pack senza pattern episodi)
        Evita falsi positivi con: episodio + sample, episodio doppio, main + trailer.
        """
        import re
        if not video_files:
            return False
        ep_set = set()
        for vf in video_files:
            path = vf.get("path", "") or ""
            m = re.search(r'[Ss](\d+)[Ee](\d+)', path)
            if m:
                ep_set.add(m.group(0).lower())
        if len(ep_set) >= 3:
            return True
        if len(video_files) >= 4:
            return True
        return False
    
    def _extract_season_number(self, title: str) -> int:
        """
        Estrae il numero di stagione dal titolo
        Restituisce -1 se non trovato
        """
        # Pattern S01, S02, Season 1, Season01
        patterns = [
            r'[Ss](\d+)',  # S01, s02
            r'[Ss]eason[\s\.]*(\d+)',  # Season 1, Season.01
            r'(\d+)\.stagione',  # 1.stagione (italiano)
        ]
        
        for pattern in patterns:
            match = re.search(pattern, title, re.IGNORECASE)
            if match:
                return int(match.group(1))
        
        return -1
    
    def _classify_tv_stream_by_name(self, stream: dict) -> dict:
        """
        Classifica uno stream TV usando:
          - stream['name'] (es. 'Torrentio\\n4k', 'Torrentio\\n4k HDR', 'Torrentio\\n1080p HDR')
          - titolo per fallback
        PRIORITÀ:
          Fullpack 4K HDR/DV > Episodi 4K HDR/DV > Fullpack 4K > Episodi 4K >
          Fullpack 1080 HDR/DV > Episodi 1080 HDR/DV > Fullpack 1080 > Episodi 1080
        """
        import re
        title = stream.get("title", "") or ""
        name  = stream.get("name", "") or ""
        if not title:
            return {}
        season = self._extract_season_number(title)
        if season < 0:
            season = 1

        # Determina se fullpack
        is_fullpack = self._is_fullpack_title(title)

        # Risoluzione / HDR dal campo name (fallback su title)
        low_name  = name.lower()
        low_title = title.lower()

        # Detection HDR/DV precisa - evita falsi positivi dvdrip
        name_has_hdr = (
            re.search(r'\bhdr10\+?\b', low_name) or
            re.search(r'\bhdr\b', low_name) or
            (re.search(r'\bDV\b', name) and 'dvdrip' not in low_name) or  # DV case-sensitive, no dvdrip
            re.search(r'\bDoVi\b', low_name, re.IGNORECASE) or  # DoVi comune
            re.search(r'DV[/.|]HDR|HDR[/.|]DV', name)  # Pattern combinati DV/HDR
        )
        title_has_hdr = bool(re.search(self.HDR_PATTERNS, title, re.IGNORECASE))
        is_hdr = name_has_hdr or title_has_hdr

        is_4k = ("4k" in low_name) or bool(re.search(r'2160p|4[kK]|UHD', title, re.IGNORECASE))
        # Detection 1080p solo esplicita - no 1080, fhd, 1080i
        is_1080 = (not is_4k) and (
            "1080p" in low_name or 
            re.search(r'\b1080p\b', low_title, re.IGNORECASE)
        )
        
        if not (is_4k or is_1080):
            return {}  # scarta 720p o ignoti

        # Calcola quality score (già aggiunto prima in _filter_tv_streams, ma ricalcoliamo se assente)
        qs = stream.get("quality_score")
        if qs is None:
            qs = self.calculate_quality_score(title + " " + name)

        # Sistema priorità a 8 livelli (800-100)
        if is_fullpack and is_4k and is_hdr:
            pri = 800  # Fullpack 4K HDR/DV (massima priorità)
        elif (not is_fullpack) and is_4k and is_hdr:
            pri = 700  # Episodi singoli 4K HDR/DV
        elif is_fullpack and is_4k:
            pri = 600  # Fullpack 4K (no HDR)
        elif (not is_fullpack) and is_4k:
            pri = 500  # Episodi singoli 4K
        elif is_fullpack and is_1080 and is_hdr:
            pri = 400  # Fullpack 1080p HDR/DV
        elif (not is_fullpack) and is_1080 and is_hdr:
            pri = 300  # Episodi singoli 1080p HDR/DV
        elif is_fullpack and is_1080:
            pri = 200  # Fullpack 1080p
        else:
            pri = 100  # Episodi singoli 1080p

        return {
            "raw": stream,
            "season": season,
            "is_fullpack": is_fullpack,
            "is_4k": is_4k,
            "is_hdr": is_hdr,
            "is_1080": is_1080,
            "pri": pri,
            "quality_score": qs
        }

    def _prioritize_tv_streams_by_name(self, streams: list) -> tuple:
        """
        Per ogni stagione:
          - Trova il tier di priorità massimo presente
          - Se nel tier c'è almeno un fullpack: scegli il miglior fullpack (uno solo)
          - Altrimenti seleziona TUTTI gli episodi singoli di quel tier
        Ritorna: (fullpacks, singles, season_has_fullpack_dict)
        """
        by_season = {}
        for s in streams:
            cl = self._classify_tv_stream_by_name(s)
            if not cl:
                continue
            by_season.setdefault(cl["season"], []).append(cl)

        selected_fullpacks = []
        selected_singles = []
        season_has_fullpack = {}

        for season, items in by_season.items():
            # Ordina per (priorità, quality score)
            items.sort(key=lambda x: (-x["pri"], -x["quality_score"]))
            top_pri = items[0]["pri"]
            tier = [x for x in items if x["pri"] == top_pri]

            fullpacks = [x for x in tier if x["is_fullpack"]]
            if fullpacks:
                best = fullpacks[0]
                selected_fullpacks.append(best["raw"])
                season_has_fullpack[season] = best["raw"]
                self.log("INFO", f"[TV/NAME] S{season:02d} fullpack scelto: {best['raw'].get('title','')[:90]} (pri={best['pri']}, qs={best['quality_score']})")
            else:
                singles = [x["raw"] for x in tier]
                selected_singles.extend(singles)
                self.log("INFO", f"[TV/NAME] S{season:02d} nessun fullpack: selezionati {len(singles)} episodi (pri={top_pri})")

        return selected_fullpacks, selected_singles, season_has_fullpack
    
    def _replace_single_episodes_with_fullpack(self, show_name: str, season_num: int, fullpack_hash: str):
        """
        Replace existing single episodes with fullpack for this season.
        Removes single episode files and torrents ONLY if fullpack has better quality score.
        """
        # Use unified directory to prevent duplicates due to capitalization
        unified_show_name = self._get_unified_show_directory(show_name)
        season_dir = os.path.join(self.TV_DIR, unified_show_name, f"Season.{season_num:02d}")
        
        if not os.path.exists(season_dir):
            return
        
        # Get fullpack torrent info to calculate quality score
        fullpack_info = self.get_torrent_info(fullpack_hash)
        if not fullpack_info:
            self.log("WARN", f"Could not get fullpack torrent info for quality comparison")
            return
        
        # Verify this is actually a fullpack before proceeding
        video_files = self._collect_video_files(fullpack_info.get("file_stats", []) or [])
        if not self._is_real_fullpack_from_files(video_files):
            self.log("INFO", f"Abort replace: hash {fullpack_hash[:8]} non è un fullpack reale (video={len(video_files)})")
            return
        
        fullpack_title = fullpack_info.get("title", "")
        fullpack_name = fullpack_info.get("name", "")
        fullpack_score = self.calculate_quality_score(fullpack_title + " " + fullpack_name)
        self.log("DEBUG", f"Fullpack quality score: {fullpack_score} for {fullpack_title}")
        
        removed_count = 0
        
        # Find all mkv files in this season directory
        try:
            for filename in os.listdir(season_dir):
                if not filename.endswith('.mkv'):
                    continue
                
                mkv_path = os.path.join(season_dir, filename)
                
                # Read existing file's hash and content
                try:
                    with open(mkv_path, 'r') as f:
                        content = f.read()
                        first_line = content.split('\n')[0] if content else ""
                        
                        # Extract hash from stream URL
                        match = re.search(r'[?&]link=([a-fA-F0-9]{40})', content)
                        if match:
                            existing_hash = match.group(1)
                            
                            # If this file uses a different hash (single episode), check quality
                            if existing_hash != fullpack_hash:
                                # Uso filename per avere tag qualità (HDR/2160p/Atmos) mantenuti
                                existing_score = self.calculate_quality_score(filename)
                                
                                # Only remove if fullpack has better quality
                                if fullpack_score > existing_score:
                                    self.log("INFO", f"Replacing single episode with better fullpack: {filename} (score {existing_score} → {fullpack_score})")
                                    
                                    # Remove the mkv file
                                    os.unlink(mkv_path)
                                    
                                    # Remove the corresponding torrent from server
                                    self.remove_torrent_from_server(existing_hash)
                                    removed_count += 1
                                else:
                                    self.log("INFO", f"Keeping single episode with better quality: {filename} (score {existing_score} > fullpack {fullpack_score})")
                except (IOError, Exception) as e:
                    self.log("WARN", f"Could not process file {mkv_path}: {e}")
                    continue
        
        except OSError as e:
            self.log("WARN", f"Could not access season directory {season_dir}: {e}")
            return
        
        if removed_count > 0:
            self.log("INFO", f"Replaced {removed_count} single episodes with better fullpack for Season {season_num}")
    
    def _cleanup_single_episode_variants_after_fullpack(self, season_dir: str, fullpack_hash: str):
        """
        Rimuove episodi singoli (hash diversi) che esistevano prima del fullpack per la stessa stagione.
        Mantiene solo i file appena creati col fullpack_hash.
        Rimuove anche i torrent orfani da GoStorm.
        """
        if not os.path.isdir(season_dir):
            return
        removed_files = 0
        orphaned_hashes = []
        
        for f in os.listdir(season_dir):
            if not f.lower().endswith('.mkv'):
                continue
            path = os.path.join(season_dir, f)
            try:
                with open(path, 'r') as fh:
                    first = fh.readline().strip()
                m = re.search(r'link=([a-f0-9]{40})', first, re.IGNORECASE)
                if not m:
                    continue
                h = m.group(1).lower()
                if h != fullpack_hash.lower():
                    # Stesso episodio? Confronta core
                    ep_core = self._core_key(f, is_tv=True)
                    # Se esiste già una versione con hash del fullpack (nuovo) per quel core → elimina questo
                    existing_new = self._tv_core_index.get(ep_core)
                    if existing_new and os.path.basename(existing_new) != f:
                        try:
                            os.remove(path)
                            removed_files += 1
                            orphaned_hashes.append(h)
                            self.log("INFO", f"Removed old single-episode variant after fullpack: {f}")
                            # Remove from core index
                            if ep_core in self._tv_core_index and self._tv_core_index[ep_core] == path:
                                del self._tv_core_index[ep_core]
                        except OSError as e:
                            self.log("WARN", f"Failed to remove file {f}: {e}")
            except Exception as e:
                self.log("WARN", f"Error processing cleanup file {f}: {e}")
                continue
        
        # Remove orphaned torrents from GoStorm
        removed_torrents = 0
        for orphaned_hash in set(orphaned_hashes):  # Remove duplicates
            try:
                if self.remove_torrent_from_server(orphaned_hash):
                    removed_torrents += 1
                    self.log("INFO", f"Removed orphaned torrent after fullpack cleanup: {orphaned_hash[:8]}...")
            except Exception as e:
                self.log("WARN", f"Failed to remove orphaned torrent {orphaned_hash[:8]}...: {e}")
        
        if removed_files or removed_torrents:
            self.log("INFO", f"Fullpack cleanup: rimossi {removed_files} episodi + {removed_torrents} torrent orfani")
            # Save updated core index if any changes were made
            if removed_files > 0:
                self._save_core_index(self.TV_CORE_INDEX_FILE, self._tv_core_index)

    def _negative_cache_expired(self, ts: int) -> bool:
        """Check if negative cache entry is expired based on TTL."""
        try:
            return (time.time() - ts) > (self.NEGATIVE_CACHE_TTL_HOURS * 3600)
        except Exception:
            return True

    def _negative_cache_has(self, info_hash: str) -> bool:
        """Check if hash is in negative cache and not expired."""
        rec = self._negative_cache.get(info_hash.lower())
        if not rec:
            return False
        if self._negative_cache_expired(rec.get('ts', 0)):
            # auto prune expired entry
            self._negative_cache.pop(info_hash.lower(), None)
            self._save_negative_cache()
            return False
        return True

    def _negative_cache_add(self, info_hash: str, reason: str):
        """Add hash to negative cache with timestamp and reason."""
        self._negative_cache[info_hash.lower()] = {"ts": int(time.time()), "reason": reason}
        self._save_negative_cache()

    def find_existing_variant(self, probe: str) -> str:
        """Enhanced variant detection.

        Obiettivo: trovare file esistenti anche se differiscono per:
          - Separatore (., _, -)
          - Differenze di case
          - Presenza/assenza di segmenti qualità (2160p, HDR, WEB-DL, audio, codec)
          - Hash finale diverso
        """
        import os, glob, re

        dir_path = os.path.dirname(probe)
        base_new = os.path.basename(probe)

        # Helper: normalizza titolo (movie o tv) rimuovendo hash, tag qualità e normalizzando separatori
        # CRITICAL: Preserve resolution/HDR tokens for quality comparison
        CRITICAL_QUALITY_TOKENS = {'2160p','1080p','720p','4k','uhd','hdr','hdr10','hdr10+','dv','atmos'}
        # GENERIC: Remove source/codec tokens for flexible matching
        GENERIC_QUALITY_TOKENS = {
            'web','webdl','web-dl','webrip','blu-ray','bluray','bdrip','hdtv','hdcam','cam','ts','dvdrip',
            'ddp','ddp5.1','ddp7.1','dts','dts-hd','dts-x','truehd','aac','ac3','mp3','flac',
            'x265','x264','hevc','h265','h264','av1','vc1','xvid','divx','remux','proper','repack',
            'extended','directors','cut','unrated','theatrical','imax','3d','sbs','ou','multi','dual'
        }
        HASH_SUFFIX_RE = re.compile(r'_[a-f0-9]{8}\.mkv$', re.IGNORECASE)
        YEAR_RE = re.compile(r'(19|20)\d{2}')
        EP_RE = re.compile(r'[Ss]\d+[Ee]\d+')

        def normalize(name: str) -> str:
            name = name.rsplit('/', 1)[-1]
            name = name[:-4] if name.lower().endswith('.mkv') else name
            name = HASH_SUFFIX_RE.sub('', name + '.mkv')  # ensure suffix removal
            name = name[:-4] if name.lower().endswith('.mkv') else name
            # unify separators (including spaces)
            name = re.sub(r'[\.\-\s]+', '_', name)
            name = re.sub(r'_+', '_', name)
            parts = name.split('_')
            norm_parts = []
            for p in parts:
                pl = p.lower()
                # PRESERVE critical quality tokens (resolution, HDR, audio)
                if pl in CRITICAL_QUALITY_TOKENS:
                    norm_parts.append(pl)  # Keep for quality comparison
                # REMOVE generic source/codec tokens for flexible matching
                elif pl in GENERIC_QUALITY_TOKENS:
                    continue  # Remove for pattern matching
                # remove codec-like tokens containing specific codecs
                elif any(tok in pl for tok in ['x264','x265','hevc','h265','av1','h264']):
                    continue
                # skip empty
                elif not pl:
                    continue
                else:
                    norm_parts.append(pl)  # Keep title/year parts
            return '_'.join(norm_parts)

        norm_new = normalize(base_new)
        new_year_match = YEAR_RE.search(norm_new)
        new_year = new_year_match.group(0) if new_year_match else None
        new_is_tv = bool(EP_RE.search(base_new))

        # Costruisci pattern base per glob (usa primi token prima dell'anno se presente)
        prefix_for_glob = norm_new
        if new_year:
            prefix_for_glob = norm_new.split(new_year)[0]
        prefix_for_glob = prefix_for_glob.strip('_')
        if not prefix_for_glob:
            prefix_for_glob = norm_new

        # Genera varianti separator per glob
        glob_variants = {
            prefix_for_glob,
            prefix_for_glob.replace('_', '.'),
            prefix_for_glob.replace('_', '-'),
        }

        candidates = []
        for gv in glob_variants:
            pattern = os.path.join(dir_path, gv + '*')
            candidates.extend(glob.glob(pattern))

        seen = set()
        for cand in candidates:
            if cand in seen:
                continue
            seen.add(cand)
            if not cand.lower().endswith('.mkv'):
                continue
            if not os.path.isfile(cand):
                continue
            norm_old = normalize(os.path.basename(cand))
            # Year consistency (if both have year and differ => skip)
            old_year_match = YEAR_RE.search(norm_old)
            old_year = old_year_match.group(0) if old_year_match else None
            if new_year and old_year and new_year != old_year:
                continue
            old_is_tv = bool(EP_RE.search(os.path.basename(cand)))
            if new_is_tv != old_is_tv:
                continue
            # Flexible match: either exact normalized equality OR one is prefix of the other (handles missing quality tokens in new target)
            if norm_new == norm_old or norm_new.startswith(norm_old) or norm_old.startswith(norm_new):
                self.log("DEBUG", f"Variant match found: new={norm_new} old={norm_old} file={os.path.basename(cand)}")
                return cand
        return ""

    def get_tmdb_latest_movies(self) -> List[Dict]:
        """
        Exact replica of bash get_tmdb_latest_movies() function
        Get latest movies from TMDB using multiple APIs and deduplication
        """
        import datetime
        import tempfile
        import os

        current_year = datetime.datetime.now().year
        previous_year = current_year - 1
        # Calculate 6 months ago for discovery window (reduces old content processing)
        six_months_ago = (datetime.datetime.now() - datetime.timedelta(days=180)).strftime('%Y-%m-%d')

        # For testing, use mock data
        if self.TMDB_API_KEY == "test-mode":
            return [
                {"id": 1, "imdb_id": "tt1234567", "original_title": "Warfare", "title": "Warfare"},
                {"id": 2, "imdb_id": "tt2345678", "original_title": "Superman", "title": "Superman"},
                {"id": 3, "imdb_id": "tt3456789", "original_title": "Ballerina", "title": "Ballerina"},
                {"id": 4, "imdb_id": "tt4567890", "original_title": "How to Train Your Dragon", "title": "How to Train Your Dragon"}
            ]
        
        # Use multiple TMDB APIs for maximum movie coverage
        all_results = []
        
        # Constants from bash version
        TMDB_MOVIE_PAGES = 20
        TMDB_POPULAR_PAGES = 5
        TMDB_API_DELAY = 0.25  # 250ms delay
        
        # API 1: Discover API - get recent releases with separate language calls for accuracy
        # English movies (20 pages = ~400 movies) - limited to last 6 months
        for page in range(1, TMDB_MOVIE_PAGES + 1):
            discover_url = f"{self.TMDB_BASE_URL}/discover/movie"
            params = {
                'api_key': self.TMDB_API_KEY,
                'primary_release_date.gte': six_months_ago,
                'primary_release_date.lte': f"{current_year}-12-31",
                'with_original_language': 'en',
                'sort_by': 'popularity.desc',
                'include_adult': False,
                'include_video': False,
                'page': page
            }
            
            response = self.safe_curl(discover_url, params=params, headers={'User-Agent': 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36'})
            if response:
                try:
                    data = response.json()
                    results = data.get('results', [])
                    all_results.extend(results)
                except (json.JSONDecodeError, AttributeError):
                    pass
            
            time.sleep(TMDB_API_DELAY)
        
        # Italian movies (5 pages to balance total content) - limited to last 6 months
        for page in range(1, 6):
            discover_url = f"{self.TMDB_BASE_URL}/discover/movie"
            params = {
                'api_key': self.TMDB_API_KEY,
                'primary_release_date.gte': six_months_ago,
                'primary_release_date.lte': f"{current_year}-12-31",
                'with_original_language': 'it',
                'sort_by': 'popularity.desc',
                'include_adult': False,
                'include_video': False,
                'page': page
            }
            
            response = self.safe_curl(discover_url, params=params, headers={'User-Agent': 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36'})
            if response:
                try:
                    data = response.json()
                    results = data.get('results', [])
                    all_results.extend(results)
                except (json.JSONDecodeError, AttributeError):
                    pass
            
            time.sleep(TMDB_API_DELAY)
        
        # Add now playing movies (current releases) per region
        for region in ['US', 'GB']:
            for page in range(1, 3):
                now_playing_url = f"{self.TMDB_BASE_URL}/movie/now_playing"
                params = {
                    'api_key': self.TMDB_API_KEY,
                    'language': 'en-US',
                    'region': region,
                    'page': page
                }

                response = self.safe_curl(now_playing_url, params=params, headers={'User-Agent': 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36'})
                if response:
                    try:
                        data = response.json()
                        results = data.get('results', [])
                        all_results.extend(results)
                    except (json.JSONDecodeError, AttributeError):
                        pass

                time.sleep(TMDB_API_DELAY)
        
        # Add trending movies (weekly)
        for page in range(1, 3):
            trending_url = f"{self.TMDB_BASE_URL}/trending/movie/week"
            params = {
                'api_key': self.TMDB_API_KEY,
                'page': page
            }
            
            response = self.safe_curl(trending_url, params=params, headers={'User-Agent': 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36'})
            if response:
                try:
                    data = response.json()
                    results = data.get('results', [])
                    all_results.extend(results)
                except (json.JSONDecodeError, AttributeError):
                    pass
            
            time.sleep(TMDB_API_DELAY)
        
        # API 2: Popular Movies API - get popular movies per region
        for region in ['US', 'GB']:
            for page in range(1, TMDB_POPULAR_PAGES + 1):
                popular_url = f"{self.TMDB_BASE_URL}/movie/popular"
                params = {
                    'api_key': self.TMDB_API_KEY,
                    'language': 'en-US',
                    'region': region,
                    'page': page
                }

                response = self.safe_curl(popular_url, params=params, headers={'User-Agent': 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36'})
                if response:
                    try:
                        data = response.json()
                        results = data.get('results', [])
                        # Filter for last 6 months (popular API doesn't support date filters)
                        filtered_results = []
                        for movie in results:
                            release_date = movie.get('release_date', '')
                            if release_date >= six_months_ago:
                                filtered_results.append(movie)
                        all_results.extend(filtered_results)
                    except (json.JSONDecodeError, AttributeError):
                        pass

                time.sleep(TMDB_API_DELAY)
        
        # Niche providers discovery (MUBI, Criterion, ARROW) — 3 pages per region/language
        for region, lang in [('US', 'en'), ('GB', 'en'), ('IT', 'it')]:
            for page in range(1, 4):
                discover_url = f"{self.TMDB_BASE_URL}/discover/movie"
                params = {
                    'api_key': self.TMDB_API_KEY,
                    'with_watch_providers': self.NICHE_PROVIDER_IDS,
                    'watch_region': region,
                    'primary_release_date.gte': six_months_ago,
                    'primary_release_date.lte': f"{current_year}-12-31",
                    'with_original_language': lang,
                    'sort_by': 'vote_average.desc',
                    'vote_count.gte': 50,
                    'include_adult': False,
                    'include_video': False,
                    'page': page
                }

                response = self.safe_curl(discover_url, params=params, headers={'User-Agent': 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36'})
                if response:
                    try:
                        data = response.json()
                        results = data.get('results', [])
                        all_results.extend(results)
                    except (json.JSONDecodeError, AttributeError):
                        pass

                time.sleep(TMDB_API_DELAY)

        # Filter for last 6 months, deduplicate and sort by release date
        filtered_results = []
        seen_ids = set()

        for movie in all_results:
            movie_id = movie.get('id')
            if movie_id in seen_ids:
                continue
            seen_ids.add(movie_id)

            # Filter by date (last 6 months)
            release_date = movie.get('release_date', '')
            if not release_date or release_date < six_months_ago:
                continue

            # Filter by language
            original_language = movie.get('original_language', '')
            if original_language not in ['en', 'it']:
                continue

            filtered_results.append(movie)
        
        # Sort by release date (newest first)
        filtered_results.sort(key=lambda x: x.get('release_date', '1900-01-01'), reverse=True)
        
        return filtered_results

    def get_tmdb_latest_tv(self) -> List[Dict]:
        """
        Exact replica of bash get_tmdb_latest_tv() function
        Get latest TV shows from TMDB using Discover API with multiple languages
        """
        import datetime
        
        current_year = datetime.datetime.now().year
        previous_year = current_year - 1
        
        # For testing, use mock data
        if self.TMDB_API_KEY == "test-mode":
            return [
                {"id": 5, "external_ids": {"imdb_id": "tt5678901"}, "name": "Wednesday", "original_name": "Wednesday"},
                {"id": 6, "external_ids": {"imdb_id": "tt6789012"}, "name": "The Night Agent", "original_name": "The Night Agent"}
            ]
        
        # Use TMDB Discover API with separate language calls for accuracy
        all_results = []
        
        # Constants from bash version
        TMDB_TV_PAGES = 8
        TMDB_API_DELAY = 0.25  # 250ms delay
        
        # English TV shows (8 pages = ~160 shows)
        for page in range(1, TMDB_TV_PAGES + 1):
            discover_url = f"{self.TMDB_BASE_URL}/discover/tv"
            params = {
                'api_key': self.TMDB_API_KEY,
                'first_air_date.gte': f"{previous_year}-01-01",
                'first_air_date.lte': f"{current_year}-12-31",
                'with_original_language': 'en',
                'sort_by': 'first_air_date.desc',
                'page': page
            }
            
            response = self.safe_curl(discover_url, params=params, headers={'User-Agent': 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36'})
            if response:
                try:
                    data = response.json()
                    results = data.get('results', [])
                    all_results.extend(results)
                except (json.JSONDecodeError, AttributeError):
                    pass
            
            time.sleep(TMDB_API_DELAY)
        
        # Italian TV shows (4 pages to balance content)
        for page in range(1, 5):
            discover_url = f"{self.TMDB_BASE_URL}/discover/tv"
            params = {
                'api_key': self.TMDB_API_KEY,
                'first_air_date.gte': f"{previous_year}-01-01",
                'first_air_date.lte': f"{current_year}-12-31",
                'with_original_language': 'it',
                'sort_by': 'first_air_date.desc',
                'page': page
            }
            
            response = self.safe_curl(discover_url, params=params, headers={'User-Agent': 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36'})
            if response:
                try:
                    data = response.json()
                    results = data.get('results', [])
                    all_results.extend(results)
                except (json.JSONDecodeError, AttributeError):
                    pass
            
            time.sleep(TMDB_API_DELAY)
        
        # Add popular TV shows (trending content)
        popular_url = f"{self.TMDB_BASE_URL}/tv/popular"
        params = {
            'api_key': self.TMDB_API_KEY,
            'page': 1
        }
        response = self.safe_curl(popular_url, params=params, headers={'User-Agent': 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36'})
        if response:
            try:
                data = response.json()
                results = data.get('results', [])
                all_results.extend(results)
            except (json.JSONDecodeError, AttributeError):
                pass
        
        time.sleep(TMDB_API_DELAY)
        
        # Add currently airing TV shows
        on_the_air_url = f"{self.TMDB_BASE_URL}/tv/on_the_air"
        params = {
            'api_key': self.TMDB_API_KEY,
            'page': 1
        }
        response = self.safe_curl(on_the_air_url, params=params, headers={'User-Agent': 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36'})
        if response:
            try:
                data = response.json()
                results = data.get('results', [])
                all_results.extend(results)
            except (json.JSONDecodeError, AttributeError):
                pass
        
        time.sleep(TMDB_API_DELAY)
        
        # Add trending TV shows (weekly)
        trending_url = f"{self.TMDB_BASE_URL}/trending/tv/week"
        params = {
            'api_key': self.TMDB_API_KEY,
            'page': 1
        }
        response = self.safe_curl(trending_url, params=params, headers={'User-Agent': 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36'})
        if response:
            try:
                data = response.json()
                results = data.get('results', [])
                all_results.extend(results)
            except (json.JSONDecodeError, AttributeError):
                pass
        
        time.sleep(TMDB_API_DELAY)

        # Add cached TV shows to preserve existing library
        if self.TV_PRESERVE_LIBRARY:
            cached_shows = self._get_cached_tv_shows()
            all_results.extend(cached_shows)
            self.log("INFO", f"Added {len(cached_shows)} cached TV shows to preserve library")

        # Deduplicate by TMDB id, filter languages, and sort by air date
        filtered_results = []
        seen_ids = set()
        
        for show in all_results:
            show_id = show.get('id')
            if show_id in seen_ids:
                continue
            seen_ids.add(show_id)
            
            # Filter by language
            original_language = show.get('original_language', '')
            if original_language not in ['en', 'it']:
                continue
            
            filtered_results.append(show)
        
        # Sort by first air date (newest first)
        filtered_results.sort(key=lambda x: x.get('first_air_date', '1900-01-01'), reverse=True)
        
        return filtered_results

    def _build_torrentio_url(self, imdb_id: str, content_type: str) -> str:
        """
        Build configured Torrentio URL for both movies and TV series
        Applies sort and quality filters based on configuration
        """
        config_parts = []

        # Add sort configuration
        if self.TORRENTIO_SORT:
            config_parts.append(f"sort={self.TORRENTIO_SORT}")

        # Add providers if specified
        if self.TORRENTIO_PROVIDERS:
            config_parts.append(f"providers={self.TORRENTIO_PROVIDERS}")

        # Add quality filter based on mode
        exclude_qualities = self.TORRENTIO_EXCLUDE_QUALITIES
        if self.TORRENTIO_4K_FOCUS and exclude_qualities:
            # Add 1080p to exclusions for aggressive 4K focus mode (since 720p already excluded)
            if "1080p" not in exclude_qualities:
                exclude_qualities = f"{exclude_qualities},1080p"

        if exclude_qualities:
            config_parts.append(f"qualityfilter={exclude_qualities}")

        # Build final URL
        if config_parts:
            config = "|".join(config_parts)
            return f"{self.TORRENTIO_BASE_URL}/{config}/stream/{content_type}/{imdb_id}.json"
        else:
            # Fallback to simple URL if no configuration
            return f"{self.TORRENTIO_BASE_URL}/stream/{content_type}/{imdb_id}.json"

    def notify_plex(self, section_id: int):
        """Send a refresh command to Plex for a specific library section."""
        PLEX_URL = _cfg.get('plex', {}).get('url', 'http://127.0.0.1:32400')
        PLEX_TOKEN = _cfg.get('plex', {}).get('token', '')

        try:
            url = f"{PLEX_URL}/library/sections/{section_id}/refresh?X-Plex-Token={PLEX_TOKEN}"
            self.log("INFO", f"🚀 Notifying Plex to refresh library section {section_id}...")
            resp = requests.get(url, timeout=10)
            if resp.status_code in (200, 201):
                self.log_success(f"Plex refresh triggered successfully for section {section_id}")
            else:
                self.log("WARN", f"Plex refresh returned status {resp.status_code}")
        except Exception as e:
            self.log("ERROR", f"Failed to notify Plex: {e}")

    def get_torrentio_streams(self, imdb_id: str, content_type: str) -> Dict:
        """
        Exact replica of bash get_torrentio_streams() function
        Get streams from Torrentio for a specific IMDB ID
        content_type: "movie" or "series"
        """
        import re
        
        # For testing, return mock magnet links (mixed 4K and 1080p for fallback testing)
        if self.TMDB_API_KEY == "test-mode":
            mock_responses = {
                "tt1234567": {"streams": [{"title": "Warfare 2025 2160p 15GB", "infoHash": "1234567890abcdef"}]},
                "tt2345678": {"streams": [{"title": "Superman 2025 2160p 12GB", "infoHash": "2345678901bcdef"}]},
                "tt3456789": {"streams": [{"title": "Ballerina 2025 1080p 5GB WEB-DL", "infoHash": "3456789012cdefab"}]},
                "tt4567890": {"streams": [{"title": "How to Train Your Dragon 2025 1080p 4.5GB BluRay", "infoHash": "4567890123defabc"}]},
                "tt5678901": {"streams": [{"title": "Wednesday S02E01 2160p 11GB", "infoHash": "5678901234cdef"}]},
            }
            return mock_responses.get(imdb_id, {"streams": []})
        
        # Real Torrentio API call with retry logic - now uses configured URL
        torrentio_url = self._build_torrentio_url(imdb_id, content_type)
        raw_streams = ""
        retry_count = 0
        max_retries = 2
        
        # Retry API call with exponential backoff
        while retry_count <= max_retries:
            response = self.safe_curl(torrentio_url, timeout=10)
            
            if response:
                try:
                    raw_streams = response.text
                    # Check if we got a valid response (non-empty and starts with {)
                    if raw_streams and raw_streams.strip().startswith('{') and raw_streams.strip().endswith('}'):
                        break
                    # Check if Cloudflare blocked the request (403 or HTML response)
                    elif response.status_code == 403 or 'cloudflare' in raw_streams.lower():
                        self.log("INFO", f"Torrentio blocked by Cloudflare, trying simple URL for {imdb_id}")
                        # Fallback to simple URL without filters
                        simple_url = f"{self.TORRENTIO_BASE_URL}/stream/{content_type}/{imdb_id}.json"
                        fallback_response = self.safe_curl(simple_url, timeout=10)
                        if fallback_response and fallback_response.status_code == 200:
                            raw_streams = fallback_response.text
                            if raw_streams and raw_streams.strip().startswith('{') and raw_streams.strip().endswith('}'):
                                self.log("INFO", f"Cloudflare fallback successful for {imdb_id}")
                                break
                except Exception:
                    raw_streams = ""
            
            retry_count += 1
            if retry_count <= max_retries:
                wait_time = retry_count * 2
                self.log("DEBUG", f"API retry {retry_count}/{max_retries} for {imdb_id}, waiting {wait_time}s...")
                time.sleep(wait_time)
        
        # If all retries failed, log the issue
        if not raw_streams or not (raw_streams.strip().startswith('{') and raw_streams.strip().endswith('}')):
            self.log("DEBUG", f"All API retries failed for {imdb_id}. Response: {raw_streams[:100] if raw_streams else 'empty'}")
            raw_streams = ""
        
        # Robust JSON validation and error recovery
        if not raw_streams:
            # Empty response - return empty streams
            streams_data = {"streams": []}
        else:
            try:
                # Try to parse JSON
                streams_data = json.loads(raw_streams)
                if not isinstance(streams_data, dict):
                    streams_data = {"streams": []}
                elif "streams" not in streams_data or not isinstance(streams_data["streams"], list):
                    streams_data = {"streams": []}
                else:
                    # Validate stream objects
                    valid_streams = []
                    for stream in streams_data["streams"]:
                        if isinstance(stream, dict) and "title" in stream and isinstance(stream["title"], str):
                            valid_streams.append(stream)
                    streams_data["streams"] = valid_streams
            except (json.JSONDecodeError, Exception):
                self.log("DEBUG", f"Invalid JSON response from Torrentio for {imdb_id}: {raw_streams[:100]}")
                streams_data = {"streams": []}
        
        # Apply filtering based on content type
        if content_type == "movie":
            # For movies, use two-pass filtering: 4K first, then 1080p fallback
            filtered_streams = self._filter_movie_streams(streams_data)
        else:
            # For TV series, filter for quality content >= 500MB with fullpack support
            filtered_streams = self._filter_tv_streams(streams_data)
        
        return filtered_streams
    
    def _filter_streams_by_resolution(self, streams: list, resolution_name: str) -> list:
        """
        Filter streams by Torrentio resolution name (e.g. 'Torrentio\n4k', 'Torrentio\n1080p')
        Apply quality criteria (seeders, size) and return valid streams with scores
        """
        import re
        
        # Filter by resolution name (handle HDR/DV variants like "Torrentio\n4k HDR", "Torrentio\n4k DV | HDR")
        resolution_streams = [s for s in streams if s.get("name", "").startswith(resolution_name)]
        
        if not resolution_streams:
            return []
        
        def extract_gb_from_title(title: str) -> float:
            """Extract size in GB from title"""
            m = re.search(r'💾\s*([0-9]+\.?[0-9]*)\s*(GB|MB)', title, re.IGNORECASE)
            if not m:
                m = re.search(r'\b([0-9]+\.?[0-9]*)\s*(GB|MB)\b', title, re.IGNORECASE)
            if m:
                size = float(m.group(1))
                unit = m.group(2).upper()
                return size if unit == "GB" else size / 1000
            return 0.0
        
        def extract_seeders_from_title(title: str) -> int:
            """Extract seeder count from title"""
            match = re.search(r'👤\s*([0-9]+)', title)
            return int(match.group(1)) if match else 0
        
        # Apply quality criteria based on resolution
        valid_streams = []
        is_4k = "4k" in resolution_name.lower()
        
        for stream in resolution_streams:
            title = stream.get("title", "")

            # Seeders filter
            seeders = extract_seeders_from_title(title)
            if seeders < self.MIN_SEEDERS:
                continue

            # Low quality source filter (CAM, TS, SCREENER, etc.)
            if re.search(r'\b(CAM|TS|TC|HDCAM|HDTS|TELESYNC|TELECINE|SCR|SCREENER|WEBSCREENER|DVDSCR|DVDSCREENER|KORSUB|HC|HARDCODED)\b', title, re.IGNORECASE):
                continue

            # Size filter
            gb_size = extract_gb_from_title(title)
            if is_4k:
                # 4K size range check, allow unknown size (gb_size==0)
                if gb_size != 0 and not (self.MOVIE_4K_MIN_GB <= gb_size <= self.MOVIE_4K_MAX_GB):
                    continue
            else:
                # 1080p size range check
                if gb_size == 0 or not (self.MOVIE_1080P_MIN_GB <= gb_size <= self.MOVIE_1080P_MAX_GB):
                    continue
            
            # Language filter
            if re.search(self.EXCLUDED_STREAM_LANGUAGES, title, re.IGNORECASE):
                continue
            
            # Add quality score and seeders for sorting
            stream_copy = stream.copy()
            name = stream.get("name", "")
            stream_copy["quality_score"] = self.calculate_quality_score(title + " " + name, seeders=seeders)
            stream_copy["seeders"] = seeders  # Store for tie-breaker sorting

            # Penalty for unknown size in 4K
            if is_4k and gb_size == 0:
                stream_copy["quality_score"] -= 5

            valid_streams.append(stream_copy)

        # Sort by: 1) quality_score (desc), 2) seeders (desc) as tie-breaker, 3) size (desc)
        valid_streams.sort(key=lambda x: (-x["quality_score"], -x.get("seeders", 0), -extract_gb_from_title(x.get("title", ""))))
        return valid_streams
    
    def _filter_movie_streams(self, streams_data: Dict) -> Dict:
        """
        Filter movie streams with 4K first, then 1080p fallback logic
        Exact replica of bash movie filtering logic
        """
        import re
        
        streams = streams_data.get("streams", [])
        
        # Use instance constants from __init__ - CLEANED for unified scoring
        VIDEO_4K_PATTERNS = self.VIDEO_4K_PATTERNS
        VIDEO_1080P_PATTERNS = self.VIDEO_1080P_PATTERNS
        VIDEO_720P_PATTERNS = self.VIDEO_720P_PATTERNS  # 720p patterns for filtering
        MIN_SEEDERS = self.MIN_SEEDERS
        EXCLUDED_STREAM_LANGUAGES = self.EXCLUDED_STREAM_LANGUAGES
        
        def extract_gb_from_title(title: str) -> float:
            """
            Estrae dimensione (GB) da:
              - '💾 15GB'
              - '15GB', '8.5GB'
              - '9500MB' (convertito in GB)
            """
            m = re.search(r'💾\s*([0-9]+\.?[0-9]*)\s*(GB|MB)', title, re.IGNORECASE)
            if not m:
                m = re.search(r'\b([0-9]+\.?[0-9]*)\s*(GB|MB)\b', title, re.IGNORECASE)
            if m:
                size = float(m.group(1))
                unit = m.group(2).upper()
                return size if unit == "GB" else size / 1000
            return 0.0
        
        def extract_seeders_from_title(title: str) -> int:
            """Extract seeder count from title"""
            match = re.search(r'👤\s*([0-9]+)', title)
            return int(match.group(1)) if match else 0
        
        
        # First pass: 4K streams
        filtered_4k = []
        for stream in streams:
            title = stream.get("title", "")
            info_hash = stream.get("infoHash", "").lower()
            
            # FASE 5.2: Blacklist Filter (Hash or Fuzzy Title)
            if info_hash in self._blacklist.get("hashes", {}):
                self.log("DEBUG", f"Blacklist hit (hash): {info_hash[:8]} for {title[:40]}...")
                continue
            
            clean_title = self._normalize_title(title)
            if clean_title in self._blacklist.get("titles", []):
                self.log("INFO", f"🚫 Blacklist hit (fuzzy title): '{clean_title}' (original: {title[:40]}...)")
                continue

            # Check if 4K
            if not re.search(VIDEO_4K_PATTERNS, title, re.IGNORECASE):
                continue
            
            # Size filter
            gb_size = extract_gb_from_title(title)
            # Accept se size sconosciuta (gb_size==0) → non scartare subito, assegna punteggio ridotto
            if gb_size != 0 and not (self.MOVIE_4K_MIN_GB <= gb_size <= self.MOVIE_4K_MAX_GB):
                continue
            
            # Language filter
            if re.search(EXCLUDED_STREAM_LANGUAGES, title, re.IGNORECASE):
                continue
            
            # Seeders filter
            if extract_seeders_from_title(title) < MIN_SEEDERS:
                continue
            
            # Add quality score using unified system
            stream_copy = stream.copy()
            seeders = extract_seeders_from_title(title)
            name = stream.get("name", "")
            stream_copy["quality_score"] = self.calculate_quality_score(title + " " + name, seeders=seeders)
            stream_copy["seeders"] = seeders  # Store for tie-breaker sorting
            filtered_4k.append(stream_copy)

        # If we have 4K streams, return them
        if filtered_4k:
            # Downgrade punteggio per size sconosciuta
            for s in filtered_4k:
                if extract_gb_from_title(s.get("title","")) == 0:
                    s["quality_score"] -= 5
            # Sort by: 1) quality_score (desc), 2) seeders (desc) as tie-breaker, 3) size (desc)
            filtered_4k.sort(key=lambda x: (-x["quality_score"], -x.get("seeders", 0), -extract_gb_from_title(x.get("title", ""))))
            return {"streams": filtered_4k}
        
        # Second pass: 1080p streams (fallback)
        filtered_1080p = []
        for stream in streams:
            title = stream.get("title", "")
            info_hash = stream.get("infoHash", "").lower()
            
            # FASE 5.2: Blacklist Filter (Hash or Fuzzy Title)
            if info_hash in self._blacklist.get("hashes", {}):
                continue
            
            clean_title = self._normalize_title(title)
            if clean_title in self._blacklist.get("titles", []):
                continue

            # Check if 1080p (but not 720p)
            if not re.search(VIDEO_1080P_PATTERNS, title, re.IGNORECASE):
                continue
            if re.search(VIDEO_720P_PATTERNS, title, re.IGNORECASE):
                continue
            
            # Size filter
            gb_size = extract_gb_from_title(title)
            if not (self.MOVIE_1080P_MIN_GB <= gb_size <= self.MOVIE_1080P_MAX_GB):
                continue
            
            # Language filter
            if re.search(EXCLUDED_STREAM_LANGUAGES, title, re.IGNORECASE):
                continue
            
            # Seeders filter
            if extract_seeders_from_title(title) < MIN_SEEDERS:
                continue
            
            # Add quality score using unified system
            stream_copy = stream.copy()
            seeders = extract_seeders_from_title(title)
            name = stream.get("name", "")
            stream_copy["quality_score"] = self.calculate_quality_score(title + " " + name, seeders=seeders)
            stream_copy["seeders"] = seeders  # Store for tie-breaker sorting
            filtered_1080p.append(stream_copy)

        # Sort by: 1) quality_score (desc), 2) seeders (desc) as tie-breaker, 3) size (desc)
        filtered_1080p.sort(key=lambda x: (-x["quality_score"], -x.get("seeders", 0), -extract_gb_from_title(x.get("title", ""))))
        return {"streams": filtered_1080p}
    
    def _filter_tv_streams(self, streams_data: Dict) -> Dict:
        """
        Filter TV streams for quality content >= 500MB with fullpack support
        Exact replica of bash TV filtering logic
        """
        import re
        
        streams = streams_data.get("streams", [])
        
        # Use instance constants from __init__ - EXACT BASH MATCH
        VIDEO_4K_PATTERNS = self.VIDEO_4K_PATTERNS
        VIDEO_1080P_PATTERNS = self.VIDEO_1080P_PATTERNS
        VIDEO_720P_PATTERNS = self.VIDEO_720P_PATTERNS  # 720p patterns for filtering
        HDR_PATTERNS = self.HDR_PATTERNS
        MIN_SEEDERS = self.MIN_SEEDERS
        EXCLUDED_STREAM_LANGUAGES = self.EXCLUDED_STREAM_LANGUAGES
        
        def extract_gb_from_title(title: str) -> float:
            """
            Estrae dimensione (GB) da:
              - '💾 15GB'
              - '15GB', '8.5GB'
              - '9500MB' (convertito in GB)
            """
            m = re.search(r'💾\s*([0-9]+\.?[0-9]*)\s*(GB|MB)', title, re.IGNORECASE)
            if not m:
                m = re.search(r'\b([0-9]+\.?[0-9]*)\s*(GB|MB)\b', title, re.IGNORECASE)
            if m:
                size = float(m.group(1))
                unit = m.group(2).upper()
                return size if unit == "GB" else size / 1000
            return 0.0
        
        def extract_seeders_from_title(title: str) -> int:
            """Extract seeder count from title"""
            match = re.search(r'👤\s*([0-9]+)', title)
            return int(match.group(1)) if match else 0
        
        filtered_streams = []
        for stream in streams:
            title = stream.get("title", "")
            
            # Escludi direttamente 720p (riduce rumore)
            if re.search(r'720p', title, re.IGNORECASE):
                continue
            
            # Quality filter (4K, 1080p only)
            if not re.search(f"{VIDEO_4K_PATTERNS}|{VIDEO_1080P_PATTERNS}", title, re.IGNORECASE):
                continue
            
            # Size filter: >= 500MB (allows both GB and MB)
            gb_match = re.search(r'\b[1-9][0-9]*\.?[0-9]*\s*GB', title, re.IGNORECASE)
            mb_match = re.search(r'\b(5[0-9]{2}|[6-9][0-9]{2}|[1-9][0-9]{3,5})\s*MB', title, re.IGNORECASE)
            
            if not (gb_match or mb_match):
                continue
            
            # FULLPACK SUPPORT ENABLED: Accept both single episodes AND season packs
            # Note: This is the OPPOSITE of the bash filter - we WANT these patterns
            if not re.search(r"s\d+e\d+|season|complete|\d+x\d+|episode", title, re.IGNORECASE):
                continue
            
            # Language filter - EXCLUDE streams with emoji flags (EXACT BASH MATCH)
            if re.search(EXCLUDED_STREAM_LANGUAGES, title):
                continue
            
            # Seeders filter - require MIN_SEEDERS
            if extract_seeders_from_title(title) < MIN_SEEDERS:
                continue
            
            filtered_streams.append(stream)
        
        # Sort by UNIFIED quality scoring system (consistency with movies)
        def extract_seeders_from_title(title: str) -> int:
            """Extract seeder count from title"""
            match = re.search(r'👤\s*([0-9]+)', title)
            return int(match.group(1)) if match else 0
        
        # Add quality scores and seeders to all streams
        for stream in filtered_streams:
            title = stream.get("title", "")
            seeders = extract_seeders_from_title(title)
            name = stream.get("name", "")
            stream["quality_score"] = self.calculate_quality_score(title + " " + name, seeders=seeders)
            stream["seeders"] = seeders  # Store for tie-breaker sorting

        # Sort by: 1) quality_score (desc), 2) seeders (desc) as tie-breaker, 3) size (desc)
        filtered_streams.sort(key=lambda x: (-x["quality_score"], -x.get("seeders", 0), -extract_gb_from_title(x.get("title", ""))))
        return {"streams": filtered_streams}

    def validate_torrent_saved(self, hash_to_check: str) -> bool:
        """
        Exact replica of bash validate_torrent_saved() function
        Check if torrent exists in GoStorm database
        """
        max_attempts = 3
        attempt = 1
        
        self.log("DEBUG", f"Validating torrent saved in database: {hash_to_check}")
        
        while attempt <= max_attempts:
            # Check if torrent exists in GoStorm database
            response = self.safe_curl(
                f"{self.TORRSERVER_URL}/torrents",
                method="POST",
                data={"action": "get", "hash": hash_to_check}
            )
            
            if response:
                try:
                    data = response.json()
                    hash_present = data.get("hash", "")
                    
                    if hash_present and hash_present == hash_to_check:
                        self.log("DEBUG", f"Torrent validation SUCCESS on attempt {attempt}: {hash_to_check[:8]}...")
                        return True
                    else:
                        self.log("DEBUG", f"Torrent not found in database (attempt {attempt}): {hash_to_check[:8]}...")
                except (json.JSONDecodeError, Exception):
                    self.log("DEBUG", f"Invalid response during validation (attempt {attempt}): {hash_to_check[:8]}...")
            
            if attempt < max_attempts:
                sleep_time = attempt * 2
                self.log("DEBUG", f"Retrying validation in {sleep_time} seconds...")
                time.sleep(sleep_time)
            
            attempt += 1
        
        self.log("WARN", f"Torrent validation FAILED after {max_attempts} attempts: {hash_to_check[:8]}...")
        return False

    def cleanup_duplicate_tv_directories(self):
        """
        Clean up duplicate TV directories that contain episode info in their names
        Consolidate files into correct TMDB-based directory structure
        """
        if not os.path.exists(self.TV_DIR):
            return
            
        duplicate_dirs = []
        prefix_duplicate_dirs = []

        # Find directories with various duplicate patterns in their names
        for item in os.listdir(self.TV_DIR):
            item_path = os.path.join(self.TV_DIR, item)
            if os.path.isdir(item_path):
                # Check for torrent site prefixes first
                cleaned_name = self._remove_torrent_site_prefixes(item)
                if cleaned_name != item:
                    # This directory has a torrent site prefix
                    prefix_duplicate_dirs.append((item_path, cleaned_name))
                    continue

                # Extended patterns for duplicate detection:
                # - Episode patterns: S01E01 (single episodes in directory name)
                # - Season patterns: .S01, .Season.01 (season info in directory name)
                # - Year+Season: 2024.S01, 2025.Season.01
                # - Episode ranges: S01E01-E10
                # - Quality + episode/season: 1080p.S01E01, 2160p.S01
                if (re.search(r'[Ss][0-9]+[Ee][0-9]+', item) or           # S01E01
                    re.search(r'\.[Ss][0-9]+(?!\w)', item) or             # .S01
                    re.search(r'\.Season\.[0-9]+', item) or               # .Season.01
                    re.search(r'[0-9]{4}\.[Ss][0-9]+', item) or           # 2024.S01
                    re.search(r'[0-9]{4}\.Season\.[0-9]+', item) or       # 2024.Season.01
                    re.search(r'[Ss][0-9]+[Ee][0-9]+-[Ee][0-9]+', item) or # S01E01-E10
                    re.search(r'(1080p|2160p|720p)\.[Ss][0-9]+', item)):  # 1080p.S01
                    duplicate_dirs.append(item_path)

        # Process prefix duplicates first (directories with torrent site prefixes)
        if prefix_duplicate_dirs:
            self.log("INFO", f"Found {len(prefix_duplicate_dirs)} TV directories with torrent site prefixes to consolidate")

            for prefix_dir, cleaned_name in prefix_duplicate_dirs:
                try:
                    dir_name = os.path.basename(prefix_dir)

                    # Use unified directory to find matching canonical directory
                    unified_name = self._get_unified_show_directory(cleaned_name)
                    target_base_dir = os.path.join(self.TV_DIR, unified_name)

                    # If target doesn't exist, just rename to cleaned name
                    if not os.path.exists(target_base_dir):
                        os.rename(prefix_dir, target_base_dir)
                        self.log("INFO", f"Renamed prefixed directory: {dir_name} -> {unified_name}")
                        continue

                    # Target exists - merge files into it
                    self.log("INFO", f"Consolidating {dir_name} into {unified_name}")

                    for root, dirs, files in os.walk(prefix_dir):
                        for file in files:
                            if file.endswith('.mkv'):
                                src_file = os.path.join(root, file)

                                # Determine target season directory
                                rel_path = os.path.relpath(root, prefix_dir)
                                if rel_path == '.':
                                    # Files in root, try to determine season
                                    season_match = re.search(r'Season\.([0-9]+)', file)
                                    if season_match:
                                        target_season_dir = os.path.join(target_base_dir, f"Season.{season_match.group(1)}")
                                    else:
                                        target_season_dir = os.path.join(target_base_dir, "Season.01")
                                else:
                                    target_season_dir = os.path.join(target_base_dir, rel_path)

                                os.makedirs(target_season_dir, exist_ok=True)
                                target_file = os.path.join(target_season_dir, file)

                                # Only move if target doesn't exist (avoid duplicates)
                                if not os.path.exists(target_file):
                                    os.rename(src_file, target_file)
                                    self.log("DEBUG", f"Moved file: {file} -> {target_season_dir}")
                                else:
                                    self.log("DEBUG", f"Skip duplicate file (already exists): {file}")

                    # Remove empty prefix directory
                    try:
                        import shutil
                        shutil.rmtree(prefix_dir)
                        self.log("INFO", f"Removed prefixed directory: {dir_name}")
                    except Exception as e:
                        self.log("WARN", f"Could not remove directory {dir_name}: {e}")

                except Exception as e:
                    self.log("ERROR", f"Error consolidating prefixed directory {prefix_dir}: {e}")

        if not duplicate_dirs:
            return

        self.log("INFO", f"Found {len(duplicate_dirs)} duplicate TV directories to clean up")
        
        for duplicate_dir in duplicate_dirs:
            try:
                # Extract base show name from directory name
                dir_name = os.path.basename(duplicate_dir)
                base_name = re.sub(r'[Ss][0-9]+[Ee][0-9]+.*', '', dir_name)
                base_name = re.sub(r'_+', '_', base_name).strip('_')
                
                # Find correct target directory
                target_base_dir = os.path.join(self.TV_DIR, base_name)
                
                # If target doesn't exist, just rename the duplicate
                if not os.path.exists(target_base_dir):
                    os.rename(duplicate_dir, target_base_dir)
                    self.log("INFO", f"Renamed duplicate directory: {dir_name} -> {base_name}")
                    continue
                
                # Merge files from duplicate into correct directory
                for root, dirs, files in os.walk(duplicate_dir):
                    for file in files:
                        if file.endswith('.mkv'):
                            src_file = os.path.join(root, file)
                            
                            # Determine target season directory
                            rel_path = os.path.relpath(root, duplicate_dir)
                            if rel_path == '.':
                                # Files in root, try to determine season
                                season_match = re.search(r'Season\.([0-9]+)', file)
                                if season_match:
                                    target_season_dir = os.path.join(target_base_dir, f"Season.{season_match.group(1)}")
                                else:
                                    target_season_dir = os.path.join(target_base_dir, "Season.01")
                            else:
                                target_season_dir = os.path.join(target_base_dir, rel_path)
                            
                            os.makedirs(target_season_dir, exist_ok=True)
                            target_file = os.path.join(target_season_dir, file)
                            
                            # Only move if target doesn't exist (avoid duplicates)
                            if not os.path.exists(target_file):
                                os.rename(src_file, target_file)
                                self.log("DEBUG", f"Moved file: {file} -> {target_season_dir}")
                
                # Remove empty duplicate directory
                try:
                    os.rmdir(duplicate_dir)
                    self.log("INFO", f"Cleaned up duplicate directory: {dir_name}")
                except OSError:
                    # Directory not empty, remove recursively
                    import shutil
                    shutil.rmtree(duplicate_dir)
                    self.log("INFO", f"Removed non-empty duplicate directory: {dir_name}")
                    
            except Exception as e:
                self.log("ERROR", f"Error cleaning up duplicate directory {duplicate_dir}: {e}")

    def remove_torrent_from_server(self, hash_value: str) -> bool:
        """
        Remove torrent from GoStorm by hash
        Also clears negative cache to ensure consistency
        Returns True if successful, False otherwise
        """
        try:
            remove_url = f"{self.TORRSERVER_URL}/torrents"
            remove_data = {"action": "rem", "hash": hash_value}
            
            response = self.safe_curl(remove_url, method="POST", data=remove_data)
            if response and response.status_code == 200:
                self.log("DEBUG", f"Successfully removed torrent: {hash_value[:8]}...")
                
                # SYNC: Clear from negative cache when torrent is removed
                if self._negative_cache.pop(hash_value.lower(), None):
                    self._save_negative_cache()
                    self.log("DEBUG", f"Cleared negative cache entry for removed torrent: {hash_value[:8]}...")
                
                # Clear from hash cache if exists
                if hasattr(self, "_hash_cache"):
                    self._hash_cache.pop(hash_value.lower(), None)
                
                # CRITICAL: Invalida cache torrents dopo rimozione
                self._invalidate_torrents_cache()
                    
                return True
            else:
                self.log("WARN", f"Failed to remove torrent: {hash_value[:8]}...")
                return False
                
        except Exception as e:
            self.log("ERROR", f"Error removing torrent {hash_value[:8]}...: {e}")
            return False

    def add_torrent_to_server(self, magnet_url: str, title: str) -> Tuple[str, str]:
        """
        Add torrent to GoStorm using the stream endpoint
        Returns (status_string, hash) tuple
        Exact replica of bash inline torrent addition logic
        """
        # Extract hash from magnet URL for unified endpoint
        hash_value = self.extract_hash_from_magnet(magnet_url)
        if not hash_value:
            self.log_error(f"Cannot extract hash from magnet URL: {magnet_url}")
            return "", ""
        
        # Add torrent via NEW GoStorm endpoint
        self.log("INFO", f"Adding torrent to GoStorm: {title}")
        
        save_url = f"{self.TORRSERVER_URL}/stream/?link={magnet_url}&save"
        response = self.safe_curl(save_url)
        
        if not response:
            self.log_error(f"GoStorm API call failed for: {title}")
            return "", ""
        
        # Check if response contains error indicators
        response_text = response.text.lower() if hasattr(response, 'text') else str(response).lower()
        if any(error in response_text for error in ["error", "not found", "400", "500"]):
            self.log_error(f"GoStorm API error in response: {response_text[:100]}")
            return "", ""
        
        # Success - return hash
        self.log("INFO", f"NEW ENDPOINT SUCCESS - Status: Torrent added, Hash: {hash_value}")
        
        # CRITICAL: Invalida cache torrents dopo aggiunta
        self._invalidate_torrents_cache()
        
        return "Torrent added", hash_value

    def get_torrent_info(self, hash_value: str, fast_mode: bool = False) -> Optional[Dict]:
        """Adaptive polling per recuperare metadata senza attesa fissa di 15s."""
        start = time.time()
        attempt = 0
        if fast_mode:
            max_wait = 4
            sleep_seq = [1, 2]
        else:
            max_wait = self.METADATA_MAX_WAIT
            sleep_seq = self.METADATA_SLEEP_SEQ
            
        while time.time() - start < max_wait:
            response = self.safe_curl(
                f"{self.TORRSERVER_URL}/torrents",
                method="POST",
                data={"action": "get", "hash": hash_value}
            )
            if response:
                try:
                    info = response.json()
                    if info.get("file_stats"):
                        return info
                except Exception:
                    pass
            # in fast mode fai massimo 2 tentativi
            if fast_mode and attempt >= len(sleep_seq):
                break
            sleep_time = sleep_seq[attempt] if attempt < len(sleep_seq) else 2
            time.sleep(sleep_time)
            attempt += 1
        if not fast_mode:
            self.log("WARN", f"Metadata timeout for hash {hash_value[:8]}...")
        return None

    def _needs_full_coverage_scan(self) -> bool:
        """Check if full coverage scan is needed by comparing active torrents vs existing .mkv coverage"""
        # Hash attivi - FORZA refresh per conteggio accurato (no cache stantia)
        torrents = self._cached_torrents_list(force=True)
        if not torrents:
            return False
        active_hashes = {t.get("hash","").lower() for t in torrents if t.get("hash")}
        if not active_hashes:
            return False
            
        # Hash già coperti da .mkv (supporta sia hash completi che troncati)
        covered = set()
        covered_8char = set()  # Fallback per hash troncati nei filename
        
        # Movies-only: scan only MOVIES_DIR
        for base in (self.MOVIES_DIR,):
            if not os.path.isdir(base):
                continue
            for root, _, files in os.walk(base):
                for f in files:
                    if not f.lower().endswith(".mkv"):
                        continue
                    
                    # Prima cerca hash completo nel contenuto
                    try:
                        with open(os.path.join(root,f),'r') as fh:
                            first = fh.readline().strip()
                        m = re.search(r'link=([a-f0-9]{40})', first, re.IGNORECASE)
                        if m:
                            covered.add(m.group(1).lower())
                    except Exception:
                        pass
                    
                    # Fallback: estrai hash troncato dal filename (pattern più flessibile)
                    # Cerca 8 caratteri hex alla fine del filename, preceduti da separatore opzionale
                    m_filename = re.search(r'[_.-]?([a-f0-9]{8})\.mkv$', f, re.IGNORECASE)
                    if m_filename:
                        hash_8char = m_filename.group(1).lower()
                        covered_8char.add(hash_8char)
                        self.log("DEBUG", f"Found 8-char hash in filename: {hash_8char} from {f}")
        
        # Calcola torrents mancanti (controlla sia hash completi che troncati)
        missing = set()
        for h in active_hashes:
            if h not in covered:
                # Check anche hash troncato (primi 8 caratteri)
                if h[:8].lower() not in covered_8char:
                    missing.add(h)
                    self.log("DEBUG", f"Missing hash: {h[:8]}... (not in covered_8char set)")
                else:
                    self.log("DEBUG", f"Hash covered by 8-char fallback: {h[:8]}...")
            else:
                self.log("DEBUG", f"Hash covered by full hash: {h[:8]}...")
        
        if not missing:
            self.log("INFO", f"Full coverage already satisfied: tutti i {len(active_hashes)} torrent hanno .mkv (complete: {len(covered)}, 8-char: {len(covered_8char)}) → skip scan finale")
            return False
        
        self.log("INFO", f"Full coverage: {len(missing)} torrent senza .mkv su {len(active_hashes)} (dopo fallback 8-char migliorato)")
        # Log prima parte degli hash mancanti per debug
        missing_preview = list(missing)[:10]
        for missing_hash in missing_preview:
            self.log("DEBUG", f"Still missing: {missing_hash[:8]}...")
        if len(missing) > 10:
            self.log("DEBUG", f"... e altri {len(missing)-10} hash mancanti")
        self.log("INFO", f"→ eseguo scan finale per processare i {len(missing)} torrents rimasti")
        return True

    def _collect_video_files(self, file_stats: list) -> list:
        """Collect all video files regardless of extension."""
        video_extensions = ('.mkv', '.mp4', '.avi', '.mov', '.webm', '.m4v')
        return [f for f in file_stats if str(f.get("path","")).lower().endswith(video_extensions)]

    def _wait_for_video_files(self, hash_value: str, expect_4k: bool) -> tuple:
        """
        Attende in modo adattivo la comparsa dei file video nel torrent.
        - Per 4K: estende la finestra fino a METADATA_4K_MAX_WAIT secondi totali.
        - Per non 4K: usa METADATA_MAX_WAIT (già usata nel primo polling get_torrent_info).
        Ritorna (torrent_info_finale, video_files_list)
        """
        max_wait = self.METADATA_4K_MAX_WAIT if expect_4k else self.METADATA_MAX_WAIT
        start = time.time()
        last_len = -1
        poll = 0
        # sequenza di sleep dopo primo get già eseguito fuori: partiamo con 2,3,4,5,6...
        dynamic_sleeps = [2,3,4,5,6,6,6]
        while time.time() - start < max_wait:
            info = self.safe_curl(
                f"{self.TORRSERVER_URL}/torrents",
                method="POST",
                data={"action": "get", "hash": hash_value}
            )
            torrent_info = None
            if info:
                try:
                    torrent_info = info.json()
                except Exception:
                    torrent_info = None
            if torrent_info:
                file_stats = torrent_info.get("file_stats", []) or []
                video_files = self._collect_video_files(file_stats)
                # Se compaiono video files → stop
                if video_files:
                    if expect_4k:
                        self.log("DEBUG", f"4K metadata ok dopo {round(time.time()-start,1)}s (hash {hash_value[:8]}...)")
                    return torrent_info, video_files
                # Logging progress (numero file_stats che cresce = metadata ancora in arrivo)
                if len(file_stats) != last_len:
                    last_len = len(file_stats)
                    self.log("DEBUG", f"Metadata progress ({len(file_stats)} file entries, 0 video) hash {hash_value[:8]}... (elapsed {round(time.time()-start,1)}s)")
            # Attesa successiva
            sleep_time = dynamic_sleeps[poll] if poll < len(dynamic_sleeps) else dynamic_sleeps[-1]
            time.sleep(sleep_time)
            poll += 1
        # Timeout senza mkv
        return torrent_info if 'torrent_info' in locals() else None, []

    # ================== CORE INDEX PERSISTENTE ==================
    def _load_core_index(self, path: str) -> dict:
        """Load core index from JSON file, auto-prune non-existent files"""
        try:
            if os.path.isfile(path):
                with open(path, 'r') as f:
                    data = json.load(f)
                    if isinstance(data, dict):
                        # Filtro percorsi non più esistenti
                        filtered = {k: v for k, v in data.items() if os.path.isfile(v)}
                        if len(filtered) != len(data):
                            try:
                                with open(path, 'w') as wf:
                                    json.dump(filtered, wf)
                            except Exception:
                                pass
                        return filtered
        except Exception:
            pass
        return {}

    def _save_core_index(self, path: str, data: dict):
        """Save core index to JSON file atomically"""
        try:
            tmp = path + ".tmp"
            with open(tmp, 'w') as f:
                json.dump(data, f)
            os.replace(tmp, path)
        except Exception as e:
            self.log("WARN", f"Save core index fail ({os.path.basename(path)}): {e}")

    def _core_key(self, filename: str, is_tv: bool = False) -> str:
        """
        Chiave normalizzata per deduplicare varianti:
          - film: tronca dopo anno (19xx/20xx) 
          - tv: include SxxEyy se presente, altrimenti Sxx per pack
        """
        import re, os
        name = os.path.basename(filename).lower()
        if name.endswith('.mkv'):
            name = name[:-4]
        name = re.sub(r'_[a-f0-9]{8}', '', name)  # Remove hash
        
        if is_tv:
            ep = re.search(r'(s\d+e\d+)', name, re.IGNORECASE)
            if ep:
                base = name.split(ep.group(1))[0]
                base = re.sub(r'(19|20)\d{2}', '', base)
                base = re.sub(r'[\.\-]+', '_', base)
                base = re.sub(r'_+', '_', base).strip('_')
                return f"{base}_{ep.group(1)}"
            season = re.search(r'(s\d+)', name, re.IGNORECASE)
            if season:
                base = name.split(season.group(1))[0]
                base = re.sub(r'(19|20)\d{2}', '', base)
                base = re.sub(r'[\.\-]+', '_', base)
                base = re.sub(r'_+', '_', base).strip('_')
                return f"{base}_{season.group(1)}"
        else:
            year = re.search(r'(19|20)\d{2}', name)
            if year:
                name = name[:year.end()]
        
        name = re.sub(r'[\.\-]+', '_', name)
        name = re.sub(r'_+', '_', name).strip('_')
        return name

    def _normalize_show_name(self, show_name: str) -> str:
        """
        Normalize TV show names for comparison (case-insensitive, punctuation-insensitive)
        Used to detect if two directory names refer to the same TV show
        """
        import re
        
        # Convert to lowercase for case-insensitive comparison
        normalized = show_name.lower()
        
        # Remove common separators and normalize to underscores
        normalized = re.sub(r'[\s\.\-]+', '_', normalized)
        
        # Remove multiple underscores
        normalized = re.sub(r'_+', '_', normalized)
        
        # Remove trailing years that might be in directory names
        normalized = re.sub(r'_(19|20)\d{2}$', '', normalized)
        
        # Strip leading/trailing underscores
        normalized = normalized.strip('_')
        
        return normalized

    def _remove_torrent_site_prefixes(self, show_name: str) -> str:
        """
        Remove common torrent site prefixes from show names.
        Handles prefixes like: BEST-TORRENTS_COM_, www_UIndex_org_-_, www_Torrenting_com_-_, etc.
        """
        import re

        # Common torrent site prefixes to remove
        prefixes = [
            r'^BEST-TORRENTS[_\.]?COM[_\-\.]?',
            r'^DEVIL-TORRENTS[_\.]?COM[_\-\.]?',
            r'^www[_\.]?UIndex[_\.]?org[_\-\.]?',
            r'^www[_\.]?Torrenting[_\.]?com[_\-\.]?',
            r'^www[_\.]?[\w]+[_\.]?(com|org|net)[_\-\.]?',
            r'^\[[\w\s\.\-]+\][_\-\.]?',  # [SiteName] or [SITE-NAME]
        ]

        cleaned = show_name
        for prefix_pattern in prefixes:
            cleaned = re.sub(prefix_pattern, '', cleaned, flags=re.IGNORECASE)

        # Clean up any leading/trailing separators after prefix removal
        cleaned = re.sub(r'^[_\-\.]+', '', cleaned)
        cleaned = re.sub(r'[_\-\.]+$', '', cleaned)

        return cleaned

    def _get_unified_show_directory(self, show_name: str) -> str:
        """
        Get the unified show directory path, checking for existing similar directories.
        If a directory with similar name exists, use that instead of creating a new one.
        This prevents duplicate directories due to capitalization differences.
        """
        # First check if exact directory exists
        exact_path = os.path.join(self.TV_DIR, show_name)
        if os.path.exists(exact_path):
            return show_name
        
        # Check for existing directories with similar normalized names
        if os.path.exists(self.TV_DIR):
            normalized_target = self._normalize_show_name(show_name)
            
            for existing_dir in os.listdir(self.TV_DIR):
                existing_path = os.path.join(self.TV_DIR, existing_dir)
                if os.path.isdir(existing_path):
                    normalized_existing = self._normalize_show_name(existing_dir)
                    if normalized_existing == normalized_target:
                        self.log("DEBUG", f"Found existing similar directory: '{existing_dir}' for '{show_name}'")
                        return existing_dir
        
        # No similar directory found, use original name
        return show_name

    def _rebuild_movie_core_index(self):
        """Rebuild movie core index from filesystem"""
        self._movie_core_index = {}
        if os.path.isdir(self.MOVIES_DIR):
            for f in os.listdir(self.MOVIES_DIR):
                if f.lower().endswith('.mkv'):
                    core = self._core_key(f, is_tv=False)
                    self._movie_core_index.setdefault(core, os.path.join(self.MOVIES_DIR, f))
        self._save_core_index(self.MOVIE_CORE_INDEX_FILE, self._movie_core_index)
        self.log("DEBUG", f"Rebuild movie core index: {len(self._movie_core_index)} entries")

    def _rebuild_tv_core_index(self):
        """Rebuild TV core index from filesystem"""
        self._tv_core_index = {}
        if os.path.isdir(self.TV_DIR):
            for root, _, files in os.walk(self.TV_DIR):
                for f in files:
                    if f.lower().endswith('.mkv'):
                        core = self._core_key(f, is_tv=True)
                        full = os.path.join(root, f)
                        self._tv_core_index.setdefault(core, full)
        self._save_core_index(self.TV_CORE_INDEX_FILE, self._tv_core_index)
        self.log("DEBUG", f"Rebuild TV core index: {len(self._tv_core_index)} entries")

    def _update_movie_core_index(self, filepath: str):
        """Update movie core index with new file"""
        core = self._core_key(os.path.basename(filepath), is_tv=False)
        self._movie_core_index[core] = filepath
        self._save_core_index(self.MOVIE_CORE_INDEX_FILE, self._movie_core_index)

    def _update_tv_core_index(self, filepath: str):
        """Update TV core index with new file"""
        core = self._core_key(os.path.basename(filepath), is_tv=True)
        self._tv_core_index[core] = filepath
        self._save_core_index(self.TV_CORE_INDEX_FILE, self._tv_core_index)

    def _prune_core_entry(self, core: str, is_tv: bool):
        """Remove core entry if file no longer exists"""
        idx = self._tv_core_index if is_tv else self._movie_core_index
        path = idx.get(core)
        if path and not os.path.isfile(path):
            idx.pop(core, None)
            self._save_core_index(self.TV_CORE_INDEX_FILE if is_tv else self.MOVIE_CORE_INDEX_FILE,
                                  self._tv_core_index if is_tv else self._movie_core_index)

    def process_movies(self) -> int:
        """
        Process movies from TMDB and create virtual files
        Exact replica of bash movie processing logic
        Returns number of files created
        """
        self.log("INFO", "=== PROCESSING MOVIES ===")
        
        # Get movies from TMDB
        movie_data = self.get_tmdb_latest_movies()
        if not movie_data:
            self.log("WARN", "No movie data from TMDB")
            return 0
        
        movie_count = len(movie_data)
        self.log("INFO", f"Found {movie_count} movies from TMDB")
        
        total_movie_files = 0
        negative_cache_hits = 0
        negative_cache_added = 0
        
        for movie in movie_data:
            # Extract movie details
            title = movie.get('title') or movie.get('original_title', '')
            tmdb_id = movie.get('id')
            imdb_id = movie.get('imdb_id')
            
            if not title or not tmdb_id:
                continue

            # V135: Blacklist check at MOVIE level (not just torrent level)
            clean_title = self._normalize_title(title)
            if clean_title in self._blacklist.get("titles", []):
                self.log("INFO", f"🚫 Blacklist: skipping movie '{title}' (normalized: {clean_title})")
                continue

            # Get IMDB ID if not available
            if not imdb_id:
                # Get external IDs from TMDB + Watch Providers
                response = self.safe_curl(f"{self.TMDB_BASE_URL}/movie/{tmdb_id}", params={'api_key': self.TMDB_API_KEY, 'append_to_response': 'external_ids,watch/providers'})
                if response:
                    try:
                        data = response.json()
                        external_ids = data.get('external_ids', {})
                        imdb_id = external_ids.get('imdb_id')
                        
                        # FASE 6.2: Premium IT Streaming Bypass for Movies
                        original_language = movie.get('original_language', '')
                        if original_language not in ['en', 'it']:
                            providers = data.get('watch/providers', {}).get('results', {}).get('IT', {}).get('flatrate', [])
                            is_premium_it = any(p.get('provider_id') in self.PREMIUM_PROVIDER_IDS for p in providers)
                            
                            if not is_premium_it:
                                self.log("DEBUG", f"Skipping non-premium international movie: {title} ({original_language})")
                                continue
                            self.log("INFO", f"🌟 Premium IT Bypass: '{title}' ({original_language}) found on Italian streaming service")
                    except (json.JSONDecodeError, Exception):
                        pass
            
            if not imdb_id:
                self.log("WARN", f"No IMDB ID for movie: {title}")
                continue
            
            self.log("INFO", f"Processing movie: {title} ({imdb_id})")
            
            # Get streams from Torrentio
            streams_data = self.get_torrentio_streams(imdb_id, "movie")
            streams = streams_data.get("streams", [])
            
            if not streams:
                self.log("WARN", f"No streams found for movie: {title}")
                continue
            
            # Resolution-based filtering: try 4K first, then 1080p fallback
            valid_streams = []
            
            # First try 4K streams
            valid_4k = self._filter_streams_by_resolution(streams, "Torrentio\n4k")
            if valid_4k:
                self.log("DEBUG", f"Found {len(valid_4k)} valid 4K streams for {title}")
                valid_streams = valid_4k
            else:
                # Fallback to 1080p streams
                valid_1080p = self._filter_streams_by_resolution(streams, "Torrentio\n1080p")
                if valid_1080p:
                    self.log("DEBUG", f"Found {len(valid_1080p)} valid 1080p streams for {title}")
                    valid_streams = valid_1080p
            
            if not valid_streams:
                self.log("INFO", f"Skip movie (no valid streams meet criteria): {title}")
                continue
            
            # Find best stream that's not already present AND better than existing
            best_stream = None
            
            # Initialize score with existing version if present
            # Simple glob-based search for existing movies with similar titles
            import glob
            # Use first word of title for broad search, then filter for relevance
            first_word = title.split()[0] if title.split() else title
            search_pattern = os.path.join(self.MOVIES_DIR, f"*{first_word}*.mkv")
            matching_files = glob.glob(search_pattern)
            
            # Filter for title relevance (both words must be present in filename)
            existing_mkv = None
            for f in matching_files:
                filename_lower = os.path.basename(f).lower()
                title_words = [word.lower() for word in title.split()]
                # Check if all title words are present in filename (fuzzy match)
                if all(word in filename_lower for word in title_words):
                    existing_mkv = f
                    break

            if existing_mkv:
                # Get existing score for comparison
                existing_filename = os.path.basename(existing_mkv)
                existing_hash_match = re.search(r'_([a-f0-9]{8})\.mkv$', existing_filename)
                if existing_hash_match:
                    existing_hash = existing_hash_match.group(1)
                    # Get full hash and torrent title for accurate scoring
                    existing_torrent_info = self._get_full_torrent_info(existing_hash)
                    if existing_torrent_info:
                        existing_title = existing_torrent_info.get('title', '')
                        best_score = self.calculate_quality_score(existing_title, seeders=0)
                        self.log("INFO", f"Existing movie score baseline: {best_score} (title: {existing_title[:60]}...)")
                    else:
                        # Fallback to filename scoring
                        best_score = self.calculate_quality_score(existing_filename, seeders=0)
                        self.log("INFO", f"Existing movie score baseline (filename): {best_score}")
                else:
                    best_score = self.calculate_quality_score(existing_filename, seeders=0)
                    self.log("INFO", f"Existing movie score baseline (no hash): {best_score}")
            else:
                best_score = 0
                self.log("INFO", f"No existing movie found, starting from score 0")

            for stream_candidate in valid_streams:
                candidate_title = stream_candidate.get("title", "")
                candidate_hash = stream_candidate.get("infoHash", "")
                candidate_score = stream_candidate.get("quality_score", 0)
                
                if not candidate_title or not candidate_hash:
                    continue
                
                # Skip if hash already present
                if self.check_existing_mkv_for_hash(candidate_hash):
                    self.log("DEBUG", f"Skip candidate (hash already present): {candidate_title[:60]}...")
                    continue
                
                # Skip if in negative cache
                if self._negative_cache_has(candidate_hash):
                    negative_cache_hits += 1  # PATCH: incrementa counter
                    self.log("DEBUG", f"Skip candidate (negative cache): {candidate_title[:60]}...")
                    continue
                
                self.log("INFO", f"Candidate stream: {candidate_title[:60]}... -> score: {candidate_score}")
                
                if candidate_score > best_score:
                    best_stream = stream_candidate
                    best_score = candidate_score
            
            if not best_stream:
                self.log("INFO", f"Skip movie (no better streams available): {title}")
                continue
            
            # Use the best stream found
            stream = best_stream
            stream_title = stream.get("title", "")
            info_hash = stream.get("infoHash", "")
            resolution = "4K" if best_stream.get("name", "").startswith("Torrentio\n4k") else "1080p"
            
            self.log("INFO", f"Selected best {resolution} stream for {title}: {stream_title[:60]}... (score: {best_score})")
            
            # Create magnet URL
            magnet_url = self._build_magnet(info_hash)
            
            # Generate quality filename with hash suffix
            quality_filename = self.extract_quality_filename(stream_title, info_hash)
            target_mkv = os.path.join(self.MOVIES_DIR, f"{quality_filename}.mkv")
            
            # UPGRADE CHECK UNIFICATO (evita doppio log / doppia eval)
            core = self._core_key(f"{quality_filename}.mkv", is_tv=False)
            core_existing_path = self._movie_core_index.get(core)
            file_size_bytes = self.extract_size_from_title(stream_title)
            old_existing_path = None

            candidate_existing = None
            if core_existing_path and os.path.isfile(core_existing_path):
                candidate_existing = core_existing_path
            else:
                probe_existing = self.find_existing_variant(target_mkv)
                if probe_existing:
                    candidate_existing = probe_existing

            if candidate_existing:
                # Se il file è esattamente lo stesso hash lo salteremo già prima (hash check)
                stream_name = stream.get("name", "")
                if self.should_replace_mkv(candidate_existing, stream_title, file_size_bytes, stream_name):
                    self.log("INFO", f"Upgrade movie: nuova variante migliore di {os.path.basename(candidate_existing)}")
                    old_existing_path = candidate_existing
                else:
                    self.log("INFO", f"Skip movie (versione adeguata già presente): {os.path.basename(candidate_existing)}")
                    continue
            
            # SYNC: Clear negative cache before attempting upgrade/add (prevents stale negative entries)
            if hash_value_from_magnet := self.extract_hash_from_magnet(magnet_url):
                if self._negative_cache.pop(hash_value_from_magnet.lower(), None):
                    self._save_negative_cache()
                    self.log("DEBUG", f"Cleared stale negative cache entry before adding: {hash_value_from_magnet[:8]}...")
            
            # Add torrent to server
            stat_string, hash_value = self.add_torrent_to_server(magnet_url, title)
            
            if not stat_string or not hash_value:
                self.log("ERROR", f"Failed to add torrent for movie: {title}")
                continue
            
            if stat_string in ["Torrent added", "Torrent working", "Torrent getting info"]:
                self.log_success(f"Successfully added torrent: {title} (hash: {hash_value[:8]}...)")
                # Validate hash format
                if not re.match(r'^[a-fA-F0-9]{40}$', hash_value):
                    self.log("ERROR", f"Invalid hash format: {hash_value}")
                    continue
                self.log("INFO", f"Hash format validated: {hash_value}")
                
                torrent_info = self.get_torrent_info(hash_value)
                if not torrent_info:
                    self.log("WARN", f"Could not get torrent info for: {title} - creating bash-style fallback")
                    if self.create_bash_style_mkv(title, hash_value, stream_title, expect_4k):
                        fallback_name = self.build_original_pattern_filename(stream_title, hash_value) + ".mkv"
                        fallback_path = os.path.join(self.MOVIES_DIR, fallback_name)
                        if os.path.isfile(fallback_path):
                            self._update_movie_core_index(fallback_path)
                        self.log_success(f"Created bash-style fallback for: {title}")
                    else:
                        self.log("WARN", f"Fallback also failed for: {title} - removing torrent")
                        self.remove_torrent_from_server(hash_value)
                    continue

                file_stats = torrent_info.get("file_stats", []) or []
                video_files = self._collect_video_files(file_stats)

                expect_4k = bool(re.search(r'2160p|4[kK]|UHD', stream_title, re.IGNORECASE))

                # Se nessun video iniziale → attesa adattiva
                if not video_files:
                    self.log("INFO", f"Nessun video (fase iniziale) per '{title}' – avvio attesa adattiva ({'4K' if expect_4k else 'STD'})")
                    torrent_info_ext, video_files_ext = self._wait_for_video_files(hash_value, expect_4k)
                    if video_files_ext:
                        torrent_info = torrent_info_ext or torrent_info
                        file_stats = torrent_info.get("file_stats", []) or file_stats
                        video_files = video_files_ext
                    else:
                        reason = "metadata_timeout_4k" if expect_4k else "no_video"
                        if reason == "metadata_timeout_4k":
                            # Fallback bash-style: create anyway with estimated values
                            self.log("INFO", f"4K metadata timeout for '{title}' - creating bash-style file")
                            if self.create_bash_style_mkv(title, hash_value, stream_title, expect_4k):
                                # RECOVERY: aggiorna core index + cleanup upgrade
                                # Ricostruisci nome file creato
                                fallback_name = self.build_original_pattern_filename(stream_title, hash_value) + ".mkv"
                                fallback_path = os.path.join(self.MOVIES_DIR, fallback_name)
                                if os.path.isfile(fallback_path):
                                    self._update_movie_core_index(fallback_path)
                                    self.remove_existing_torrent_if_any(fallback_path)
                                self.log_success(f"Created bash-style fallback for: {title}")
                                created_files = 1
                                total_movie_files += 1  # Fix: increment total counter for accurate stats
                            else:
                                self.log("WARN", f"Failed to create bash-style fallback for: {title}")
                                self.remove_torrent_from_server(hash_value)
                        else:
                            self.log("INFO", f"Skip: nessun .mkv dopo attesa estesa ({reason}) '{title}' - removing torrent")
                            self.remove_torrent_from_server(hash_value)
                            self._negative_cache_add(info_hash, reason)
                            negative_cache_added += 1
                        continue
                
                self.log_success(f"Metadata ready for: {title}")
                
                # Process files with quality-based size filter (EXACT BASH LOGIC)
                valid_files = []
                # Flag qualità dal titolo stream (selezionato a monte)
                expect_4k = bool(re.search(r'2160p|4[kK]|UHD', stream_title, re.IGNORECASE))
                for file_stat in file_stats:
                    file_length = file_stat.get("length", 0)
                    file_path = file_stat.get("path", "")
                    file_id = file_stat.get("id", "")
                    
                    # Check if it's a video file
                    video_extensions = ('.mkv', '.mp4', '.avi', '.mov', '.webm', '.m4v')
                    if not file_path.lower().endswith(video_extensions):
                        continue
                    
                    # Trattiamo come 4K se (a) path contiene marker O (b) lo stream era 4K (fallback bash‑style)
                    path_is_4k = bool(re.search(r'2160p|4[kK]|UHD', file_path, re.IGNORECASE))
                    is_4k_content = path_is_4k or expect_4k
                    
                    if is_4k_content:
                        min_size = self.MOVIE_4K_MIN_SIZE
                        tier = "4K"
                    else:
                        min_size = self.MOVIE_1080P_MIN_SIZE
                        tier = "1080p/720p"
                    
                    if file_length >= min_size:
                        valid_files.append((file_path, file_id, file_length))
                        self.log("DEBUG", f"Accept {tier} file: {os.path.basename(file_path)} ({round(file_length / self.BYTES_PER_GB, 1)}GB)")
                    else:
                        gb_size = round(file_length / self.BYTES_PER_GB, 1)
                        min_gb = round(min_size / self.BYTES_PER_GB, 1)
                        self.log("DEBUG", f"Skip {tier} file too small: {os.path.basename(file_path)} ({gb_size}GB < {min_gb}GB)")
                
                if not valid_files:
                    # Diagnostica: logga eventuali video file visti (anche se scartati)
                    if video_files:
                        for fs in video_files:
                            fp = fs.get("path","")
                            flen = fs.get("length",0)
                            if fp.lower().endswith(".mkv"):
                                gb = round(flen / self.BYTES_PER_GB, 2)
                                tier = "4K" if expect_4k else "1080p"
                                min_gb = self.MOVIE_4K_MIN_GB if expect_4k else self.MOVIE_1080P_MIN_GB
                                self.log("DEBUG", f"Discarded mkv candidate: {os.path.basename(fp)} ({gb}GB) "
                                                  f"threshold={tier} min={min_gb}GB")
                    self.log("WARN", f"No valid files found for movie: {title} -> removing torrent + negative cache")
                    self.remove_torrent_from_server(hash_value)
                    self._negative_cache_add(info_hash, "no_valid_files")
                    negative_cache_added += 1
                    continue
                
                self.log("DEBUG", f"Found {len(valid_files)} valid files for {title}")

                # Take only the largest valid file (one .mkv per movie torrent)
                file_path, file_id, file_length = max(valid_files, key=lambda x: x[2])
                filename = os.path.basename(file_path)
                clean_filename = self.build_original_pattern_filename(filename, hash_value)
                video_file = os.path.join(self.MOVIES_DIR, f"{clean_filename}.mkv")

                stream_url = f"{self.TORRSERVER_URL}/stream?link={hash_value}&index={file_id}&play"

                created_files = 0
                last_created_mkv = ""
                if self.create_mkv_with_metadata(video_file, stream_url, str(file_length), magnet_url, imdb_id):
                    self.log_success(f"Created virtual file: {os.path.basename(video_file)}")
                    last_created_mkv = video_file
                    created_files = 1
                    total_movie_files += 1
                    self._update_movie_core_index(video_file)
                    # Keep _hash_cache fresh for dedup within same run
                    if hasattr(self, '_hash_cache'):
                        self._hash_cache[hash_value.lower()] = video_file
                
                self.log("INFO", f"Created {created_files} files for movie: {title}")
                
                # SAFE UPGRADE CLEANUP: Remove existing torrent AFTER successful creation
                if last_created_mkv:
                    self.log("INFO", f"UPGRADE: Starting safe cleanup for movie: {title}")
                    self.remove_existing_torrent_if_any(last_created_mkv)
                    # Remove hash-based duplicates after cleanup
                    self.remove_movie_duplicates_by_hash(self.MOVIES_DIR)
                elif old_existing_path and os.path.isfile(old_existing_path):
                    self.log("INFO", f"UPGRADE: Starting cleanup for old variant: {title}")
                    self.remove_existing_torrent_if_any(old_existing_path)
                    # Remove hash-based duplicates after cleanup
                    self.remove_movie_duplicates_by_hash(self.MOVIES_DIR)
                else:
                    self.log("INFO", f"UPGRADE: No cleanup needed - no files created for: {title}")
            
            # Add processing pause like bash version
            time.sleep(self.PROCESS_INTERVAL)
        
        self.log("INFO", f"=== MOVIES COMPLETE: {total_movie_files} files created ===")
        self.log("INFO", f"Movies negative cache stats: hits={negative_cache_hits} added={negative_cache_added} active={len(self._negative_cache)} TTLh={self.NEGATIVE_CACHE_TTL_HOURS}")
        return total_movie_files

    def process_tv_shows(self) -> int:
        """
        Process TV shows from TMDB and create virtual files
        Exact replica of bash TV processing logic
        Returns number of files created
        """
        self.log("INFO", "=== PROCESSING TV SHOWS ===")
        
        # Get TV shows from TMDB
        tv_data = self.get_tmdb_latest_tv()
        if not tv_data:
            self.log("WARN", "No TV data from TMDB")
            return 0

        tv_count = len(tv_data)
        self.log("INFO", f"Found {tv_count} TV shows from TMDB")

        total_tv_files = 0
        tv_negative_cache_hits = 0
        tv_negative_cache_added = 0
        fullpack_cache_hits = 0

        for show in tv_data:
            # Extract show details
            title = show.get('name') or show.get('original_name', '')
            tmdb_id = show.get('id')
            
            if not title or not tmdb_id:
                continue
            
            # Get IMDB ID from external_ids API
            response = self.safe_curl(f"{self.TMDB_BASE_URL}/tv/{tmdb_id}/external_ids", params={'api_key': self.TMDB_API_KEY})
            imdb_id = None
            if response:
                try:
                    data = response.json()
                    imdb_id = data.get('imdb_id')
                except (json.JSONDecodeError, Exception):
                    pass
            
            if not imdb_id:
                self.log("WARN", f"No IMDB ID for TV show: {title}")
                continue

            # Add to TV library cache to prevent future deletion
            self._add_to_tv_library_cache(tmdb_id, imdb_id, title)

            self.log("INFO", f"Processing TV show: {title} ({imdb_id})")
            
            # Get streams from Torrentio
            streams_data = self.get_torrentio_streams(imdb_id, "series")
            streams = streams_data.get("streams", [])
            
            if not streams:
                self.log("WARN", f"No streams found for TV show: {title}")
                continue
            
            created_files = 0

            # >>> PATCH REPLACEMENT START (USO FILTRO name) <<<
            # Nuova selezione prioritaria basata su stream['name'] (come film)
            selected_fullpacks, selected_single_episode_streams, season_has_fullpack = self._prioritize_tv_streams_by_name(streams)
            fullpack_streams = selected_fullpacks
            single_episode_streams = selected_single_episode_streams
            self.log("DEBUG", f"[TV/NAME] fullpacks: {len(fullpack_streams)} singles: {len(single_episode_streams)}")
            
            # PATCH 4: TV fullpack fallback logic
            if self.TV_FULLPACK_ONLY and not fullpack_streams and single_episode_streams:
                self.log("INFO", f"[TV] FULLPACK_ONLY fallback: uso {len(single_episode_streams)} episodi (tier migliore)")
                fullpack_streams = []
                single_episode_streams = single_episode_streams
            # >>> PATCH REPLACEMENT END <<<
            
            for stream in fullpack_streams:
                # Initialize for each TV stream to prevent UnboundLocalError
                last_created_mkv = ""
                is_fullpack = True  # This is the fullpack processing phase
                stream_title = stream.get("title", "")
                info_hash = stream.get("infoHash", "")
                
                if not stream_title or not info_hash:
                    continue
                
                # Solo fullpack? (skip episodi singoli se abilitato)
                if self.TV_FULLPACK_ONLY and not self._is_fullpack_title(stream_title):
                    self.log("INFO", f"Skip episodio singolo (TV_FULLPACK_ONLY attivo): {stream_title}")
                    continue
                
                # Create magnet URL
                magnet_url = self._build_magnet(info_hash)

                if self._negative_cache_has(info_hash):
                    tv_negative_cache_hits += 1
                    self.log("DEBUG", f"Skip (negative cache no-mkv TTL active) hash {info_hash[:8]}... for TV '{stream_title}'")
                    continue
                
                # SECONDARY DEFENSE: Skip if exact hash already present (after quality comparison)
                existing_same_hash_tv = self.check_existing_mkv_for_hash(info_hash)
                if existing_same_hash_tv:
                    self.log("INFO", f"Skip TV (exact hash already present) -> {os.path.basename(existing_same_hash_tv)}")
                    continue
                
                # Generate quality filename with hash suffix
                quality_filename = self.extract_quality_filename(stream_title, info_hash)
                
                # Determine if it's a season pack or episode
                # CRITICAL: Always use TMDB title for show directory, never stream title
                # Use the clean TMDB title directly - no need to remove prefixes since TMDB doesn't have them
                show_name = re.sub(r'[^a-zA-Z0-9._-]', '_', title)
                show_name = re.sub(r'_+', '_', show_name).strip('_')
                
                # Ensure show_name doesn't contain episode info (safety check)
                # Remove any episode patterns that might have contaminated the title
                show_name = re.sub(r'[Ss][0-9]+[Ee][0-9]+.*', '', show_name)
                show_name = re.sub(r'_+', '_', show_name).strip('_')
                
                # Debug logging to track directory naming
                self.log("DEBUG", f"TV directory naming - TMDB title: '{title}' -> show_name: '{show_name}'")
                
                # NEW: Check if we should replace existing fullpack with better quality
                unified_show_name = self._get_unified_show_directory(show_name)
                existing_show_dir = os.path.join(self.TV_DIR, unified_show_name)
                stream_name = stream.get("name", "")
                
                # If show exists, check if we should upgrade it
                if os.path.exists(existing_show_dir):
                    if not self.should_replace_tv_fullpack(existing_show_dir, stream_title, stream_name):
                        # Skip this stream - existing is better or equivalent
                        continue
                    else:
                        # Remove existing show directory for upgrade
                        try:
                            import shutil
                            
                            # First, collect all hash values from existing files for cleanup
                            hashes_to_remove = []
                            for root, dirs, files in os.walk(existing_show_dir):
                                for file in files:
                                    if file.lower().endswith('.mkv'):
                                        file_path = os.path.join(root, file)
                                        try:
                                            with open(file_path, 'r') as f:
                                                url_line = f.readline().strip()
                                            hash_match = re.search(r'link=([a-f0-9]{40})', url_line, re.IGNORECASE)
                                            if hash_match:
                                                hashes_to_remove.append(hash_match.group(1).lower())
                                        except (IOError, Exception):
                                            continue
                            
                            # Remove the directory
                            shutil.rmtree(existing_show_dir)
                            self.log("INFO", f"TV fullpack upgrade: removed existing show directory {unified_show_name}")
                            
                            # Remove old torrents from GoStorm
                            for old_hash in hashes_to_remove:
                                if self.remove_torrent_from_server(old_hash):
                                    self.log("INFO", f"Removed old TV torrent from GoStorm: {old_hash[:8]}...")
                                else:
                                    self.log("WARN", f"Failed to remove old TV torrent: {old_hash[:8]}...")
                            
                        except Exception as e:
                            self.log("WARN", f"Failed to remove existing TV show directory: {e}")
                            continue
                
                # Extract season number from stream title (fallback for single episodes)
                season_match = re.search(r'[Ss]([0-9]+)', stream_title)
                season_num = int(season_match.group(1)) if season_match else 1
                # Use unified directory to prevent duplicates due to capitalization
                season_dir = os.path.join(self.TV_DIR, unified_show_name, f"Season.{season_num:02d}")
                
                target_mkv = os.path.join(season_dir, f"{quality_filename}.mkv")
                
                # CORE INDEX CHECK per TV
                core = self._core_key(f"{quality_filename}.mkv", is_tv=True)
                core_existing_tv = self._tv_core_index.get(core)
                
                if core_existing_tv and os.path.isfile(core_existing_tv):
                    file_size_bytes = self.extract_size_from_title(stream_title)
                    stream_name = stream.get("name", "")
                    if not self.should_replace_mkv(core_existing_tv, stream_title, file_size_bytes, stream_name):
                        self.log("INFO", f"Skip TV (core presente, no upgrade): {os.path.basename(core_existing_tv)}")
                        continue
                    else:
                        self.log("INFO", f"Upgrade TV: nuova variante migliore di {os.path.basename(core_existing_tv)}")
                
                # Check if better version already exists (fallback)
                existing = self.find_existing_variant(target_mkv)
                if existing:
                    file_size_bytes = self.extract_size_from_title(stream_title)
                    stream_name = stream.get("name", "")
                    if not self.should_replace_mkv(existing, stream_title, file_size_bytes, stream_name):
                        self.log("INFO", f"Skip: versione migliore già presente (found: {os.path.basename(existing)}), nessun add torrent per TV '{stream_title}'")
                        # FIXED: No cleanup on SKIP - existing file is SUPERIOR, not inferior
                        # Removing cleanup call to prevent orphaning the good file
                        continue
                
                # SYNC: Clear negative cache before attempting TV torrent add
                if hash_value_from_magnet := self.extract_hash_from_magnet(magnet_url):
                    if self._negative_cache.pop(hash_value_from_magnet.lower(), None):
                        self._save_negative_cache()
                        self.log("DEBUG", f"Cleared stale negative cache entry before adding TV: {hash_value_from_magnet[:8]}...")
                
                # Add torrent to server
                stat_string, hash_value = self.add_torrent_to_server(magnet_url, stream_title)
                
                if not stat_string or not hash_value:
                    continue
                
                if stat_string in ["Torrent added", "Torrent working", "Torrent getting info"]:
                    self.log_success(f"Successfully added TV torrent: {stream_title} (hash: {hash_value[:8]}...)")
                    
                    # Validate hash and torrent
                    if not re.match(r'^[a-fA-F0-9]{40}$', hash_value):
                        continue
                    
                    torrent_info = self.get_torrent_info(hash_value)
                    if not torrent_info:
                        continue

                    file_stats = torrent_info.get("file_stats", []) or []
                    video_files = self._collect_video_files(file_stats)
                    expect_4k_tv = bool(re.search(r'2160p|4[kK]|UHD', stream_title, re.IGNORECASE))

                    if not video_files:
                        self.log("INFO", f"Nessun video (fase iniziale) per TV '{stream_title}' – attesa adattiva ({'4K' if expect_4k_tv else 'STD'})")
                        torrent_info_ext, video_files_ext = self._wait_for_video_files(hash_value, expect_4k_tv)
                        if video_files_ext:
                            torrent_info = torrent_info_ext or torrent_info
                            file_stats = torrent_info.get("file_stats", []) or file_stats
                            video_files = video_files_ext
                        else:
                            reason = "metadata_timeout_4k" if expect_4k_tv else "no_video"
                            if reason == "metadata_timeout_4k":
                                self.log("INFO", f"4K metadata timeout for TV '{stream_title}' - creating bash-style file")
                                if self.create_bash_style_mkv(stream_title, hash_value, stream_title, expect_4k_tv, is_tv=True, season_dir=season_dir):
                                    fallback_name = self.build_original_pattern_filename(stream_title, hash_value) + ".mkv"
                                    fallback_path = os.path.join(season_dir, fallback_name)
                                    if os.path.isfile(fallback_path):
                                        self._update_tv_core_index(fallback_path)
                                        # Cleanup solo se episodio singolo (fullpack ignoto senza metadata completo non si tocca)
                                        self.remove_existing_torrent_if_any(fallback_path)
                                    self.log_success(f"Created bash-style TV fallback for: {stream_title}")
                                    created_files += 1         # Fix: increment counters for accurate stats
                                    total_tv_files += 1         # Fix: increment counters for accurate stats
                                    continue                    # Fix: avoid zombie flow with empty mkv_files
                                else:
                                    self.log("WARN", f"Failed to create bash-style TV fallback for: {stream_title}")
                                    self.remove_torrent_from_server(hash_value)
                            else:
                                self.log("INFO", f"Skip: nessun .mkv dopo attesa estesa ({reason}) TV '{stream_title}' - removing torrent")
                                self.remove_torrent_from_server(hash_value)
                                self._negative_cache_add(info_hash, reason)
                                tv_negative_cache_added += 1
                            continue
                    
                    # Create directory for TV show season
                    os.makedirs(season_dir, exist_ok=True)
                    
                    # Check if this is a fullpack with multiple episodes
                    # Rilevazione fullpack più conservativa
                    is_fullpack = False
                    if self._is_fullpack_title(stream_title):
                        is_fullpack = True
                    else:
                        if self._is_real_fullpack_from_files(video_files):
                            is_fullpack = True
                    self.log("DEBUG", f"Fullpack detection: title_flag={self._is_fullpack_title(stream_title)} files_flag={self._is_real_fullpack_from_files(video_files)} -> {is_fullpack}")
                    
                    # TV SERIES UPGRADE LOGIC: Check for existing fullpack series and compare quality
                    if is_fullpack:
                        existing_tv_directories = []
                        if os.path.exists(self.TV_DIR):
                            for item in os.listdir(self.TV_DIR):
                                item_path = os.path.join(self.TV_DIR, item)
                                if os.path.isdir(item_path):
                                    # Check if this directory is for the same show (case-insensitive comparison)
                                    if self._normalize_show_name(item) == self._normalize_show_name(show_name):
                                        existing_tv_directories.append(item_path)
                        
                        # If we found existing directories for the same show, check if we should upgrade
                        if existing_tv_directories:
                            for existing_dir in existing_tv_directories:
                                if existing_dir == os.path.join(self.TV_DIR, show_name):
                                    continue  # Skip our own new directory
                                
                                # Find any .mkv files in the existing directory to extract torrent info
                                existing_video_files = []
                                video_extensions = ('.mkv', '.mp4', '.avi', '.mov', '.webm', '.m4v')
                                for root, dirs, files in os.walk(existing_dir):
                                    for file in files:
                                        if file.lower().endswith(video_extensions):
                                            existing_video_files.append(os.path.join(root, file))
                                
                                if not existing_video_files:
                                    continue
                                
                                # Get torrent info from first video file to compare
                                try:
                                    with open(existing_video_files[0], 'r') as f:
                                        existing_stream_url = f.readline().strip()
                                    
                                    # Extract hash from existing stream URL
                                    existing_hash_match = re.search(r'link=([a-f0-9]{40})', existing_stream_url, re.IGNORECASE)
                                    if not existing_hash_match:
                                        continue
                                    
                                    existing_hash = existing_hash_match.group(1).lower()
                                    existing_torrent_info = self.get_torrent_info(existing_hash)
                                    
                                    if existing_torrent_info:
                                        # Create torrent objects for comparison
                                        existing_torrent = {
                                            "title": existing_torrent_info.get("title", ""),
                                            "size": existing_torrent_info.get("length", 0),
                                            "hash": existing_hash
                                        }
                                        
                                        new_torrent = {
                                            "title": stream_title,
                                            "size": torrent_info.get("length", 0),
                                            "hash": hash_value.lower()
                                        }
                                        
                                        # Compare torrents and decide if we should upgrade
                                        if self.should_replace_tv_series(existing_torrent, new_torrent, existing_dir):
                                            self.log("INFO", f"TV series upgrade detected - removing inferior version: {os.path.basename(existing_dir)}")
                                            
                                            # Remove all video files from inferior version
                                            removed_files = 0
                                            for video_file in existing_video_files:
                                                try:
                                                    os.remove(video_file)
                                                    removed_files += 1
                                                    self.log("DEBUG", f"Removed inferior TV file: {os.path.basename(video_file)}")
                                                except Exception as e:
                                                    self.log("WARN", f"Failed to remove TV file {video_file}: {e}")
                                            
                                            # Remove directory if empty
                                            try:
                                                if removed_files > 0:
                                                    # Remove empty subdirectories
                                                    for root, dirs, files in os.walk(existing_dir, topdown=False):
                                                        if not files and not dirs:
                                                            os.rmdir(root)
                                                    
                                                    # Try to remove main directory if empty
                                                    if not os.listdir(existing_dir):
                                                        os.rmdir(existing_dir)
                                                        self.log("INFO", f"Removed empty TV directory: {os.path.basename(existing_dir)}")
                                            except Exception as e:
                                                self.log("WARN", f"Failed to cleanup TV directory {existing_dir}: {e}")
                                            
                                            # Remove torrent from GoStorm
                                            if self.remove_torrent_from_server(existing_hash):
                                                self.log("INFO", f"Removed inferior TV torrent from GoStorm: {existing_hash[:8]}...")
                                            else:
                                                self.log("WARN", f"Failed to remove TV torrent from GoStorm: {existing_hash[:8]}...")
                                        
                                        else:
                                            self.log("INFO", f"TV series keep existing - new version is not better: {os.path.basename(existing_dir)}")
                                
                                except Exception as e:
                                    self.log("WARN", f"Error comparing TV series torrents: {e}")
                                    continue
                    
                    if is_fullpack:
                        # Check if fullpack torrent is already in GoStorm (not cache - we need to create files anyway)
                        cache_key = self._fullpack_key(show_name, season_num, hash_value)
                        torrent_already_added = cache_key in self._fullpack_cache
                        
                        if torrent_already_added:
                            fullpack_cache_hits += 1
                            self.log("DEBUG", f"Fullpack torrent already in GoStorm (cache hit): {stream_title} ({hash_value[:8]}...)")
                            # Torrent already added, but we still need to create/verify .mkv files
                        else:
                            self._fullpack_cache[cache_key] = int(time.time())
                            self._save_fullpack_cache()
                            self.log("DEBUG", f"Added fullpack torrent to cache: {stream_title} ({hash_value[:8]}...)")
                        # FULLPACK: Create one video file for each episode with correct index
                        self.log("DEBUG", f"TV Fullpack detected with {len(video_files)} episodes")
                        
                        for file_stat in video_files:
                            file_path = file_stat.get("path", "")
                            file_id = file_stat.get("id", "")
                            file_length = file_stat.get("length", 0)
                            
                            # FIXED: Apply TV size filtering for fullpack episodes
                            if file_length < self.TV_SERIES_MIN_SIZE or file_length > self.TV_SERIES_MAX_SIZE:
                                gb_size = round(file_length / self.BYTES_PER_GB, 1)
                                min_gb = round(self.TV_SERIES_MIN_SIZE / self.BYTES_PER_GB, 1)
                                max_gb = round(self.TV_SERIES_MAX_SIZE / self.BYTES_PER_GB, 1)
                                self.log("DEBUG", f"Skip TV fullpack episode out of range: {os.path.basename(file_path)} ({gb_size}GB not in {min_gb}-{max_gb}GB)")
                                continue
                            
                            # Extract filename from path
                            filename = os.path.basename(file_path)

                            # FIXED: Extract season number from individual file for fullpack
                            file_season_num = self._extract_season_number(filename)
                            if file_season_num < 0:
                                file_season_num = season_num  # fallback to stream title

                            # Create correct season directory for this specific episode
                            file_season_dir = os.path.join(self.TV_DIR, unified_show_name, f"Season.{file_season_num:02d}")
                            os.makedirs(file_season_dir, exist_ok=True)

                            # Use consistent naming pattern with release group + hash8
                            clean_filename = self.build_original_pattern_filename(filename, hash_value)
                            episode_mkv = os.path.join(file_season_dir, f"{clean_filename}.mkv")
                            # Idempotence per-episode: skip creation if file already exists and has valid content
                            if os.path.isfile(episode_mkv):
                                try:
                                    # minimal validity check: first line starts with torrent stream url pattern
                                    with open(episode_mkv, 'r') as ef:
                                        first_line = ef.readline().strip()
                                    if 'stream?link=' in first_line:
                                        self.log("DEBUG", f"Skip existing episode file (idempotent): {os.path.basename(episode_mkv)}")
                                        continue
                                except Exception:
                                    pass  # fallthrough to recreate if unreadable
                            
                            # Create stream URL WITH index for specific episode
                            stream_url = f"{self.TORRSERVER_URL}/stream?link={hash_value}&index={file_id}&play"
                            
                            if self.create_mkv_with_metadata(episode_mkv, stream_url, str(file_length), magnet_url):
                                self.log_success(f"Created TV episode: {os.path.basename(episode_mkv)}")
                                last_created_mkv = episode_mkv
                                created_files += 1
                                total_tv_files += 1
                                # UPDATE INDICE TV (episodio fullpack)
                                self._update_tv_core_index(episode_mkv)
                        
                        # Dopo aver creato gli episodi del fullpack, pulizia varianti singole precedenti
                        if self.TV_FULLPACK_ONLY:
                            self._cleanup_single_episode_variants_after_fullpack(season_dir, hash_value)
                        
                        # FULLPACK REPLACEMENT: Remove any existing single episodes for this season
                        season_num = self._extract_season_number(stream_title)
                        if season_num >= 0:
                            self._replace_single_episodes_with_fullpack(show_name, season_num, hash_value)
                    else:
                        # SINGLE EPISODE: Create one video file using actual filename from metadata
                        # For single episodes, find the video file
                        video_file = video_files[0] if video_files else None
                        if video_file:
                            file_id = video_file.get("id", "")
                            file_length = video_file.get("length", 0)
                            file_path = video_file.get("path", "")
                            
                            # FIXED: Apply TV size filtering for single episodes
                            if file_length < self.TV_SERIES_MIN_SIZE or file_length > self.TV_SERIES_MAX_SIZE:
                                gb_size = round(file_length / self.BYTES_PER_GB, 1)
                                min_gb = round(self.TV_SERIES_MIN_SIZE / self.BYTES_PER_GB, 1)
                                max_gb = round(self.TV_SERIES_MAX_SIZE / self.BYTES_PER_GB, 1)
                                self.log("INFO", f"Skip TV single episode out of range: {stream_title} ({gb_size}GB not in {min_gb}-{max_gb}GB)")
                                self.remove_torrent_from_server(hash_value)
                                continue
                            
                            # FIXED: Use actual filename with consistent naming pattern + hash8
                            # This matches bash behavior and ensures consistency with fullpack naming
                            actual_filename = os.path.basename(file_path) if file_path else f"{quality_filename}.mkv"
                            clean_filename = self.build_original_pattern_filename(actual_filename, hash_value)
                            single_episode_mkv = os.path.join(season_dir, f"{clean_filename}.mkv")
                            
                            # Create stream URL with index for the single episode
                            stream_url = f"{self.TORRSERVER_URL}/stream?link={hash_value}&index={file_id}&play"
                            
                            if self.create_mkv_with_metadata(single_episode_mkv, stream_url, str(file_length), magnet_url):
                                self.log_success(f"Created TV virtual file: {os.path.basename(single_episode_mkv)}")
                                last_created_mkv = single_episode_mkv
                                created_files += 1
                                total_tv_files += 1
                                # UPDATE INDICE TV (episodio singolo)
                                self._update_tv_core_index(single_episode_mkv)
                        else:
                            # Fallback rimosso: torrent senza file .mkv scartato (evita file virtuali inconsistenti)
                            self.log("INFO", f"Removed torrent senza .mkv valido per sicurezza: {stream_title}")
                            self.remove_torrent_from_server(hash_value)
                            self._negative_cache_add(info_hash, "no_valid_tv_files")
                            tv_negative_cache_added += 1
                            continue
                
                # Add processing pause
                time.sleep(self.PROCESS_INTERVAL)
                
                # SAFE UPGRADE CLEANUP: Remove existing torrent AFTER successful creation
                # IMPORTANT: Skip cleanup for fullpack to avoid removing episodes we just created
                if last_created_mkv and not is_fullpack:
                    # Only cleanup for single episodes, not for fullpack
                    self.log("INFO", f"UPGRADE: Starting safe cleanup for TV single episode: {stream_title}")
                    self.remove_existing_torrent_if_any(last_created_mkv)
                elif is_fullpack:
                    # For fullpack, always check if we should replace inferior single episodes
                    self.log("INFO", f"UPGRADE: Checking for inferior single episodes to replace with fullpack")
                    season_num = self._extract_season_number(stream_title)
                    if season_num >= 0:
                        self._replace_single_episodes_with_fullpack(show_name, season_num, hash_value)
                else:
                    self.log("INFO", f"UPGRADE: No cleanup needed - no TV files created for: {stream_title}")
            
            # PHASE 2: Process single episodes only if no fullpack exists for that season
            if not self.TV_FULLPACK_ONLY:  # Only process singles if FULLPACK_ONLY is disabled
                for stream in single_episode_streams:
                    stream_title = stream.get("title", "")
                    season_num = self._extract_season_number(stream_title)
                    
                    # Skip single episode if fullpack exists for this season
                    if season_num in season_has_fullpack:
                        fullpack_title = season_has_fullpack[season_num].get("title", "")
                        self.log("INFO", f"Skip single episode (fullpack exists): {stream_title} -> replaced by fullpack: {fullpack_title}")
                        continue
                    
                    # Initialize for each TV stream to prevent UnboundLocalError
                    last_created_mkv = ""
                    is_fullpack = False
                    info_hash = stream.get("infoHash", "")
                    
                    if not stream_title or not info_hash:
                        continue
                    
                    # SKIP se hash già rappresentato da un file TV esistente
                    existing_same_hash_tv = self.check_existing_mkv_for_hash(info_hash)
                    if existing_same_hash_tv:
                        self.log("INFO", f"Skip TV (hash già presente) -> {os.path.basename(existing_same_hash_tv)}")
                        continue
                    
                    # Create magnet URL
                    magnet_url = self._build_magnet(info_hash)

                    if self._negative_cache_has(info_hash):
                        tv_negative_cache_hits += 1
                        self.log("DEBUG", f"Skip (negative cache no-mkv TTL active) hash {info_hash[:8]}... for TV '{stream_title}'")
                        continue
                    
                    # Generate quality filename with hash suffix
                    quality_filename = self.extract_quality_filename(stream_title, info_hash)
                    
                    # Use same directory naming logic as fullpacks - TMDB title directly
                    show_name = re.sub(r'[^a-zA-Z0-9._-]', '_', title)
                    show_name = re.sub(r'_+', '_', show_name).strip('_')
                    show_name = re.sub(r'[Ss][0-9]+[Ee][0-9]+.*', '', show_name)
                    show_name = re.sub(r'_+', '_', show_name).strip('_')
                    
                    # Extract season number for directory
                    season_match = re.search(r'[Ss]([0-9]+)', stream_title)
                    season_num_dir = int(season_match.group(1)) if season_match else 1
                    # Use unified directory to prevent duplicates due to capitalization
                    unified_show_name = self._get_unified_show_directory(show_name)
                    season_dir = os.path.join(self.TV_DIR, unified_show_name, f"Season.{season_num_dir:02d}")
                    
                    # CRITICAL FIX: Extract episode pattern BEFORE core check to ensure key consistency
                    episode_match = re.search(r'(S\d+E\d+|Season\.\d+\.Episode\.\d+|\d+x\d+)', stream_title, re.IGNORECASE)
                    if episode_match:
                        episode_name = episode_match.group(1)
                        episode_name_clean = re.sub(r'[^a-zA-Z0-9._-]', '_', episode_name)
                        # Use show name + episode pattern + hash for consistency with file creation
                        episode_filename_for_core = f"{show_name}_{episode_name_clean}_{info_hash[-8:]}"
                        core_check_filename = f"{episode_filename_for_core}.mkv"
                    else:
                        # Fallback to quality filename if no episode pattern found
                        core_check_filename = f"{quality_filename}.mkv"
                    
                    target_mkv = os.path.join(season_dir, f"{quality_filename}.mkv")
                    
                    # CORE INDEX CHECK per TV - now uses episode-aware filename
                    core = self._core_key(core_check_filename, is_tv=True)
                    core_existing_tv = self._tv_core_index.get(core)
                    
                    if core_existing_tv and os.path.isfile(core_existing_tv):
                        file_size_bytes = self.extract_size_from_title(stream_title)
                        stream_name = stream.get("name", "")
                        if not self.should_replace_mkv(core_existing_tv, stream_title, file_size_bytes, stream_name):
                            self.log("INFO", f"Skip TV single episode (core presente, no upgrade): {os.path.basename(core_existing_tv)}")
                            continue
                        else:
                            self.log("INFO", f"Upgrade TV single episode: nuova variante migliore di {os.path.basename(core_existing_tv)}")
                    
                    # Check if better version already exists (fallback)
                    existing = self.find_existing_variant(target_mkv)
                    if existing:
                        file_size_bytes = self.extract_size_from_title(stream_title)
                        stream_name = stream.get("name", "")
                        if not self.should_replace_mkv(existing, stream_title, file_size_bytes, stream_name):
                            self.log("INFO", f"Skip single episode: versione migliore già presente (found: {os.path.basename(existing)})")
                            continue
                    
                    # Process single episode (use existing logic for single episode processing)
                    # SYNC: Clear negative cache before attempting single episode torrent add
                    if hash_value_from_magnet := self.extract_hash_from_magnet(magnet_url):
                        if self._negative_cache.pop(hash_value_from_magnet.lower(), None):
                            self._save_negative_cache()
                            self.log("DEBUG", f"Cleared stale negative cache entry before adding episode: {hash_value_from_magnet[:8]}...")
                    
                    # Add torrent to server
                    stat_string, hash_value = self.add_torrent_to_server(magnet_url, stream_title)
                    
                    if not stat_string or not hash_value:
                        continue
                    
                    if stat_string in ["Torrent added", "Torrent working", "Torrent getting info"]:
                        self.log_success(f"Successfully added TV single episode torrent: {stream_title} (hash: {hash_value[:8]}...)")
                        
                        # Validate hash and torrent
                        if not re.match(r'^[a-fA-F0-9]{40}$', hash_value):
                            continue
                        
                        torrent_info = self.get_torrent_info(hash_value)
                        if not torrent_info:
                            continue

                        file_stats = torrent_info.get("file_stats", []) or []
                        video_files = self._collect_video_files(file_stats)
                        
                        if video_files:
                            # Process single episode video files
                            for idx, file_stat in enumerate(video_files):
                                file_path = file_stat.get('path', '')
                                file_length = file_stat.get('length', 0)
                                
                                if not file_path or file_length <= 0:
                                    continue
                                
                                # FIX: Different naming logic for fullpack vs single episode
                                if len(video_files) > 1:
                                    # FULLPACK: Extract episode name from file path (e.g., "S01E01.mkv" -> "S01E01")
                                    episode_name = os.path.splitext(os.path.basename(file_path))[0]
                                    # Clean episode name and add hash suffix for uniqueness
                                    episode_name_clean = re.sub(r'[^a-zA-Z0-9._-]', '_', episode_name)
                                    episode_filename = f"{episode_name_clean}_{hash_value[-8:]}"
                                else:
                                    # SINGLE EPISODE: Use previously extracted episode pattern for consistency
                                    # This ensures the filename matches the core key used for checking
                                    if episode_match:
                                        episode_name = episode_match.group(1)
                                        episode_name_clean = re.sub(r'[^a-zA-Z0-9._-]', '_', episode_name)
                                        # Use show name + episode pattern + hash (consistent with core check)
                                        episode_filename = f"{show_name}_{episode_name_clean}_{hash_value[-8:]}"
                                    else:
                                        # Fallback to quality filename if no episode pattern found
                                        episode_filename = quality_filename
                                
                                # Always use .mkv extension for virtual files (consistency with Plex)
                                # GoStorm handles mp4/avi/mov streaming correctly regardless of virtual file extension
                                stream_url = f"{self.TORRSERVER_URL}/stream?link={hash_value}&index={idx}&play"
                                single_episode_file = os.path.join(season_dir, f"{episode_filename}.mkv")
                                
                                if self.create_mkv_with_metadata(single_episode_file, stream_url, str(file_length), magnet_url):
                                    self.log_success(f"Created TV single episode: {os.path.basename(single_episode_file)}")
                                    last_created_mkv = single_episode_file
                                    created_files += 1
                                    total_tv_files += 1
                                    # UPDATE INDICE TV (episodio singolo)
                                    self._update_tv_core_index(single_episode_file)
                        else:
                            # Fallback rimosso: torrent senza file video scartato
                            self.log("INFO", f"Removed single episode torrent senza video valido per sicurezza: {stream_title}")
                            self.remove_torrent_from_server(hash_value)
                            continue
                            
                        time.sleep(self.PROCESS_INTERVAL)
                        
                        # SAFE UPGRADE CLEANUP per single episodes
                        if last_created_mkv:
                            self.log("INFO", f"UPGRADE: Starting safe cleanup for TV single episode: {stream_title}")
                            self.remove_existing_torrent_if_any(last_created_mkv)
            
            self.log("INFO", f"Created {created_files} files for TV show: {title}")

        # Summary for TV processing
        self.log("INFO", f"=== TV SHOWS COMPLETE: {total_tv_files} files created ===")
        # Negative & fullpack cache statistics
        self.log("INFO", f"TV Negative cache: hits={tv_negative_cache_hits} added={tv_negative_cache_added} active={len(self._negative_cache)} TTLh={self.NEGATIVE_CACHE_TTL_HOURS}")
        self.log("INFO", f"TV Fullpack cache: hits={fullpack_cache_hits} entries={len(self._fullpack_cache)}")
        return total_tv_files

    def cleanup_orphaned_files(self) -> bool:
        """
        Clean up orphaned video files that don't have corresponding torrents
        Exact replica of bash cleanup_orphaned_files() function
        """
        self.log("INFO", "Starting cleanup of orphaned video files...")
        
        # PATCH: Get current active torrents with fresh list (no cache stantia)
        active_torrents = self._cached_torrents_list(force=True)
        if not active_torrents:
            self.log_error("Failed to get active torrents for cleanup (cache empty)")
            return False
        
        # Extract all active hashes
        active_hashes = set()
        for torrent in active_torrents:
            hash_val = torrent.get("hash")
            if hash_val:
                active_hashes.add(hash_val.lower())
        
        cleanup_count = 0
        
        # Check movie files
        video_extensions = ('.mkv', '.mp4', '.avi', '.mov', '.webm', '.m4v')
        if os.path.exists(self.MOVIES_DIR):
            for root, dirs, files in os.walk(self.MOVIES_DIR):
                for file in files:
                    if file.lower().endswith(video_extensions):
                        video_file = os.path.join(root, file)
                        try:
                            with open(video_file, 'r') as f:
                                stream_url = f.readline().strip()
                            
                            # Extract hash from stream URL
                            import re
                            hash_match = re.search(r'link=([a-f0-9]{40})', stream_url, re.IGNORECASE)
                            if hash_match:
                                file_hash = hash_match.group(1).lower()
                                
                                # Check if this hash exists in active torrents
                                if file_hash not in active_hashes:
                                    self.log("INFO", f"Removing orphaned movie file: {os.path.basename(video_file)}")
                                    os.remove(video_file)
                                    cleanup_count += 1
                                    
                                    # SYNC: Clear from negative cache when orphaned file is removed
                                    if self._negative_cache.pop(file_hash, None):
                                        self.log("DEBUG", f"Cleared negative cache entry for orphaned file: {file_hash[:8]}...")
                        except (IOError, Exception):
                            continue
        
        # NOTE: TV orphan cleanup intentionally skipped in movies-only script.
        
        # Save negative cache changes after cleanup
        self._save_negative_cache()
        
        self.log_success(f"Cleanup completed: {cleanup_count} orphaned files removed")
        
        # HASH-BASED DEDUPLICATION: Remove movie duplicates pointing to same torrent
        duplicate_count = self.remove_movie_duplicates_by_hash(self.MOVIES_DIR)

        # V255: IMDB-BASED DEDUPLICATION: Remove duplicates with same IMDB ID (different torrents, same movie)
        imdb_duplicate_count = self.cleanup_duplicate_movies_by_imdb()

        # QUALITY-BASED DEDUPLICATION: Remove lower quality versions of same movies (title fallback)
        quality_duplicate_count = self.cleanup_duplicate_movies_by_title()

        # BIDIRECTIONAL CLEANUP: Ensure ALL remaining torrents have video files (if enabled and needed)
        if self.FULL_SCAN_AFTER_CLEANUP and self._needs_full_coverage_scan():
            self.log("INFO", "Ensuring all remaining torrents have video files...")
            self.process_all_existing_torrents()
            # V255: Re-run dedup AFTER coverage scan to catch any duplicates it created
            self.remove_movie_duplicates_by_hash(self.MOVIES_DIR)
            self.cleanup_duplicate_movies_by_imdb()
            self.cleanup_duplicate_movies_by_title()
        else:
            self.log("INFO", "Skip process_all_existing_torrents (disabilitato o non necessario)")

        return True

    def remove_movie_duplicates_by_hash(self, movie_dir: str) -> int:
        """
        Remove duplicate movie files that point to the same torrent hash.
        Keeps only the most recent file for each hash.
        Returns number of duplicates removed.
        """
        self.log("INFO", "Checking for movie duplicates by torrent hash...")
        
        if not os.path.exists(movie_dir):
            return 0
        
        # Collect all video files and their hashes
        hash_to_files = {}
        video_extensions = ('.mkv', '.mp4', '.avi', '.mov', '.webm', '.m4v')
        
        for root, dirs, files in os.walk(movie_dir):
            for file in files:
                if file.lower().endswith(video_extensions):
                    file_path = os.path.join(root, file)
                    try:
                        # Extract hash from file content
                        with open(file_path, 'r') as f:
                            stream_url = f.readline().strip()
                        
                        # Extract torrent hash from URL
                        import re
                        hash_match = re.search(r'link=([a-f0-9]{40})', stream_url, re.IGNORECASE)
                        if hash_match:
                            torrent_hash = hash_match.group(1).lower()
                            if torrent_hash not in hash_to_files:
                                hash_to_files[torrent_hash] = []
                            hash_to_files[torrent_hash].append(file_path)
                    except (IOError, Exception) as e:
                        self.log("DEBUG", f"Could not read hash from {file}: {e}")
                        continue
        
        # Find and remove duplicates
        removed_count = 0
        for torrent_hash, file_paths in hash_to_files.items():
            if len(file_paths) > 1:
                # Sort by modification time (newest first)
                file_paths.sort(key=lambda x: os.path.getmtime(x), reverse=True)
                
                # Keep the newest file, remove the rest
                newest_file = file_paths[0]
                self.log("INFO", f"Hash {torrent_hash[:8]}... has {len(file_paths)} duplicates")
                self.log("INFO", f"Keeping newest: {os.path.basename(newest_file)}")
                
                for old_file in file_paths[1:]:
                    try:
                        os.remove(old_file)
                        self.log("INFO", f"Removed duplicate: {os.path.basename(old_file)}")
                        removed_count += 1
                    except Exception as e:
                        self.log("WARN", f"Failed to remove duplicate {old_file}: {e}")
        
        if removed_count > 0:
            self.log_success(f"Removed {removed_count} movie duplicates by hash")
        else:
            self.log("INFO", "No movie duplicates found by hash")
        
        return removed_count


    def cleanup_duplicate_movies_by_imdb(self) -> int:
        """
        V255: Remove duplicate movie files that share the same IMDB ID.
        This catches duplicates that title normalization misses:
        - Edition suffixes ("Unrated", "Director's Cut")
        - Non-standard quality tags ("Ultra HD" vs "2160p")
        - Localized titles ("dei Killer" vs "of Killers")
        Each MKV stores its IMDB ID on line 4. Files sharing the same IMDB ID
        are grouped, scored by quality, and only the best is kept.
        Returns number of duplicates removed.
        """
        self.log("INFO", "Checking for movie duplicates by IMDB ID...")

        if not os.path.exists(self.MOVIES_DIR):
            return 0

        import re
        from collections import defaultdict

        # Group movies by IMDB ID
        imdb_to_files = defaultdict(list)
        video_extensions = ('.mkv', '.mp4', '.avi', '.mov', '.webm', '.m4v')

        for root, dirs, files in os.walk(self.MOVIES_DIR):
            for file in files:
                if file.lower().endswith(video_extensions):
                    file_path = os.path.join(root, file)
                    try:
                        with open(file_path, 'r') as f:
                            lines = f.readlines()
                        # Line 4 (index 3) = IMDB ID
                        if len(lines) >= 4:
                            imdb_id = lines[3].strip()
                            if imdb_id and imdb_id.startswith('tt'):
                                imdb_to_files[imdb_id].append(file_path)
                    except Exception:
                        pass

        # Find movies with duplicates
        duplicates = {imdb: files for imdb, files in imdb_to_files.items() if len(files) > 1}

        if not duplicates:
            self.log("INFO", "No movie duplicates found by IMDB ID")
            return 0

        self.log("INFO", f"Found {len(duplicates)} movies with duplicate IMDB IDs")

        removed_count = 0
        for imdb_id, file_paths in duplicates.items():
            self.log("INFO", f"IMDB {imdb_id}: {len(file_paths)} versions")

            # Calculate quality scores for each file
            files_with_scores = []
            for file_path in file_paths:
                filename = os.path.basename(file_path)
                quality_score = self.calculate_quality_score(filename)
                files_with_scores.append((file_path, filename, quality_score))
                self.log("DEBUG", f"  {filename[:60]}... -> score: {quality_score}")

            # Sort by quality score (highest first)
            files_with_scores.sort(key=lambda x: x[2], reverse=True)

            # Keep the highest quality version
            best_file, best_filename, best_score = files_with_scores[0]
            self.log("INFO", f"  Keep best ({best_score}): {best_filename[:60]}...")

            # Remove lower quality versions
            for file_path, filename, score in files_with_scores[1:]:
                self.log("INFO", f"  Remove ({score}): {filename[:60]}...")
                try:
                    # Extract torrent hash from stream URL (line 1)
                    torrent_hash = None
                    try:
                        with open(file_path, 'r') as f:
                            stream_url = f.readline().strip()
                        hash_match = re.search(r'link=([a-f0-9]{40})', stream_url, re.IGNORECASE)
                        if hash_match:
                            torrent_hash = hash_match.group(1)
                    except Exception:
                        pass

                    os.remove(file_path)
                    self.log("SUCCESS", f"  Removed: {filename[:60]}...")
                    removed_count += 1

                    # Remove corresponding torrent from GoStorm
                    if torrent_hash:
                        if self.remove_torrent_from_server(torrent_hash):
                            self.log("SUCCESS", f"  Torrent removed: {torrent_hash[:8]}")
                        else:
                            self.log("WARN", f"  Failed to remove torrent: {torrent_hash[:8]}")

                except Exception as e:
                    self.log("ERROR", f"Failed to remove duplicate {filename}: {e}")

        if removed_count > 0:
            self.log_success(f"IMDB-based deduplication: {removed_count} duplicate movies removed")
        else:
            self.log("INFO", f"IMDB-based deduplication: Found {len(duplicates)} groups but removed 0 files")

        return removed_count


    def cleanup_duplicate_movies_by_title(self) -> int:
        """
        Remove duplicate movie files with same normalized title but different quality.
        Keeps only the highest quality version for each movie title.
        Returns number of duplicates removed.
        """
        self.log("INFO", "Checking for movie duplicates by title (quality-based cleanup)...")
        
        if not os.path.exists(self.MOVIES_DIR):
            return 0
        
        import re
        from collections import defaultdict
        
        # Group movies by normalized title
        title_to_files = defaultdict(list)
        video_extensions = ('.mkv', '.mp4', '.avi', '.mov', '.webm', '.m4v')
        
        for root, dirs, files in os.walk(self.MOVIES_DIR):
            for file in files:
                if file.lower().endswith(video_extensions):
                    file_path = os.path.join(root, file)
                    
                    # Normalize title: remove hash, year, quality, separators
                    normalized_title = re.sub(r'_[a-f0-9]{8}\.mkv$', '', file, flags=re.IGNORECASE)
                    normalized_title = re.sub(r'[_.\-]', ' ', normalized_title)
                    normalized_title = re.sub(r' (19|20)\d\d .*', '', normalized_title)
                    normalized_title = re.sub(r' (19|20)\d\d$', '', normalized_title)
                    normalized_title = re.sub(r' \d{3,4}p .*', '', normalized_title)
                    normalized_title = re.sub(r'\s+', ' ', normalized_title).strip().lower()
                    
                    if normalized_title:
                        title_to_files[normalized_title].append(file_path)
        
        # Find movies with duplicates
        duplicates = {title: files for title, files in title_to_files.items() if len(files) > 1}
        
        if not duplicates:
            self.log("INFO", "No movie duplicates found by title")
            return 0
        
        self.log("INFO", f"Found {len(duplicates)} movies with duplicate versions")
        
        removed_count = 0
        for title, file_paths in duplicates.items():
            self.log("INFO", f"Processing {len(file_paths)} duplicates for: {title}")
            
            # Calculate quality scores for each file
            files_with_scores = []
            for file_path in file_paths:
                filename = os.path.basename(file_path)
                quality_score = self.calculate_quality_score(filename)
                files_with_scores.append((file_path, filename, quality_score))
                self.log("DEBUG", f"  {filename[:50]}... -> score: {quality_score}")
            
            # Sort by quality score (highest first)
            files_with_scores.sort(key=lambda x: x[2], reverse=True)
            
            # Keep the highest quality version
            best_file, best_filename, best_score = files_with_scores[0]
            self.log("INFO", f"Keep best quality ({best_score}): {best_filename[:60]}...")
            
            # Remove lower quality versions
            for file_path, filename, score in files_with_scores[1:]:
                self.log("INFO", f"Attempting to remove: {filename[:60]}... (score: {score})")
                try:
                    # Extract hash from file to remove corresponding torrent
                    torrent_hash = None
                    stream_url = ""
                    try:
                        with open(file_path, 'r') as f:
                            stream_url = f.readline().strip()
                        self.log("DEBUG", f"  Stream URL: {stream_url[:80]}...")
                        hash_match = re.search(r'link=([a-f0-9]{40})', stream_url, re.IGNORECASE)
                        if hash_match:
                            torrent_hash = hash_match.group(1)
                            self.log("DEBUG", f"  Found hash: {torrent_hash[:8]}...")
                        else:
                            self.log("WARN", f"  No hash found in URL: {stream_url}")
                    except Exception as e:
                        self.log("ERROR", f"  Failed to read file {file_path}: {e}")
                    
                    # Remove the .mkv file
                    self.log("DEBUG", f"  Removing file: {file_path}")
                    os.remove(file_path)
                    self.log("SUCCESS", f"Removed lower quality ({score}): {filename[:60]}...")
                    removed_count += 1
                    
                    # Remove corresponding torrent from GoStorm
                    if torrent_hash:
                        self.log("DEBUG", f"  Removing torrent: {torrent_hash[:8]}...")
                        if self.remove_torrent_from_server(torrent_hash):
                            self.log("SUCCESS", f"  Torrent removed: {torrent_hash[:8]}")
                        else:
                            self.log("WARN", f"  Failed to remove torrent: {torrent_hash[:8]}")
                    else:
                        self.log("WARN", f"  No torrent hash to remove for: {filename[:40]}...")
                        
                except Exception as e:
                    self.log("ERROR", f"Failed to remove duplicate {filename}: {e}")
                    import traceback
                    self.log("DEBUG", f"  Traceback: {traceback.format_exc()}")
        
        if removed_count > 0:
            self.log_success(f"Quality-based deduplication: {removed_count} duplicate movies removed")
        else:
            self.log("INFO", f"Quality-based deduplication: Found {len(duplicates)} duplicate groups but removed 0 files")
        
        return removed_count

    def process_all_existing_torrents(self) -> bool:
        """
        Process ALL existing torrents to ensure 100% .mkv coverage
        Exact replica of bash process_all_existing_torrents() function
        """
        self.log("INFO", "Processing ALL existing torrents for 100% .mkv coverage...")
        
        # PATCH: Get all active torrents with fresh list (evita coverage su lista stantia)
        all_torrents = self._cached_torrents_list(force=True)
        if not all_torrents:
            self.log_error("Torrent list empty (cannot process existing)")
            return False
        
        total_torrents = len(all_torrents)
        processed = 0
        created = 0
        skipped = 0
        # Nuovo: track episodi già coperti in questo pass per evitare duplicati cross-torrent
        processed_episode_cores: set[str] = set()
        
        self.log("INFO", f"Found {total_torrents} existing torrents to process")
        
        for torrent in all_torrents:
            hash_value = torrent.get("hash", "")
            title = torrent.get("title", "")
            
            if not hash_value or not title:
                continue
            
            processed += 1
            
            # Get torrent info (fast mode if enabled)
            torrent_info = self.get_torrent_info(hash_value, fast_mode=self.FAST_EXISTING_PASS)
            if not torrent_info:
                skipped += 1
                continue
            
            file_stats = torrent_info.get("file_stats", [])
            if not file_stats:
                skipped += 1
                continue
            
            # Raggruppa per episodio (solo TV) prima di creare
            episode_groups = {}  # core_episode -> list[ (file_stat, is_tv_episode, is_4k, hdr_flag, size_gb, filename) ]
            movie_candidates = []  # (file_stat, filename)

            for file_stat in file_stats:
                file_path = file_stat.get("path", "")
                if not file_path.lower().endswith(".mkv"):
                    continue
                file_length = file_stat.get("length", 0)
                filename = os.path.basename(file_path)
                is_tv_episode = bool(re.search(r'S[0-9]+E[0-9]+', filename, re.IGNORECASE))
                
                # STRICT FILTER: Skip TV episodes entirely in this movie script
                if is_tv_episode:
                    continue
                
                # Movie size filter
                is_4k_content = bool(re.search(r'2160p|4[kK]|UHD', filename, re.IGNORECASE)) or \
                                bool(re.search(r'2160p|4[kK]|UHD', title, re.IGNORECASE))
                min_size = self.MOVIE_4K_MIN_SIZE if is_4k_content else self.MOVIE_1080P_MIN_SIZE
                max_size = self.MOVIE_4K_MAX_SIZE if is_4k_content else self.MOVIE_1080P_MAX_SIZE
                if file_length < min_size or file_length > max_size:
                    continue
                movie_candidates.append((file_stat, filename))

            # Crea movie file (only the largest per torrent)
            if movie_candidates:
                # Take only the largest candidate
                file_stat, filename = max(movie_candidates, key=lambda x: x[0].get("length", 0))
                file_id = file_stat.get("id", "")
                file_length = file_stat.get("length", 0)
                clean_filename = self.build_original_pattern_filename(filename, hash_value)
                mkv_file = os.path.join(self.MOVIES_DIR, f"{clean_filename}.mkv")
                if not os.path.exists(mkv_file):
                    # Check core index to avoid duplicates
                    core = self._core_key(f"{clean_filename}.mkv", is_tv=False)
                    if not self._movie_core_index.get(core):
                        # Also check hash-based dedup
                        if not self.check_existing_mkv_for_hash(hash_value):
                            os.makedirs(self.MOVIES_DIR, exist_ok=True)
                            stream_url = f"{self.TORRSERVER_URL}/stream?link={hash_value}&index={file_id}&play"
                            magnet_url = self._build_magnet(hash_value)
                            if self.create_mkv_with_metadata(mkv_file, stream_url, str(file_length), magnet_url):
                                self.log("INFO", f"Created missing .mkv: {os.path.basename(mkv_file)}")
                                self._update_movie_core_index(mkv_file)
                                if hasattr(self, '_hash_cache'):
                                    self._hash_cache[hash_value.lower()] = mkv_file
                                created += 1

            # FULLPACK COVERAGE DETECTION (runtime cache cross‑torrent)
            if self.TV_FULLPACK_ONLY and episode_groups:
                # Conta episodi per (season_key, hash) in questo torrent
                season_counts = {}  # (season_key, hash) -> count
                for core_ep, variants in episode_groups.items():
                    sample_filename = variants[0][5]
                    season_match = re.search(r'S([0-9]+)E', sample_filename, re.IGNORECASE)
                    if not season_match:
                        continue
                    season_num = int(season_match.group(1))
                    # Deriva identificatore show base dal core episodio
                    show_core = re.sub(r'_s\d+e\d+.*$', '', core_ep.lower())
                    season_key = f"{show_core}_s{season_num:02d}"
                    for v in variants:
                        v_hash = v[1]  # Now correctly contains hash_value, not True
                        season_counts[(season_key, v_hash)] = season_counts.get((season_key, v_hash), 0) + 1
                
                # Promuovi a fullpack se almeno 3 episodi
                for (season_key, v_hash), cnt in season_counts.items():
                    if cnt >= 3:
                        if season_key not in self._season_fullpack_hash:
                            self._season_fullpack_hash[season_key] = v_hash
                            self.log("INFO", f"Detected new fullpack: {season_key} hash {v_hash[:8]}... ({cnt} eps)")
                        elif self._season_fullpack_hash[season_key] != v_hash:
                            # Già coperto da altro hash: questo torrent è episodio singolo rispetto a fullpack esistente
                            self.log("DEBUG", f"Season {season_key} already covered by different fullpack {self._season_fullpack_hash[season_key][:8]}... vs {v_hash[:8]}...")

            # Seleziona miglior release per ogni episodio
            for core_ep, variants in episode_groups.items():
                # Ordina: 4K desc, HDR desc, size desc
                variants.sort(key=lambda t: (-int(t[2]), -int(t[3]), -t[4]))
                best = variants[0]
                file_stat = best[0]
                filename = best[5]
                file_id = file_stat.get("id", "")
                file_length = file_stat.get("length", 0)

                # Deriva show/season
                season_match = re.search(r'S([0-9]+)E', filename, re.IGNORECASE)
                season_num = int(season_match.group(1)) if season_match else 1
                show_match = re.search(r'^(.+?)\.S[0-9]+E[0-9]+', title, re.IGNORECASE)
                show_name = show_match.group(1).replace('.', '_') if show_match else title.replace('.', '_')
                # CRITICAL: Remove torrent site prefixes to prevent duplicate directories
                show_name = self._remove_torrent_site_prefixes(show_name)
                show_name = re.sub(r'[^a-zA-Z0-9._-]', '_', show_name)
                show_name = re.sub(r'_+', '_', show_name).strip('_')
                
                # Skip se stagione già coperta da fullpack diverso
                if self.TV_FULLPACK_ONLY:
                    show_core = re.sub(r'_s\d+e\d+.*$', '', core_ep.lower())
                    season_key = f"{show_core}_s{season_num:02d}"
                    fullpack_hash = self._season_fullpack_hash.get(season_key)
                    if fullpack_hash and fullpack_hash != hash_value:
                        self.log("INFO", f"Skip episodio (season fullpack present): {filename} ({fullpack_hash[:8]}...)")
                        continue
                
                # Use unified directory to prevent duplicates due to capitalization
                unified_show_name = self._get_unified_show_directory(show_name)
                season_dir = os.path.join(self.TV_DIR, unified_show_name, f"Season.{season_num:02d}")
                clean_filename = self.build_original_pattern_filename(filename, hash_value)
                mkv_file = os.path.join(season_dir, f"{clean_filename}.mkv")

                # Normalized core for dedup
                ep_core = self._core_key(f"{clean_filename}.mkv", is_tv=True)
                if ep_core in processed_episode_cores:
                    self.log("DEBUG", f"Skip duplicate episode (already processed this run): {ep_core}")
                    continue

                # Controlla se esiste già una variante (indice core globale)
                existing_path = self._tv_core_index.get(ep_core)
                if existing_path and os.path.isfile(existing_path):
                    # Decide upgrade
                    new_size = str(file_length)
                    if not self.should_replace_mkv(existing_path, filename, new_size):
                        self.log("INFO", f"Keep existing episode variant: {os.path.basename(existing_path)}")
                        processed_episode_cores.add(ep_core)
                        continue
                    else:
                        try:
                            os.remove(existing_path)
                            self.log("INFO", f"Removed older episode variant: {os.path.basename(existing_path)}")
                        except OSError:
                            self.log("WARN", f"Failed to remove old episode variant: {existing_path}")

                else:
                    # Fallback variant search
                    variant = self.find_existing_variant(mkv_file)
                    if variant:
                        new_size = str(file_length)
                        if not self.should_replace_mkv(variant, filename, new_size):
                            self.log("INFO", f"Skip (variant already adequate): {os.path.basename(variant)}")
                            processed_episode_cores.add(ep_core)
                            continue
                        else:
                            try:
                                os.remove(variant)
                                self.log("INFO", f"Removed older variant (upgrade): {os.path.basename(variant)}")
                            except OSError:
                                self.log("WARN", f"Failed to remove old variant: {variant}")

                if os.path.exists(mkv_file):
                    # File esattamente target esiste (caso raro dopo cleanup) → evita duplicato
                    processed_episode_cores.add(ep_core)
                    continue
                os.makedirs(season_dir, exist_ok=True)
                stream_url = f"{self.TORRSERVER_URL}/stream?link={hash_value}&index={file_id}&play"
                magnet_url = self._build_magnet(hash_value)
                if self.create_mkv_with_metadata(mkv_file, stream_url, str(file_length), magnet_url):
                    self.log("INFO", f"Created missing .mkv: {os.path.basename(mkv_file)} (best variant)")
                    self._update_tv_core_index(mkv_file)
                    created += 1
                    processed_episode_cores.add(ep_core)
        
        self.log_success(f"Processed {processed}/{total_torrents} torrents, created {created} missing files, skipped {skipped}")
        return True

    def rehydrate_missing_torrents(self, include_tv: bool = False):
        """Re-add torrents per cui esiste .mkv ma il torrent non è più attivo e abbiamo magnet salvato.
        Per default NON reidrata le serie TV (include_tv=False)."""
        # Load active torrent hashes
        torrents = self._cached_torrents_list()
        active_hashes = set()
        for torrent in torrents:
            hash_val = torrent.get("hash")
            if hash_val:
                active_hashes.add(hash_val.lower())
        
        rehydrated = 0
        base_dirs = (self.MOVIES_DIR, self.TV_DIR) if include_tv else (self.MOVIES_DIR,)
        for base_dir in base_dirs:
            if not os.path.isdir(base_dir):
                continue
            for root, _, files in os.walk(base_dir):
                for filename in files:
                    if not filename.lower().endswith(".mkv"):
                        continue
                    file_path = os.path.join(root, filename)
                    try:
                        with open(file_path, 'r') as fh:
                            lines = fh.readlines()
                        if len(lines) < 3:
                            continue
                        stream_line = lines[0].strip()
                        magnet_line = lines[2].strip()
                        
                        # Extract hash from stream URL
                        hash_match = re.search(r'link=([a-f0-9]{40})', stream_line, re.IGNORECASE)
                        if not hash_match:
                            continue
                        hash_val = hash_match.group(1).lower()
                        
                        # Skip if torrent already active
                        if hash_val in active_hashes:
                            continue
                            
                        # Rehydrate if we have magnet - rebuild with trackers for faster resolution
                        if magnet_line.startswith("magnet:?"):
                            self.log("INFO", f"Rehydrating missing torrent: {filename} (hash {hash_val[:8]}...)")
                            # Rebuild magnet with dynamic trackers (old files may not have them)
                            magnet_with_trackers = self._build_magnet(hash_val)
                            self.add_torrent_to_server(magnet_with_trackers, filename)
                            rehydrated += 1
                    except Exception:
                        continue
        
        if rehydrated > 0:
            self.log_success(f"Rehydrated {rehydrated} missing torrents from magnet URLs")
        else:
            self.log("INFO", "No torrents needed rehydration")

    def fix_existing_movie_indexes(self):
        """Skip fix_existing_movie_indexes (index richiesto per movies)"""
        self.log("DEBUG", "Skip fix_existing_movie_indexes (index richiesto per movies)")

    def fix_movie_indexes_intelligent(self):
        """Calculate and fix correct index for existing movie .mkv files pointing to wrong files"""
        if not os.path.isdir(self.MOVIES_DIR):
            return
            
        analyzed = 0
        fixed = 0
        errors = 0
        
        self.log("INFO", "Starting intelligent index correction for movie files...")
        
        for root, _, files in os.walk(self.MOVIES_DIR):
            for filename in files:
                if not filename.lower().endswith(".mkv"):
                    continue
                file_path = os.path.join(root, filename)
                analyzed += 1
                
                try:
                    # Read current file content
                    with open(file_path, 'r') as f:
                        lines = f.readlines()
                    
                    if not lines:
                        continue
                        
                    # Extract hash and current index from stream URL
                    stream_line = lines[0].strip()
                    if 'stream?link=' not in stream_line:
                        continue
                        
                    # Extract hash
                    hash_match = re.search(r'link=([a-f0-9]{40})', stream_line, re.IGNORECASE)
                    if not hash_match:
                        continue
                    hash_value = hash_match.group(1)
                    
                    # Extract current index
                    current_index = 1  # default
                    index_match = re.search(r'index=(\d+)', stream_line)
                    if index_match:
                        current_index = int(index_match.group(1))
                    
                    self.log("DEBUG", f"Analyzing {filename}: hash={hash_value[:8]}..., current_index={current_index}, has_index_param={'index=' in stream_line}")
                    
                    # Query GoStorm for file stats
                    response = self.safe_curl(
                        f"{self.TORRSERVER_URL}/torrents",
                        method="POST",
                        data={"action": "get", "hash": hash_value}
                    )
                    
                    if not response:
                        continue
                        
                    try:
                        torrent_data = response.json()
                        file_stats = torrent_data.get("file_stats", [])
                        
                        # If file_stats is empty, try to extract from data.GoStorm.Files
                        if not file_stats and "data" in torrent_data:
                            try:
                                import json
                                nested_data = json.loads(torrent_data["data"])
                                if "GoStorm" in nested_data and "Files" in nested_data["GoStorm"]:
                                    file_stats = nested_data["GoStorm"]["Files"]
                            except Exception as e:
                                self.log("DEBUG", f"Failed to parse nested data for {hash_value}: {e}")
                    except Exception:
                        continue
                    
                    if not file_stats:
                        continue
                    
                    # Find the largest video file (.mkv or .mp4)
                    video_files = []
                    for file_stat in file_stats:
                        file_path_torrent = file_stat.get("path", "")
                        file_length = file_stat.get("length", 0)
                        file_id = file_stat.get("id", 0)
                        
                        # Check if it's a video file
                        if file_path_torrent.lower().endswith(('.mkv', '.mp4', '.avi')):
                            video_files.append({
                                'id': file_id,
                                'size': file_length,
                                'path': file_path_torrent
                            })
                    
                    if not video_files:
                        continue
                    
                    # Sort by size descending and get the largest
                    largest_video = max(video_files, key=lambda x: x['size'])
                    correct_index = largest_video['id']
                    
                    size_gb = round(largest_video['size'] / self.BYTES_PER_GB, 1)
                    self.log("DEBUG", f"Video analysis: found {len(video_files)} video files, largest is ID {correct_index} ({size_gb}GB)")
                    
                    # Skip if already correct
                    if current_index == correct_index:
                        self.log("DEBUG", f"Skip {filename}: index already correct ({current_index})")
                        continue
                    
                    # Log what we're fixing
                    self.log("INFO", f"Fixing {filename}: index {current_index} → {correct_index} "
                            f"(largest video: {size_gb}GB)")
                    
                    # Create corrected stream URL
                    new_stream_line = re.sub(
                        r'index=\d+', 
                        f'index={correct_index}', 
                        stream_line
                    )
                    
                    # If no index parameter existed, add it
                    if 'index=' not in stream_line:
                        new_stream_line = stream_line.replace('&play', f'&index={correct_index}&play')
                    
                    # Update first line while preserving the rest
                    lines[0] = new_stream_line + '\n'
                    
                    # Write back to file
                    with open(file_path, 'w') as f:
                        f.writelines(lines)
                    
                    fixed += 1
                    
                except Exception as e:
                    self.log("WARN", f"Failed to analyze/fix {filename}: {e}")
                    errors += 1
                    continue
        
        self.log_success(f"Index correction complete: analyzed {analyzed}, fixed {fixed}, errors {errors}")

    def fix_tv_episodes_completeness(self):
        """
        Fix missing episodes and remove duplicates in TV shows
        Ensures complete episode sets without gaps and removes duplicate files
        """
        self.log("INFO", "Fixing TV episode completeness and removing duplicates...")
        
        try:
            # Import and run the fixer
            from tv_episode_fixer import TVEpisodeFixer
            fixer = TVEpisodeFixer()
            fixer.process_all_shows()
            
            self.log("INFO", f"TV episodes fixed - Missing: {fixer.missing_fixed}, Duplicates removed: {fixer.duplicates_removed}")
        except ImportError:
            self.log("WARN", "TV episode fixer module not found, skipping episode completeness check")
        except Exception as e:
            self.log("ERROR", f"Failed to fix TV episodes: {e}")
    
    def _kill_existing_instances(self):
        """
        Ensure single instance using atomic file locking (fcntl.flock).
        This is the standard Unix approach - no race conditions possible.

        Uses LOCK_EX | LOCK_NB for non-blocking exclusive lock.
        If another instance holds the lock, exits gracefully.
        Handles stale locks from crashed processes automatically.
        """
        import fcntl
        import os
        import sys
        import atexit

        lock_file = "/tmp/gostorm-sync.lock"
        pid_file = "/tmp/gostorm-sync.pid"

        # Open lock file (create if not exists)
        try:
            self._lock_fd = open(lock_file, 'w')
        except IOError as e:
            self.log("ERROR", f"Cannot create lock file: {e}")
            sys.exit(1)

        # Try to acquire EXCLUSIVE lock (non-blocking)
        try:
            fcntl.flock(self._lock_fd.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except IOError:
            # Lock held by another process - check if it's alive (handle stale locks)
            try:
                with open(pid_file, 'r') as f:
                    old_pid = int(f.read().strip())

                # Check if process exists (signal 0 = existence check only)
                os.kill(old_pid, 0)

                # Process is alive - exit gracefully
                self.log("ERROR", f"Another instance already running (PID {old_pid}). Exiting.")
                self._lock_fd.close()
                sys.exit(1)

            except (ProcessLookupError, ValueError, FileNotFoundError, PermissionError):
                # Stale lock - old process crashed without cleanup
                self.log("WARN", "Stale lock detected (previous instance crashed), forcing acquisition...")
                fcntl.flock(self._lock_fd.fileno(), fcntl.LOCK_EX)  # Blocking acquire
                self.log("INFO", "Lock acquired after stale cleanup")

        # Lock acquired successfully - write our PID
        try:
            with open(pid_file, 'w') as f:
                f.write(str(os.getpid()))
        except IOError:
            pass  # Non-critical if PID file fails

        self.log("INFO", f"Lock acquired, running as PID {os.getpid()}")

        # Register cleanup on exit (normal termination, exceptions, or sys.exit)
        atexit.register(self._release_lock)

    def _release_lock(self):
        """
        Release lock file on exit (called automatically via atexit).
        Also called by signal handlers for clean shutdown.
        """
        import fcntl
        import os

        lock_file = "/tmp/gostorm-sync.lock"
        pid_file = "/tmp/gostorm-sync.pid"

        try:
            if hasattr(self, '_lock_fd') and self._lock_fd:
                fcntl.flock(self._lock_fd.fileno(), fcntl.LOCK_UN)
                self._lock_fd.close()
                self._lock_fd = None
        except:
            pass

        # Cleanup files
        for f in [lock_file, pid_file]:
            try:
                os.remove(f)
            except OSError:
                pass

    def run(self) -> bool:
        """
        Main execution function - runs the complete sync process
        Exact replica of bash main execution flow
        """
        # AGGRESSIVE instance management - kill ALL existing instances before starting
        self._kill_existing_instances()
        
        self.log("INFO", "🎬 GoStorm Sync Starting - TMDB → Stremio → GoStorm integration")
        self.log("INFO", f"Start time: {time.strftime('%Y-%m-%d %H:%M:%S')}")

        # Clean up unreferenced torrents BEFORE processing new content
        self.log("INFO", "=== CLEANUP PHASE ====")
        self._prune_unreferenced_torrents()

        # Process movies
        total_movie_files = self.process_movies()

        # ============================================================
        # TV SERIES PROCESSING DISABLED - Use gostorm-tv-sync.py
        # ============================================================
        # total_tv_files = self.process_tv_shows()  # DISABLED - Moved to gostorm-tv-sync.py
        total_tv_files = 0  # TV processing disabled - use dedicated TV script

        # Rehydrate missing torrents (before cleanup)
        self.rehydrate_missing_torrents()

        # Prune unreferenced torrents to keep system clean
        if os.getenv("PRUNE_AFTER_PROCESS", "1") == "1":
            self._prune_unreferenced_torrents()
        
        # RIMOSSO: non rimuovere index dai film (necessario per playback)
        # self.fix_existing_movie_indexes()
        
        # Fix movie indexes intelligently - calculate correct index for largest video file
        self.fix_movie_indexes_intelligent()
        
        # Cleanup orphaned files
        self.cleanup_orphaned_files()

        # Get final statistics
        final_torrents = self._cached_torrents_list()
        final_torrent_count = len(final_torrents)

        # ============================================================
        # TV CLEANUP & MAINTENANCE DISABLED - Use gostorm-tv-sync.py
        # ============================================================
        # self.cleanup_duplicate_tv_directories()  # DISABLED - Moved to TV script
        # self.fix_tv_episodes_completeness()      # DISABLED - Moved to TV script

        # Count current files (Movies only)
        movie_count = 0
        tv_count = 0  # TV counting disabled

        if os.path.exists(self.MOVIES_DIR):
            movie_count = len([f for f in os.listdir(self.MOVIES_DIR) if f.endswith('.mkv')])

        # TV file counting disabled - use dedicated TV script
        # if os.path.exists(self.TV_DIR):
        #     for root, dirs, files in os.walk(self.TV_DIR):
        #         tv_count += len([f for f in files if f.endswith('.mkv')])
        
        # Final statistics (Movies only)
        self.log_success("Movies sync completed!")
        self.log("INFO", "Statistics:")
        self.log("INFO", f"  - Final torrents in GoStorm: {final_torrent_count}")
        self.log("INFO", f"  - Movie files processed: {total_movie_files}")
        # self.log("INFO", f"  - TV files processed: {total_tv_files}")  # DISABLED - Use TV script
        self.log("INFO", f"  - Movie files actual: {movie_count}")
        # self.log("INFO", f"  - TV files actual: {tv_count}")  # DISABLED - Use TV script
        self.log("INFO", "Current catalog:")
        self.log("INFO", f"  - Movies: {movie_count} .mkv virtual files")
        # self.log("INFO", f"  - TV Episodes: {tv_count} .mkv virtual files")  # DISABLED - Use TV script
        self.log("INFO", "")
        self.log("INFO", "⚠️  TV Series processing disabled in this script")
        self.log("INFO", "    Run gostorm-tv-sync.py for TV shows management")
        self.log("INFO", f"Complete Movies sync finished at {time.strftime('%Y-%m-%d %H:%M:%S')}")
        
        # Trigger Plex refresh for Film Go (Library ID 32)
        self.notify_plex(31)
        
        return True

if __name__ == "__main__":
    sync = GoStormSync()
    try:
        success = sync.run()
        sys.exit(0 if success else 1)
    except KeyboardInterrupt:
        sync.log("WARN", "Sync interrupted by user")
        sys.exit(1)
    except Exception as e:
        sync.log_error(f"Sync failed with error: {e}")
        sys.exit(1)
