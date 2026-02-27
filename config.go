package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds all configurable parameters for the FUSE proxy
type Config struct {
	// --- Internal / Derived Fields ---
	ConfigPath string `json:"-"`
	RootPath   string `json:"-"` // V138: Root path for state/config (default: /home/pi)

	// --- Core Tuning (JSON Mapped) ---
	MasterConcurrencyLimit int    `json:"master_concurrency_limit"` // Global limit for concurrent HTTP requests to GoStorm
	ReadAheadBudgetMB      int64  `json:"read_ahead_budget_mb"`     // Global budget for read-ahead in MB
	MetadataCacheSizeMB    int64  `json:"metadata_cache_size_mb"`   // Size of metadata LRU cache in MB (V178)
	WriteBufferSizeKB      int    `json:"write_buffer_size_kb"`     // Size of write buffer in KB
	ReadBufferSizeKB       int    `json:"read_buffer_size_kb"`
	FuseBlockSize          int    `json:"fuse_block_size_bytes"`
	StreamingThresholdKB   int64  `json:"streaming_threshold_kb"`
	MaxConcurrentStreaming int    `json:"max_concurrent_streaming"` // V187: Aligned to 25 (Native Bridge offload)
	LogLevel               string `json:"log_level"`

	// --- FUSE Timing ---
	AttrTimeoutSeconds     float64 `json:"attr_timeout_seconds"`
	EntryTimeoutSeconds    float64 `json:"entry_timeout_seconds"`
	NegativeTimeoutSeconds float64 `json:"negative_timeout_seconds"`

	// --- HTTP Resilience ---
	HTTPClientTimeoutSeconds int `json:"http_client_timeout_seconds"`
	MaxRetryAttempts         int `json:"max_retry_attempts"`
	RetryDelayMS             int `json:"retry_delay_ms"`
	RescueGracePeriodSeconds int `json:"rescue_grace_period_seconds"`
	RescueCooldownHours      int `json:"rescue_cooldown_hours"`

	// --- Preload Engine ---
	PreloadWorkersCount   int `json:"preload_workers_count"`
	PreloadInitialDelayMS int `json:"preload_initial_delay_ms"`
	WarmStartIdleSeconds  int `json:"warm_start_idle_seconds"`
	MaxConcurrentPrefetch int `json:"max_concurrent_prefetch"`

	// --- Cache Management ---
	CacheCleanupIntervalMin int `json:"cache_cleanup_interval_min"`
	MaxCacheEntries         int `json:"max_cache_entries"`

	// --- Connectivity ---
	GoStormBaseURL string `json:"gostorm_url"`
	ProxyListenPort   int    `json:"proxy_listen_port"`
	MetricsPort       int    `json:"metrics_port"`
	BlockListURL      string `json:"blocklist_url"`

	// --- FUSE Paths ---
	// Fallback when CLI args are omitted. CLI args always take precedence.
	PhysicalSourcePath string `json:"physical_source_path"` // Real MKV dir (e.g. /mnt/torrserver)
	FuseMountPath      string `json:"fuse_mount_path"`      // FUSE virtual mount (e.g. /mnt/torrserver-go)

	// --- Legacy Compatibility Fields (populated from above) ---
	DefaultFileSize         int64         `json:"-"`
	ReadAheadBudget         int64         `json:"-"`
	MetadataCacheSize       int64         `json:"-"` // V178
	ReadAheadBase           int64         `json:"-"`
	ReadAheadInitial        int64         `json:"-"`
	StreamingThreshold      int64         `json:"-"`
	SequentialTolerance     int64         `json:"-"`
	MaxConcurrentHTTP       int           `json:"-"`
	RateLimitRequestsPerSec int           `json:"-"`
	PreloadWorkers          int           `json:"-"`
	MaxIdleConns            int           `json:"-"`
	MaxIdleConnsPerHost     int           `json:"-"`
	MaxConnsPerHost         int           `json:"-"`
	ConcurrencyLimit        int           `json:"-"`
	HTTPConnectTimeout      time.Duration `json:"-"`
	HTTPReadTimeout         time.Duration `json:"-"`
	KeepaliveInterval       time.Duration `json:"-"`
	KeepaliveIdleStart      time.Duration `json:"-"`
	KeepaliveMaxIdle        time.Duration `json:"-"`
	CacheTTL                time.Duration `json:"-"`
	UID                     uint32        `json:"-"`
	GID                     uint32        `json:"-"`
	WriteBufferSize         int           `json:"-"`
	ReadBufferSize          int           `json:"-"`

	// --- Disk Warmup ---
	DiskWarmupQuotaGB  int64 `json:"disk_warmup_quota_gb"`   // Total SSD quota for warmup cache (default: 32)
	WarmupHeadSizeMB   int64 `json:"warmup_head_size_mb"`    // Per-file head warmup cap in MB (default: 64)

	// --- NAT-PMP (V228) ---
	NatPMP NatPMPConfig `json:"natpmp"`
}

// LoadConfig loads configuration from environment variables with defaults
func LoadConfig() Config {
	// 1. Initial Defaults (V138 Gold Standard)
	cfg := Config{
		MasterConcurrencyLimit: 25,
		ReadAheadBudgetMB:      512,
		MetadataCacheSizeMB:    50, // Default 50MB for metadata
		WriteBufferSizeKB:      64,
		ReadBufferSizeKB:       64,
		FuseBlockSize:          1048576,
		StreamingThresholdKB:   128,
		MaxConcurrentStreaming: 25,
		LogLevel:               "INFO",

		AttrTimeoutSeconds:     1.0,
		EntryTimeoutSeconds:    1.0,
		NegativeTimeoutSeconds: 0.0,

		HTTPClientTimeoutSeconds: 30,
		MaxRetryAttempts:         6,
		RetryDelayMS:             500,

		PreloadWorkersCount:   4,
		PreloadInitialDelayMS: 1000,
		WarmStartIdleSeconds:  6,
		MaxConcurrentPrefetch: 3,

		CacheCleanupIntervalMin: 5,
		MaxCacheEntries:         10000,
		DiskWarmupQuotaGB:       32,
		WarmupHeadSizeMB:        64,

		GoStormBaseURL: "http://127.0.0.1:8090",
		ProxyListenPort:   8080,
		MetricsPort:       8096,

		// Legacy Fixed Defaults
		DefaultFileSize:         30 * 1024 * 1024 * 1024,
		ReadAheadBase:           16 * 1024 * 1024,
		ReadAheadInitial:        2 * 1024 * 1024,
		SequentialTolerance:     512 * 1024,
		RateLimitRequestsPerSec: 500,
		KeepaliveInterval:       15 * time.Second,
		KeepaliveIdleStart:      8 * time.Second,
		KeepaliveMaxIdle:        600 * time.Second,
		CacheTTL:                10 * time.Second,
		UID:                     1000,
		GID:                     1000,
	}

	// 2. Resolve Config Path — always co-located with the binary
	if p := os.Getenv("MKV_PROXY_CONFIG_PATH"); p != "" {
		cfg.ConfigPath = p
	} else {
		exe, err := os.Executable()
		if err == nil {
			cfg.ConfigPath = filepath.Join(filepath.Dir(exe), "config.json")
		} else {
			cfg.ConfigPath = "config.json" // fallback: CWD
		}
	}

	// 3. Try to load JSON
	if data, err := os.ReadFile(cfg.ConfigPath); err == nil {
		// V138: Support comments in JSON by stripping them before unmarshaling
		cleanData := stripJSONComments(data)
		if err := json.Unmarshal(cleanData, &cfg); err != nil {
			log.Printf("[Config] WARNING: Failed to parse %s: %v", cfg.ConfigPath, err)
		} else {
			log.Printf("[Config] Loaded settings from %s", cfg.ConfigPath)
			// Backward compat: if gostorm_url was not present in config, fall back to legacy torrserver_url key
			if cfg.GoStormBaseURL == "" {
				var raw map[string]json.RawMessage
				if json.Unmarshal(cleanData, &raw) == nil {
					if v, ok := raw["torrserver_url"]; ok {
						var s string
						if json.Unmarshal(v, &s) == nil && s != "" {
							cfg.GoStormBaseURL = s
							log.Printf("[Config] Loaded GoStormBaseURL from legacy key 'torrserver_url': %s", s)
						}
					}
				}
			}
		}
	}

	// 4. Override from environment (Highest Priority)
	cfg.applyEnvOverrides()

	// 5. Finalize and map derived fields
	cfg.finalize()

	return cfg
}

// stripJSONComments removes // comments from JSON data and preserves valid syntax.
// It is careful not to strip // when part of a URL (e.g., http://).
func stripJSONComments(data []byte) []byte {
	lines := strings.Split(string(data), "\n")
	var result []string
	for _, line := range lines {
		// Find // but only if not preceded by : (simple check for http://)
		idx := strings.Index(line, "//")
		if idx != -1 {
			if idx > 0 && line[idx-1] == ':' {
				// It's likely a URL, look for another // later in the line
				secondIdx := strings.Index(line[idx+2:], "//")
				if secondIdx != -1 {
					line = line[:idx+2+secondIdx]
				}
			} else {
				line = line[:idx]
			}
		}
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return []byte(strings.Join(result, " "))
}

func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("MKV_PROXY_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.MasterConcurrencyLimit = n
		}
	}
	if v := os.Getenv("MKV_PROXY_READ_AHEAD_BUDGET"); v != "" {
		if size, err := parseBytes(v); err == nil {
			c.ReadAheadBudgetMB = size / (1024 * 1024)
		}
	}
	if v := os.Getenv("MKV_PROXY_GOSTORM_URL"); v != "" {
		c.GoStormBaseURL = v
	}
	if v := os.Getenv("MKV_PROXY_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := os.Getenv("MKV_PROXY_UID"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil {
			c.UID = uint32(n)
		}
	}
	if v := os.Getenv("MKV_PROXY_GID"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil {
			c.GID = uint32(n)
		}
	}
}

func (c *Config) finalize() {
	// Sync legacy fields with unified master limit
	c.ConcurrencyLimit = c.MasterConcurrencyLimit
	c.MaxConcurrentHTTP = c.MasterConcurrencyLimit
	c.MaxIdleConns = c.MasterConcurrencyLimit
	c.MaxIdleConnsPerHost = c.MasterConcurrencyLimit
	c.MaxConnsPerHost = c.MasterConcurrencyLimit

	// Map JSON fields to internal logic fields
	// Calculate ReadAheadBudget in bytes
	c.ReadAheadBudget = c.ReadAheadBudgetMB * 1024 * 1024
	if c.ReadAheadBudget < 10*1024*1024 {
		c.ReadAheadBudget = 10 * 1024 * 1024 // Min 10MB
	}

	// Calculate MetadataCacheSize in bytes
	c.MetadataCacheSize = c.MetadataCacheSizeMB * 1024 * 1024
	if c.MetadataCacheSize < 1*1024*1024 {
		c.MetadataCacheSize = 1 * 1024 * 1024 // Min 1MB
	}

	// Calculate buffer sizes in bytes
	c.WriteBufferSize = c.WriteBufferSizeKB * 1024
	c.ReadBufferSize = c.ReadBufferSizeKB * 1024
	c.StreamingThreshold = c.StreamingThresholdKB * 1024
	c.HTTPConnectTimeout = time.Duration(c.HTTPClientTimeoutSeconds) * time.Second
	c.HTTPReadTimeout = 45 * time.Second // Keep fixed for now
	c.PreloadWorkers = c.PreloadWorkersCount
	if c.MaxConcurrentPrefetch <= 0 {
		c.MaxConcurrentPrefetch = 3 // Safety fallback
	}
}

// parseBytes parses byte size strings like "80MB", "128KB", "1GB"
func parseBytes(s string) (int64, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	multipliers := map[string]int64{
		"KB": 1024, "MB": 1024 * 1024, "GB": 1024 * 1024 * 1024,
		"K": 1024, "M": 1024 * 1024, "G": 1024 * 1024 * 1024,
	}
	for suffix, mult := range multipliers {
		if len(s) > len(suffix) && s[len(s)-len(suffix):] == suffix {
			numPart := s[:len(s)-len(suffix)]
			if n, err := strconv.ParseInt(numPart, 10, 64); err == nil {
				return n * mult, nil
			}
		}
	}
	return 0, strconv.ErrSyntax
}

// LogConfig logs the active configuration
func (c *Config) LogConfig(logger *log.Logger) {
	logger.Printf("=== Configuration ===")
	logger.Printf("Source: %s", c.ConfigPath)
	logger.Printf("MasterConcurrencyLimit: %d", c.MasterConcurrencyLimit)
	logger.Printf("ReadAheadBudget: %d MB", c.ReadAheadBudgetMB)
	logger.Printf("Buffers (W/R): %d KB / %d KB (Block: %d)", c.WriteBufferSizeKB, c.ReadBufferSizeKB, c.FuseBlockSize)
	logger.Printf("StreamingThreshold: %d KB", c.StreamingThresholdKB)
	logger.Printf("LogLevel: %s", c.LogLevel)
	logger.Printf("GoStormBaseURL: %s", c.GoStormBaseURL)
	logger.Printf("FUSE Timeouts (Attr/Entry/Neg): %.1f/%.1f/%.1f", c.AttrTimeoutSeconds, c.EntryTimeoutSeconds, c.NegativeTimeoutSeconds)
	logger.Printf("HTTP Connect Timeout: %ds, Retries: %d, Delay: %dms", c.HTTPClientTimeoutSeconds, c.MaxRetryAttempts, c.RetryDelayMS)
	logger.Printf("Preload Engine: Workers=%d, Delay=%dms", c.PreloadWorkersCount, c.PreloadInitialDelayMS)

	logger.Printf("Cache Management: Cleanup=%dm, MaxEntries=%d", c.CacheCleanupIntervalMin, c.MaxCacheEntries)
	logger.Printf("Network: ProxyPort=%d, MetricsPort=%d", c.ProxyListenPort, c.MetricsPort)
	logger.Printf("=====================")
}

// GetStateDir returns the centralized state directory path
func GetStateDir() string {
	if globalConfig.RootPath == "" {
		return "/home/pi/STATE" // Default fallback
	}
	return filepath.Join(globalConfig.RootPath, "STATE")
}
