# Prowlarr Adapter Integration

## Overview
The Prowlarr Adapter (`prowlarr_client.py`) is a Python module designed to interface the GoStorm synchronization scripts with a local Prowlarr instance. It acts as a transparent proxy that translates Prowlarr's Newznab/JSON search results into the Stremio/Torrentio JSON format required by the project's existing filtering and discovery logic.

This integration was implemented to ensure system resilience following the frequent downtimes of the public Torrentio service.

## Architecture

### Component: `prowlarr_client.py`
- **Search Logic**: Utilizes the Prowlarr `/api/v1/search` endpoint using the `imdbId` parameter for high-precision results.
- **Protocol Mapping**: Converts Prowlarr attributes (`size`, `seeders`, `infoHash`) into a formatted multiline `title` string that mirrors Torrentio's output.
- **Formatting Parity**:
    - **Seeders**: Prefixed with `👤` (e.g., `👤 150`).
    - **Size**: Formatted in GB and prefixed with `💾` (e.g., `💾 15.50GB`).
    - **Metadata**: Preserves the original release name for resolution and HDR filtering.

### Integration Logic: Strict Fallback
The sync scripts (`gostorm-sync-complete.py`, `plex-watchlist-sync.py`, `gostorm-tv-sync.py`) implement a "Prowlarr-First" strategy:
1. **Primary**: Query Prowlarr using the IMDB ID.
2. **Filtering**: Apply project-specific quality (4K/1080p), language (EN/IT), and size filters to Prowlarr results.
3. **Fallback**: If Prowlarr returns zero valid results or the API call fails, the script automatically attempts a query via the original Torrentio API.

## Configuration
The adapter is should be configured with yur Prowlarr parameters:
- **Prowlarr IP**: `http://<your-ip>:9696`
- **API Key**: `your-api-key`
- **Timeout**: `45 seconds` (Optimized for deep indexer searches).

## Validation
- **Unit Tests**: `test_prowlarr_client.py` verifies API response parsing and Torrentio-compatible title generation.
- **Integration Tests**: `test_sync_integration.py` verifies the fallback mechanism and successful communication between components.
