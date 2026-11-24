package app

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"tiramisu/internal/apidocs"
	"tiramisu/internal/auth"
	"tiramisu/internal/autoremove"
	"tiramisu/internal/blocklist"
	"tiramisu/internal/bridge"
	"tiramisu/internal/cache"
	"tiramisu/internal/catalog"
	"tiramisu/internal/catalog/mediaserver"
	"tiramisu/internal/config"
	"tiramisu/internal/engine"
	engset "tiramisu/internal/engine/settings"
	"tiramisu/internal/enginehost"
	"tiramisu/internal/friendlib"
	"tiramisu/internal/friends"
	"tiramisu/internal/geoip"
	"tiramisu/internal/library"
	"tiramisu/internal/lockmgr"
	"tiramisu/internal/mcp"
	"tiramisu/internal/monitor/clients"
	"tiramisu/internal/monitor/collector"
	"tiramisu/internal/monitor/dashboard"
	"tiramisu/internal/natpmp"
	"tiramisu/internal/preload"
	"tiramisu/internal/prowlarr"
	"tiramisu/internal/ratelimit"
	"tiramisu/internal/registry"
	"tiramisu/internal/streamer"
	syncercache "tiramisu/internal/syncer/cache"
	"tiramisu/internal/syncer/engines"
	"tiramisu/internal/syncer/scheduler"
	"tiramisu/internal/telemetry"
	"tiramisu/internal/tuner"
	"tiramisu/internal/updater"
	"tiramisu/internal/vfs"
	"tiramisu/internal/warmup"
	"tiramisu/internal/watchdog"
	"tiramisu/internal/webtls"
	"tiramisu/internal/webui"

	"golang.org/x/sys/cpu"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

//go:embed settings.html
var settingsHTML []byte

// checkHardwareSupport warns when the CPU sits below the Raspberry Pi 4's effective floor.
// On amd64 that means AVX2: any x86_64 CPU without it (everything before ~2013, Haswell/
// Excavator) forces Go's crypto/sha1 onto its slowest pure-scalar path, and full-swarm piece
// verification struggles to keep up with real playback bitrates - the failure mode diagnosed
// on a 2008 Core2Quad (100% CPU, stutter, no hardware acceleration to fall back on). It is a
// warning, not a gate: the operator decides whether the machine is good enough for their
// bitrates, and a logged reason beats an unexplained stutter. arm64 needs no equivalent check:
// NEON is mandatory in every ARMv8-A chip, so any real arm64 CPU - including the Pi4's own
// Cortex-A72 - already clears this floor. Writes to stderr because it runs before the loggers
// exist; systemd captures it into the service log either way.
func checkHardwareSupport() {
	if runtime.GOARCH == "amd64" && !cpu.X86.HasAVX2 {
		fmt.Fprintln(os.Stderr, "WARNING: this CPU does not support AVX2 (pre-2013 x86_64: before Intel Haswell / AMD Excavator).")
		fmt.Fprintln(os.Stderr, "WARNING: SHA1 piece verification falls back to its slowest path, which may not sustain real playback bitrates.")
		fmt.Fprintln(os.Stderr, "WARNING: expect high CPU and stuttering on high-bitrate files. Starting anyway.")
	}
}

// Run starts Tiramisu and returns when the FUSE server ends; version is the build's release version.
func Run(version string) {
	AppVersion = version
	checkHardwareSupport()

	var dbPath string
	flag.StringVar(&dbPath, "path", "", "path to database and config")
	resetAuth := flag.Bool("reset-auth", false, "clear the :9080 user, sessions and tokens and turn sign-in off")
	exportCfg := flag.String("export-config", "", "write the saved configuration to a file (- for stdout) and exit")
	importCfg := flag.String("import-config", "", "save a config.json as the configuration (applied at the next start) and exit")
	flag.Parse()

	// Default --path to the directory containing the binary (portable install)
	if dbPath == "" {
		if exe, err := os.Executable(); err == nil {
			dbPath = filepath.Dir(exe)
		}
	}
	// The engine's legacy state (config.db, settings.json, trackers.txt, blocklist) lives in
	// --path; older Docker installs wrote it to the working directory: move it first.
	engineStateDir = dbPath
	if dbPath != "" {
		engset.MigrateFromWorkdir(dbPath, logger.Printf)
	}

	source, mount := flag.Arg(0), flag.Arg(1)

	// The configuration lives in the state DB: paths first, then the DB, then the configuration.
	boot, bootErr := config.Prepare(dbPath)
	if bootErr != nil {
		config.Fatal(bootErr)
	}
	if *resetAuth {
		// Before anything starts: a second engine next to the running service would mount FUSE twice.
		os.Exit(auth.RunResetFlag(boot.DBPath, boot.StateDir))
	}
	if bootErr := boot.Open(logger, logger.Printf); bootErr != nil {
		config.Fatal(bootErr)
	}
	if *exportCfg != "" || *importCfg != "" {
		code := 0
		if *exportCfg != "" {
			code = config.RunExport(boot.Store, *exportCfg)
		} else {
			code = config.RunImport(boot.Store, *importCfg)
		}
		boot.DB.Close() // checkpoint: no -wal left behind, owned by whoever ran this
		config.WarnOwner(boot.DBPath)
		os.Exit(code)
	}
	cfg, _, bootErr := boot.Load()
	if bootErr != nil {
		config.Fatal(bootErr)
	}
	configBoot = boot
	globalConfig.Store(&cfg)
	prowlarrClient = prowlarr.NewClient(gc().Prowlarr)
	telemetry.SendHeartbeat(*gc(), AppVersion)
	logger.Printf("[DEBUG] BlockListURL loaded: '%s'", gc().BlockListURL)

	// CLI args take precedence; fall back to config.json values if omitted
	if source == "" {
		source = gc().PhysicalSourcePath
	}
	if mount == "" {
		mount = gc().FuseMountPath
	}
	if source == "" || mount == "" {
		fmt.Println("Usage: tiramisu [--path /path/to/db] <source_path> <mount_path>")
		fmt.Println("  Or set physical_source_path and fuse_mount_path in config.json")
		os.Exit(1)
	}
	physicalSourcePath = source
	virtualMountPath = mount

	gc().LogConfig(logger)

	var peerInfo engine.PeerInfoFunc
	if gc().PeerGeoIPEnabled {
		peerInfo = geoip.Start(context.Background(), GetStateDir(), logger.Printf).PeerInfo
	}
	// engine.md §1.13: legacy import, settings, blocklist, start; Boot returns once ready.
	logger.Println("Starting the engine...")
	host, hostErr := enginehost.Boot(enginehost.Options{DB: boot.DB, StateDir: engineStateDir, PortManaged: gc().NatPMP.Enabled,
		BlockListEnabled: gc().BlockListEnabled, BlockListFilter: gc().BlockListFilter, PeerInfo: peerInfo, Logf: logger.Printf,
		Playing: hashPlaying})
	if hostErr != nil {
		config.Fatal(hostErr)
	}
	engineHost, eng, nativeBridge = host, host.Engine, host.Bridge
	engSets := host.Settings.Current()

	warmup.InitDiskWarmup(gc().DiskWarmupQuotaGB, engSets.UseDisk, engSets.TorrentsSavePath)
	eng.ServeWarmup(warmup.DiskWarmup)
	// Fires synchronously from DiskWarmup's single writeWorker goroutine at the exact moment a
	// head warmup fetch starts/completes - avoids the race where a caller checking IsWarmingUp()
	// right after WriteChunk() returns could read stale state, since WriteChunk only enqueues and
	// returns immediately (see OnWarmupStateChange doc comment in internal/warmup/warmup.go).
	warmup.OnWarmupStateChange = host.WarmupActive
	// Lets the warmup package pull the tail window itself (EnsureTail): a prefetch nobody
	// waits on; it cannot import the bridge, so the dependency is injected.
	warmup.TailFetch = nativeBridge.PrefetchBlock
	warmup.TailFillYield = func(hash string, _ int) bool { return hashInPlayback(hash) }
	warmup.TailPaused = streamer.ErrPaused
	go registry.StartRegistryWatchdog(backgroundStopChan)
	go natpmp.NatpmpLoop(backgroundStopChan, gc().NatPMP, logger, host.PeerPort())

	masterDataSemaphore = make(chan struct{}, gc().MasterConcurrencyLimit)
	eng.SetBackground(masterDataSemaphore, anyConfirmedPlayback)
	startHandleGC()

	// Initialize global helpers
	globalRateLimiter = ratelimit.NewRateLimiter(gc().RateLimitRequestsPerSec, 1*time.Second)
	globalLockManager = lockmgr.NewLockManager(1 * time.Hour)

	poolSize := int(gc().ReadAheadBase)
	if poolSize == 0 {
		poolSize = 16 * 1024 * 1024
	}
	readBufferPool = &sync.Pool{
		New: func() interface{} {
			buf := make([]byte, poolSize)
			return &buf
		},
	}
	logger.Printf("ReadBufferPool initialized with size: %d bytes (matches ReadAheadBase)", poolSize)

	// Config is loaded: anchor the raCache recycled-buffer pool to the budget now (at package
	// init gc() was nil, so newRaCache kept the static 32-buffer default).
	raCache.AnchorPoolToConfig()

	catalog.SetRetryDefaults(gc().MaxRetryAttempts, time.Duration(gc().RetryDelayMS)*time.Millisecond)

	go tuner.Start(context.Background(), eng)

	if gc().BlockListEnabled && gc().BlockListURL != "" {
		blockLists.Start(gc().BlockListURL)
	}

	safeGo(func() {
		updater.Start(AppVersion, backgroundStopChan)
	})

	peerPreloader = preload.NewPeerPreloader(nativeBridge)

	// Metadata LRU cache: capacity from config, 24h TTL.
	metaCache = cache.NewLRUCache(gc().MetadataCacheSize, 24*time.Hour)

	// Deterministic inode map ensures Plex doesn't see "new files" after restarts.
	if err := InitGlobalInodeMap(GetStateDir(), logger); err != nil {
		logger.Printf("WARNING: Failed to initialize inode map: %v (falling back to filename hash)", err)
	}

	// The state DB, opened at start-up for the configuration, also holds the engine state.
	{
		stateDB = configBoot.DB
		{
			// Migrate JSON files if needed; on failure the state falls back to JSON, but the
			// DB stays open: the configuration lives there.
			if err := stateDB.MigrateFromJSON(GetStateDir()); err != nil {
				logger.Printf("WARNING: StateDB migration failed: %v (falling back to JSON)", err)
				stateDB = nil
			} else {
				// Wire up DB to InodeMap, then reload from DB (covers boot 2+ where JSON is gone)
				globalInodeMap.SetDB(stateDB)
				if err := globalInodeMap.LoadFromDisk(); err != nil {
					logger.Printf("WARNING: Failed to reload InodeMap from DB: %v", err)
				}
				registry.SetRegistryDB(stateDB)
				// V750: Restore playback states from previous sessions
				restorePlaybackStates(stateDB)
				registry.SetStateDir(GetStateDir())
				logger.Printf("[StateDB] Active: %s", configBoot.DBPath)

				// V304 ban persistence: restore prior bans (30-day TTL) and persist new ones.
				var v304Bans []string
				if ips, err := stateDB.LoadV304Bans(30 * 24 * time.Hour); err != nil {
					logger.Printf("WARNING: Failed to load V304 bans: %v", err)
				} else if len(ips) > 0 {
					v304Bans = ips
					logger.Printf("[V304] Restored %d persisted peer bans", len(ips))
				}
				// Reachability outcomes: the sync engines read the counter to tell a dead
				// release from a slow one before dropping a title.
				failDB := stateDB
				bridge.ReachabilityOutcome = func(hash string, resolved bool) {
					var err error
					if resolved {
						// Success is the common case: read first so the usual read costs a
						// lookup instead of a write transaction.
						if n, qerr := failDB.MetadataFailureCount(hash); qerr != nil || n == 0 {
							return
						}
						err = failDB.ClearMetadataFailure(hash)
					} else {
						short := hash
						if len(short) > 8 {
							short = short[:8]
						}
						// Deliberately does not say why: the session layer that condemned
						// already logged its own reason, with duration and outcome.
						logger.Printf("[DeadSwarm] %s did not answer", short)
						err = failDB.RecordMetadataFailure(hash)
					}
					if err != nil {
						logger.Printf("WARNING: reachability bookkeeping for %s: %v", hash, err)
					}
				}

				banDB := stateDB
				engine.SetV304Bans(v304Bans, func(ip string) {
					if err := banDB.SaveV304Ban(ip); err != nil {
						logger.Printf("WARNING: Failed to persist V304 ban for %s: %v", ip, err)
					}
				})
			}
		}
	}

	finishGlobalInodeMapInit(logger)

	// The dir cache must exist before the startup builder runs: audio
	// reconciliation invalidates stale listings when it publishes, and a nil cache
	// there is a panic in a bare goroutine that would kill the process before mount.
	globalDirCache = vfs.NewDirCache(10 * time.Second)

	// Pre-populate cache at startup to improve Plex scan performance.
	cacheBuilder := NewStartupCacheBuilder(source, metaCache, logger)
	cacheBuilder.Start()

	globalCleanupManager = NewCleanupManager(logger, peerPreloader, metaCache, nativeBridge)
	globalCleanupManager.Start()

	globalTorrentRemover = autoremove.New(nativeBridge, logger, autoremove.Deps{
		StateDir:      GetStateDir,
		AudioRegistry: audioOwnershipRegistry,
		ClearNegative: func(hash string) {
			if globalSyncCacheManager != nil {
				globalSyncCacheManager.ClearNegativeCache(hash)
			}
		},
	})
	globalSyncCacheManager = syncercache.NewSyncCacheManager(GetStateDir(), logger)

	// V1.7.1: Wire up StateDB to sync cache manager (after it's created)
	if stateDB != nil {
		globalSyncCacheManager.SetDB(stateDB)
	}

	if err := globalSyncCacheManager.LoadCachesFromDisk(); err != nil {
		logger.Printf("WARNING: Failed to load sync caches from disk: %v", err)
	}

	// Sync caches to disk every 30s.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := globalSyncCacheManager.SyncToDisk(); err != nil {
					logger.Printf("SyncCache: Failed to sync to disk: %v", err)
				}
			case <-backgroundStopChan:
				return
			}
		}
	}()

	// Start periodic cleanup of sync caches (every 1 hour)
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// Cleanup stale entries: negative cache 12h TTL
				globalSyncCacheManager.CleanupStaleEntries(12 * time.Hour)
			case <-backgroundStopChan:
				return
			}
		}
	}()

	// Sign-in on :9080 (opt-in). The state lives in tiramisu.db, never in config.json.
	// The DB opened at start-up, not stateDB: a failed JSON migration leaves sign-in working.
	authSvc, authTrusted := auth.Start(http.DefaultServeMux, configBoot.DB, GetStateDir(), gc().AuthTrustedProxies, logger.Printf)
	apidocs.Register(http.DefaultServeMux, authSvc.Enabled)
	tlsSrv := webtls.New(webtls.Options{
		Settings: gc().HTTPS, StateDir: GetStateDir(),
		AccessOn: authSvc.Enabled() || authSvc.FailClosed(), Trusted: authTrusted,
		Saved: func() config.HTTPSConfig { return gc().HTTPS }, AccessNow: authSvc.Enabled,
		OnBearer: authSvc.NoteCleartext, Logf: logger.Printf,
	})
	tlsSrv.Register(http.DefaultServeMux, func() any { return authSvc.CleartextUses() })
	authSvc.SetHSTS(tlsSrv.OwnCertificate())

	http.HandleFunc("/plex/webhook", handlePlexWebhook)

	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		cacheStats := metaCache.Stats()
		cleanupStats := globalCleanupManager.Stats()
		lockStats := globalLockManager.Stats()
		syncCacheStats := globalSyncCacheManager.Stats()

		// Get read-ahead buffer stats (for dashboard FUSE Buffer display)
		raTotal, raActive, raEntries := raCache.Stats()
		raStale := raTotal - raActive
		raBudget := gc().ReadAheadBudget
		var raPercent, raActivePercent, raStalePercent float64 // 0, not NaN, with no budget
		if raBudget > 0 {
			raPercent = float64(raTotal) / float64(raBudget) * 100
			raActivePercent = float64(raActive) / float64(raBudget) * 100
			raStalePercent = float64(raStale) / float64(raBudget) * 100
		}

		natPort := atomic.LoadInt64(&natpmp.CurrentNatPort)

		warmupBuckets := warmup.WarmupDurationBucketCounts()
		warmupBucketsJSON := fmt.Sprintf("[%d,%d,%d,%d,%d,%d,%d,%d]",
			warmupBuckets[0], warmupBuckets[1], warmupBuckets[2], warmupBuckets[3],
			warmupBuckets[4], warmupBuckets[5], warmupBuckets[6], warmupBuckets[7])

		// Task 4: hedge counters live per-Torrent on the fork - sum trigger count across active
		// torrents, report circuit breaker as open if any active torrent currently has it tripped.
		hedgeTriggerTotal, deadlinePieces, hedgeCircuitOpenAny := eng.HedgeTotals()
		// Peer view split by transport - conn slots are capped, so a transport is only worth
		// the share of useful bytes it returns for the share of slots it takes.
		peerStats := eng.CollectPeerTransportStats()
		// Gradient2 closed-loop state: the dry run reads backing_off/limit_max to see the loop
		// react to queueing before its 32-request ceiling is raised.
		g2Stats := eng.CollectGradient2Stats()
		shieldOn, shieldLastCorr, shieldNeed := eng.ShieldStatus()
		blockRejections, blockIPs := engine.BlocklistCounters()

		shortStream, shortFetch := bridge.ShortReadCounts()
		repairedStream, unfilledStream := bridge.ShortReadRepairCounts()

		jsonString := func(v string) string { b, _ := json.Marshal(v); return string(b) }
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"version":%s, "config_source":%s, "uptime":"%s", "cache_entries":%d, "cache_size_mb":%.2f, "cleanup_hashes":%d, "cleanup_offsets":%d, "cleanup_activities":%d, "locks_total":%d, "master_concurrency_limit":%d, "negative_cache_entries":%d, "fullpack_cache_entries":%d, "streaming_threshold_kb":%d, "config_preload_workers":%d, "max_conns_per_host":%d, "read_ahead_total_bytes":%d, "read_ahead_active_bytes":%d, "read_ahead_stale_bytes":%d, "read_ahead_entries":%d, "read_ahead_budget":%d, "read_ahead_percent":%.2f, "read_ahead_active_percent":%.2f, "read_ahead_stale_percent":%.2f, "natpmp_port":%d, "latest_version":%s, "update_available":%t, "warmup_duration_buckets_lt_2_5_10_15_30_60_120_gte120s":%s, "hedge_trigger_count":%d, "hedge_circuit_open":%t, "fetch_singleflight_dedup":%d, "peer_eject_count":%d, "v304_banned_peers":%d, "ip_blocklist_rejections":%d, "ip_blocklist_ips":%d, "fuse_short_reads":%d, "fuse_short_reads_repaired":%d, "fuse_short_reads_failed":%d, "short_read_stream":%d, "short_read_fetch":%d, "short_read_repaired":%d, "short_read_unfilled":%d, "peer_conns_tcp":%d, "peer_conns_utp":%d, "peer_rated_tcp":%d, "peer_rated_utp":%d, "peer_useful_bps_tcp":%.0f, "peer_useful_bps_utp":%.0f, "peer_eject_count_utp":%d, "peer_churn_count":%d, "peer_churn_count_utp":%d, "deadline_pieces":%d, "audio_namespace_state":%s, "audio_namespace_entries":%d, "gradient2_peers":%d, "gradient2_backing_off":%d, "gradient2_limit_min":%d, "gradient2_limit_max":%d, "shield_active":%t, "shield_last_corruption":%d, "shield_need_s":%d, "github_stars":%d}`,
			jsonString(AppVersion),
			jsonString(gc().ConfigPath),
			time.Since(startTime),
			cacheStats.Entries, float64(cacheStats.Size)/(1024*1024),
			cleanupStats.DeletedHashesTotal, cleanupStats.OffsetsTotal, cleanupStats.ActivitiesTotal,
			lockStats.TotalLocks,
			gc().MasterConcurrencyLimit,
			syncCacheStats.NegativeCacheEntries,
			syncCacheStats.FullpackCacheEntries,
			gc().StreamingThreshold/1024,
			gc().PreloadWorkers,
			gc().MaxConnsPerHost,
			raTotal, raActive, raStale, raEntries, raBudget,
			raPercent, raActivePercent, raStalePercent,
			natPort,
			jsonString(updater.LatestVersion()), updater.UpdateAvailable(),
			warmupBucketsJSON,
			hedgeTriggerTotal, hedgeCircuitOpenAny, fetchFlightDedupCount.Load(), peerStats.EjectTotal, eng.V304BannedCount(),
			blockRejections, blockIPs,
			fuseShortReadCount.Load(), fuseShortReadRepaired.Load(), fuseShortReadFailed.Load(), shortStream, shortFetch, repairedStream, unfilledStream,
			peerStats.ConnsTCP, peerStats.ConnsUTP, peerStats.RatedTCP, peerStats.RatedUTP,
			peerStats.UsefulBpsTCP, peerStats.UsefulBpsUTP,
			peerStats.EjectUTP, peerStats.ChurnTotal, peerStats.ChurnUTP, deadlinePieces,
			jsonString(audioNamespaceState()), audioNamespaceEntries(),
			g2Stats.Peers, g2Stats.BackingOff, g2Stats.LimitMin, g2Stats.LimitMax,
			shieldOn, shieldLastCorr, shieldNeed, updater.Stars())
	})

	// Which blocklist ranges are actually rejecting peers. The aggregate counters say
	// how much is blocked; this says who, which is the only way to tell a list that
	// catches monitoring outfits from one quietly eating ordinary peers.
	http.HandleFunc("/metrics/blocklist", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		rejections, distinct := engine.BlocklistCounters()
		json.NewEncoder(w).Encode(map[string]any{
			"enabled":      gc().BlockListEnabled,
			"url":          gc().BlockListURL,
			"distinct_ips": distinct,
			"rejections":   rejections,
			"top_ranges":   engine.BlocklistTopRanges(25),
		})
	})

	http.HandleFunc("/webhook", handlePlexWebhook)

	http.HandleFunc("/metrics/profiling", func(w http.ResponseWriter, r *http.Request) {
		totalReads, cacheHits, httpFetches, streamingReads, avgHTTPLatency, avgCacheLatency, cacheHitRate := globalProfilingStats.Stats()

		fmt.Fprintf(w, `{"version":"%s", "total_reads":%d, "cache_hits":%d, "cache_hit_rate_pct":%.2f, "http_fetches":%d, "streaming_reads":%d, "non_streaming_reads":%d, "avg_http_latency_ms":%.2f, "avg_cache_latency_ms":%.2f, "max_conns_per_host":%d}`,
			AppVersion,
			totalReads,
			cacheHits,
			cacheHitRate,
			httpFetches,
			streamingReads,
			totalReads-streamingReads,
			float64(avgHTTPLatency.Microseconds())/1000.0,
			float64(avgCacheLatency.Microseconds())/1000.0,
			gc().MaxConnsPerHost)
	})

	http.HandleFunc("/api/pieces", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		m := pieceMapFor(r.URL.Query().Get("hash"))
		if m == nil {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("{}"))
			return
		}
		json.NewEncoder(w).Encode(m)
	})

	http.HandleFunc("/api/recent-streams", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(recentSnapshot())
	})

	http.HandleFunc("/metrics/ttff", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"sessions_completed":%d,"sessions_filtered":%d,`+
			`"open_to_header_ms":%s,"open_to_deep_read_ms":%s,"seek_latency_ms":%s,`+
			`"read_latency_ms":{"warmup_head":%s,"warmup_tail":%s,"racache_hit":%s,"fetch_block":%s},`+
			`"stalls":{"count":%d,"max_ms":%d}}`,
			ttffStats.sessionsCompleted.Load(), ttffStats.sessionsFiltered.Load(),
			histJSON(&ttffStats.openToHeader), histJSON(&ttffStats.openToDeepRead), histJSON(&ttffStats.seekLatency),
			histJSON(&ttffStats.warmupHead), histJSON(&ttffStats.warmupTail),
			histJSON(&ttffStats.raCacheHit), histJSON(&ttffStats.fetchBlock),
			ttffStats.stallCount.Load(), ttffStats.maxStallMS.Load())
	})

	http.Handle("/ui/", http.StripPrefix("/ui/", webui.Handler()))

	http.HandleFunc("/control", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(settingsHTML)
	})

	var configMu sync.Mutex // one save at a time: each one patches the config the last one stored
	http.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			json.NewEncoder(w).Encode(gc().Redacted()) // secrets are write-only (issue #26)
			return
		}
		if r.Method != "POST" {
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		{
			configMu.Lock()
			defer configMu.Unlock()
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			// A merge onto the saved document: keys left out keep their value, and environment
			// overrides are never written.
			base, newCfg, err := configBoot.PatchSaved(body)
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			// Who is trusted and whether traffic is encrypted: only a browser session may change
			// these, not an API token.
			if authSvc.Enabled() && auth.SessionFrom(r) == "" {
				if k := config.SignInOnlyChange(base, newCfg); k != "" {
					http.Error(w, k+" changes only from the Control Panel, signed in", http.StatusForbidden)
					return
				}
			}
			source, actor := authSvc.Actor(r)
			cfg, _, _, err := configBoot.SaveConfig(newCfg, source, actor, "")
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			applyConfig(cfg)
			logger.Printf("[Config] Updated via Dashboard API")
			w.WriteHeader(200)
		}
	})
	configBoot.RegisterHistory(http.DefaultServeMux, config.HistoryDeps{
		Mu: &configMu, Actor: authSvc.Actor, Apply: applyConfig, Logf: logger.Printf,
		// Also closes them to the internal token, which the middleware lets through.
		Allowed: func(r *http.Request) bool { return !authSvc.Enabled() || auth.SessionFrom(r) != "" }})
	friends.Register(http.DefaultServeMux, func() *friends.Manager { return eng.Friends() },
		func(r *http.Request) bool { return !authSvc.Enabled() || auth.SessionFrom(r) != "" })
	configBoot.RegisterEngineHistory(http.DefaultServeMux, config.EngineHistoryDeps{
		Actor: authSvc.Actor, Logf: logger.Printf,
		Allowed: func(r *http.Request) bool { return !authSvc.Enabled() || auth.SessionFrom(r) != "" },
		Ignored: engineHost.Settings.IgnoredKeys, Current: engineHost.Settings.Document, Restore: engineHost.Restore,
		NeedsRestart: func(k string) bool { kind, ok := engset.KindOf(k); return ok && kind == engset.KindRestart }})

	http.HandleFunc("/api/prowlarr/search", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if prowlarrClient == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			w.Write([]byte(`[]`))
			return
		}
		imdbID := r.URL.Query().Get("imdb_id")
		contentType := r.URL.Query().Get("type")
		title := r.URL.Query().Get("title")
		if imdbID == "" {
			http.Error(w, "missing imdb_id parameter", http.StatusBadRequest)
			return
		}
		if contentType == "" {
			contentType = "movie"
		}
		year, _ := strconv.Atoi(r.URL.Query().Get("year"))
		streams, err := prowlarrClient.FetchTorrents(r.Context(), imdbID, contentType, title, year)
		if err != nil {
			http.Error(w, "prowlarr search failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		if streams == nil {
			streams = []prowlarr.Stream{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(streams)
	})

	// The Control Panel's engine settings, same-origin and in-process: the body and answers
	// of the retired :8090 /settings, with source and actor from the session.
	settingsHandler := engineHost.SettingsHandler(authSvc.Actor)
	http.HandleFunc("/api/tentacle/settings", settingsHandler)
	http.HandleFunc("/api/gostorm/settings", settingsHandler) // alias for existing clients

	http.HandleFunc("/api/restart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
		// Flush response before exiting
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Graceful shutdown, then exit with restartExitCode: systemd restarts on any
		// code, and the Docker entrypoint's supervision loop restarts only on this one.
		restartRequested.Store(true)
		go func() {
			time.Sleep(150 * time.Millisecond)
			p, _ := os.FindProcess(os.Getpid())
			p.Signal(syscall.SIGTERM)
		}()
	})

	// The syncs are built before the library manager, and release torrents through it.
	var libraryRef atomic.Pointer[library.Manager]
	releaseTorrent := func(ctx context.Context, hash string) (bool, error) {
		m := libraryRef.Load()
		if m == nil {
			return false, errors.New("library not ready: torrent kept")
		}
		return m.ReleaseIfUnused(ctx, hash)
	}

	var sched *scheduler.Scheduler // the library wiring below stops its held jobs

	// Sync Scheduler (Fase 1)
	// Routes are always registered so the Control Panel's manual "Run now" works
	// immediately after enabling+saving, without requiring a restart. Only the
	// automatic cron loop (sched.Run) is gated by Scheduler.Enabled, since that's
	// what could collide with a user's own external cron setup.
	{
		schedCfg := scheduler.SchedulerConfig{
			Enabled:       gc().Scheduler.Enabled,
			MoviesSync:    scheduler.DailyJobConfig(gc().Scheduler.MoviesSync),
			TVSync:        scheduler.DailyJobConfig(gc().Scheduler.TVSync),
			MusicSync:     scheduler.DailyJobConfig(gc().Scheduler.MusicSync),
			WatchlistSync: scheduler.WatchlistSyncConfig(gc().Scheduler.WatchlistSync),
		}

		statePath := filepath.Join(GetStateDir(), "scheduler_state.json")

		logsDir := gc().LogDir

		// Start midnight log truncation
		engines.StartLogTruncator(logsDir, backgroundStopChan)

		syncers := map[string]scheduler.Syncer{
			"movies": engines.NewMoviesSyncer(engines.MoviesSyncerConfig{
				GoStormURL:      gc().GoStormBaseURL,
				Torrents:        engines.EngineBackend(eng),
				TMDBAPIKey:      gc().TMDBAPIKey,
				TorrentioURL:    gc().TorrentioURL,
				PlexURL:         gc().Plex.URL,
				PlexToken:       gc().Plex.Token,
				PlexLib:         gc().Plex.LibraryID,
				MediaServerType: gc().MediaServerType,
				MoviesDir:       filepath.Join(gc().PhysicalSourcePath, "movies"),
				StateDir:        GetStateDir(),
				LogsDir:         logsDir,
				ProwlarrCfg:     gc().Prowlarr,
				Language:        gc().Language,
				QualityScoring:  gc().QualityScoringConfig,
				InvalidatePath:  invalidateSyncRemovedPath,
				DB:              stateDB,
				AudioRegistry:   audioOwnershipRegistry(),
				Release:         releaseTorrent,
				AfterRun: func(ctx context.Context) {
					if oc := gc().OrphanCollection; oc.Enabled && libraryRef.Load() != nil {
						libraryRef.Load().AutoCollect(ctx, oc.MinAgeDays, oc.MaxPerRun)
					}
				},
			}),
			"tv": engines.NewTVSyncer(engines.TVSyncerConfig{
				GoStormURL:      gc().GoStormBaseURL,
				Torrents:        engines.EngineBackend(eng),
				TMDBAPIKey:      gc().TMDBAPIKey,
				TorrentioURL:    gc().TorrentioURL,
				PlexURL:         gc().Plex.URL,
				PlexToken:       gc().Plex.Token,
				PlexTVLib:       gc().Plex.TVLibraryID,
				MediaServerType: gc().MediaServerType,
				TVDir:           filepath.Join(gc().PhysicalSourcePath, "tv"),
				StateDir:        GetStateDir(),
				LogsDir:         logsDir,
				ProwlarrCfg:     gc().Prowlarr,
				Language:        gc().Language,
				QualityScoring:  gc().QualityScoringConfig,
				DB:              stateDB,
				AudioRegistry:   audioOwnershipRegistry(),
				Release:         releaseTorrent,
				InvalidatePath:  invalidateSyncRemovedPath,
			}),
			"music": engines.NewMusicSyncEngine(engines.MusicSyncConfig{
				PlexURL:      gc().Plex.URL,
				PlexToken:    gc().Plex.Token,
				PlexMusicLib: strconv.Itoa(gc().Plex.MusicLibraryID),
				LibraryURL:   fmt.Sprintf("http://127.0.0.1:%d", gc().MetricsPort),
				LibraryToken: authSvc.InternalToken(),
				StateDir:     GetStateDir(),
				LogsDir:      logsDir,
				ProwlarrCfg:  gc().Prowlarr,
				Discovery:    gc().MusicDiscovery,
			}),
			"watchlist": engines.NewWatchlistSyncer(engines.WatchlistSyncerConfig{
				GoStormURL:      gc().GoStormBaseURL,
				Torrents:        engines.EngineBackend(eng),
				DB:              stateDB,
				AudioRegistry:   audioOwnershipRegistry(),
				Release:         releaseTorrent,
				TMDBAPIKey:      gc().TMDBAPIKey,
				TorrentioURL:    gc().TorrentioURL,
				PlexURL:         gc().Plex.URL,
				PlexToken:       gc().Plex.Token,
				PlexSection:     gc().Plex.LibraryID,
				MoviesDir:       filepath.Join(gc().PhysicalSourcePath, "movies"),
				MediaServerType: gc().MediaServerType,
				LogsDir:         logsDir,
				ProwlarrCfg:     gc().Prowlarr,
				QualityScoring:  gc().QualityScoringConfig,
				Language:        gc().Language,
			}),
		}

		sched = scheduler.New(schedCfg, syncers, statePath)
		sched.SetAfterJob(friendlib.AfterSync)
		sched.SetHold(eng.Friends().LibraryMaster)

		http.HandleFunc("/api/scheduler/status", sched.ServeStatus)
		http.HandleFunc("/api/scheduler/", sched.ServeAction)

		if gc().Scheduler.Enabled {
			safeGo(func() {
				sched.Run(backgroundStopChan)
			})
			logger.Printf("[Scheduler] enabled (Go native)")
		} else {
			logger.Printf("[Scheduler] auto-run disabled, manual API available")
		}
	}

	// Library API (external clients)
	// Same work the sync engines do for one title, without the discovery: register the
	// torrent, wait for its file list, write the stub. It exists so an agent that has
	// no access to the filesystem can still file a title into the library.
	{
		var registry library.EpisodeRegistry
		if stateDB != nil {
			registry = stateDB
		}
		libMgr := library.New(library.Config{
			MoviesDir:  filepath.Join(gc().PhysicalSourcePath, "movies"),
			TVDir:      filepath.Join(gc().PhysicalSourcePath, "tv"),
			GoStormURL: gc().GoStormBaseURL,
			GoStorm:    engines.NewGoStormClient(engines.EngineBackend(eng), gc().GoStormBaseURL),
			ReadRange:  nativeBridge.ReadRange,
			Registry:   registry,
			// Without this the drop path sees only movie and tv stubs, and removing
			// the last one for a shared hash takes the torrent out from under any
			// audio projection still backed by it.
			//
			// audioOwnershipRegistry(), not a raw stateDB value: when the state migration
			// failed the raw value is nil and this path would permit exactly that drop.
			AudioRegistry: audioOwnershipRegistry(),
			// The audio sections live beside movies/ and tv/, and the projection
			// registry is authoritative for what is published under them.
			AudioRoot:        gc().PhysicalSourcePath,
			AudioProjections: audioProjectionRegistry(),
			// Without this a projection stays invisible to Readdir and Lookup
			// until the next startup reconciliation.
			PublishAudioPath: publishAudioProjectionsLive,
			// Removal reaches the same object through the narrower interface it
			// actually calls.
			AudioRemoval: audioRemovalRegistry(),
			// Without this a removed projection keeps resolving until its stub is
			// gone and the next reconciliation runs.
			UnpublishAudioPath: unpublishAudioProjectionLive,
			// The audio reaper skips albums with an open session: the swarm counter is
			// only acquitted when that session closes.
			ActiveSession: ttffActiveByHash,
			// The library removes through the engine API, which leaves the SSD warmup files.
			OnRelease: func(hash string) {
				if warmup.DiskWarmup != nil {
					warmup.DiskWarmup.RemoveHash(hash)
				}
			},
			InvalidatePath: invalidateSyncRemovedPath,
			Diagnostics:    diagnosticsDB(),
			RowFiles:       engines.RowFiles(eng),
			LibraryMaster:  eng.Friends().LibraryMaster,
			// Read-only view of the holes the reaper left, for a client that can decide
			// what to do about them.
			Gaps: func() ([]library.Gap, error) {
				if stateDB == nil {
					return nil, nil
				}
				rows, err := stateDB.EpisodeGaps()
				if err != nil {
					return nil, err
				}
				out := make([]library.Gap, 0, len(rows))
				for _, g := range rows {
					// Same spelling the sync logs use, so a client and the log agree on
					// what the show is called.
					show := engines.ShowNameFromEpisodePath(g.FilePath)
					out = append(out, library.Gap{
						EpisodeKey: g.EpisodeKey, Show: show, Season: g.Season,
						ShowIMDB: g.ShowIMDB, Path: g.FilePath,
						DeadHash: g.DeadHash, RemovedAt: g.RemovedAt,
						LastAttempt: g.LastAttempt,
					})
				}
				return out, nil
			},
			// Same record the FUSE unlink handler writes: without it the sync engines
			// add the title back on their next run.
			Blacklist: func(path, hash string) error {
				if globalTorrentRemover == nil {
					return fmt.Errorf("the torrent remover is not running")
				}
				if len(hash) != 40 {
					return fmt.Errorf("malformed info hash %q", hash)
				}
				return globalTorrentRemover.AddToBlacklist(hash, globalTorrentRemover.DeriveTitleFromPath(path))
			},
			Logger:       logger,
			MediaServer:  mediaserver.New(gc().MediaServerType, gc().Plex.URL, gc().Plex.Token),
			MovieSection: gc().Plex.LibraryID,
			TVSection:    gc().Plex.TVLibraryID,
			MusicSection: gc().Plex.MusicLibraryID,
		})
		libraryRef.Store(libMgr)
		libMgr.GateUnlinks()
		friendlib.Wire(friendlib.Config{DB: stateDB, Friends: eng.Friends(), Library: libMgr, Root: gc().PhysicalSourcePath,
			Registry: registry, RegisterInfo: eng.RegisterInfo, InfoBytes: eng.InfoBytes, StopHeld: sched.StopHeld, WaitHeld: sched.WaitHeld, Logf: logger.Printf})
		if globalTorrentRemover != nil {
			globalTorrentRemover.SetRelease(libMgr.ReleaseIfUnused)
		}
		libHandler := library.NewHandler(libMgr)
		http.HandleFunc("/api/library/add", libHandler.Add)
		http.HandleFunc("/api/library/remove", libHandler.Remove)
		http.HandleFunc("/api/library/list", libHandler.List)
		http.HandleFunc("/api/library/inspect", libHandler.Inspect)
		http.HandleFunc("/api/library/diagnostics", libHandler.Diagnostics)
		http.HandleFunc("/api/library/orphans", libHandler.Orphans)
		http.HandleFunc("/api/library/release", libHandler.Release)
		mcp.Register(http.DefaultServeMux, mcp.Deps{
			Library:  libMgr,
			Config:   gc,
			StateDir: GetStateDir,
			Version:  AppVersion,
			Logger:   logger,

			InternalToken: authSvc.InternalToken(),
			SignInOn:      authSvc.Enabled,
		})
	}

	// Health Monitor + Dashboard (Fase 5)
	logsDir := gc().LogDir
	monCollector := collector.New(
		eng,
		gc().FuseMountPath,
		physicalSourcePath,
		gc().NatPMP.VPNInterface,
		gc().Plex.URL,
		gc().Plex.Token,
		gc().NatPMP.LocalPort,
		gc().MetricsPort,
		logsDir,
	)
	dashHandler := dashboard.New(monCollector, logsDir)
	http.HandleFunc("/dashboard", dashHandler.Dashboard)
	http.HandleFunc("/api/health", dashHandler.Health)
	http.HandleFunc("/api/torrents", dashHandler.Torrents)
	http.HandleFunc("/api/speed-history", dashHandler.SpeedHistory)
	http.HandleFunc("/api/shield-events", dashHandler.ShieldEvents)
	http.HandleFunc("/api/logs", dashHandler.Logs)
	http.HandleFunc("/api/plex-thumb", dashHandler.PlexThumb)
	http.HandleFunc("/api/kill-stream/", dashHandler.KillStream)
	clientSvc := clients.New(func() clients.Config {
		return clients.Config{
			ServerType: gc().MediaServerType,
			URL:        gc().Plex.URL,
			Token:      gc().Plex.Token,
			Active: func() (playing bool) {
				activeHandles.Range(func(_, _ interface{}) bool { playing = true; return false })
				return playing
			},
			// Only files open right now: playbackRegistry keeps entries for minutes after close.
			Resolve: func(base string) (hash string) {
				open := false
				activeHandles.Range(func(key, _ interface{}) bool {
					if filepath.Base(key.(*MkvHandle).path) == base {
						open = true
						return false
					}
					return true
				})
				if !open {
					return ""
				}
				playbackRegistry.Range(func(key, value interface{}) bool {
					if filepath.Base(key.(string)) == base {
						hash = value.(*PlaybackState).Hash
						return false
					}
					return true
				})
				return hash
			},
		}
	})
	http.HandleFunc("/api/clients", clientSvc.Handler)
	http.HandleFunc("/api/scan", clientSvc.ScanHandler)
	monCollector.SetAuthToken(authSvc.InternalToken())
	safeGo(func() {
		monCollector.Run(backgroundStopChan)
	})
	logger.Printf("[Dashboard] enabled at :%d/dashboard", gc().MetricsPort)

	go func() {
		if err := tlsSrv.ListenAndServe(fmt.Sprintf(":%d", gc().MetricsPort), auth.Wrap(authSvc, authTrusted, http.DefaultServeMux)); err != nil {
			logger.Printf("[Dashboard] ERROR :%d stopped: %v", gc().MetricsPort, err)
		}
	}()

	// Graceful shutdown: saves inode map and sync caches before exit.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	var server *fuse.Server // Declare here to be accessible in goroutine
	var err error           // Declare err here too
	go func() {
		sig := <-sigChan
		logger.Printf("Received signal %v, initiating graceful shutdown...", sig)

		// Save inode map before exit
		if globalInodeMap != nil {
			if globalInodeMap.IsDirty() {
				if err := globalInodeMap.SaveToDisk(); err != nil {
					logger.Printf("InodeMap: Shutdown save FAILED: %v", err)
				} else {
					files, dirs, _, _ := GetInodeMapStats()
					logger.Printf("InodeMap: Shutdown save complete (%d files, %d dirs)", files, dirs)
				}
			}
			ShutdownGlobalInodeMap()
		}

		// Clean up sync caches
		if globalSyncCacheManager != nil {
			if err := globalSyncCacheManager.SyncToDisk(); err != nil {
				logger.Printf("SyncCache: Shutdown save FAILED: %v", err)
			}
		}

		// CRITICAL FIX: Stop all background managers explicitly before os.Exit()
		// because defer statements are bypassed by os.Exit().
		backgroundStopOnce.Do(func() { close(backgroundStopChan) })
		friendlib.Shutdown()

		if globalRateLimiter != nil {
			globalRateLimiter.Stop()
		}
		if globalLockManager != nil {
			globalLockManager.Stop()
		}
		if globalCleanupManager != nil {
			globalCleanupManager.Stop()
		}

		// Try to unmount gracefully
		if server != nil {
			server.Unmount()
		}

		if restartRequested.Load() {
			logger.Printf("Graceful shutdown complete, exiting with %d to request a restart...", restartExitCode)
			os.Exit(restartExitCode)
		}
		logger.Println("Graceful shutdown complete, exiting...")
		os.Exit(0)
	}()

	// Crea root node virtuale
	rootData := &VirtualMkvRoot{sourcePath: source}

	// Enable attribute caching from config
	attrTimeout := time.Duration(gc().AttrTimeoutSeconds * float64(time.Second))
	entryTimeout := time.Duration(gc().EntryTimeoutSeconds * float64(time.Second))
	negativeTimeout := time.Duration(gc().NegativeTimeoutSeconds * float64(time.Second))

	// go-fuse checks out ~MaxWrite per in-flight request; 0 leaves that unbounded.
	// Past the cap it stops reading /dev/fuse, so this trades RAM for latency.
	maxInflight := int(gc().FuseMaxInflightMB * 1024 * 1024)

	server, err = fs.Mount(mount, rootData, &fs.Options{
		AttrTimeout: &attrTimeout, EntryTimeout: &entryTimeout,
		NegativeTimeout: &negativeTimeout,
		MountOptions: fuse.MountOptions{
			MaxInflightRequestBytes: maxInflight,
			AllowOther:              true,
			MaxBackground:           fuseMaxBackground(),
			// Reads are served from memory, where splice only adds a copy through a pipe; with 1MB
			// readahead every reply also overflowed the 1MB pipe and logged "trySplice" per read.
			DisableSplice: true,
			// MaxWrite:                 1024 * 1024,
			MaxWrite: 4 * 1024 * 1024, // Samba Turbo: 4MB write buffer
			// MaxReadAhead:             1024 * 1024,
			MaxReadAhead:             4 * 1024 * 1024, // Samba Turbo: 4MB read-ahead
			RememberInodes:           true,            // ENABLED: safe with explicit cache control
			ExplicitDataCacheControl: true,            // PREVENTS kernel freezes during invalidation
			SyncRead:                 false,           // ENABLED ASYNC READS for 4K performance
			// NFS Export: Stable filesystem identification
			FsName: "tiramisu",
		},
		UID: gc().UID, // Default file ownership: pi user (1000)
		GID: gc().GID, // Default file ownership: pi group (1000)
	})
	if err != nil {
		log.Fatal(err)
	}

	logger.Printf("FUSE mounted at %s with VirtualMkvRoot, all systems active", mount)
	kernelRoot.Store(rootData.EmbeddedInode())
	vfs.SetMountReadAhead(mount, gc().FuseReadAheadKB)

	go watchdog.Smbd(logger.Printf, softInterruptPumps, func() {
		restartRequested.Store(true)
		syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	})
	go ttffCleanupLoop()

	server.Wait()
}

// softInterruptPumps closes every pump's pipe reader to unblock hung FUSE reads; the pumps
// stay alive and retry on the next read.
func softInterruptPumps() {
	activePumps.Range(func(key, value interface{}) bool {
		if ps, ok := value.(*NativePumpState); ok {
			if ps.reader != nil {
				logger.Printf("[Watchdog] Soft-interrupting pump: %s", filepath.Base(ps.path))
				ps.reader.Interrupt()
			}
		}
		return true
	})
}

func startHandleGC() {
	ticker := time.NewTicker(15 * time.Minute)
	safeGo(func() {
		for range ticker.C {
			count := 0
			activeHandles.Range(func(key, value interface{}) bool {
				h := key.(*MkvHandle)
				h.mu.Lock()
				idle := time.Since(h.lastActivityTime)
				path := h.path
				h.mu.Unlock()

				if idle > 1*time.Hour {
					logger.Printf("[V239] Force closing orphan handle (idle 1h): %s", path)
					// We simulate a FUSE Release to cleanup all resources
					h.Release(context.Background())
					count++
				}
				return true
			})
			if count > 0 {
				logger.Printf("[V239] Handle GC: cleaned %d orphan handles", count)
			}
		}
	})
}

// blockLists keeps the BitTorrent IP blocklist file fresh and loaded into the engine.
var blockLists = &blocklist.Loop{
	Dir:         func() string { return engineStateDir },
	URL:         func() string { return gc().BlockListURL },
	CountRanges: engine.CountRangesFile,
	Install: func() error {
		list, err := eng.ReadBlockedIP()
		if err == nil {
			eng.SetIPBlocklist(list)
		}
		return err
	},
	Logf: logger.Printf,
	Done: backgroundStopChan,
	Go:   safeGo,
}

// GetStateDir returns the centralized state directory path.
func GetStateDir() string {
	if gc().RootPath == "" {
		return "/home/pi/STATE"
	}
	return filepath.Join(gc().RootPath, "STATE")
}

// --- InodeMap globals & wrappers ---
