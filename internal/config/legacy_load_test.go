package config

// legacyLoadConfig is LoadConfig as it was before the configuration moved to the DB,
// kept verbatim (minus the telemetry write) as the reference for the equivalence test.

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

func legacyLoadConfig() Config {
	// 1. Initial Defaults (V138 Gold Standard)
	cfg := Config{
		MasterConcurrencyLimit: 25,
		ReadAheadBudgetMB:      256,
		MetadataCacheSizeMB:    50, // Default 50MB for metadata
		FuseBlockSize:          1048576,
		StreamingThresholdKB:   128,
		LogLevel:               "INFO",
		BlockListURL:           DefaultBlockListURL,
		BlockListFilter:        DefaultBlockListFilter,

		AttrTimeoutSeconds:     5.0, // attributes are content-addressed; out-of-band changes notify the kernel
		EntryTimeoutSeconds:    5.0,
		NegativeTimeoutSeconds: 1.0,
		FuseMaxInflightMB:      0, // off: config.json-only knob, matches go-fuse's own default
		FuseReadAheadKB:        1024,

		MaxRetryAttempts: 6,
		RetryDelayMS:     500,

		PreloadWorkersCount:   4,
		PreloadInitialDelayMS: 1000,
		WarmStartIdleSeconds:  6,
		MaxConcurrentPrefetch: 3,

		CacheCleanupIntervalMin: 5,
		MaxCacheEntries:         10000,
		DiskWarmupQuotaGB:       15,
		WarmupHeadSizeMB:        64,

		Language: LanguageConfig{
			PreferredTerms: []string{"ita", "multi", "dual"},
			PreferredFlags: []string{"IT"},
			ExcludedFlags: []string{
				"ES", "FR", "DE", "RU", "CN", "JP", "KR", "TH", "PT", "BR",
				"UA", "PL", "NL", "TR", "SA", "IN", "CZ", "HU", "RO",
			},
		},

		Scheduler: SchedulerConfig{
			Enabled:       false, // off by default — won't break installs using cron
			MoviesSync:    DailyJobConfig{Enabled: true, DaysOfWeek: []int{1, 4}, Hour: 3, Minute: 0},
			TVSync:        DailyJobConfig{Enabled: true, DaysOfWeek: []int{3, 5}, Hour: 4, Minute: 0},
			MusicSync:     DailyJobConfig{Enabled: true, DaysOfWeek: []int{0}, Hour: 15, Minute: 0},
			WatchlistSync: WatchlistSyncConfig{Enabled: true, IntervalHours: 1},
		},

		MusicDiscovery: DefaultMusicDiscovery(),

		TorrentioURL:     "https://torrentio.strem.fun",
		GoStormBaseURL:   "http://127.0.0.1:8090",
		ProxyListenPort:  8080,
		MetricsPort:      9080,
		BlockListEnabled: false,

		EnableTelemetry: true,
		TelemetryURL:    "https://telemetry.gostream.workers.dev",

		EnableStateDB: true,

		// Legacy Fixed Defaults
		DefaultFileSize:         30 * 1024 * 1024 * 1024,
		ReadAheadBase:           16 * 1024 * 1024,
		ReadAheadInitial:        16 * 1024 * 1024,
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

	if cfg.LogDir == "" {
		cfg.LogDir = filepath.Join(filepath.Dir(cfg.ConfigPath), "logs")
	}

	// 5b. Populate engine script paths
	exe, _ := os.Executable()
	binDir := filepath.Dir(exe)
	scriptsDir := filepath.Join(binDir, "scripts")
	logsDir := filepath.Join(binDir, "logs")
	cfg.EngineScripts = map[string]EngineConfig{
		"movies":    {ScriptPath: filepath.Join(scriptsDir, "gostorm-sync-complete.py"), LogsDir: logsDir},
		"tv":        {ScriptPath: filepath.Join(scriptsDir, "gostorm-tv-sync.py"), LogsDir: logsDir},
		"watchlist": {ScriptPath: filepath.Join(scriptsDir, "plex-watchlist-sync.py"), LogsDir: logsDir},
	}

	return cfg
}
