#!/usr/bin/env python3
"""
GoStorm TV Sync - Clean Architecture
Fullpack-first approach with automatic quality upgrades

Features:
- Episode Registry: Prevents duplicates across runs
- Fullpack Priority: Season packs processed before singles (+1000 priority)
- Quality Scoring: Unified scoring (4K=200, HDR=100, Atmos=50)
- Automatic Upgrades: Replaces inferior versions when better quality found
- Cleanup: Removes orphaned files automatically

Directory Structure:
  /mnt/torrserver/tv/
    Show_Name/
      Season.01/
        Show_Name_S01E01_hash8.mkv
        Show_Name_S01E02_hash8.mkv
"""

import json
import logging
import os
import re
import signal
import sys
import time
import requests
from datetime import datetime, timedelta
from pathlib import Path
from typing import Dict, List, Optional, Tuple, Set, NamedTuple, Any


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

# Global shutdown flag for graceful termination
_shutdown_requested = False

def _signal_handler(signum, frame):
    """Handle SIGTERM/SIGINT for graceful shutdown"""
    global _shutdown_requested
    _shutdown_requested = True
    print(f"\n[SIGNAL] Received signal {signum}, finishing current operation...", flush=True)

# Register signal handlers
signal.signal(signal.SIGTERM, _signal_handler)
signal.signal(signal.SIGINT, _signal_handler)
from urllib.parse import quote


class EpisodeInfo(NamedTuple):
    """Episode information extracted from stream"""
    season: int
    episode: int
    file_id: int = 0
    file_size: int = 0
    file_path: str = ""


class GoStormTV:
    """Clean TV series sync with Episode Registry"""

    def __init__(self):
        # ===== CONFIGURATION (read from config.json, env overrides supported) =====
        self.TORRSERVER_URL = os.getenv("TORRSERVER_URL", _cfg.get('gostorm_url', 'http://127.0.0.1:8090'))
        _mount = _cfg.get('physical_source_path', '/mnt/torrserver')
        self.TV_DIR = os.getenv("TV_DIR", os.path.join(_mount, "tv"))
        self.STATE_DIR = os.getenv("STATE_DIR", _cfg.get('_state_dir', '/home/pi/STATE'))
        self.LOG_FILE = os.getenv("LOG_FILE", os.path.join(_cfg.get('_log_dir', '/home/pi/logs'), 'gostorm-tv-sync.log'))

        # API Keys
        self.TMDB_API_KEY = _cfg.get('tmdb_api_key', '')
        self.TMDB_BASE_URL = "https://api.themoviedb.org/3"
        self.TORRENTIO_BASE_URL = "https://torrentio.strem.fun"

        # Processing settings
        self.FULLPACK_PRIORITY_BONUS = 500  # Bonus per fullpack nel sorting (High Priority)
        self.UPGRADE_THRESHOLD = 1.2  # 20% improvement required for upgrade
        self.MIN_SEEDERS = 5  # Minimum seeders
        self.MIN_EPISODE_SIZE = 200 * 1024 * 1024  # 200MB
        self.MAX_EPISODE_SIZE = 30 * 1024 * 1024 * 1024  # 30GB

        # Quality thresholds for skipping already-complete seasons
        # Score breakdown: 4K=1000, HDR/DV=100, Atmos=50, 5.1=25
        self.MIN_QUALITY_SKIP = 1000  # Skip season if avg score >= 1000 (4K)
        self.MIN_QUALITY_ACCEPTABLE = 200  # Acceptable quality (1080p base)

        # Show age filter - only include shows from last 6 months
        self.MAX_SHOW_AGE_DAYS = int(os.getenv("MAX_SHOW_AGE_DAYS", "180"))

        # Genre filter - exclude non-scripted content
        self.EXCLUDED_GENRE_IDS = {99, 10763, 10764, 10767, 16}

        # Premium IT Providers (TMDB IDs)
        # 350: Apple TV Plus, 8: Netflix, 337: Disney Plus, 119: Amazon Prime Video
        self.PREMIUM_PROVIDER_IDS = {350, 8, 337, 119}

        # Rate limiting
        self.API_DELAY = 3.0
        self._last_api_call = 0

        # Trackers for magnet URLs (faster torrent resolution)
        self.TRACKERS_URL = "https://raw.githubusercontent.com/ngosang/trackerslist/master/trackers_best.txt"
        self.TRACKERS_CACHE_TTL = 3600  # 1 hour cache
        self._trackers_cache: List[str] = []
        self._trackers_cache_time = 0

        # Ensure directories exist
        for d in [self.STATE_DIR, self.TV_DIR, os.path.dirname(self.LOG_FILE)]:
            try:
                os.makedirs(d, exist_ok=True)
            except OSError:
                pass

        # Setup logging
        self._setup_logging()

        # HTTP session with browser User-Agent (Cloudflare bypass)
        self.session = requests.Session()
        self.session.headers.update({
            'User-Agent': 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36'
        })

        # ===== EPISODE REGISTRY (Core deduplication system) =====
        self.REGISTRY_FILE = os.path.join(self.STATE_DIR, "tv_episode_registry.json")
        self.BLACKLIST_FILE = os.path.join(self.STATE_DIR, "blacklist.json")
        self._registry: Dict[str, Dict] = self._load_registry()
        self._blacklist: Dict[str, Any] = self._load_blacklist()

        # Runtime tracking
        self._processed_this_run: Set[str] = set()
        self._stats = {"shows": 0, "episodes_created": 0, "episodes_skipped": 0, "upgrades": 0}

    # ===== LOGGING =====

    def _setup_logging(self):
        """Setup logging to file and console"""
        level = getattr(logging, os.getenv("LOG_LEVEL", "INFO").upper(), logging.INFO)

        handlers = []
        try:
            handlers.append(logging.FileHandler(self.LOG_FILE, encoding='utf-8'))
        except (OSError, PermissionError):
            pass

        if sys.stdout.isatty():
            handlers.append(logging.StreamHandler(sys.stdout))

        if not handlers:
            handlers.append(logging.StreamHandler(sys.stdout))

        logging.basicConfig(
            level=level,
            format='[%(asctime)s] [%(levelname)s] %(message)s',
            datefmt='%Y-%m-%d %H:%M:%S',
            handlers=handlers
        )
        self.logger = logging.getLogger("GoStormTV")

    def log(self, level: str, msg: str):
        getattr(self.logger, level.lower(), self.logger.info)(msg)

    # ===== EPISODE REGISTRY =====

    def _load_registry(self) -> Dict[str, Dict]:
        """Load episode registry from disk"""
        if not os.path.exists(self.REGISTRY_FILE):
            return {}
        try:
            with open(self.REGISTRY_FILE, 'r') as f:
                return json.load(f)
        except (json.JSONDecodeError, IOError) as e:
            self.log("WARN", f"Failed to load registry: {e}")
            return {}

    def _load_blacklist(self) -> Dict[str, Any]:
        """Load blacklist from disk (populated by Go Proxy on file deletion)"""
        if not os.path.exists(self.BLACKLIST_FILE):
            return {"hashes": {}, "titles": []}
        try:
            with open(self.BLACKLIST_FILE, 'r') as f:
                return json.load(f)
        except (json.JSONDecodeError, IOError) as e:
            self.log("WARN", f"Failed to load blacklist: {e}")
            return {"hashes": {}, "titles": []}

    def _normalize_title(self, title: str) -> str:
        """Normalize title for fuzzy blacklist matching (Squeeze logic)"""
        if not title: return ""
        t = title.lower()
        # Remove year FIRST
        t = re.sub(r'\(?\d{4}\)?', '', t)
        # Remove quality markers and tags
        t = re.sub(r'\b(2160p|1080p|720p|4k|uhd|hdr|dv|dovi|web|bluray|remux)\b.*', '', t)
        # Remove ALL non-alphanumeric characters INCLUDING spaces
        t = re.sub(r'[^a-z0-9]', '', t)
        return t.strip()

    def _save_registry(self):
        """Save episode registry atomically with file lock to prevent corruption"""
        import fcntl
        lock_file = self.REGISTRY_FILE + '.lock'
        # Use PID in temp filename to avoid race conditions between processes
        tmp = f"{self.REGISTRY_FILE}.{os.getpid()}.tmp"

        try:
            # Ensure STATE_DIR exists
            os.makedirs(os.path.dirname(self.REGISTRY_FILE), exist_ok=True)

            # Acquire lock FIRST to prevent concurrent writes
            with open(lock_file, 'w') as lf:
                fcntl.flock(lf.fileno(), fcntl.LOCK_EX)
                try:
                    # Write to temp file while holding lock
                    with open(tmp, 'w') as f:
                        json.dump(self._registry, f, indent=2)
                        f.flush()
                        os.fsync(f.fileno())

                    # Atomic replace
                    os.replace(tmp, self.REGISTRY_FILE)
                finally:
                    fcntl.flock(lf.fileno(), fcntl.LOCK_UN)
                    # Cleanup temp file if it still exists
                    if os.path.exists(tmp):
                        try:
                            os.remove(tmp)
                        except:
                            pass

        except (IOError, OSError) as e:
            self.log("ERROR", f"Failed to save registry: {e}")
            if os.path.exists(tmp):
                try:
                    os.remove(tmp)
                except:
                    pass

    def _episode_key(self, show: str, season: int, episode: int) -> str:
        """Generate unique episode key: showname_s01e05"""
        normalized = re.sub(r'[^a-z0-9]', '', show.lower())
        return f"{normalized}_s{season:02d}e{episode:02d}"

    def _register_episode(self, key: str, quality_score: int, hash_full: str,
                          file_path: str, source: str):
        """Register episode in registry and save immediately"""
        self._registry[key] = {
            "quality_score": quality_score,
            "hash": hash_full,
            "file_path": file_path,
            "source": source,
            "created": int(time.time())
        }
        # Save immediately after each episode to prevent data loss
        self._save_registry()

    def _populate_registry_from_existing(self):
        """
        Populate registry with existing .mkv files to prevent cleanup.
        This protects previously downloaded shows that may now be filtered out.
        """
        if not os.path.exists(self.TV_DIR):
            return

        added = 0
        for root, _, files in os.walk(self.TV_DIR):
            for f in files:
                if not f.endswith('.mkv'):
                    continue

                filepath = os.path.join(root, f)

                # Skip if already in registry
                if any(v.get('file_path') == filepath for v in self._registry.values()):
                    continue

                # Extract info from filename: ShowName_S01E05_hash8.mkv
                match = re.match(r'(.+)_S(\d+)E(\d+)_([a-f0-9]{8})\.mkv$', f)
                if not match:
                    continue

                show_name, season, episode, hash8 = match.groups()
                key = self._episode_key(show_name, int(season), int(episode))

                # Skip if key already exists (different file for same episode)
                if key in self._registry:
                    continue

                # Read file to get full hash
                try:
                    with open(filepath, 'r') as mf:
                        lines = mf.readlines()
                    if len(lines) >= 1:
                        hash_match = re.search(r'link=([a-f0-9]{40})', lines[0], re.I)
                        full_hash = hash_match.group(1) if hash_match else None
                    else:
                        full_hash = None
                except:
                    full_hash = None

                # Resolve hash8 to full 40-char hash via GoStorm lookup
                if not full_hash and hash8:
                    if not hasattr(self, '_ts_hash_cache'):
                        all_torrents = self._ts_list_torrents() or []
                        self._ts_hash_cache = {(t.get('hash') or '')[:8].lower(): (t.get('hash') or '').lower() for t in all_torrents}
                    full_hash = self._ts_hash_cache.get(hash8.lower(), hash8)

                # Add to registry with minimal score (won't block upgrades)
                self._registry[key] = {
                    "quality_score": 1,  # Minimal score - allows upgrades
                    "hash": full_hash,
                    "file_path": filepath,
                    "source": "existing",
                    "created": int(os.path.getmtime(filepath))
                }
                added += 1

        if added > 0:
            self.log("INFO", f"Protected {added} existing episodes from cleanup")

    # ===== HTTP HELPERS =====

    def _rate_limit(self):
        """Enforce rate limiting between API calls"""
        elapsed = time.time() - self._last_api_call
        if elapsed < self.API_DELAY:
            time.sleep(self.API_DELAY - elapsed)
        self._last_api_call = time.time()

    def _get(self, url: str, params: Dict = None, timeout: int = 30) -> Optional[requests.Response]:
        """GET request with rate limiting and retries"""
        self._rate_limit()
        for attempt in range(3):
            try:
                resp = self.session.get(url, params=params, timeout=timeout)
                if resp.status_code == 200:
                    return resp
                self.log("WARN", f"HTTP {resp.status_code} for {url}")
            except requests.RequestException as e:
                self.log("WARN", f"Request failed (attempt {attempt+1}): {e}")
            time.sleep(2 ** attempt)
        return None

    def _post(self, url: str, data: Dict, timeout: int = 30) -> Optional[requests.Response]:
        """POST request with rate limiting"""
        self._rate_limit()
        try:
            resp = self.session.post(url, json=data, timeout=timeout)
            if resp.status_code == 200:
                return resp
        except requests.RequestException as e:
            self.log("WARN", f"POST failed: {e}")
        return None

    # ===== TRACKERS & MAGNET =====

    def _fetch_trackers(self) -> List[str]:
        """Fetch public trackers from GitHub with caching for faster torrent resolution"""
        now = time.time()
        if self._trackers_cache and (now - self._trackers_cache_time) < self.TRACKERS_CACHE_TTL:
            return self._trackers_cache

        try:
            resp = self.session.get(self.TRACKERS_URL, timeout=10)
            if resp.status_code == 200:
                # Parse trackers (one per line, skip empty)
                trackers = [line.strip() for line in resp.text.split('\n') if line.strip()]
                if trackers:
                    self._trackers_cache = trackers[:20]  # Limit to 20 best
                    self._trackers_cache_time = now
                    self.log("DEBUG", f"Fetched {len(self._trackers_cache)} trackers from GitHub")
                    return self._trackers_cache
        except Exception as e:
            self.log("DEBUG", f"Failed to fetch trackers: {e}")

        # Fallback to hardcoded trackers
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
        magnet = f"magnet:?xt=urn:btih:{info_hash}"
        if name:
            magnet += f"&dn={quote(name)}"
        trackers = self._setup_trackers_cache() if hasattr(self, '_setup_trackers_cache') else self._fetch_trackers()
        for tr in trackers:
            magnet += f"&tr={quote(tr)}"
        return magnet

    def notify_plex(self, section_id: int):
        """Send a refresh command to Plex for a specific library section."""
        PLEX_URL = _cfg.get('plex', {}).get('url', 'http://127.0.0.1:32400')
        PLEX_TOKEN = _cfg.get('plex', {}).get('token', '')

        try:
            url = f"{PLEX_URL}/library/sections/{section_id}/refresh?X-Plex-Token={PLEX_TOKEN}"
            self.log("INFO", f"🚀 Notifying Plex to refresh library section {section_id}...")
            resp = requests.get(url, timeout=10)
            if resp.status_code in (200, 201):
                self.log("INFO", f"Plex refresh triggered successfully for section {section_id}")
            else:
                self.log("WARN", f"Plex refresh returned status {resp.status_code}")
        except Exception as e:
            self.log("ERROR", f"Failed to notify Plex: {e}")

    # ===== QUALITY SCORING =====

    def _calculate_quality_score(self, text: str, seeders: int = 0) -> int:
        """
        Unified quality scoring (same as Movies script)
        Returns 0 for excluded content (720p or lower)
        """
        t = text.lower()
        score = 0

        # Resolution (base score) - Massive 4K Priority
        if re.search(r'2160p|4k|uhd', t):
            score += 1000
        elif re.search(r'1080p', t):
            score += 200
        else:
            # FIX: Se non è almeno 1080p, scartiamo tutto (480p, SD, 720p)
            return 0

        # HDR/DV bonus
        if re.search(r'\bhdr\b|hdr10\+?|\bdv\b|dovi|dolby.?vision', t):
            score += 100

        # Audio bonus
        if re.search(r'atmos', t):
            score += 50
        elif re.search(r'5\.1|dd5|ddp5|dts|truehd', t):
            score += 25

        # Lingua Bonus (ITA / MULTI prioritario)
        if re.search(r'ita|🇮🇹|multi|dual', t):
            score += 40

        # Seeder bonus - UNIFIED
        if seeders >= 100:
            score += 100
        elif seeders >= 50:
            score += 50
        elif seeders >= 20:
            score += 10

        return score

    # ===== STREAM CLASSIFICATION =====

    def _is_fullpack(self, title: str) -> bool:
        """Detect if stream is a season pack/fullpack"""
        # Use only first line (torrent name) - other lines are file names
        first_line = title.split('\n')[0].lower()
        t = first_line

        # Keywords
        if re.search(r'\b(season|complete|full|pack)\b', t):
            return True
        # Range pattern: S01E01-E10, S01E01-10
        if re.search(r's\d+e\d+\s*-\s*e?\d+', t):
            return True
        # Multiple episodes in title
        if len(re.findall(r's\d+e\d+', t)) >= 2:
            return True
        # Season number without episode (S01 but NOT S01E01)
        # e.g., "Show.S01.2160p..." is a fullpack
        if re.search(r'\.s\d{2}\.', t) and not re.search(r's\d+e\d+', t):
            return True
        # Parenthetical S01 pattern: "Show (2025) S01 (1080p..."
        if re.search(r'\ss\d{2}\s*\(', t) and not re.search(r's\d+e\d+', t):
            return True
        return False

    def _extract_season(self, title: str) -> int:
        """Extract season number from title"""
        patterns = [
            r'[Ss](\d+)',
            r'[Ss]eason[\s.]?(\d+)',
            r'(\d+)(?:st|nd|rd|th)?\s*[Ss]eason'
        ]
        for p in patterns:
            m = re.search(p, title)
            if m:
                return int(m.group(1))
        return 1  # Default season 1

    def _extract_season_span(self, title: str) -> Optional[Tuple[int, int]]:
        """
        Detect multi-season packs like: S01-S21, Season 1-21, Seasons 01-21
        Returns (min_season, max_season) if detected, else None.
        Uses only first line (torrent name).
        """
        first_line = title.split('\n')[0].lower()

        # Patterns like: S01-S21 or S1 - S21
        m = re.search(r'\bs(\d{1,2})\s*[-–]\s*s(\d{1,2})\b', first_line)
        if m:
            a, b = int(m.group(1)), int(m.group(2))
            return (min(a, b), max(a, b))

        # Patterns like: Season 1-21 / Seasons 01 - 21
        m = re.search(r'\bseasons?\s*(\d{1,2})\s*[-–]\s*(\d{1,2})\b', first_line)
        if m:
            a, b = int(m.group(1)), int(m.group(2))
            return (min(a, b), max(a, b))

        # "Complete Series" without season numbers - likely includes all seasons
        if re.search(r'\b(complete\s+series|all\s+seasons|full\s+series)\b', first_line):
            return (1, 99)  # Mark as spanning all seasons

        return None

    def _extract_seeders(self, title: str) -> int:
        """Extract seeder count from Torrentio title"""
        # 👤 emoji followed by number
        m = re.search(r'👤\s*(\d+)', title)
        return int(m.group(1)) if m else 0

    def _extract_size_gb(self, title: str) -> float:
        """Extract size in GB from title"""
        m = re.search(r'(\d+\.?\d*)\s*(GB|TB)', title, re.I)
        if m:
            size = float(m.group(1))
            unit = m.group(2).upper()
            return size * 1024 if unit == "TB" else size
        return 0

    def _classify_stream(self, stream: Dict) -> Optional[Dict]:
        """
        Classify a Torrentio stream
        Returns None if stream should be excluded
        """
        title = stream.get('title', '')
        name = stream.get('name', '')  # Torrentio quality field
        info_hash = stream.get('infoHash', '')

        if not title or not info_hash:
            return None

        # FASE 5.2: Blacklist Filter (Hash or Normalized Title)
        if info_hash.lower() in self._blacklist.get("hashes", {}):
            self.log("DEBUG", f"Blacklist hit (hash): {info_hash[:8]} for {title[:40]}...")
            return None
        
        # Check normalized titles (fuzzy match)
        clean_title = self._normalize_title(title)
        if clean_title in self._blacklist.get("titles", []):
            self.log("INFO", f"🚫 Blacklist hit (fuzzy title): '{clean_title}' (original: {title[:40]}...)")
            return None

        # Combine title and name for quality detection
        full_text = f"{title} {name}"

        # Calculate quality score
        seeders = self._extract_seeders(title)
        quality_score = self._calculate_quality_score(full_text, seeders)

        # Skip if excluded (720p or lower)
        if quality_score == 0:
            return None

        # FASE 5.3: Adaptive Seeder Limit (4K needs more health)
        is_4k = bool(re.search(r'2160p|4k|uhd', full_text.lower()))
        min_required = 10 if is_4k else self.MIN_SEEDERS
        
        if seeders < min_required:
            self.log("DEBUG", f"Skip low health stream ({seeders}/{min_required} seeders): {title[:50]}...")
            return None

        # Language filter - skip non-English/Italian
        excluded_langs = r'🇪🇸|🇫🇷|🇩🇪|🇷🇺|🇨🇳|🇯🇵|🇰🇷|🇹🇭|🇵🇹|🇧🇷'
        if re.search(excluded_langs, title):
            return None

        is_fullpack = self._is_fullpack(title)
        season = self._extract_season(title)

        # Detect partial fullpacks (E01-02, E01-03, etc.) - these are NOT complete seasons
        first_line = title.split('\n')[0].lower()
        is_partial_pack = bool(re.search(r'e\d+-e?\d+', first_line) and is_fullpack)

        # Priority: complete fullpacks > partial fullpacks > singles
        # Partial packs get only half the bonus
        if is_fullpack:
            priority_bonus = self.FULLPACK_PRIORITY_BONUS // 2 if is_partial_pack else self.FULLPACK_PRIORITY_BONUS
        else:
            priority_bonus = 0

        return {
            'title': title,
            'hash': info_hash,
            'is_fullpack': is_fullpack,
            'is_partial_pack': is_partial_pack,
            'quality_score': quality_score,
            'season': season,
            'seeders': seeders,
            'size_gb': self._extract_size_gb(title),
            'priority': quality_score + priority_bonus
        }

    # ===== TMDB DISCOVERY =====

    def _tmdb_get(self, endpoint: str, params: Dict = None) -> Optional[Dict]:
        """TMDB API request"""
        url = f"{self.TMDB_BASE_URL}/{endpoint}"
        params = params or {}
        params['api_key'] = self.TMDB_API_KEY
        resp = self._get(url, params=params)
        if resp:
            try:
                return resp.json()
            except json.JSONDecodeError:
                pass
        return None

    def _is_show_recent(self, show: Dict) -> bool:
        """
        Check if show has CURRENT/RECENT season (airing in last year or upcoming).
        Includes old shows with new seasons (Stranger Things S5, Grey's Anatomy S21).
        """
        # Check if first_air_date is recent (new show)
        first_air = show.get('first_air_date', '')
        if first_air:
            try:
                first_date = datetime.strptime(first_air, '%Y-%m-%d')
                if (datetime.now() - first_date).days <= self.MAX_SHOW_AGE_DAYS:
                    return True
            except ValueError:
                pass

        # For older shows, check if they have recent/upcoming episodes
        tmdb_id = show.get('id')
        if not tmdb_id:
            return False

        details = self._fetch_show_details(tmdb_id)
        if not details:
            return False

        # Check last_air_date (recent episode aired)
        last_air = details.get('last_air_date', '')
        if last_air:
            try:
                last_date = datetime.strptime(last_air, '%Y-%m-%d')
                if (datetime.now() - last_date).days <= self.MAX_SHOW_AGE_DAYS:
                    return True
            except ValueError:
                pass

        # Check next_episode_to_air (upcoming episodes)
        next_ep = details.get('next_episode_to_air')
        if next_ep and next_ep.get('air_date'):
            return True  # Has upcoming episode = currently airing

        return False

    def _fetch_show_details(self, tmdb_id: int) -> Optional[Dict]:
        """Fetch full show details from TMDB to get last_air_date and watch providers"""
        url = f"{self.TMDB_BASE_URL}/tv/{tmdb_id}"
        # FASE 6.1: Include watch providers in details call
        params = {"api_key": self.TMDB_API_KEY, "append_to_response": "watch/providers"}
        try:
            resp = self.session.get(url, params=params, timeout=10)
            if resp.status_code == 200:
                return resp.json()
        except requests.RequestException:
            pass
        return None

    def _is_on_premium_streaming_it(self, details: Dict) -> bool:
        """Check if show is available on premium services in Italy (flatrate)"""
        try:
            providers = details.get('watch/providers', {}).get('results', {}).get('IT', {}).get('flatrate', [])
            for p in providers:
                if p.get('provider_id') in self.PREMIUM_PROVIDER_IDS:
                    self.log("DEBUG", f"Premium IT Provider match: {p.get('provider_name')} for {details.get('name')}")
                    return True
        except Exception:
            pass
        return False

    def discover_shows(self) -> List[Dict]:
        """
        Discover TV shows from multiple TMDB endpoints
        Returns list of shows with IMDB IDs
        Only includes shows from the last MAX_SHOW_AGE_DAYS (default 365)
        """
        self.log("INFO", f"Discovering TV shows (max age: {self.MAX_SHOW_AGE_DAYS} days)...")
        all_results = []
        seen_ids = set()
        skipped_old = 0
        skipped_genre = 0
        skipped_lang = 0

        # Calculate cutoff date for discover endpoints
        cutoff_date = (datetime.now() - timedelta(days=self.MAX_SHOW_AGE_DAYS)).strftime('%Y-%m-%d')

        # Endpoints to query - ONLY recent shows (first_air_date filter on discover)
        # Reduced pages for faster processing
        endpoints = [
            # Currently airing - highest priority (already filtered by TMDB)
            ("tv/on_the_air", {}, 3),
            # Airing today
            ("tv/airing_today", {}, 2),
            # Trending this week
            ("trending/tv/week", {}, 3),
            # Discover English - RECENT shows only (first_air_date filter)
            ("discover/tv", {
                'with_original_language': 'en',
                'sort_by': 'popularity.desc',
                'first_air_date.gte': cutoff_date  # Only shows from last year
            }, 5),
        ]

        for endpoint, extra_params, max_pages in endpoints:
            # Get multiple pages per endpoint
            for page in range(1, max_pages + 1):
                params = {'page': page, **extra_params}
                data = self._tmdb_get(endpoint, params)
                if not data:
                    continue

                for show in data.get('results', []):
                    show_id = show.get('id')
                    if show_id in seen_ids:
                        continue
                    seen_ids.add(show_id)

                    # Filter by genre - exclude Reality, Talk, Anime (check early to save detail calls)
                    show_genres = set(show.get('genre_ids', []))
                    if show_genres & self.EXCLUDED_GENRE_IDS:
                        skipped_genre += 1
                        continue

                    # Language bypass logic: English is always accepted, 
                    # International shows are checked against Italian Premium Providers
                    lang = show.get('original_language', '')
                    
                    details = self._fetch_show_details(show_id)
                    if not details:
                        continue

                    is_premium_it = self._is_on_premium_streaming_it(details)
                    
                    if lang != 'en' and not is_premium_it:
                        skipped_lang += 1
                        continue

                    # Filter by age - only recent shows
                    if not self._is_show_recent(show):
                        skipped_old += 1
                        continue

                    all_results.append(show)

        self.log("INFO", f"Discovered {len(all_results)} TV shows (skipped: {skipped_old} old, {skipped_genre} reality/talk/anime, {skipped_lang} non-premium international)")
        return all_results

    def _get_imdb_id(self, tmdb_id: int) -> Optional[str]:
        """Get IMDB ID for a TMDB show"""
        data = self._tmdb_get(f"tv/{tmdb_id}/external_ids")
        if data:
            return data.get('imdb_id')
        return None

    # ===== TORRENTIO =====

    def get_streams(self, imdb_id: str, tmdb_id: int = 0) -> List[Dict]:
        """
        Get and classify streams from Torrentio for all seasons.
        Torrentio requires season:episode in URL, so we query EACH EPISODE.
        Returns classified streams sorted by priority.
        """
        # Get season info from TMDB (including episode counts)
        num_seasons = 5  # Default fallback
        season_episodes = {}  # season_num -> episode_count

        if tmdb_id:
            details = self._fetch_show_details(tmdb_id)
            if details:
                num_seasons = details.get('number_of_seasons', 5)
                # Build episode count per season
                for season_data in details.get('seasons', []):
                    s_num = season_data.get('season_number', 0)
                    if s_num > 0:  # Skip specials (season 0)
                        ep_count = season_data.get('episode_count', 10)
                        season_episodes[s_num] = ep_count

        # Only download last 2 seasons (current + previous)
        # For Grey's Anatomy S21: downloads S20, S21 only
        MAX_SEASONS = 2
        start_season = max(1, num_seasons - MAX_SEASONS + 1)
        end_season = num_seasons

        all_streams = []
        seen_hashes = set()

        # Query EACH EPISODE in recent seasons (not just E01)
        # This ensures singles for E02, E03, etc. are included
        for season in range(start_season, num_seasons + 1):
            # Get episode count for this season (default 10)
            ep_count = season_episodes.get(season, 10)

            # Query each episode to get all singles + fullpacks
            for episode in range(1, ep_count + 1):
                url = f"{self.TORRENTIO_BASE_URL}/stream/series/{imdb_id}:{season}:{episode}.json"
                resp = self._get(url, timeout=15)

                if not resp:
                    continue

                try:
                    data = resp.json()
                except json.JSONDecodeError:
                    continue

                streams = data.get('streams', [])
                for s in streams:
                    # Deduplicate by infoHash
                    h = s.get('infoHash', '')
                    if h and h not in seen_hashes:
                        seen_hashes.add(h)
                        all_streams.append(s)

                # Delay between episode requests to avoid Torrentio 502 errors
                time.sleep(0.5)

        if not all_streams:
            return []

        # Classify all streams and filter by allowed seasons
        classified = []
        skipped_season = 0
        skipped_span = 0

        for s in all_streams:
            c = self._classify_stream(s)
            if not c:
                continue

            # FILTER 1: Keep only streams with season in allowed range
            if not (start_season <= c['season'] <= end_season):
                skipped_season += 1
                continue

            # FILTER 2: Drop multi-season packs that include older seasons
            # e.g., "Complete Series S01-S21" when we only want S20-S21
            span = self._extract_season_span(c['title'])
            if span and span[0] < start_season:
                skipped_span += 1
                continue

            classified.append(c)

        if skipped_season or skipped_span:
            self.log("DEBUG", f"Filtered streams: {skipped_season} wrong season, {skipped_span} multi-season packs")

        # Sort by priority (descending)
        classified.sort(key=lambda x: -x['priority'])

        return classified

    # ===== TORRSERVER API =====

    def _ts_add_torrent(self, magnet: str, title: str = "") -> Optional[str]:
        """Add torrent to GoStorm, returns hash"""
        data = {"action": "add", "link": magnet, "title": title, "save": True}
        resp = self._post(f"{self.TORRSERVER_URL}/torrents", data, timeout=60)
        if resp:
            try:
                result = resp.json()
                return result.get('hash', '').lower()
            except json.JSONDecodeError:
                pass
        return None

    def _is_hash_on_disk(self, full_hash: str) -> bool:
        """Check if any .mkv file on disk references this hash (via filename hash8)."""
        hash8 = full_hash[:8].lower()
        for root, dirs, files in os.walk(self.TV_DIR):
            for f in files:
                if f.endswith('.mkv') and hash8 in f.lower():
                    return True
        return False

    def _ts_remove_torrent(self, hash_val: str) -> bool:
        """Remove torrent from GoStorm"""
        data = {"action": "rem", "hash": hash_val}
        resp = self._post(f"{self.TORRSERVER_URL}/torrents", data)
        return resp is not None

    def _ts_get_torrent(self, hash_val: str) -> Optional[Dict]:
        """Get torrent info from GoStorm"""
        data = {"action": "get", "hash": hash_val}
        resp = self._post(f"{self.TORRSERVER_URL}/torrents", data)
        if resp:
            try:
                return resp.json()
            except json.JSONDecodeError:
                pass
        return None

    def _ts_list_torrents(self) -> List[Dict]:
        """List all torrents in GoStorm"""
        data = {"action": "list"}
        resp = self._post(f"{self.TORRSERVER_URL}/torrents", data)
        if resp:
            try:
                return resp.json()
            except json.JSONDecodeError:
                pass
        return []

    def _wait_for_metadata(self, hash_val: str, max_wait: int = 60) -> Optional[Dict]:
        """Wait for torrent metadata to be available"""
        start = time.time()
        while (time.time() - start) < max_wait:
            info = self._ts_get_torrent(hash_val)
            if info:
                file_stats = info.get('file_stats', [])
                if file_stats:
                    return info
            time.sleep(3)
        return None

    # ===== FILE OPERATIONS =====

    def _sanitize_name(self, name: str) -> str:
        """Sanitize show/file name for filesystem"""
        # Remove problematic characters including quotes and ampersand
        clean = re.sub(r'[<>:"/\\|?*\'\"&]', '', name)
        # Replace spaces with underscores
        clean = re.sub(r'\s+', '_', clean)
        # Remove consecutive underscores
        clean = re.sub(r'_+', '_', clean)
        return clean.strip('_')

    def _get_show_folder_name(self, show_name: str, first_air_date: str) -> str:
        """Build folder name with year for Plex disambiguation: 'Show_Name (2025)'"""
        clean_name = self._sanitize_name(show_name)
        year = ""
        if first_air_date:
            try:
                year = first_air_date[:4]  # Extract YYYY from YYYY-MM-DD
            except (IndexError, TypeError):
                pass
        if year:
            return f"{clean_name} ({year})"
        return clean_name

    def _build_filename(self, show: str, season: int, episode: int, hash8: str) -> str:
        """Build consistent episode filename"""
        clean_show = self._sanitize_name(show)
        return f"{clean_show}_S{season:02d}E{episode:02d}_{hash8}.mkv"

    def _create_mkv(self, filepath: str, stream_url: str, file_size: int, magnet: str) -> bool:
        """Create virtual .mkv file with metadata"""
        try:
            os.makedirs(os.path.dirname(filepath), exist_ok=True)
            with open(filepath, 'w') as f:
                f.write(stream_url + '\n')
                f.write(str(file_size) + '\n')
                f.write(magnet + '\n')
            return True
        except IOError as e:
            self.log("ERROR", f"Failed to create {filepath}: {e}")
            return False

    def _is_video_file(self, path: str) -> bool:
        """Check if file is a video"""
        return path.lower().endswith(('.mkv', '.mp4', '.avi', '.mov', '.m4v'))

    def _extract_episode_from_filename(self, filename: str) -> Optional[Tuple[int, int]]:
        """Extract (season, episode) from filename"""
        # S01E05 pattern
        m = re.search(r'[Ss](\d+)[Ee](\d+)', filename)
        if m:
            return (int(m.group(1)), int(m.group(2)))
        # 1x05 pattern
        m = re.search(r'(\d+)x(\d+)', filename)
        if m:
            return (int(m.group(1)), int(m.group(2)))
        return None

    # ===== EPISODE PROCESSING =====

    def _process_fullpack(self, show_name: str, stream: Dict, first_air_date: str = "") -> int:
        """
        Process fullpack stream - create episodes from torrent files
        Returns number of episodes created
        """
        hash_val = stream['hash']
        title = stream['title']
        quality_score = stream['quality_score']

        # Build magnet with trackers for faster resolution
        magnet = self._build_magnet(hash_val, title)

        # Add to GoStorm
        added_hash = self._ts_add_torrent(magnet, title)
        if not added_hash:
            self.log("WARN", f"Failed to add fullpack: {title[:60]}...")
            return 0

        # Wait for metadata
        info = self._wait_for_metadata(added_hash, max_wait=90)
        if not info:
            self.log("WARN", f"Metadata timeout for fullpack: {title[:60]}...")
            self._ts_remove_torrent(added_hash)
            return 0

        # Process video files
        file_stats = info.get('file_stats', [])
        video_files = [f for f in file_stats if self._is_video_file(f.get('path', ''))]

        if not video_files:
            self.log("WARN", f"No video files in fullpack: {title[:60]}...")
            return 0

        created = 0
        clean_show = self._get_show_folder_name(show_name, first_air_date)

        for vf in video_files:
            filepath = vf.get('path', '')
            file_id = vf.get('id', 0)
            file_size = vf.get('length', 0)

            # Size filter
            if file_size < self.MIN_EPISODE_SIZE or file_size > self.MAX_EPISODE_SIZE:
                continue

            # Extract episode info
            filename = os.path.basename(filepath)
            ep_info = self._extract_episode_from_filename(filename)
            if not ep_info:
                continue

            season, episode = ep_info
            key = self._episode_key(show_name, season, episode)

            # Skip if already processed this run
            if key in self._processed_this_run:
                continue

            # Check registry for existing version
            if key in self._registry:
                existing = self._registry[key]
                if quality_score <= existing['quality_score'] * self.UPGRADE_THRESHOLD:
                    self._stats['episodes_skipped'] += 1
                    self._processed_this_run.add(key)
                    continue
                # Upgrade - remove old file
                old_path = existing.get('file_path', '')
                if old_path and os.path.exists(old_path):
                    try:
                        os.remove(old_path)
                        self.log("INFO", f"Removed inferior version: {os.path.basename(old_path)}")
                        self._stats['upgrades'] += 1
                    except OSError:
                        pass

            # Create season directory (format: Season.01 to match Movies script)
            season_dir = os.path.join(self.TV_DIR, clean_show, f"Season.{season:02d}")

            # Build filename and path
            ep_filename = self._build_filename(show_name, season, episode, hash_val[:8])
            ep_path = os.path.join(season_dir, ep_filename)

            # Create stream URL
            stream_url = f"{self.TORRSERVER_URL}/stream?link={hash_val}&index={file_id}&play"

            # Create .mkv file
            if self._create_mkv(ep_path, stream_url, file_size, magnet):
                self._register_episode(key, quality_score, hash_val, ep_path, "fullpack")
                self._processed_this_run.add(key)
                self._stats['episodes_created'] += 1
                created += 1
                self.log("INFO", f"Created: {ep_filename}")

        # Clean up: remove torrent from GoStorm if no episodes were created
        if created == 0 and added_hash:
            self._ts_remove_torrent(added_hash)
            self.log("DEBUG", f"Removed unused fullpack (0 episodes created): {title[:60]}...")

        return created

    def _process_single(self, show_name: str, stream: Dict, first_air_date: str = "") -> int:
        """
        Process single episode stream
        Returns 1 if created, 0 otherwise
        """
        hash_val = stream['hash']
        title = stream['title']
        quality_score = stream['quality_score']
        season = stream['season']

        # Extract episode number from title
        m = re.search(r'[Ss]\d+[Ee](\d+)', title)
        if not m:
            return 0
        episode = int(m.group(1))

        key = self._episode_key(show_name, season, episode)

        # Skip if already processed
        if key in self._processed_this_run:
            return 0

        # Check registry
        if key in self._registry:
            existing = self._registry[key]
            if quality_score <= existing['quality_score'] * self.UPGRADE_THRESHOLD:
                self._stats['episodes_skipped'] += 1
                self._processed_this_run.add(key)
                return 0

        # Build magnet with trackers and add to GoStorm
        magnet = self._build_magnet(hash_val, title)
        added_hash = self._ts_add_torrent(magnet, title)
        if not added_hash:
            return 0

        # Wait for metadata
        info = self._wait_for_metadata(added_hash, max_wait=45)
        if not info:
            self._ts_remove_torrent(added_hash)
            return 0

        # Find the video file
        file_stats = info.get('file_stats', [])
        video_files = [f for f in file_stats if self._is_video_file(f.get('path', ''))]

        if not video_files:
            return 0

        # Use largest video file
        video_files.sort(key=lambda x: x.get('length', 0), reverse=True)
        vf = video_files[0]
        file_id = vf.get('id', 0)
        file_size = vf.get('length', 0)

        # Size filter
        if file_size < self.MIN_EPISODE_SIZE:
            return 0

        # If upgrading, remove old
        if key in self._registry:
            old_path = self._registry[key].get('file_path', '')
            if old_path and os.path.exists(old_path):
                try:
                    os.remove(old_path)
                    self.log("INFO", f"Removed inferior version: {os.path.basename(old_path)}")
                    self._stats['upgrades'] += 1
                except OSError:
                    pass

        # Create paths (format: Season.01 to match Movies script)
        clean_show = self._get_show_folder_name(show_name, first_air_date)
        season_dir = os.path.join(self.TV_DIR, clean_show, f"Season.{season:02d}")
        ep_filename = self._build_filename(show_name, season, episode, hash_val[:8])
        ep_path = os.path.join(season_dir, ep_filename)

        stream_url = f"{self.TORRSERVER_URL}/stream?link={hash_val}&index={file_id}&play"

        if self._create_mkv(ep_path, stream_url, file_size, magnet):
            self._register_episode(key, quality_score, hash_val, ep_path, "single")
            self._processed_this_run.add(key)
            self._stats['episodes_created'] += 1
            self.log("INFO", f"Created: {ep_filename}")
            return 1

        return 0

    def _get_complete_seasons(self, show_name: str, tmdb_id: int) -> Dict[int, Tuple[int, float]]:
        """
        Check which seasons are already complete with good quality.
        Returns dict: {season_num: (episode_count, avg_quality_score)}
        """
        # Normalize show name for registry lookup
        normalized = re.sub(r'[^a-z0-9]', '', show_name.lower())

        # Get total episodes per season from TMDB
        details = self._fetch_show_details(tmdb_id)
        if not details:
            return {}

        seasons_info = {}
        for season_data in details.get('seasons', []):
            season_num = season_data.get('season_number', 0)
            if season_num == 0:  # Skip specials
                continue
            episode_count = season_data.get('episode_count', 0)
            if episode_count > 0:
                seasons_info[season_num] = episode_count

        # Count episodes in registry per season and calculate avg score
        complete_seasons = {}
        for season_num, expected_count in seasons_info.items():
            episodes_found = []
            for key, entry in self._registry.items():
                # Match pattern: showname_s01e05
                if key.startswith(normalized) and f'_s{season_num:02d}e' in key:
                    episodes_found.append(entry.get('quality_score', 0))

            if len(episodes_found) >= expected_count:
                avg_score = sum(episodes_found) / len(episodes_found) if episodes_found else 0
                complete_seasons[season_num] = (len(episodes_found), avg_score)

        return complete_seasons

    def _should_skip_season(self, season_num: int, complete_seasons: Dict[int, Tuple[int, float]]) -> bool:
        """Check if a season should be skipped (complete with good quality)"""
        if season_num not in complete_seasons:
            return False

        episode_count, avg_score = complete_seasons[season_num]
        # Skip if complete and quality is good enough
        return avg_score >= self.MIN_QUALITY_SKIP

    def process_show(self, show: Dict) -> int:
        """
        Process a TV show - get streams, sort, and create episodes
        Skips seasons that are already complete with good quality.
        Returns total episodes created
        """
        show_name = show.get('name') or show.get('original_name', '')
        tmdb_id = show.get('id')
        first_air_date = show.get('first_air_date', '')

        if not show_name or not tmdb_id:
            return 0

        # V135: Blacklist check at SHOW level (not just torrent level)
        clean_show_name = self._normalize_title(show_name)
        if clean_show_name in self._blacklist.get("titles", []):
            self.log("INFO", f"🚫 Blacklist: skipping show '{show_name}' (normalized: {clean_show_name})")
            return 0

        # Get IMDB ID
        imdb_id = self._get_imdb_id(tmdb_id)
        if not imdb_id:
            self.log("DEBUG", f"No IMDB ID for: {show_name}")
            return 0

        self.log("INFO", f"Processing: {show_name} ({imdb_id})")

        # Check which seasons are already complete with good quality
        complete_seasons = self._get_complete_seasons(show_name, tmdb_id)
        skipped_seasons = set()
        for season_num, (ep_count, avg_score) in complete_seasons.items():
            if avg_score >= self.MIN_QUALITY_SKIP:
                skipped_seasons.add(season_num)
                self.log("DEBUG", f"Skip S{season_num:02d}: {ep_count} eps, avg score {avg_score:.0f} >= {self.MIN_QUALITY_SKIP}")

        if skipped_seasons:
            self.log("INFO", f"Skipping seasons {sorted(skipped_seasons)} (complete, quality >= {self.MIN_QUALITY_SKIP})")

        # Get and sort streams (pass tmdb_id to get correct number of seasons)
        streams = self.get_streams(imdb_id, tmdb_id)
        if not streams:
            self.log("DEBUG", f"No streams for: {show_name}")
            return 0

        self.log("DEBUG", f"Found {len(streams)} streams for {show_name}")

        # Sort streams by priority (highest first) - complete fullpacks before partials before singles
        streams = sorted(streams, key=lambda x: -x['priority'])

        created = 0
        seasons_with_complete_fullpack = set()
        seasons_episode_count = {}  # Track total episodes created per season

        # Get expected episode counts per season from TMDB
        tmdb_season_eps = {}
        details = self._fetch_show_details(tmdb_id)
        if details:
            for sd in details.get('seasons', []):
                sn = sd.get('season_number', 0)
                ec = sd.get('episode_count', 0)
                if sn > 0 and ec > 0:
                    tmdb_season_eps[sn] = ec

        # Process fullpacks first (now properly sorted by priority)
        for stream in streams:
            if not stream['is_fullpack']:
                continue

            season = stream['season']
            # Skip if already complete with good quality
            if season in skipped_seasons:
                continue
            # Skip if we already have enough episodes for this season
            if season in seasons_with_complete_fullpack:
                continue

            count = self._process_fullpack(show_name, stream, first_air_date)
            if count > 0:
                created += count
                seasons_episode_count[season] = seasons_episode_count.get(season, 0) + count
                # Mark season complete when we have all episodes (from TMDB) or fallback 80%
                expected = tmdb_season_eps.get(season, 0)
                total = seasons_episode_count[season]
                if expected > 0 and total >= expected:
                    seasons_with_complete_fullpack.add(season)
                elif not stream.get('is_partial_pack', False) and expected == 0 and count >= 5:
                    # Fallback if TMDB has no data for this season
                    seasons_with_complete_fullpack.add(season)

        # Process singles for seasons without fullpacks
        singles_limit = 15  # Max singles per show to prevent overload
        singles_processed = 0

        for stream in streams:
            if stream['is_fullpack']:
                continue
            if singles_processed >= singles_limit:
                break

            season = stream['season']
            # Skip if already complete with good quality
            if season in skipped_seasons:
                continue
            if season in seasons_with_complete_fullpack:
                continue

            count = self._process_single(show_name, stream, first_air_date)
            created += count
            singles_processed += 1

        if created > 0:
            self._stats['shows'] += 1
            # Save registry incrementally after each show with new episodes
            self._save_registry()

        return created

    # ===== CLEANUP =====

    def cleanup_orphaned_files(self):
        """Remove .mkv files not in registry"""
        if not os.path.exists(self.TV_DIR):
            return

        removed = 0
        registry_paths = {v['file_path'] for v in self._registry.values()}

        for root, dirs, files in os.walk(self.TV_DIR):
            for f in files:
                if not f.endswith('.mkv'):
                    continue

                filepath = os.path.join(root, f)
                if filepath not in registry_paths:
                    try:
                        os.remove(filepath)
                        self.log("INFO", f"Removed orphaned: {f}")
                        removed += 1
                    except OSError as e:
                        self.log("WARN", f"Failed to remove {f}: {e}")

        # Remove empty directories
        for root, dirs, files in os.walk(self.TV_DIR, topdown=False):
            for d in dirs:
                dirpath = os.path.join(root, d)
                try:
                    if not os.listdir(dirpath):
                        os.rmdir(dirpath)
                except OSError:
                    pass

        if removed:
            self.log("INFO", f"Cleanup complete: removed {removed} orphaned files")

    def cleanup_orphaned_torrents(self):
        """Remove torrents not referenced by any .mkv file"""
        torrents = self._ts_list_torrents()
        if not torrents:
            return

        # Get all hashes from registry
        registry_hashes = {v['hash'].lower() for v in self._registry.values()}

        removed = 0
        for t in torrents:
            h = (t.get('hash') or '').lower()
            if not h:
                continue

            # Check if TV torrent (has series pattern in title)
            title = t.get('title', '').lower()
            if not re.search(r's\d+e\d+|season|episode', title):
                continue  # Skip non-TV torrents

            if h not in registry_hashes:
                if not self._is_hash_on_disk(h):
                    if self._ts_remove_torrent(h):
                        removed += 1
                        self.log("INFO", f"Removed orphaned torrent: {h[:8]}...")
                else:
                    self.log("DEBUG", f"Torrent {h[:8]} not in registry but found on disk, keeping")

        if removed:
            self.log("INFO", f"Removed {removed} orphaned torrents")

    def rehydrate_missing_torrents(self):
        """
        Re-add torrents for .mkv files where torrent is no longer active.
        Reads magnet URL from third line of .mkv file and re-adds with fresh trackers.
        """
        # Get active torrent hashes
        torrents = self._ts_list_torrents()
        active_hashes = {(t.get('hash') or '').lower() for t in torrents if t.get('hash')}

        rehydrated = 0
        MAX_REHYDRATE_PER_RUN = 20  # Limit to prevent GoStorm saturation

        if not os.path.exists(self.TV_DIR):
            return

        self.log("INFO", "Scanning for missing torrents to rehydrate...")
        
        for root, _, files in os.walk(self.TV_DIR):
            if rehydrated >= MAX_REHYDRATE_PER_RUN:
                self.log("INFO", f"Reached limit of {MAX_REHYDRATE_PER_RUN} rehydrations, stopping scan.")
                break

            for filename in files:
                if not filename.lower().endswith('.mkv'):
                    continue

                if rehydrated >= MAX_REHYDRATE_PER_RUN:
                    break

                filepath = os.path.join(root, filename)
                try:
                    with open(filepath, 'r') as f:
                        lines = f.readlines()

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

                    # Rehydrate with fresh magnet (new trackers)
                    if magnet_line.startswith('magnet:?'):
                        self.log("INFO", f"Rehydrating ({rehydrated+1}/{MAX_REHYDRATE_PER_RUN}): {filename}...")
                        # Rebuild magnet with dynamic trackers
                        fresh_magnet = self._build_magnet(hash_val)
                        if self._ts_add_torrent(fresh_magnet, filename):
                            rehydrated += 1
                            active_hashes.add(hash_val)  # Prevent duplicate adds
                            # CRITICAL: Sleep between adds to let GoStorm resolve DHT/Metadata
                            time.sleep(5) 

                except Exception as e:
                    self.log("DEBUG", f"Error reading {filename}: {e}")
                    continue

        if rehydrated > 0:
            self.log("INFO", f"Rehydrated {rehydrated} missing torrents")
        else:
            self.log("DEBUG", "No torrents needed rehydration")

    # ===== INSTANCE MANAGEMENT =====

    def _kill_existing_instances(self):
        """
        Ensure single instance using atomic file locking (fcntl.flock).
        This is the standard Unix approach - no race conditions possible.
        """
        import fcntl
        import atexit

        lock_file = "/tmp/gostorm-tv-sync.lock"
        pid_file = "/tmp/gostorm-tv-sync.pid"

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
            # Lock held by another process - check if it's alive
            try:
                with open(pid_file, 'r') as f:
                    old_pid = int(f.read().strip())
                os.kill(old_pid, 0)  # Check if process exists
                self.log("ERROR", f"Another instance already running (PID {old_pid}). Exiting.")
                self._lock_fd.close()
                sys.exit(1)
            except (ProcessLookupError, ValueError, FileNotFoundError, PermissionError):
                # Stale lock - old process crashed
                self.log("WARN", "Stale lock detected, forcing acquisition...")
                fcntl.flock(self._lock_fd.fileno(), fcntl.LOCK_EX)

        # Write our PID
        try:
            with open(pid_file, 'w') as f:
                f.write(str(os.getpid()))
        except IOError:
            pass

        self.log("INFO", f"Lock acquired, running as PID {os.getpid()}")
        atexit.register(self._release_lock)

    def _release_lock(self):
        """Release lock file on exit."""
        import fcntl

        lock_file = "/tmp/gostorm-tv-sync.lock"
        pid_file = "/tmp/gostorm-tv-sync.pid"

        try:
            if hasattr(self, '_lock_fd') and self._lock_fd:
                fcntl.flock(self._lock_fd.fileno(), fcntl.LOCK_UN)
                self._lock_fd.close()
                self._lock_fd = None
        except:
            pass

        try:
            os.remove(pid_file)
            os.remove(lock_file)
        except:
            pass

    # ===== MAIN EXECUTION =====

    def run(self, max_shows: int = 0) -> bool:
        """
        Main execution: discover shows, process, cleanup
        max_shows: limit processing (0 = unlimited)
        """
        # Ensure single instance
        self._kill_existing_instances()

        start_time = time.time()
        self.log("INFO", "=" * 60)
        self.log("INFO", "GoStorm TV Sync - Starting")
        self.log("INFO", f"Time: {datetime.now().strftime('%Y-%m-%d %H:%M:%S')}")
        self.log("INFO", "=" * 60)

        try:
            # Protect existing files from cleanup (for shows now filtered by age/genre)
            self._populate_registry_from_existing()

            # Discover shows
            shows = self.discover_shows()
            if not shows:
                self.log("WARN", "No shows discovered")
                return False

            # Limit if requested
            if max_shows > 0:
                shows = shows[:max_shows]
                self.log("INFO", f"Limited to {max_shows} shows")

            # Process each show
            for i, show in enumerate(shows, 1):
                # Check for graceful shutdown request
                if _shutdown_requested:
                    self.log("INFO", "Shutdown requested, saving registry and exiting...")
                    self._save_registry()
                    self.log("INFO", f"Registry saved with {len(self._registry)} episodes")
                    return True

                name = show.get('name') or show.get('original_name', '')
                self.log("INFO", f"[{i}/{len(shows)}] {name}")
                self.process_show(show)

            # Save registry
            self._save_registry()
            self.log("INFO", "Registry saved")

            # Cleanup
            self.log("INFO", "Running cleanup...")
            self.cleanup_orphaned_files()
            self.cleanup_orphaned_torrents()

            # Rehydrate missing torrents (re-add from saved magnets)
            self.log("INFO", "Checking for torrents to rehydrate...")
            self.rehydrate_missing_torrents()

            # Stats
            elapsed = time.time() - start_time
            self.log("INFO", "=" * 60)
            self.log("INFO", "Sync Complete!")
            self.log("INFO", f"Shows processed: {self._stats['shows']}")
            self.log("INFO", f"Episodes created: {self._stats['episodes_created']}")
            self.log("INFO", f"Episodes skipped: {self._stats['episodes_skipped']}")
            self.log("INFO", f"Upgrades: {self._stats['upgrades']}")
            self.log("INFO", f"Registry size: {len(self._registry)} episodes")
            self.log("INFO", f"Duration: {elapsed:.1f}s")
            self.log("INFO", "=" * 60)

            # Trigger Plex refresh for TV library
            tv_lib_id = _cfg.get('plex', {}).get('tv_library_id', 0)
            if tv_lib_id:
                self.notify_plex(tv_lib_id)

            return True

        except Exception as e:
            self.log("ERROR", f"Fatal error: {e}")
            import traceback
            traceback.print_exc()
            return False


def main():
    """Entry point"""
    import argparse

    parser = argparse.ArgumentParser(description='GoStorm TV Sync')
    parser.add_argument('--max-shows', type=int, default=0,
                        help='Limit number of shows to process (0=unlimited)')
    parser.add_argument('--dry-run', action='store_true',
                        help='Show what would be done without making changes')
    args = parser.parse_args()

    if args.dry_run:
        print("Dry-run mode not yet implemented")
        return 1

    sync = GoStormTV()
    success = sync.run(max_shows=args.max_shows)
    return 0 if success else 1


if __name__ == "__main__":
    sys.exit(main())
