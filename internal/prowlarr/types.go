package prowlarr

// ConfigProwlarr holds Prowlarr indexer connection settings.
// Mirrors the "prowlarr" section in config.json.
type ConfigProwlarr struct {
	Enabled bool   `json:"enabled"`
	APIKey  string `json:"api_key"`
	URL     string `json:"url"`
}

// ProwlarrResult mirrors a single item from the Prowlarr /api/v1/search response.
type ProwlarrResult struct {
	Guid        string `json:"guid"`
	Title       string `json:"title"`
	Size        int64  `json:"size"`
	Seeders     int    `json:"seeders"`
	Leechers    int    `json:"leechers"`
	InfoHash    string `json:"infoHash"`
	DownloadUrl string `json:"downloadUrl"`
	Quality     struct {
		Quality struct {
			Resolution int `json:"resolution"`
		} `json:"quality"`
	} `json:"quality"`
}

// Stream represents a Stremio/Torrentio stream entry returned by FetchTorrents.
type Stream struct {
	Name          string        `json:"name"`
	Title         string        `json:"title"`
	InfoHash      string        `json:"infoHash"`
	BehaviorHints BehaviorHints `json:"behaviorHints"`
}

// BehaviorHints is part of the Stremio stream format.
type BehaviorHints struct {
	BingeGroup string `json:"bingeGroup"`
}
