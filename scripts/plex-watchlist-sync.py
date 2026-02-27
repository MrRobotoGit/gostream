#!/usr/bin/env python3
"""
plex-watchlist-sync.py — Sync Plex Watchlist → GoStorm
Reads the Plex cloud watchlist, finds movies not already in the virtual
library, picks the best torrent from Torrentio, adds it to GoStorm,
creates the virtual .mkv and triggers a Plex library refresh.

Usage:
  python3 plex-watchlist-sync.py [--dry-run] [--movies-only] [--verbose]
"""

import argparse
import json
import logging
import os
import re
import sys
import time
import random
from pathlib import Path
from typing import Dict, List, Optional, Tuple
from urllib.parse import quote

import requests


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

# ── Configuration ─────────────────────────────────────────────────────────────
PLEX_TOKEN   = _cfg.get('plex', {}).get('token', '')
PLEX_URL     = _cfg.get('plex', {}).get('url', 'http://127.0.0.1:32400')
TORRSERVER   = _cfg.get('gostorm_url', 'http://127.0.0.1:8090')
MOVIES_DIR   = os.path.join(_cfg.get('physical_source_path', '/mnt/torrserver'), 'movies')
TMDB_API_KEY = _cfg.get('tmdb_api_key', '')
SECTION_ID   = _cfg.get('plex', {}).get('library_id', 0)

TORRENTIO_BASE   = "https://torrentio.strem.fun"
TORRENTIO_CONFIG = "sort=qualitysize|qualityfilter=480p,720p,scr,cam"

TMDB_BASE          = "https://api.themoviedb.org/3"
PLEX_WATCHLIST_URL = "https://discover.provider.plex.tv/library/sections/watchlist/all"

BYTES_PER_GB = 1_073_741_824

# Quality thresholds (mirrored from gostorm-sync-complete.py)
MOVIE_4K_MIN_GB    = 10
MOVIE_4K_MAX_GB    = 50
MOVIE_1080P_MIN_GB = 6
MOVIE_1080P_MAX_GB = 20
MIN_SEEDERS        = 10

EXCLUDED_LANGUAGES = r"🇪🇸|🇫🇷|🇩🇪|🇷🇺|🇨🇳|🇯🇵|🇰🇷|🇹🇭|🇵🇹|🇧🇷|🇺🇦|🇵🇱|🇳🇱|🇹🇷|🇸🇦|🇮🇳|🇨🇿|🇭🇺|🇷🇴"
# Non-English single dubs (text-based, for streams without flag emojis) — Italian kept
EXCLUDED_DUB_PATTERN = r"\b(Ukr|Ukrainian|Ger|German|Fra|French|Spa|Spanish|Por|Portuguese|Rus|Russian|Chi|Chinese|Pol|Polish|Tur|Turkish|Ara|Arabic|Hin|Hindi|Cze|Czech|Hun|Hungarian)\s+Dub\b"

FALLBACK_TRACKERS = [
    "udp://tracker.opentrackr.org:1337/announce",
    "udp://open.stealth.si:80/announce",
    "udp://tracker.torrent.eu.org:451/announce",
    "udp://exodus.desync.com:6969/announce",
    "udp://tracker.openbittorrent.com:6969/announce",
]
TRACKERS_URL = (
    "https://raw.githubusercontent.com/ngosang/trackerslist/master/trackers_best.txt"
)

# ── Globals ───────────────────────────────────────────────────────────────────
log = logging.getLogger("watchlist-sync")
session = requests.Session()
session.headers.update({
    "User-Agent": "Mozilla/5.0 (compatible; plex-watchlist-sync/1.0)"
})

_trackers_cache: List[str] = []
_trackers_cache_time: float = 0.0
_last_api_call: float = 0.0
MIN_API_INTERVAL = 0.5   # seconds between external API calls


# ── HTTP helpers ──────────────────────────────────────────────────────────────

def setup_logging(verbose: bool) -> None:
    level = logging.DEBUG if verbose else logging.INFO
    logging.basicConfig(
        level=level,
        format="[%(asctime)s] %(levelname)s  %(message)s",
        datefmt="%H:%M:%S",
        stream=sys.stdout,
    )


def _is_local(url: str) -> bool:
    from urllib.parse import urlparse
    host = (urlparse(url).hostname or "").lower()
    return (
        host in ("localhost", "127.0.0.1")
        or host.startswith("192.168.")
        or host.startswith("10.")
    )


def safe_get(url: str, timeout: int = 15, **kwargs) -> Optional[requests.Response]:
    """GET with rate-limiting for external hosts, exponential backoff on errors."""
    global _last_api_call
    if not _is_local(url):
        elapsed = time.time() - _last_api_call
        if elapsed < MIN_API_INTERVAL:
            time.sleep(MIN_API_INTERVAL - elapsed)
        _last_api_call = time.time()

    backoff = 2.0
    for attempt in range(3):
        try:
            r = session.get(url, timeout=timeout, **kwargs)
            if r.status_code == 200:
                return r
            log.debug(f"HTTP {r.status_code} — {url}")
        except requests.RequestException as e:
            log.debug(f"Request error (attempt {attempt+1}/3): {e}")
        if attempt < 2:
            jitter = random.uniform(0, backoff * 0.1)
            time.sleep(min(backoff + jitter, 30))
            backoff *= 2
    return None


def safe_post(url: str, data: dict, timeout: int = 15) -> Optional[requests.Response]:
    try:
        r = session.post(url, json=data, timeout=timeout)
        return r if r.status_code == 200 else None
    except requests.RequestException as e:
        log.debug(f"POST error: {e}")
        return None


# ── Tracker helpers ───────────────────────────────────────────────────────────

def fetch_trackers() -> List[str]:
    global _trackers_cache, _trackers_cache_time
    now = time.time()
    if _trackers_cache and (now - _trackers_cache_time) < 3600:
        return _trackers_cache
    try:
        r = session.get(TRACKERS_URL, timeout=10)
        if r.status_code == 200:
            lines = [l.strip() for l in r.text.split("\n") if l.strip()]
            if lines:
                _trackers_cache = lines[:20]
                _trackers_cache_time = now
                return _trackers_cache
    except Exception:
        pass
    if not _trackers_cache:
        _trackers_cache = list(FALLBACK_TRACKERS)
        _trackers_cache_time = now
    return _trackers_cache


def build_magnet(info_hash: str, name: str = "") -> str:
    magnet = f"magnet:?xt=urn:btih:{info_hash}"
    if name:
        magnet += f"&dn={quote(name)}"
    for tr in fetch_trackers():
        magnet += f"&tr={quote(tr)}"
    return magnet


# ── 1. Plex Watchlist ─────────────────────────────────────────────────────────

def get_plex_watchlist() -> List[Dict]:
    """
    Fetch Plex watchlist from discover.provider.plex.tv (cloud API).
    Returns list of {title, year, imdb_id, type} dicts.
    """
    log.info("Fetching Plex watchlist…")
    params = {
        "X-Plex-Token":    PLEX_TOKEN,
        "X-Plex-Platform": "Web",
        "format":          "json",
    }
    r = safe_get(PLEX_WATCHLIST_URL, timeout=20, params=params)
    if not r:
        log.error("Failed to fetch Plex watchlist — check PLEX_TOKEN and internet connectivity")
        return []

    try:
        data = r.json()
    except Exception as e:
        log.error(f"Invalid JSON from Plex watchlist: {e}")
        return []

    # The watchlist endpoint wraps items in MediaContainer.Metadata
    mc = data.get("MediaContainer", {})
    items_raw = (
        mc.get("Metadata")
        or mc.get("Video")
        or mc.get("items")
        or (data if isinstance(data, list) else [])
    )

    items = []
    for item in items_raw:
        title = (item.get("title") or "").strip()
        year  = str(item.get("year") or item.get("Year") or "")
        typ   = (item.get("type") or item.get("Type") or "").lower()

        # Extract IMDB ID from Guid list: [{"id": "imdb://tt1234567"}, ...]
        imdb_id = ""
        for g in (item.get("Guid") or []):
            gid = g.get("id") or ""
            if gid.startswith("imdb://"):
                imdb_id = gid[len("imdb://"):]
                break

        items.append({"title": title, "year": year, "imdb_id": imdb_id, "type": typ})

    log.info(f"Watchlist: {len(items)} item(s) total")
    return items


def resolve_imdb_via_tmdb(title: str, year: str) -> str:
    """Fallback: search TMDB by title+year, return IMDB ID."""
    r = safe_get(
        f"{TMDB_BASE}/search/movie",
        params={"api_key": TMDB_API_KEY, "query": title, "year": year},
    )
    if not r:
        return ""
    try:
        results = r.json().get("results", [])
        if not results:
            return ""
        tmdb_id = results[0]["id"]
        r2 = safe_get(
            f"{TMDB_BASE}/movie/{tmdb_id}/external_ids",
            params={"api_key": TMDB_API_KEY},
        )
        if r2:
            return r2.json().get("imdb_id", "") or ""
    except Exception as e:
        log.debug(f"TMDB lookup failed for '{title}': {e}")
    return ""


# ── 2. Existing index ─────────────────────────────────────────────────────────

def build_existing_index() -> Tuple[set, Dict[str, str]]:
    """
    Scan MOVIES_DIR for .mkv files.
    Returns:
      - imdb_set:    set of IMDB IDs already covered (read from line 4)
      - title_index: normalised_title+year → path (title+year fallback)
    """
    imdb_set: set = set()
    title_index: Dict[str, str] = {}

    movies_path = Path(MOVIES_DIR)
    if not movies_path.is_dir():
        log.warning(f"MOVIES_DIR not found or not accessible: {MOVIES_DIR}")
        return imdb_set, title_index

    count = 0
    for mkv in movies_path.rglob("*.mkv"):
        count += 1
        # Skip files that are too large to be our virtual stubs (prevents D-state hangs)
        try:
            if mkv.stat().st_size > 10240: # 10KB limit
                _extract_title_year(mkv, title_index)
                continue
                
            # V290: Read full file (up to 4KB) and split lines to handle long tracker lists.
            # Normal readline() can fail or stall on FUSE if preceding lines are too long.
            with open(mkv, "r", errors="ignore") as f:
                content = f.read(4096)
                lines = [l.strip() for l in content.splitlines()]
        except Exception:
            continue

        # Skip broken MKVs (size=0 on line 2 means metadata never resolved,
        # Plex ignores 0-byte files — treat as absent so we can recreate)
        size_val = lines[1] if len(lines) > 1 else "0"
        if size_val == "0" or size_val == "":
            log.debug(f"  Broken MKV (size=0), will re-create: {mkv.name}")
            continue

        # Line 4 (index 3) = IMDB ID
        if len(lines) >= 4:
            imdb_val = lines[3]
            if re.match(r"tt\d+", imdb_val):
                imdb_set.add(imdb_val)

        _extract_title_year(mkv, title_index)

    log.info(
        f"Existing library: {count} MKV files, "
        f"{len(imdb_set)} with IMDB ID, {len(title_index)} title entries"
    )
    return imdb_set, title_index


def _extract_title_year(mkv_path: Path, title_index: Dict[str, str]) -> None:
    """Helper to extract title+year from filename stem for fallback indexing."""
    stem = mkv_path.stem
    # Match title followed by year (separated by . or _)
    m = re.search(r"[._]((?:19|20)\d{2})[._]", stem)
    if m:
        yr = m.group(1)
        # Normalise title: remove all non-alphanumeric and lowercase
        base = re.sub(r"\W+", "", stem[: m.start()].lower())
        title_index[f"{base}{yr}"] = str(mkv_path)


def is_already_present(item: Dict, imdb_set: set, title_index: Dict) -> bool:
    if item["imdb_id"] and item["imdb_id"] in imdb_set:
        return True
    # Normalised title+year fallback
    norm = re.sub(r"\W+", "", item["title"]).lower() + item["year"]
    return norm in title_index


# ── 3. Torrentio ──────────────────────────────────────────────────────────────

def get_torrentio_streams(imdb_id: str) -> List[Dict]:
    """Fetch movie streams from Torrentio for the given IMDB ID."""
    url = f"{TORRENTIO_BASE}/{TORRENTIO_CONFIG}/stream/movie/{imdb_id}.json"
    r = safe_get(url, timeout=15)
    if not r:
        log.debug(f"Torrentio: no response for {imdb_id}")
        return []
    try:
        streams = r.json().get("streams", [])
        if not isinstance(streams, list):
            return []
        return [s for s in streams if isinstance(s, dict) and s.get("title")]
    except Exception as e:
        log.debug(f"Torrentio JSON error for {imdb_id}: {e}")
        return []


def _extract_gb(title: str) -> float:
    m = re.search(r"💾\s*([0-9]+\.?[0-9]*)\s*(GB|MB)", title, re.IGNORECASE)
    if not m:
        m = re.search(r"\b([0-9]+\.?[0-9]*)\s*(GB|MB)\b", title, re.IGNORECASE)
    if m:
        v, unit = float(m.group(1)), m.group(2).upper()
        return v if unit == "GB" else v / 1000.0
    return 0.0


def _extract_seeders(title: str) -> int:
    m = re.search(r"👤\s*([0-9]+)", title)
    return int(m.group(1)) if m else 0


def score_stream(stream: Dict) -> int:
    """
    Quality score — mirrors calculate_quality_score() from sync scripts.
    Returns 0 to exclude (720p), positive otherwise.
    """
    text = (stream.get("title", "") + " " + stream.get("name", "")).lower()
    seeders = _extract_seeders(stream.get("title", ""))

    # Resolution
    if re.search(r"2160p|4[kK]|uhd", text):
        base = 200
    elif re.search(r"1080p", text):
        base = 50
    elif re.search(r"720p", text):
        return 0
    else:
        base = 50  # assumed 1080p

    # HDR / Dolby Vision
    if re.search(r"\bdv\b|dovi|dolby.?vision", text):
        hdr_score = 150  # Dolby Vision priority
    elif re.search(r"hdr|hdr10\+?", text):
        hdr_score = 100
    else:
        hdr_score = 0

    # Audio
    if re.search(r"atmos", text):
        audio = 50
    elif re.search(r"5\.1|ddp?5\.?1|dd5\.?1|eac3|dts(?!.*hd)", text):
        audio = 25
    elif re.search(r"stereo|2\.0|\b2ch\b", text):
        audio = -50
    else:
        audio = 5

    # Source
    source = 10 if re.search(r"bluray|bd|web-dl|webrip", text) else 0

    # Seeder bonus
    seeder_bonus = 5 if seeders >= 50 else 0

    return base + hdr_score + audio + source + seeder_bonus


def pick_best_stream(streams: List[Dict]) -> List[Dict]:
    """
    Two-pass selection: 4K first, then 1080p fallback.
    Applies seeder, size, and language filters before scoring.
    """
    def is_4k(s: Dict) -> bool:
        return bool(re.search(r"2160p|4[kK]|uhd", s.get("title", ""), re.IGNORECASE))

    def passes(s: Dict, require_4k: bool) -> bool:
        title = s.get("title", "")
        if _extract_seeders(title) < MIN_SEEDERS:
            return False
        if re.search(EXCLUDED_LANGUAGES, title):
            return False
        if re.search(EXCLUDED_DUB_PATTERN, title, re.IGNORECASE):
            return False
        if re.search(r'webscreener|screener|\bscr\b|\bcam\b|camrip|hdcam|telesync|\bts\b|telecine|\btc\b', title, re.IGNORECASE):
            return False
        gb = _extract_gb(title)
        if require_4k:
            if gb != 0 and not (MOVIE_4K_MIN_GB <= gb <= MOVIE_4K_MAX_GB):
                return False
        else:
            if gb == 0 or not (MOVIE_1080P_MIN_GB <= gb <= MOVIE_1080P_MAX_GB):
                return False
        return score_stream(s) > 0

    c4k = sorted([s for s in streams if is_4k(s) and passes(s, True)], key=score_stream, reverse=True)
    c1080 = sorted([s for s in streams if not is_4k(s) and passes(s, False)], key=score_stream, reverse=True)
    return c4k + c1080


# ── 4. GoStorm ─────────────────────────────────────────────────────────────

def add_to_gostorm(magnet: str, title: str) -> str:
    """Add magnet to GoStorm via /stream/?link=…&save. Returns 40-char hash or ''."""
    m = re.search(r"xt=urn:btih:([a-fA-F0-9]{32,40})", magnet)
    if not m:
        log.error(f"Cannot extract hash from magnet: {magnet[:80]}")
        return ""
    h = m.group(1).lower()

    save_url = f"{TORRSERVER}/stream/?link={magnet}&save"
    r = safe_get(save_url, timeout=20)
    if not r:
        log.error(f"GoStorm add failed for: {title}")
        return ""
    body = r.text.lower()
    if any(e in body for e in ("error", "400", "500")):
        log.error(f"GoStorm error for '{title}': {body[:120]}")
        return ""

    log.info(f"Added to GoStorm: {title} [{h[:8]}…]")
    return h


def get_torrent_file_info(hash_val: str, max_wait: int = 25) -> Optional[Dict]:
    """Poll GoStorm until file_stats appear (torrent metadata resolved)."""
    sleep_seq = [1, 2, 3, 3, 3, 5]
    start = time.time()
    attempt = 0
    while time.time() - start < max_wait:
        r = safe_post(
            f"{TORRSERVER}/torrents",
            data={"action": "get", "hash": hash_val},
        )
        if r:
            try:
                info = r.json()
                if info.get("file_stats"):
                    return info
            except Exception:
                pass
        sleep = sleep_seq[attempt] if attempt < len(sleep_seq) else 3
        time.sleep(sleep)
        attempt += 1
    log.warning(f"Metadata timeout for {hash_val[:8]}… (waited {max_wait}s)")
    return None


# ── 5. Virtual MKV ────────────────────────────────────────────────────────────

def _clean_filename(s: str) -> str:
    s = re.sub(r"[^a-zA-Z0-9._-]", "_", s)
    s = re.sub(r"_+", "_", s).strip("_")
    return s[:90]


def _quality_tag(stream_title: str) -> str:
    t = stream_title.lower()
    tag = "2160p" if re.search(r"2160p|4[kK]|uhd", t) else ("1080p" if re.search(r"1080p", t) else "")
    if re.search(r"\bdv\b|dovi|dolby.?vision", t):
        tag += "_DV"
    elif re.search(r"hdr|hdr10\+?", t):
        tag += "_HDR"
    return tag


def create_mkv(
    hash_val: str,
    stream_title: str,
    file_index: int,
    file_size: int,
    magnet: str,
    imdb_id: str,
    movie_title: str,
    year: str,
) -> str:
    """
    Write virtual .mkv (4 lines):
      line 1: GoStorm stream URL
      line 2: file size in bytes
      line 3: magnet URL (for rehydration)
      line 4: IMDB ID (for existing-index lookup)
    Returns path on success, '' on failure.
    """
    stream_url = f"{TORRSERVER}/stream?link={hash_val}&index={file_index}&play"
    qtag = _quality_tag(stream_title)
    base = _clean_filename(f"{movie_title}_{year}" + (f"_{qtag}" if qtag else ""))
    filename = f"{base}_{hash_val[-8:]}.mkv"
    path = os.path.join(MOVIES_DIR, filename)

    try:
        os.makedirs(MOVIES_DIR, exist_ok=True)
        with open(path, "w") as f:
            f.write(stream_url + "\n")
            f.write((str(file_size) if file_size > 0 else "0") + "\n")
            f.write(magnet + "\n")
            if imdb_id:
                f.write(imdb_id + "\n")
        log.info(f"Created: {path}")
        return path
    except IOError as e:
        log.error(f"Failed to create {path}: {e}")
        return ""


# ── 6. Plex refresh ───────────────────────────────────────────────────────────

def notify_plex() -> None:
    url = f"{PLEX_URL}/library/sections/{SECTION_ID}/refresh?X-Plex-Token={PLEX_TOKEN}"
    try:
        r = session.get(url, timeout=10)
        if r.status_code in (200, 201):
            log.info(f"Plex refresh triggered (section {SECTION_ID})")
        else:
            log.warning(f"Plex refresh returned HTTP {r.status_code}")
    except Exception as e:
        log.error(f"Plex notify failed: {e}")


# ── Main ──────────────────────────────────────────────────────────────────────

def main() -> None:
    parser = argparse.ArgumentParser(
        description="Sync Plex Watchlist → GoStorm (movies only by default)"
    )
    parser.add_argument(
        "--dry-run", action="store_true",
        help="Print what would be done without making any changes"
    )
    parser.add_argument(
        "--movies-only", action="store_true", default=True,
        help="Process only movies, skip TV shows (this is the default)"
    )
    parser.add_argument(
        "--include-tv", action="store_true",
        help="Also process TV shows from the watchlist (not fully supported yet)"
    )
    parser.add_argument(
        "--verbose", action="store_true",
        help="Enable debug-level logging"
    )
    args = parser.parse_args()

    setup_logging(args.verbose)
    log.info("=== plex-watchlist-sync ===")
    if args.dry_run:
        log.info("DRY-RUN — no changes will be made")

    # 1. Fetch watchlist
    watchlist = get_plex_watchlist()
    if not watchlist:
        log.info("Watchlist empty or unreachable. Exiting.")
        return

    # 2. Build existing-library index
    imdb_set, title_index = build_existing_index()

    # 3. Filter to processable items (Pre-filter by type)
    to_process = []
    for item in watchlist:
        typ = item["type"]
        if typ == "show" and not args.include_tv:
            log.debug(f"Skip TV (use --include-tv): {item['title']}")
            continue
        to_process.append(item)

    log.info(f"Items to check: {len(to_process)}")
    if not to_process:
        log.info("Nothing to add. Exiting.")
        return

    added = 0
    skipped = 0

    for item in to_process:
        title   = item["title"]
        year    = item["year"]
        imdb_id = item["imdb_id"]
        
        # Resolve missing IMDB ID via TMDB if not provided by Plex
        if not imdb_id:
            log.debug(f"  No IMDB ID from Plex — searching TMDB for '{title}'…")
            imdb_id = resolve_imdb_via_tmdb(title, year)
            if imdb_id:
                item["imdb_id"] = imdb_id
                log.debug(f"  Resolved: {imdb_id}")
            else:
                log.warning(f"  Cannot resolve IMDB ID for '{title}' — skipping")
                skipped += 1
                continue

        # V291: Final presence check AFTER IMDB ID resolution
        if is_already_present(item, imdb_set, title_index):
            log.info(f"Already in library: {title} ({year}) [{imdb_id}]")
            continue

        log.info(f"─── {title} ({year})")

        # Get Torrentio streams
        streams = get_torrentio_streams(imdb_id)
        log.debug(f"  Torrentio: {len(streams)} stream(s) for {imdb_id}")

        candidates = pick_best_stream(streams)
        if not candidates:
            log.warning(f"  No suitable stream found for '{title}' ({imdb_id})")
            if args.verbose:
                for s in streams[:5]:
                    log.debug(f"    rejected: {s.get('title', '')[:100]}")
            skipped += 1
            continue

        if args.dry_run:
            best = candidates[0]
            info_hash = (best.get("infoHash") or "").lower().strip()
            log.info(f"  [DRY-RUN] Would add {info_hash[:8]}… and create MKV")
            log.debug(f"  Stream: {best.get('title', '')[:100]}")
            added += 1
            time.sleep(0.2)
            continue

        # Real execution block
        mkv_created = False
        for candidate in candidates:
            stream_title = candidate.get("title", "")
            info_hash    = (candidate.get("infoHash") or "").lower().strip()
            if not info_hash:
                continue

            gb   = _extract_gb(stream_title)
            sc   = score_stream(candidate)
            seed = _extract_seeders(stream_title)
            log.info(f"  Trying: score={sc} seeders={seed} size={gb:.1f}GB")
            log.debug(f"  Stream: {stream_title[:100]}")

            magnet = build_magnet(info_hash, title)
            h = add_to_gostorm(magnet, title)
            if not h:
                continue

            log.debug(f"  Waiting for GoStorm metadata [{h[:8]}…]")
            torrent_info = get_torrent_file_info(h)

            if not torrent_info:
                log.warning(f"  Metadata timeout for {h[:8]}… — removing, will retry next run")
                safe_post(f"{TORRSERVER}/torrents", data={"action": "rem", "hash": h})
                continue  # try next candidate

            file_stats = torrent_info.get("file_stats") or []

            # Skip BDMV ISOs/folders which GoStorm handles poorly for direct play
            if any("BDMV" in f.get("path", "") for f in file_stats):
                log.warning(f"  BDMV disc image for {h[:8]}… — trying next candidate")
                safe_post(f"{TORRSERVER}/torrents", data={"action": "rem", "hash": h})
                continue

            mkv_files = [f for f in file_stats if str(f.get("path", "")).lower().endswith(".mkv")]
            if not mkv_files:
                log.warning(f"  No MKV in metadata for {h[:8]}… — trying next candidate")
                safe_post(f"{TORRSERVER}/torrents", data={"action": "rem", "hash": h})
                continue

            best_file  = max(mkv_files, key=lambda f: f.get("length", 0))
            file_index = best_file.get("id", 1)
            file_size  = best_file.get("length", 0)
            log.debug(f"  MKV: idx={file_index} size={file_size/BYTES_PER_GB:.1f}GB")

            mkv_path = create_mkv(
                hash_val=h,
                stream_title=stream_title,
                file_index=file_index,
                file_size=file_size,
                magnet=magnet,
                imdb_id=imdb_id,
                movie_title=title,
                year=year,
            )
            if mkv_path:
                added += 1
                mkv_created = True
                break # Success! Stop trying candidates

        if not mkv_created:
            skipped += 1

        time.sleep(1)  # brief pause between items

    # Notify Plex if anything was added
    if added > 0:
        if args.dry_run:
            log.info(f"[DRY-RUN] Would refresh Plex section {SECTION_ID}")
        else:
            notify_plex()

    log.info(f"=== Done: {added} added, {skipped} skipped ===")


if __name__ == "__main__":
    main()
