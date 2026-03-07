#!/usr/bin/env python3
"""
Prowlarr Adapter for GoStorm Sync Scripts
Converts Prowlarr/Newznab results to Stremio/Torrentio format.
"""

import json
import logging
import requests
from typing import List, Dict, Any, Optional

class ProwlarrClient:
    def __init__(self):
        self.API_KEY = "API_KEY"
        self.BASE_URL = "http://<your-ip>:9696"
        self.SEARCH_ENDPOINT = f"{self.BASE_URL}/api/v1/search"
        
    def fetch_from_prowlarr(self, imdb_id: str, content_type: str = "movie") -> List[Dict[str, Any]]:
        """
        Directly query Prowlarr API for a specific IMDB ID.
        """
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
            response = requests.get(self.SEARCH_ENDPOINT, params=params, timeout=2)
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
