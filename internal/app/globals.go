package app

import (
	"log"
	_ "net/http/pprof"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"tiramisu/internal/autoremove"
	"tiramisu/internal/bridge"
	"tiramisu/internal/cache"
	"tiramisu/internal/config"
	"tiramisu/internal/engine"
	"tiramisu/internal/enginehost"
	"tiramisu/internal/lockmgr"
	"tiramisu/internal/metadb"
	"tiramisu/internal/opentracker"
	"tiramisu/internal/preload"
	"tiramisu/internal/prowlarr"
	"tiramisu/internal/ratelimit"
	"tiramisu/internal/readstats"
	"tiramisu/internal/stallnotice"
	syncercache "tiramisu/internal/syncer/cache"
	"tiramisu/internal/vfs"
)

var logger = log.New(os.Stdout, "[GoProxy] ", log.LstdFlags)

// masterDataSemaphore limits concurrent data operations (Native, HTTP, Prefetch).
var masterDataSemaphore chan struct{}

var startTime = time.Now()
var metaCache *cache.LRUCache
var raCache = newRaCache()
var stallNotices = stallnotice.New(stallnotice.Deps{}) // off until Boot: tests and the simulator

// globalProfilingStats counts FUSE reads for /metrics/profiling.
var globalProfilingStats = &readstats.Stats{}

var globalRateLimiter *ratelimit.RateLimiter
var globalLockManager *lockmgr.LockManager
var globalDirCache *vfs.DirCache

// Global peer-based preloader (FASE 2 - Performance)
var peerPreloader *preload.PeerPreloader
var nativeBridge *bridge.NativeClient

// eng is the in-process engine; nil in tests and simulators (its lookups are nil-safe).
var eng *engine.Engine
var engineHost *enginehost.Host

// engineStateDir holds config.db, settings.json, trackers.txt and the blocklist (--path).
var engineStateDir string

var globalCleanupManager *CleanupManager
var globalTorrentRemover *autoremove.Remover
var globalConfig atomic.Pointer[config.Config]

func gc() *config.Config { return globalConfig.Load() }

// configBoot holds the state DB and the configuration store opened at start-up.
var configBoot *config.Boot

// Global Prowlarr client for indexer queries (nil when disabled).
var prowlarrClient *prowlarr.Client

func GetEffectiveConcurrencyLimit() int {
	return gc().MasterConcurrencyLimit
}

// tailFillTarget carries what EnsureTail needs, resolved at Open and read on media.stop.
type tailFillTarget struct {
	hash   string
	fileID int
	size   int64
}

var tailFillTargets sync.Map // path -> tailFillTarget

// Global sync cache manager (FASE 4.13 - Sync Script Caches)
var globalSyncCacheManager *syncercache.SyncCacheManager

// StateDB is the optional SQLite backend for persistent state.
var stateDB *metadb.DB

var physicalSourcePath string
var virtualMountPath string

var backgroundStopChan = make(chan struct{})
var backgroundStopOnce sync.Once

// restartExitCode tells a supervisor that this exit was a restart request from
// the Control Panel, not a crash. The Docker entrypoint restarts only on this
// code; systemd restarts on any. Keep in sync with docker-entrypoint.sh.
const restartExitCode = 75

var restartRequested atomic.Bool

// readBufferPool size matches Config.ReadAheadBase (set in main).
var readBufferPool *sync.Pool

var activeHandles sync.Map      // key: *MkvHandle, value: bool
var inFlightPrefetches sync.Map // key: "path:offset", value: bool

// inFlightFetches dedups the sync FetchBlock fallback: concurrent misses in the same chunk
// share one fetch instead of each running FetchBlock independently.
var inFlightFetches sync.Map // key: "path:chunkAlignedOffset", value: *fetchFlight
var fetchFlightDedupCount atomic.Int64

// fuseShortReadCount counts reads served to FUSE with fewer bytes than requested, away from EOF.
// FUSE runs on the kernel page cache here (no direct_io, see the mount options in main()), so the
// kernel zero-fills the rest of the page and marks it uptodate: that region then serves zeros
// silently. A short read is therefore data corruption, not a slow read, and must stay visible.
var fuseShortReadCount atomic.Int64

// Outcome of the refill in Read: repaired = the gap was filled and the read served whole,
// failed = it could not be, and EIO was returned rather than caching a zero-filled page.
var fuseShortReadRepaired atomic.Int64
var fuseShortReadFailed atomic.Int64

// A blocked FUSE read puts smbd in D-state, which is why fetch timeouts were cut to 8s in
// V283; the refill must stay well inside that budget rather than stacking another 8s on top.
const (
	shortReadRefillAttempts = 2
	shortReadRefillBudget   = 4 * time.Second
)

type fetchFlight struct {
	done  chan struct{} // closed by the leader once its result is in raCache
	start int64         // leader's read offset: its window never covers bytes below it
}

var activePumps sync.Map    // Map[string]*NativePumpState — one pump per file path
var pumpTimers sync.Map     // key: path, value: *time.Timer
var priorityTimers sync.Map // key: path, value: *time.Timer

// OpenTracker: contatori O(1) per handle aperti (per hash e per path).
// Permette query rapide da cleanup e priority timer senza scansionare activeHandles.
var globalOpenTracker = opentracker.New()

// Serializes concurrent pump creation for the same file.
var pumpCreationMu sync.Mutex
