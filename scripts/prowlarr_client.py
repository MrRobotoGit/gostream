#!/usr/bin/env python3
"""
Prowlarr Adapter for GoStorm Sync Scripts
Converts Prowlarr/Newznab results to Stremio/Torrentio format.
"""

import json
import logging
import os
import requests
from typing import List, Dict, Any, Optional

class ProwlarrClient:
    def __init__(self, config_path=None):
        # Load config from GoStream config.json (co-located with binary, one level up from scripts/)
        if config_path is None:
            script_dir = os.path.dirname(os.path.abspath(__file__))
            config_path = os.path.join(script_dir, '..', 'config.json')

        prowlarr_cfg = {}
        try:
            with open(config_path, 'r') as f:
                cfg = json.load(f)
                prowlarr_cfg = cfg.get('prowlarr', {})
        except Exception as e:
            logging.warning(f"Could not load Prowlarr config from {config_path}: {e}")

        self.ENABLED = prowlarr_cfg.get('enabled', False)
        self.API_KEY = prowlarr_cfg.get('api_key', '')
        self.BASE_URL = prowlarr_cfg.get('url', '')
        self.SEARCH_ENDPOINT = f"{self.BASE_URL}/api/v1/search"
        self.session = requests.Session()
        self.session.headers.update({
            'User-Agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36',
            'Accept': 'application/json'
        })
        
    def fetch_from_prowlarr(self, imdb_id: str, content_type: str = "movie") -> List[Dict[str, Any]]:
        """
        Directly query Prowlarr API for a specific IMDB ID.
        """
        if not self.ENABLED:
            return []
            
        # Map Stremio types to Prowlarr search types
        # Movies -> movie, Series -> tvsearch
        prowlarr_type = "tvsearch" if content_type == "series" else "movie"
        
        params = {
            "apikey": self.API_KEY,
            "query": imdb_id,  # Prowlarr V1 uses query for ID searches
            "type": prowlarr_type,
            "indexerIds": "-2"  # All indexers
        }
        try:
            # Use session for connection reuse and 30s timeout
            response = self.session.get(self.SEARCH_ENDPOINT, params=params, timeout=30)
            if response.status_code == 200:
                return response.json()
            else:
                logging.warning(f"Prowlarr API returned status {response.status_code}")
        except Exception as e:
            logging.error(f"Error fetching from Prowlarr: {e}")
        return []

    def fetch_torrents(self, imdb_id: str, content_type: str = "movie") -> List[Dict[str, Any]]:
        """
        Fetch torrents from Prowlarr and return them in Stremio/Torrentio format.
        """
        if not self.ENABLED:
            return []
            
        prowlarr_results = self.fetch_from_prowlarr(imdb_id, content_type)
        return self._map_to_stremio_format(prowlarr_results)

    def _map_to_stremio_format(self, prowlarr_results: List[Dict[str, Any]]) -> List[Dict[str, Any]]:
        """
        Maps Prowlarr results to Stremio/Torrentio 'streams' format.
        Fakes Torrentio name to trigger GoStorm quality filters.
        """
        import re
        streams = []
        for res in prowlarr_results:
            title = res.get("title", "")
            size_bytes = res.get("size", 0)
            seeders = res.get("seeders", 0)
            leechers = res.get("leechers", 0)
            info_hash = res.get("infoHash", "")
            
            if not info_hash:
                continue

            # V1.4.6-Fix: Exclude garbage releases (HDTS, WEBSCREENER, etc.)
            if re.search(r'hdts|ts|tc|telecine|telesync|screener|scr|webscreener', title, re.IGNORECASE):
                continue

            # Resolution Mapping (Prowlarr API -> Torrentio Semantics)
            # res_val is numeric: 2160, 1080, 720
            res_val = res.get("quality", {}).get("quality", {}).get("resolution", 0)
            
            if res_val == 2160:
                res_tag = "4k"
            elif res_val == 1080:
                res_tag = "1080p"
            elif res_val == 720:
                res_tag = "720p"
            else:
                # Fallback to regex on title if API resolution is unknown
                if re.search(r'2160p|4k|uhd', title, re.IGNORECASE):
                    res_tag = "4k"
                elif re.search(r'1080p', title, re.IGNORECASE):
                    res_tag = "1080p"
                elif re.search(r'720p', title, re.IGNORECASE):
                    res_tag = "720p"
                else:
                    res_tag = "1080p" # Safe default

            # Convert size to GB for the title string
            size_gb = size_bytes / (1024 * 1024 * 1024)
            
            # Format title to match Torrentio's multiline format (essential for existing regex)
            formatted_title = f"{title}\n👤 {seeders} ⬇️ {leechers}\n💾 {size_gb:.2f}GB"
            
            stream = {
                # CRITICAL: Must start with "Torrentio\n" followed by resolution to trigger filters
                "name": f"Torrentio\n{res_tag}",
                "title": formatted_title,
                "infoHash": info_hash,
                "behaviorHints": {
                    "bingeGroup": f"prowlarr-{res_tag}"
                }
            }
            streams.append(stream)
            
        return streams
