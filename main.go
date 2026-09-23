package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/cespare/xxhash/v2"
	"golang.org/x/sys/cpu"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"tiramisu/internal/cache"
	"tiramisu/internal/catalog"
	"tiramisu/internal/catalog/mediaserver"
	"tiramisu/internal/config"
	server "tiramisu/internal/gostorm"
	"tiramisu/internal/gostorm/native"
	"tiramisu/internal/gostorm/settings"
	"tiramisu/internal/gostorm/torr"
	torrstor "tiramisu/internal/gostorm/torr/storage/torrstor"
	torrutils "tiramisu/internal/gostorm/torr/utils"
	tsutils "tiramisu/internal/gostorm/utils"
	"tiramisu/internal/gostorm/web"
	"tiramisu/internal/library"
	"tiramisu/internal/lockmgr"
	"tiramisu/internal/metadb"
	"tiramisu/internal/monitor/collector"
	"tiramisu/internal/monitor/dashboard"
	"tiramisu/internal/natpmp"
	"tiramisu/internal/opentracker"
	"tiramisu/internal/preload"
	"tiramisu/internal/prowlarr"
	"tiramisu/internal/ratelimit"
	"tiramisu/internal/registry"
	syncercache "tiramisu/internal/syncer/cache"
	"tiramisu/internal/syncer/engines"
	"tiramisu/internal/syncer/scheduler"
	"tiramisu/internal/telemetry"
	"tiramisu/internal/tuner"
	"tiramisu/internal/updater"
	"tiramisu/internal/vfs"
	"tiramisu/internal/warmup"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// --- CONFIGURAZIONE (V71-Zero-Latency) ---
// Note: Configuration is now handled entirely by config.go via globalConfig
// Constants removed to ensure single source of truth

var logger = log.New(os.Stdout, "[GoProxy] ", log.LstdFlags)

// masterDataSemaphore limits concurrent data operations (Native, HTTP, Prefetch).
var masterDataSemaphore chan struct{}

var startTime = time.Now()
var metaCache *cache.LRUCache
var raCache = newReadAheadCache()

var globalRateLimiter *ratelimit.RateLimiter
var globalLockManager *lockmgr.LockManager
var globalDirCache *vfs.DirCache

// Global peer-based preloader (FASE 2 - Performance)
var peerPreloader *preload.PeerPreloader
var nativeBridge *native.NativeClient

var globalCleanupManager *CleanupManager
var globalTorrentRemover *TorrentRemover
var globalConfig atomic.Pointer[config.Config]

func gc() *config.Config { return globalConfig.Load() }

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

// PlaybackState traccia lo stato di una sessione di visione reale
type PlaybackState struct {
	mu     sync.RWMutex
	Path   string
	Hash   string // InfoHash for GoStorm priority management
	ImdbID string // IMDB ID from MKV line 4, used for webhook matching
	// ExternalID/ExternalIDNamespace carry the audio projection's registered
	// identity ("musicbrainz", "asin", ...) so a music webhook can match the
	// session the same way a movie webhook matches on ImdbID.
	ExternalID          string
	ExternalIDNamespace string
	OpenedAt            time.Time
	ConfirmedAt         time.Time // Set when Plex webhook arrives
	IsHealthy           bool      // Confirmed by Plex
	IsStopped           bool      // Set on explicit media.stop webhook
	// V750: Inferred playback detection (self-healing when webhook is lost)
	ReadCount   int64
	LastSeekOff int64
	LastReadAt  time.Time
}

func (ps *PlaybackState) SetHealthy(healthy bool) {
	ps.mu.Lock()
	ps.IsHealthy = healthy
	ps.ConfirmedAt = time.Now()
	ps.mu.Unlock()
	// V750: Persist to SQLite
	savePlaybackStateToDB(ps)
}

// V750: Determine if this path shows evidence of active playback,
// even without a webhook confirmation.
func (ps *PlaybackState) IsInferredPlayback() bool {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	if ps.IsHealthy || ps.IsStopped {
		return false
	}
	now := time.Now()
	active := !ps.LastReadAt.IsZero() && now.Sub(ps.LastReadAt) < 10*time.Minute
	significantReads := ps.ReadCount >= 3
	streaming := ps.LastSeekOff > 2*1024*1024 // >2MB offset = real playback, not scanner
	return active && significantReads && streaming
}

func (ps *PlaybackState) GetOpenedAt() time.Time {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.OpenedAt
}

func (ps *PlaybackState) GetImdbID() string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.ImdbID
}

// GetExternalIdentity returns the audio projection identity and its namespace.
func (ps *PlaybackState) GetExternalIdentity() (string, string) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.ExternalID, ps.ExternalIDNamespace
}

// LastSignOfLife is the later of the open and the webhook confirmation.
func (ps *PlaybackState) LastSignOfLife() time.Time {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	if ps.ConfirmedAt.After(ps.OpenedAt) {
		return ps.ConfirmedAt
	}
	return ps.OpenedAt
}

func (ps *PlaybackState) GetStatus() bool {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.IsHealthy
}

var playbackRegistry sync.Map // path -> *PlaybackState

// Global sync cache manager (FASE 4.13 - Sync Script Caches)
var globalSyncCacheManager *syncercache.SyncCacheManager

// StateDB is the optional SQLite backend for persistent state.
var stateDB *metadb.DB

var physicalSourcePath string
var virtualMountPath string

var backgroundStopChan = make(chan struct{})
var backgroundStopOnce sync.Once

var (
	blockListMu   sync.Mutex
	blockListStop chan struct{}
)

// restartExitCode tells a supervisor that this exit was a restart request from
// the Control Panel, not a crash. The Docker entrypoint restarts only on this
// code; systemd restarts on any. Keep in sync with docker-entrypoint.sh.
const restartExitCode = 75

var restartRequested atomic.Bool

// readBufferPool size matches Config.ReadAheadBase (set in main).
var readBufferPool *sync.Pool

// reImdbID matches "imdb://tt1234567" in the Guid array of Plex webhook payloads.
var reImdbID = regexp.MustCompile(`"imdb://(tt\d+)"`)

// webhookMaxBody bounds a webhook post. Plex sends multipart with a thumbnail, so
// the ceiling has to clear a poster, not just the JSON.
const webhookMaxBody = 10 * 1024 * 1024

// reMbid matches "mbid://<uuid>" in Plex webhook payloads for music. A track
// payload can carry several: the recording, its release group and the artist.
var reMbid = regexp.MustCompile(`"mbid://([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})"`)

// reUUID matches a bare MusicBrainz uuid, which is how Jellyfin's ProviderIds
// carry the same identities.
var reUUID = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// webhookIdentityKeys is the identity set a music payload carries, ranked by how
// specific each id is: the item's own recording beats its release group, which
// beats the artist. The ranking is what keeps concurrent sessions of the same
// artist apart when only a coarser id is shared between them.
type webhookIdentityKeys struct {
	recording []string
	release   []string
	artist    []string
}

func (k webhookIdentityKeys) empty() bool {
	return len(k.recording) == 0 && len(k.release) == 0 && len(k.artist) == 0
}

// rank reports the most specific group id belongs to: 2 recording, 1 release,
// 0 artist. An unclassified id is deliberately not a match here; the caller puts
// those in the artist group, where they only win when nothing better matches.
func (k webhookIdentityKeys) rank(id string) (int, bool) {
	for rank, group := range [][]string{k.recording, k.release, k.artist} {
		for _, want := range group {
			if strings.EqualFold(want, id) {
				return 2 - rank, true
			}
		}
	}
	return 0, false
}

// webhookIdentityKeysFromPayload reads the mbid of the item, its parent and its
// grandparent from the payload's JSON shape, because a flat scan cannot tell an
// artist id shared by a whole discography from a recording id unique to one track.
// Ids from fields this does not model stay matchable at the lowest rank.
func webhookIdentityKeysFromPayload(payloadStr string) webhookIdentityKeys {
	keys := webhookIdentityKeys{}
	var raw struct {
		Metadata map[string]interface{} `json:"Metadata"`
	}
	if err := json.Unmarshal([]byte(payloadStr), &raw); err == nil {
		meta := raw.Metadata
		keys.recording = collectMbids(meta["guid"], meta["Guid"])
		keys.release = collectMbids(meta["parentGuid"], meta["ParentGuid"])
		keys.artist = collectMbids(meta["grandparentGuid"], meta["GrandparentGuid"])
		// Jellyfin has no mbid:// scheme: the same identities arrive as bare
		// uuids under ProviderIds.
		if pids, ok := meta["ProviderIds"].(map[string]interface{}); ok {
			keys.recording = append(keys.recording, collectUUIDs(pids["MusicBrainzTrack"])...)
			keys.release = append(keys.release, collectUUIDs(
				pids["MusicBrainzReleaseGroup"], pids["MusicBrainzAlbum"])...)
			keys.artist = append(keys.artist, collectUUIDs(pids["MusicBrainzArtist"])...)
		}
	}
	// Fallback for a payload shape this does not model: keep every mbid matchable,
	// deduplicated into the lowest rank.
	for _, id := range flatMbidIDs(payloadStr) {
		if _, ok := keys.rank(id); !ok {
			keys.artist = append(keys.artist, id)
		}
	}
	keys.recording = dedupeIDs(keys.recording)
	keys.release = dedupeIDs(keys.release)
	keys.artist = dedupeIDs(keys.artist)
	return keys
}

// collectMbids extracts "mbid://<uuid>" values from any JSON shape a metadata
// field takes (a string or an array of objects with an "id").
func collectMbids(values ...interface{}) []string {
	var ids []string
	for _, value := range values {
		walkJSONStrings(value, func(s string) {
			for _, m := range reMbid.FindAllStringSubmatch(`"`+s+`"`, -1) {
				ids = append(ids, strings.ToLower(m[1]))
			}
		})
	}
	return ids
}

// collectUUIDs extracts bare MusicBrainz uuids from any JSON shape.
func collectUUIDs(values ...interface{}) []string {
	var ids []string
	for _, value := range values {
		walkJSONStrings(value, func(s string) {
			for _, m := range reUUID.FindAllString(s, -1) {
				ids = append(ids, strings.ToLower(m))
			}
		})
	}
	return ids
}

// walkJSONStrings visits every string inside a decoded JSON value.
func walkJSONStrings(value interface{}, visit func(string)) {
	switch t := value.(type) {
	case string:
		visit(t)
	case []interface{}:
		for _, item := range t {
			walkJSONStrings(item, visit)
		}
	case map[string]interface{}:
		for _, item := range t {
			walkJSONStrings(item, visit)
		}
	}
}

// flatMbidIDs is the shape-blind scan, kept as a fallback for payload variants the
// structural reader does not know.
func flatMbidIDs(payloadStr string) []string {
	matches := reMbid.FindAllStringSubmatch(payloadStr, -1)
	if len(matches) == 0 {
		return nil
	}
	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		ids = append(ids, strings.ToLower(m[1]))
	}
	return dedupeIDs(ids)
}

func dedupeIDs(ids []string) []string {
	if len(ids) < 2 {
		return ids
	}
	seen := make(map[string]bool, len(ids))
	out := ids[:0]
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

var reEmptyNumber = regexp.MustCompile(`"(\w+)":\s*,`)

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
	done chan struct{} // closed by the leader once its result is in raCache
}

var activePumps sync.Map    // Map[string]*NativePumpState — one pump per file path
var pumpTimers sync.Map     // key: path, value: *time.Timer
var priorityTimers sync.Map // key: path, value: *time.Timer

// OpenTracker: contatori O(1) per handle aperti (per hash e per path).
// Permette query rapide da cleanup e priority timer senza scansionare activeHandles.
var globalOpenTracker = opentracker.New()

// Serializes concurrent pump creation for the same file.
var pumpCreationMu sync.Mutex

// NativePumpState tracks a shared pump across multiple handles for the same file.
type NativePumpState struct {
	cancel           context.CancelFunc
	reader           *native.NativeReader
	path             string
	refCount         int32
	playerOff        int64       // last known player position, saved on handle release
	interruptPending atomic.Bool // prevents cascade: only the first handle per seek fires Interrupt()
}

// attachToExistingPump links the handle to a running pump exactly once. The caller holds
// pumpCreationMu, which serializes attaches on this path, and the hasSlot re-check under
// h.mu keeps the state assignment and the refCount increment together: two reads racing on
// the same handle would otherwise both attach, and Release could never bring refCount back
// to zero, pinning the pump and its slot until the reader idle timeout.
func (h *MkvHandle) attachToExistingPump(ps *NativePumpState) bool {
	h.mu.Lock()
	if h.hasSlot.Load() {
		h.mu.Unlock()
		return false
	}
	h.hasSlot.Store(true)
	h.pumpState = ps
	h.isWatching = true
	h.nativeReader = ps.reader
	h.pumpCancel = ps.cancel
	h.mu.Unlock()
	atomic.AddInt32(&ps.refCount, 1)
	globalOpenTracker.Inc(h.hash, h.path)
	return true
}

// terminateOrphanPump cancels and forgets any pump registered for path. The cleanup sweep
// uses it when pruning a zombie registry entry: without it the pump keeps its slot and keeps
// fetching until the reader idle timeout (V262, up to 2h with an open handle).
func terminateOrphanPump(path string) bool {
	val, ok := activePumps.Load(path)
	if !ok {
		return false
	}
	if ps, ok := val.(*NativePumpState); ok && ps.cancel != nil {
		ps.cancel()
	}
	activePumps.Delete(path)
	return true
}

// resolveTargetFile finds the torrent hash and file index for a given URL and size.
func resolveTargetFile(url string, targetSize int64, physicalPath string) (string, int, error) {
	if nativeBridge == nil {
		return "", 0, fmt.Errorf("nativeBridge is nil")
	}
	if !strings.Contains(url, "link=") {
		return "", 0, fmt.Errorf("file not found in torrent")
	}

	hashStr := urlQueryValue(url, "link=")
	urlIndex := 0
	if raw := urlQueryValue(url, "index="); raw != "" {
		if idx, err := strconv.Atoi(raw); err == nil {
			urlIndex = idx
		}
	}

	section, _ := library.SectionForPath(physicalSourcePath, physicalPath)

	// The engine's list is shared state, so it is copied before sorting. IDs are
	// assigned from the sorted order, the same way GoStorm assigns them. Audio never
	// reads this list - its identity comes from the registry - so a music scan does
	// not pay for a copy and sort of the whole resident file list on every Open.
	var files []library.TorrentFile
	if !library.IsAudioSection(section) {
		if t := web.BTS.GetTorrent(metainfo.NewHashFromHex(hashStr)); t != nil {
			resident := append([]*torrent.File(nil), t.Files()...)
			sort.Slice(resident, func(i, j int) bool {
				return tsutils.CompareStrings(resident[i].Path(), resident[j].Path())
			})
			files = make([]library.TorrentFile, 0, len(resident))
			for i, f := range resident {
				files = append(files, library.TorrentFile{Index: i + 1, Path: f.Path(), Length: f.Length()})
			}
		}
	}

	target, err := library.ResolveOpenTarget(section, hashStr, urlIndex, targetSize, physicalPath, files)
	if err != nil {
		return "", 0, err
	}
	return target.Hash, target.FileIndex, nil
}

// urlQueryValue reads a bare query value without parsing the whole URL, which
// this FUSE path does on every Open.
func urlQueryValue(url, key string) string {
	start := strings.Index(url, key)
	if start < 0 {
		return ""
	}
	start += len(key)
	if end := strings.Index(url[start:], "&"); end >= 0 {
		return url[start : start+end]
	}
	return url[start:]
}

// Fast deterministic inode from FNV-1a hash to avoid syscalls in Readdir.
// Uses POSIX bits (syscall.S_IFDIR/S_IFREG) for FUSE/Samba/kernel compatibility.
func hashFilenameToInode(name string) uint64 {
	return xxhash.Sum64String(name)
}

// ReadTiming collects per-read latency metrics for profiling.
type ReadTiming struct {
	StartTime          time.Time
	MetadataLookupTime time.Duration
	HTTPFetchTime      time.Duration
	CacheHitTime       time.Duration
	TotalTime          time.Duration
	BytesRead          int
	IsStreaming        bool
	UsedCache          bool
}

// Global profiling statistics
type ProfilingStats struct {
	mu                 sync.RWMutex
	TotalReads         int64
	CacheHits          int64
	HTTPFetches        int64
	AvgHTTPLatency     time.Duration
	AvgCacheLatency    time.Duration
	AvgMetadataLatency time.Duration
	StreamingReads     int64
	NonStreamingReads  int64
}

var globalProfilingStats = &ProfilingStats{}

// RecordRead updates global profiling statistics
func (ps *ProfilingStats) RecordRead(t *ReadTiming) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	ps.TotalReads++

	if t.UsedCache {
		oldCount := ps.CacheHits
		ps.CacheHits++
		// Update average cache latency: (avg * old + new) / new
		ps.AvgCacheLatency = time.Duration((int64(ps.AvgCacheLatency)*oldCount + int64(t.CacheHitTime)) / ps.CacheHits)
	} else {
		oldCount := ps.HTTPFetches
		ps.HTTPFetches++
		// Update average HTTP latency: (avg * old + new) / new
		ps.AvgHTTPLatency = time.Duration((int64(ps.AvgHTTPLatency)*oldCount + int64(t.HTTPFetchTime)) / ps.HTTPFetches)
	}

	if t.IsStreaming {
		ps.StreamingReads++
	} else {
		ps.NonStreamingReads++
	}
}

// Stats returns current profiling statistics
func (ps *ProfilingStats) Stats() (totalReads, cacheHits, httpFetches, streamingReads int64, avgHTTPLatency, avgCacheLatency time.Duration, cacheHitRate float64) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()

	cacheHitRate = 0.0
	if ps.TotalReads > 0 {
		cacheHitRate = float64(ps.CacheHits) / float64(ps.TotalReads) * 100.0
	}

	return ps.TotalReads, ps.CacheHits, ps.HTTPFetches, ps.StreamingReads,
		ps.AvgHTTPLatency, ps.AvgCacheLatency, cacheHitRate
}

// sanitizeTime ensures a time is never "zero" (1970), falling back to current time
func sanitizeTime(t time.Time) uint64 {
	if t.IsZero() || t.Unix() <= 0 {
		return uint64(time.Now().Unix())
	}
	return uint64(t.Unix())
}

// fillAttrFromStat populates FUSE attributes from a standard syscall.Stat_t.
func fillAttrFromStat(st *syscall.Stat_t, out *fuse.Attr) {
	out.Ino = st.Ino
	out.Nlink = uint32(st.Nlink)
	out.Mode = uint32(st.Mode)
	out.Uid = st.Uid
	out.Gid = st.Gid
	out.Rdev = uint32(st.Rdev)
	// out.Blksize = uint32(st.Blksize) // CRITICAL: Samba uses for buffer sizing
	out.Blksize = uint32(gc().FuseBlockSize) // Configurable block size (default 1MB)
	out.Blocks = uint64(st.Blocks)           // CRITICAL: Samba uses for throughput calc
	out.Size = uint64(st.Size)

	// Use time.Now() as cross-platform baseline for virtualized FUSE attributes.
	now := time.Now()
	out.Mtime = sanitizeTime(now)
	out.Atime = sanitizeTime(now)
	out.Ctime = sanitizeTime(now)
}

// fillAttrFromMetadata populates FUSE attributes from our internal Metadata.
func fillAttrFromMetadata(m *vfs.Metadata, out *fuse.Attr) {
	out.Size = uint64(m.Size)
	out.Mode = syscall.S_IFREG | 0644
	// Audio projections are read-only to their clients (spec 4): the API owns every
	// mutation, so a conventional scanner must never see a writable stub.
	if m.Audio {
		out.Mode = syscall.S_IFREG | 0444
	}
	out.Uid, out.Gid = gc().UID, gc().GID
	out.Nlink = 1
	// out.Blksize = 4096                                 // Standard block size
	out.Blksize = uint32(gc().FuseBlockSize)  // Configurable block size (default 1MB)
	out.Blocks = (uint64(m.Size) + 511) / 512 // Estimate blocks based on size

	ts := sanitizeTime(m.Mtime)
	out.Mtime = ts
	out.Atime = ts
	out.Ctime = ts
}

// isAudioSectionPath reports whether an absolute path lives under a Phase 1 audio
// section root (music/ or audiobooks/). Prefix matching is component-wise, so a
// sibling like "music-videos/" is not an audio section.
func isAudioSectionPath(fullPath string) bool {
	rel, err := filepath.Rel(physicalSourcePath, fullPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 0 || parts[0] == "." {
		return false
	}
	return library.IsAudioSection(library.Section(parts[0]))
}

// audioWriteIntent reports whether open flags ask for anything a read-only
// projection cannot give.
func audioWriteIntent(flags uint32) bool {
	return flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_TRUNC|syscall.O_APPEND) != 0
}

// audioMutationErrno keeps every audio mutation on the deliberate EROFS the spec
// requires; video keeps the untouched ENOSYS it has always answered with.
func audioMutationErrno(fullPath string) syscall.Errno {
	if isAudioSectionPath(fullPath) {
		return syscall.EROFS
	}
	return syscall.ENOSYS
}

// VirtualMkvRoot - nodo radice per file virtuali .mkv
type VirtualMkvRoot struct {
	fs.Inode
	sourcePath string
}

// Compile-time interface checks - verificano che implementiamo correttamente le interfacce
var _ = (fs.InodeEmbedder)((*VirtualMkvRoot)(nil))
var _ = (fs.NodeLookuper)((*VirtualMkvRoot)(nil))
var _ = (fs.NodeReaddirer)((*VirtualMkvRoot)(nil))
var _ = (fs.NodeGetattrer)((*VirtualMkvRoot)(nil))
var _ = (fs.NodeStatfser)((*VirtualMkvRoot)(nil))

func (r *VirtualMkvRoot) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	st := syscall.Stat_t{}
	if err := syscall.Stat(r.sourcePath, &st); err != nil {
		logger.Printf("ROOT GETATTR ERROR: stat failed for %s: %v", r.sourcePath, err)
		return vfs.ToErrno(err)
	}

	fillAttrFromStat(&st, &out.Attr)

	// Root inode must be constant (required by Plex).
	out.Ino = vfs.InodeRoot
	out.Mode = syscall.S_IFDIR | 0755
	out.Size = 4096

	return 0
}

func (r *VirtualMkvRoot) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	fullPath := filepath.Join(r.sourcePath, name)

	if classifyVFSPath(fullPath).Stub {
		meta, err := stubMeta(fullPath)
		if err == nil {
			addMetaToInodeMap(fullPath, meta)
			ino := getFileInodeFromMap(fullPath)
			node := &VirtualMkvNode{vMeta: meta}
			stable := fs.StableAttr{
				Mode: syscall.S_IFREG,
				Ino:  ino,
				Gen:  1,
			}
			child := r.NewInode(ctx, node, stable)
			fillAttrFromMetadata(meta, &out.Attr)
			out.Ino = ino
			return child, 0
		}
	}

	// Fallback for directories or other files
	st := syscall.Stat_t{}
	if err := syscall.Lstat(fullPath, &st); err != nil {
		return nil, syscall.ENOENT
	}

	if (st.Mode & syscall.S_IFMT) == syscall.S_IFDIR {
		node := &VirtualDirNode{physicalPath: fullPath}
		dirIno := getDirInodeFromMap(fullPath)
		stable := fs.StableAttr{
			Mode: syscall.S_IFDIR,
			Ino:  dirIno,
			Gen:  1,
		}
		child := r.NewInode(ctx, node, stable)
		fillAttrFromStat(&st, &out.Attr)
		out.Ino = dirIno
		return child, 0
	}

	node := &fs.LoopbackNode{RootData: &fs.LoopbackRoot{Path: r.sourcePath}}
	stable := fs.StableAttr{Mode: uint32(st.Mode & syscall.S_IFMT)}
	child := r.NewInode(ctx, node, stable)
	return child, 0
}

type nfsDirStream struct {
	entries []fuse.DirEntry
	index   int
}

func (s *nfsDirStream) HasNext() bool {
	return s.index < len(s.entries)
}

func (s *nfsDirStream) Next() (fuse.DirEntry, syscall.Errno) {
	e := s.entries[s.index]
	s.index++
	return e, 0
}

func (s *nfsDirStream) Seekdir(ctx context.Context, off uint64) syscall.Errno {
	if off == 0 {
		s.index = 0
		return 0
	}
	for i, e := range s.entries {
		if e.Off == off {
			s.index = i + 1
			return 0
		}
	}
	return 0
}

func (s *nfsDirStream) Close() {}

func (r *VirtualMkvRoot) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	if entries, found := globalDirCache.Get(r.sourcePath); found {
		return &nfsDirStream{entries: entries}, 0
	}
	// Snapshot the generation before the walk: a concurrent invalidate must not be
	// overwritten by this listing landing after it.
	generation := globalDirCache.Generation(r.sourcePath)

	entries, err := os.ReadDir(r.sourcePath)
	if err != nil {
		logger.Printf("READDIR ERROR: %v", err)
		return nil, vfs.ToErrno(err)
	}

	result := make([]fuse.DirEntry, 0, len(entries))
	for i, e := range entries {
		mode := uint32(syscall.S_IFREG)
		var ino uint64
		fullPath := filepath.Join(r.sourcePath, e.Name())
		if e.IsDir() {
			mode = syscall.S_IFDIR
			ino = getDirInodeFromMap(fullPath)
		} else {
			ino = getFileInodeFromMap(fullPath)
		}

		result = append(result, fuse.DirEntry{
			Name: e.Name(),
			Mode: mode,
			Ino:  ino,
			Off:  uint64(i + 1),
		})
	}

	globalDirCache.PutIfGeneration(r.sourcePath, result, generation)

	return &nfsDirStream{entries: result}, 0
}

// Statfs implements fs.NodeStatfser to provide filesystem statistics for Samba compatibility
func (r *VirtualMkvRoot) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	logger.Printf("=== STATFS === path=%s", r.sourcePath)

	// Calculate realistic values based on virtual files
	// Block size: standard 4KB
	out.Bsize = 4096

	// Total blocks: ~1TB virtual filesystem (arbitrary but realistic)
	out.Blocks = 250 * 1024 * 1024 // 1TB / 4KB blocks

	// Free blocks: ~500GB available (half of total, arbitrary)
	out.Bfree = 125 * 1024 * 1024
	out.Bavail = 125 * 1024 * 1024 // Available to non-root users

	// File counts: based on actual cache size
	totalFiles := uint64(metaCache.Len())
	if totalFiles == 0 {
		totalFiles = 1000 // Fallback estimate if cache not populated
	}
	out.Files = totalFiles
	out.Ffree = 500 // Arbitrary free inodes

	// Namemax: maximum filename length
	out.NameLen = 255

	logger.Printf("STATFS: blocks=%d/%d files=%d/%d bsize=%d",
		out.Blocks, out.Bfree, out.Files, out.Ffree, out.Bsize)

	return 0
}

// VirtualDirNode - nodo per directory (movies, tv) con file .mkv virtuali
type VirtualDirNode struct {
	fs.Inode
	physicalPath string // Path fisico della directory (es. /mnt/torrserver/movies)
}

// Compile-time interface checks for VirtualDirNode
var _ fs.NodeReaddirer = (*VirtualDirNode)(nil)
var _ fs.NodeLookuper = (*VirtualDirNode)(nil)
var _ fs.NodeGetattrer = (*VirtualDirNode)(nil)
var _ fs.NodeUnlinker = (*VirtualDirNode)(nil)
var _ fs.NodeMkdirer = (*VirtualDirNode)(nil)
var _ fs.NodeCreater = (*VirtualDirNode)(nil)
var _ fs.NodeRenamer = (*VirtualDirNode)(nil)
var _ fs.NodeSetattrer = (*VirtualDirNode)(nil)

// Every mutation of an audio directory answers EROFS (spec 4); video directories keep
// the untouched ENOSYS they have always answered with.
func (d *VirtualDirNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, audioMutationErrno(filepath.Join(d.physicalPath, name))
}

func (d *VirtualDirNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	return nil, nil, 0, audioMutationErrno(filepath.Join(d.physicalPath, name))
}

func (d *VirtualDirNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	return audioMutationErrno(filepath.Join(d.physicalPath, name))
}

func (d *VirtualDirNode) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if isAudioSectionPath(d.physicalPath) {
		return syscall.EROFS
	}
	return syscall.ENOSYS
}

func (d *VirtualDirNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	// Inside an audio section before reconciliation has published, the committed
	// set is unknown. Wait for it rather than answering: an empty-but-successful
	// listing tells a scanner the library was deleted (spec 9: never a plausible
	// but incomplete tree).
	//
	// This check stays AHEAD of the directory cache deliberately. A listing cached
	// while the namespace was Ready would otherwise still be served after a later
	// MarkFailed, masking a terminal failure behind stale entries.
	if errno := audioWaitServable(ctx, d.physicalPath); errno != 0 {
		return nil, errno
	}

	if entries, found := globalDirCache.Get(d.physicalPath); found {
		return &nfsDirStream{entries: entries}, 0
	}
	// Snapshot the generation before the walk: a concurrent invalidate must not be
	// overwritten by this listing landing after it.
	generation := globalDirCache.Generation(d.physicalPath)

	entries, err := os.ReadDir(d.physicalPath)
	if err != nil {
		logger.Printf("READDIR DIR ERROR: %v", err)
		return nil, vfs.ToErrno(err)
	}

	result := make([]fuse.DirEntry, 0, len(entries))
	for i, e := range entries {
		fullPath := filepath.Join(d.physicalPath, e.Name())
		if e.IsDir() {
			ino := getDirInodeFromMap(fullPath)
			result = append(result, fuse.DirEntry{
				Name: e.Name(),
				Mode: syscall.S_IFDIR,
				Ino:  ino,
				Off:  uint64(i + 1),
			})
		} else if classifyVFSPath(fullPath).Stub {
			ino := getFileInodeFromMap(fullPath)
			result = append(result, fuse.DirEntry{
				Name: e.Name(),
				Mode: syscall.S_IFREG,
				Ino:  ino,
				Off:  uint64(i + 1),
			})
		}
	}

	globalDirCache.PutIfGeneration(d.physicalPath, result, generation)

	return &nfsDirStream{entries: result}, 0
}

func (d *VirtualDirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	fullPath := filepath.Join(d.physicalPath, name)

	// Only Readdir used to check readiness, so a stat during the unready window
	// fell through to ENOENT below - semantic absence rather than refusal, which
	// a non-zero NegativeTimeoutSeconds then let the kernel cache past
	// publication. Waiting here removes the false negative at its source, so the
	// correctness no longer depends on that timeout being zero.
	//
	// Directories inside an audio section wait too. The caller is about to read
	// the directory anyway, so exempting them would only move the wait one call
	// later while adding a branch.
	if errno := audioWaitServable(ctx, d.physicalPath); errno != 0 {
		return nil, errno
	}

	if classifyVFSPath(fullPath).Stub {
		meta, err := stubMeta(fullPath)
		if err == nil {
			addMetaToInodeMap(fullPath, meta)
			ino := getFileInodeFromMap(fullPath)
			node := &VirtualMkvNode{vMeta: meta}
			stable := fs.StableAttr{
				Mode: syscall.S_IFREG,
				Ino:  ino,
				Gen:  1,
			}
			child := d.NewInode(ctx, node, stable)
			fillAttrFromMetadata(meta, &out.Attr)
			out.Ino = ino
			return child, 0
		}
	}

	// Fallback for directories
	st := syscall.Stat_t{}
	if err := syscall.Stat(fullPath, &st); err != nil {
		return nil, syscall.ENOENT
	}

	if (st.Mode & syscall.S_IFMT) == syscall.S_IFDIR {
		node := &VirtualDirNode{physicalPath: fullPath}
		dirIno := getDirInodeFromMap(fullPath)
		stable := fs.StableAttr{
			Mode: syscall.S_IFDIR,
			Ino:  dirIno,
			Gen:  1,
		}
		child := d.NewInode(ctx, node, stable)
		fillAttrFromStat(&st, &out.Attr)
		out.Ino = dirIno
		return child, 0
	}

	return nil, syscall.ENOENT
}

// Getattr returns directory attributes
func (d *VirtualDirNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	st := syscall.Stat_t{}
	if err := syscall.Stat(d.physicalPath, &st); err != nil {
		logger.Printf("GETATTR DIR ERROR: %v", err)
		return vfs.ToErrno(err)
	}

	fillAttrFromStat(&st, &out.Attr)

	// Use full-path hash to avoid inode collisions (e.g. Season.01 dirs).
	out.Ino = getDirInodeFromMap(d.physicalPath)

	// Override ONLY Mode and Size to ensure directory permissions and Samba compliance
	out.Mode = syscall.S_IFDIR | 0755
	// Projected audio directories are read-only to their clients (spec 4); the API
	// owns every mutation, so 0555 is the honest answer.
	if section, rel, ok := audioSectionRel(d.physicalPath); ok {
		out.Mode = syscall.S_IFDIR | 0555
		// Spec 7: an audio directory's mtime moves when its committed child set does,
		// and at no other time. Deriving it from the registry rows also keeps a
		// rolled-back staging file from leaving a spurious timestamp behind.
		if mtime, found := globalAudioNamespace.DirMtime(section, rel); found {
			ts := sanitizeTime(mtime)
			out.Mtime, out.Atime, out.Ctime = ts, ts, ts
		}
	}
	out.Size = 4096

	return 0
}

// audioSectionRel splits a physical path into its audio section and the section-relative
// directory path. ok is false outside music/ and audiobooks/.
func audioSectionRel(fullPath string) (library.Section, string, bool) {
	rel, err := filepath.Rel(physicalSourcePath, fullPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	section := library.Section(parts[0])
	if !library.IsAudioSection(section) {
		return "", "", false
	}
	if len(parts) == 1 {
		return section, "", true
	}
	return section, strings.Join(parts[1:], "/"), true
}

// forceCloseVirtualFile terminates the active pump and closes all open handles for a
// virtual .mkv path. Without this, smbd D-states on a file with an active blocking read.
// Shared by the FUSE Unlink handler and invalidateSyncRemovedPath.
func forceCloseVirtualFile(fullPath string) {
	// V-OpenTracker: attendi che i Read() in volo completino prima di cancellare
	// la pump, per evitare nil-deref su nativeReader durante cancel concorrente.
	if globalOpenTracker.IsPathOpen(fullPath) {
		for i := 0; i < 3; i++ {
			if !globalOpenTracker.IsPathOpen(fullPath) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if val, ok := activePumps.Load(fullPath); ok {
		ps := val.(*NativePumpState)
		if ps.cancel != nil {
			ps.cancel()
		}
		activePumps.Delete(fullPath)
		logger.Printf("UNLINK: force-terminated active pump for %s", filepath.Base(fullPath))
	}
	// Close all handles referencing this file
	activeHandles.Range(func(key, value interface{}) bool {
		h := key.(*MkvHandle)
		if h.path == fullPath {
			if h.nativeReader != nil {
				h.nativeReader.Close()
			}
			activeHandles.Delete(h)
			logger.Printf("UNLINK: force-closed handle for %s", filepath.Base(fullPath))
		}
		return true
	})
}

// invalidateSyncRemovedPath drops FUSE-layer state for a stub removed out-of-band by the
// sync engines: their os.Remove on the physical path bypasses the Unlink handler, so
// without this the virtual file keeps being listed and served for hours (dircache), and
// a Plex scan in that window registers the replaced file as a ghost duplicate version
// that later fails to play once the cache finally expires.
func invalidateSyncRemovedPath(path string) {
	if strings.HasSuffix(path, ".mkv") {
		forceCloseVirtualFile(path)
		registry.RemoveFromRegistry(path)
	}
	// The directory listing is not the only way back to a removed stub: a lookup by
	// exact path is served from the metadata cache, which holds entries for 24h.
	if metaCache != nil {
		metaCache.Delete(path)
	}
	// The file's own directory and every ancestor up to the source root: adding the
	// first track of an album changes the listing of each directory on the way up, and
	// a Readdir of any of them inside the TTL would otherwise miss it. Deleting the
	// path itself covers removed directories too (empty season/show cleanup).
	if physicalSourcePath == "" {
		globalDirCache.Delete(filepath.Dir(path))
	} else {
		dir := filepath.Dir(path)
		for strings.HasPrefix(dir, physicalSourcePath) {
			globalDirCache.Delete(dir)
			if dir == physicalSourcePath {
				break
			}
			next := filepath.Dir(dir)
			if next == dir {
				break
			}
			dir = next
		}
	}
	globalDirCache.Delete(path)
}

// Unlink handles file deletion and triggers torrent auto-remove (FASE 4.2)
func (d *VirtualDirNode) Unlink(ctx context.Context, name string) syscall.Errno {
	logger.Printf("=== UNLINK === dir=%s file=%s", d.physicalPath, name)

	// Only handle .mkv files
	if !strings.HasSuffix(name, ".mkv") {
		logger.Printf("UNLINK: not an mkv file, skipping auto-remove")
		return syscall.EPERM // Not permitted for non-mkv files
	}

	fullPath := filepath.Join(d.physicalPath, name)

	forceCloseVirtualFile(fullPath)
	tailFillTargets.Delete(fullPath)

	// Extract hash and remove torrent from GoStorm
	success, err := globalTorrentRemover.RemoveTorrentFromFile(fullPath)
	if err != nil {
		logger.Printf("UNLINK ERROR: failed to remove torrent: %v", err)
		// Continue with file deletion even if torrent removal fails
	} else if success {
		logger.Printf("UNLINK: torrent successfully removed from GoStorm")
	} else {
		// Also reached when an audio projection still owns the torrent, which the
		// guard logs with its own reason.
		logger.Printf("UNLINK: torrent not removed (already gone, or still referenced), but hash was blacklisted")
	}

	// Delete physical .mkv file
	if err := os.Remove(fullPath); err != nil {
		logger.Printf("UNLINK ERROR: failed to delete file: %v", err)
		return vfs.ToErrno(err)
	}

	registry.RemoveFromRegistry(fullPath)
	globalDirCache.Delete(d.physicalPath)
	// Same reason as the sync removal path: a lookup by exact path is answered from
	// the metadata cache, which would keep serving this file for the whole TTL.
	if metaCache != nil {
		metaCache.Delete(fullPath)
	}

	logger.Printf("UNLINK COMPLETE: file deleted successfully")
	return 0
}

// VirtualMkvNode - nodo per singolo file .mkv virtuale
type VirtualMkvNode struct {
	fs.Inode
	vMeta *vfs.Metadata
	wake  func(context.Context, string, int) error // per-node activation dependency; wired at Open after RED
}

// Compile-time interface checks
var _ fs.NodeGetattrer = (*VirtualMkvNode)(nil)
var _ fs.NodeOpener = (*VirtualMkvNode)(nil)

func (n *VirtualMkvNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	fillAttrFromMetadata(n.vMeta, &out.Attr)
	out.Ino = getFileInodeFromMap(n.vMeta.Path)
	return 0
}

var _ fs.NodeSetattrer = (*VirtualMkvNode)(nil)

// Setattr refuses every metadata mutation of an audio projection: chmod, chown and
// truncate are all API-owned (spec 4). Video keeps answering ENOSYS as before.
func (n *VirtualMkvNode) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if n.vMeta.Audio {
		return syscall.EROFS
	}
	return syscall.ENOSYS
}

func (n *VirtualMkvNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if gc().LogLevel == "DEBUG" {
		logger.Printf("=== OPEN VIRTUAL === path=%s", n.vMeta.Path)
	}
	// Read-only first: an audio projection refuses a writable open before any handle
	// exists, so no write path ever has to exist behind it (spec 4).
	if n.vMeta.Audio && audioWriteIntent(flags) {
		return nil, 0, syscall.EROFS
	}
	if len(n.vMeta.SegmentHeader) > 0 {
		return n.openCueTrack(ctx, flags)
	}

	// PROACTIVE CLEANUP TRIGGER (V246): must be sync before any Read() can arrive.
	raCache.SwitchContext(n.vMeta.Path)

	// Cancel any pending release timers (Debounce)
	if oldTimer, ok := pumpTimers.LoadAndDelete(n.vMeta.Path); ok {
		oldTimer.(*time.Timer).Stop()
	}
	if oldTimer, ok := priorityTimers.LoadAndDelete(n.vMeta.Path); ok {
		oldTimer.(*time.Timer).Stop()
	}

	hashStr, urlFileIdx := vfs.ExtractHashAndIndex(n.vMeta.URL)

	// hasFullWarmup: Open returns instantly only if both head and tail warmup files are ready.
	// headReady: Allows async Wake and direct ID injection for instant start.
	// Audio projections skip the movie/TV head/tail SSD warmup entirely, and Phase 1
	// puts nothing in its place: demand fetch plus the existing read-ahead is the
	// whole policy. Decided once here rather than special-cased at each warmup site.
	usesWarmup := pathUsesSSDWarmup(n.vMeta.Path)

	headReady := false
	tailReady := false
	if usesWarmup && warmup.DiskWarmup != nil && hashStr != "" {
		headReady = warmup.DiskWarmup.HeadReady(hashStr, urlFileIdx)
		tailReady = warmup.DiskWarmup.TailReady(hashStr, urlFileIdx)
	}
	magnetCandidate := n.vMeta.URL
	if hashStr != "" && (strings.HasPrefix(n.vMeta.URL, "http://") || strings.HasPrefix(n.vMeta.URL, "https://")) {
		magnetCandidate = "magnet:?xt=urn:btih:" + hashStr
	}

	// Async Wake when head warmup is ready (Open returns instantly); sync Wake otherwise.
	wake := n.wake
	if wake == nil && nativeBridge != nil {
		wake = nativeBridge.Wake
	}
	if wake != nil && magnetCandidate != "" {
		if headReady {
			// Open returns instantly here, so the activation must not inherit the
			// request context: the kernel tears it down as soon as Open returns, and
			// a background context lets the wake finish for the reader that is about
			// to open the same path.
			safeGo(func() {
				_ = wake(context.Background(), magnetCandidate, urlFileIdx)
			})
		} else {
			if ctx.Err() != nil {
				return nil, 0, syscall.EINTR
			}
			// A synchronous activation that cannot start is terminal: returning a
			// handle here gives a scanner a file whose reads can never be served.
			// The request context goes with it, so a cancelled Open makes the wake
			// return (and release its semaphore token) instead of running to the
			// metadata timeout detached.
			activated := make(chan error, 1)
			go func() { activated <- wake(ctx, magnetCandidate, urlFileIdx) }()
			select {
			case err := <-activated:
				if err != nil {
					logger.Printf("[VFS] activation failed for %s: %v", n.vMeta.Path, err)
					return nil, 0, syscall.EIO
				}
			case <-ctx.Done():
				return nil, 0, syscall.EINTR
			}
		}
	}

	// Registered only once activation succeeded: a session for an Open that is
	// about to fail outlives the call and is never closed.
	ttffRegister(n.vMeta.Path, n.vMeta.Size, hashStr, headReady, tailReady)

	if val, exists := playbackRegistry.Load(n.vMeta.Path); !exists {
		playbackRegistry.Store(n.vMeta.Path, &PlaybackState{
			Path:                n.vMeta.Path,
			Hash:                hashStr,
			ImdbID:              n.vMeta.ImdbID,
			ExternalID:          n.vMeta.ExternalID,
			ExternalIDNamespace: n.vMeta.ExternalIDNamespace,
			OpenedAt:            time.Now(),
		})
	} else {
		if val != nil {
			state := val.(*PlaybackState)
			state.mu.Lock()
			state.OpenedAt = time.Now()
			state.IsStopped = false

			// Restore priority only if webhook confirmed recently (<30m) to avoid zombie torrents.
			recentlyConfirmed := !state.ConfirmedAt.IsZero() && time.Since(state.ConfirmedAt) < 30*time.Minute
			isHealthy := state.IsHealthy
			state.mu.Unlock()

			if isHealthy && recentlyConfirmed && state.Hash != "" {
				hHash := metainfo.NewHashFromHex(state.Hash)
				if t := web.BTS.GetTorrent(hHash); t != nil {
					if !t.IsPriority.Load() {
						t.IsPriority.Store(true)
						logger.Printf("[NativeBridge] Priority RESTORED for Silent Re-Open: %s", state.Hash)
					}
				}
			}
		}
	}

	now := time.Now()

	// Use warmup IDs directly when available, skipping the resolveTargetFile retry loop.
	var finalHash string
	var fileIdx int
	var isNative bool

	if headReady && hashStr != "" {
		finalHash = hashStr
		fileIdx = urlFileIdx
		isNative = true
	} else {
		var err error
		finalHash, fileIdx, err = resolveTargetFile(n.vMeta.URL, n.vMeta.Size, n.vMeta.Path)
		isNative = (err == nil)
		if !isNative && gc().LogLevel == "DEBUG" {
			logger.Printf("[NativeBridge] Resolution failed for %s: %v. Access will rely on cache/retry.", filepath.Base(n.vMeta.Path), err)
		}
	}

	h := &MkvHandle{
		url:              n.vMeta.URL,
		magnet:           magnetCandidate, // Store for potential re-wake
		size:             n.vMeta.Size,
		path:             n.vMeta.Path,
		lastTime:         now,
		lastOff:          -1,
		lastActivityTime: now,       // Initialize activity tracking
		hasWarmup:        headReady, // Eligibility for fast SSD probes
		usesWarmup:       usesWarmup,
	}
	h.state.Store(stateWarmup) // Initial state; transitions to stateStreaming on seek/resume.

	if isNative {
		h.hash = finalHash
		h.fileID = fileIdx
		// media.stop only knows the path; keep what EnsureTail needs to reach the file.
		if usesWarmup {
			tailFillTargets.Store(n.vMeta.Path, tailFillTarget{hash: finalHash, fileID: fileIdx, size: n.vMeta.Size})
		}
		// Gillian: proactive pump start at Open() — pump ready before first Read().
		// pumpOnce ensures single start; late rescue path in Read() handles hash=='' case.
		h.pumpOnce.Do(func() {
			h.startNativePump(finalHash, fileIdx)
		})
		if usesWarmup && !headReady {
			// Real cold start: no warmup data present yet. Signal warmupActive here, at Open(),
			// rather than waiting for the first WriteChunk - that first-connection burst is
			// exactly the window aggressive PEX churn (Task 3) needs to catch, and by the time
			// WriteChunk fires once, the torrent's initial peer set has often already connected.
			// warmup.OnWarmupStateChange (wired at startup) still owns turning this back off once
			// the writeWorker synchronously observes real STARTING/COMPLETED transitions.
			forceTorrentWarmupActive(finalHash, fileIdx)
		}
	}

	activeHandles.Store(h, true)

	return h, 0, 0
}

// handleState values for MkvHandle.state (atomic.Uint32).
// A handle transitions one-way: stateWarmup → stateStreaming.
const (
	stateWarmup    uint32 = 0 // Initial: SSD warmup eligible (TTFF phase)
	stateStreaming uint32 = 1 // Pump-only streaming; no SSD warmup
)

// watchSample is a V752 starvation-watchdog observation: pump offset and player
// offset at time t, used to compute both rates over a sliding 10s window.
type watchSample struct {
	t         time.Time
	offset    int64
	playerOff int64
}

func stateName(s uint32) string {
	switch s {
	case stateWarmup:
		return "WARMUP"
	case stateStreaming:
		return "STREAMING"
	default:
		return "UNKNOWN"
	}
}

type MkvHandle struct {
	url, path        string
	size             int64
	lastOff          int64
	lastLen          int
	lastTime         time.Time
	lastActivityTime time.Time
	monitorStarted   bool
	lastGlobalUpdate time.Time

	nativeReader *native.NativeReader
	hash         string
	magnet       string
	fileID       int
	mu           sync.Mutex
	pumpCancel   context.CancelFunc
	hasSlot      atomic.Bool
	// pumpState is the state this handle incremented. activePumps is keyed by path
	// and the state is replaced wholesale, so Release must not decrement a pump it
	// never counted.
	pumpState       *NativePumpState
	isWatching      bool
	hasWarmup       bool          // true if both head+tail warmup available at Open time
	usesWarmup      bool          // false for audio projections: they skip SSD warmup entirely
	state           atomic.Uint32 // handleState: stateWarmup | stateStreaming
	pumpOnce        sync.Once
	isPrimaryHandle atomic.Bool  // pump creator, primary reconnects (refCount 0→1), proven readers
	seqAdvances     atomic.Int32 // consecutive sequential streaming reads, feeds the promotion below
}

// startNativePump acquires a slot and starts the background pump.
// Called from Open (proactive) or Read (rescue for late resolution).
// scanSlotLimit is how many pump slots a not-yet-confirmed playback may find taken before it
// is denied one. capacity is the semaphore's, fixed at make(): reading the live config here
// would apply half of a limit change, since the channel itself cannot be resized.
// The floor of 1 matters because a request cannot reach IsHealthy without pumping first.
func scanSlotLimit(capacity int, anyHealthyPlayback bool) int {
	if anyHealthyPlayback {
		return 5
	}
	if n := capacity - 5; n > 1 {
		return n
	}
	return 1
}

func (h *MkvHandle) startNativePump(finalHash string, fileIdx int) {
	// 1. Verify we don't already have a slot or an active pump
	if h.hasSlot.Load() {
		return
	}

	isHealthy := false
	if val, ok := playbackRegistry.Load(h.path); ok {
		if ps, ok := val.(*PlaybackState); ok && ps.GetStatus() {
			isHealthy = true
		}
	}

	// StrategicReserve: limit background scan slots.
	// If any confirmed playback is active (IsHealthy), tighten the limit to 5 slots
	// so the scan cannot consume memory that the active stream needs.
	// Without active playback, allow up to MasterConcurrencyLimit-5 (default 20).
	canTakeSlot := true
	if !isHealthy {
		anyHealthyPlayback := false
		playbackRegistry.Range(func(_, v interface{}) bool {
			if ps, ok := v.(*PlaybackState); ok && ps.GetStatus() {
				anyHealthyPlayback = true
				return false
			}
			return true
		})
		scanLimit := scanSlotLimit(cap(masterDataSemaphore), anyHealthyPlayback)
		if len(masterDataSemaphore) >= scanLimit {
			canTakeSlot = false
			logger.Printf("[StrategicReserve] Denying pump slot to background scan (Saturation: %d/%d, healthyPlayback=%v): %s",
				len(masterDataSemaphore), gc().MasterConcurrencyLimit, anyHealthyPlayback, filepath.Base(h.path))
		}
	}

	if !canTakeSlot {
		return
	}

	pumpCreationMu.Lock()

	if val, ok := activePumps.Load(h.path); ok {
		ps := val.(*NativePumpState)
		newRefs := atomic.AddInt32(&ps.refCount, 1)
		h.mu.Lock()
		h.hasSlot.Store(true)
		h.pumpState = ps
		h.isWatching = true
		h.nativeReader = ps.reader
		h.pumpCancel = ps.cancel
		h.mu.Unlock()
		globalOpenTracker.Inc(h.hash, h.path)
		// Primary reconnect (refCount 0→1): inherit player position.
		// Secondary handles (Plex probes at arbitrary offsets): no inheritance.
		if newRefs == 1 {
			becomePrimary(h)
			if curPos := atomic.LoadInt64(&ps.playerOff); curPos > 0 {
				atomic.StoreInt64(&h.lastOff, curPos)
			}
		}
		logger.Printf("[V264] Attached to existing pump (Refs: %d, primary=%v): %s", newRefs, h.isPrimaryHandle.Load(), filepath.Base(h.path))
		pumpCreationMu.Unlock()
		return
	}

	// Release mutex before blocking on semaphore to avoid holding it during I/O.
	pumpCreationMu.Unlock()

	select {
	case masterDataSemaphore <- struct{}{}:
		// Double-check activePumps after acquiring semaphore (another goroutine may have created it).
		pumpCreationMu.Lock()
		if val, ok := activePumps.Load(h.path); ok {
			<-masterDataSemaphore
			ps := val.(*NativePumpState)
			newRefs := atomic.AddInt32(&ps.refCount, 1)
			h.mu.Lock()
			h.hasSlot.Store(true)
			h.pumpState = ps
			h.isWatching = true
			h.nativeReader = ps.reader
			h.pumpCancel = ps.cancel
			h.mu.Unlock()
			globalOpenTracker.Inc(h.hash, h.path)
			if newRefs == 1 {
				becomePrimary(h)
				if curPos := atomic.LoadInt64(&ps.playerOff); curPos > 0 {
					atomic.StoreInt64(&h.lastOff, curPos)
				}
			}
			pumpCreationMu.Unlock()
			return
		}

		h.hasSlot.Store(true)
		becomePrimary(h) // pump creator is always primary
		// Capture the reader now and pass it to the pump goroutine: re-reading h.nativeReader
		// inside the goroutine raced with Release setting it to nil, and the resulting early
		// return (before the teardown defer) leaked the master slot.
		pumpReader := nativeBridge.NewStreamReader(finalHash, fileIdx, h.size)
		h.nativeReader = pumpReader
		if tr := torr.PeekTorrent(finalHash); tr != nil && tr.Torrent != nil {
			if info := tr.Torrent.Info(); info != nil && info.PieceLength > 0 {
				pl := int64(info.PieceLength)
				pumpReader.SetPieceLen(pl)
				raCache.SetPieceLen(h.path, pl)
			}
		}

		// Register in activePumps BEFORE releasing lock, but BEFORE doing I/O
		pumpCtx, pumpCancel := context.WithCancel(context.Background())
		h.pumpCancel = pumpCancel
		sharedState := &NativePumpState{
			cancel:   pumpCancel,
			reader:   pumpReader,
			path:     h.path,
			refCount: 1,
		}
		activePumps.Store(h.path, sharedState)
		h.pumpState = sharedState
		globalOpenTracker.Inc(h.hash, h.path)
		pumpCreationMu.Unlock() // SUCCESS: Shared state registered, global lock released

		logger.Printf("[V264] Native Pump Started (Slot Acquired): %s", filepath.Base(h.path))

		// Seed h.lastOff from the persisted resume position (V750 playback_states.last_seek_off)
		// before any of the anchoring logic below runs, so a cold pump-start (h.lastOff still -1,
		// no live signal yet) for a file resumed deep into its runtime is treated exactly like an
		// inherited primary-reconnect position (see newRefs==1 above) instead of starting near
		// MaxCachedOffset/0 - without this, resuming at e.g. 19GB left the pump buffering from
		// scratch while the player was served entirely by FetchBlock until something else caught
		// up. >2MB matches the existing real-playback-vs-scanner threshold (main.go IsInferredPlayback).
		if atomic.LoadInt64(&h.lastOff) < 0 {
			seeded := false
			if val, ok := playbackRegistry.Load(h.path); ok {
				if pbs, ok := val.(*PlaybackState); ok {
					pbs.mu.RLock()
					lastSeekOff := pbs.LastSeekOff
					pbs.mu.RUnlock()
					seeded = h.seedResumeAnchor(pbs, lastSeekOff)
				}
			}
			// V751: registry miss or fresh entry (a clean stop before boot leaves
			// IsStopped=true, which restorePlaybackStates filters out) → fall back to the
			// DB so a mid-file resume still anchors the pump at the real position instead
			// of starting from 0 while the player is served entirely by FetchBlock.
			if !seeded && stateDB != nil {
				if rec, err := stateDB.LoadPlaybackStateByPath(h.path); err == nil && rec != nil && rec.LastSeekOff > 0 {
					if h.seedResumeAnchor(nil, rec.LastSeekOff) {
						logger.Printf("[ResumeAnchor] Seeded pump start from DB: %.1fMB", float64(rec.LastSeekOff)/(1<<20))
					}
				}
			}
		}

		// Start background pump — resume from last cached position
		resumeOffset := raCache.MaxCachedOffset(h.path)

		// Start pump near end of warmup zone so it buffers past 64MB before SSD handover.
		if h.usesWarmup && warmup.DiskWarmup != nil && h.hash != "" {
			diskOffset := warmup.DiskWarmup.GetAvailableRange(h.hash, h.fileID)
			if diskOffset > 16*1024*1024 {
				safetyMargin := int64(16 * 1024 * 1024)
				skipOffset := diskOffset - safetyMargin
				if skipOffset > resumeOffset {
					resumeOffset = skipOffset
					logger.Printf("[DiskWarmup] PUMP SKIP: Starting from %.1fMB to bridge SSD handover", float64(resumeOffset)/(1<<20))
				}
			}
		}

		// Anchor pump to player position when MaxCachedOffset is stale-high to prevent EOF loops.
		if playerOff := atomic.LoadInt64(&h.lastOff); playerOff > 0 {
			chunkSize := raCache.ChunkSize(h.path)
			if resumeOffset > playerOff+chunkSize*2 {
				aligned := (playerOff / chunkSize) * chunkSize
				logger.Printf("[V700] Pump anchored to player: %.1fMB (MaxCached was %.1fMB)",
					float64(aligned)/(1<<20), float64(resumeOffset)/(1<<20))
				resumeOffset = aligned
			}
		} else if playerOff < 0 && resumeOffset > 0 {
			// New handle: reset stale MaxCachedOffset unless warmup is active and covers the range.
			// If resumeOffset >= warmup.FileSize, pump skip cannot fire → dead zone in raCache.
			warmupCoverage := int64(0)
			if h.usesWarmup && warmup.DiskWarmup != nil && h.hash != "" {
				warmupCoverage = warmup.DiskWarmup.GetAvailableRange(h.hash, h.fileID)
			}
			if warmupCoverage == 0 || resumeOffset >= warmup.FileSize {
				logger.Printf("[V700] New handle: reset stale MaxCachedOffset %.1fMB → 0",
					float64(resumeOffset)/(1<<20))
				resumeOffset = 0
			}
		}

		// Anchor pump to player position on resume to eliminate anacrolix priority competition.
		{
			chunkSize := raCache.ChunkSize(h.path)
			if raV310 := atomic.LoadInt64(&h.lastOff); raV310 > 0 && resumeOffset+chunkSize < raV310 {
				pumpStartV310 := (raV310 / chunkSize) * chunkSize
				logger.Printf("[V310] Resume anchor: pump start → %dMB (player at %dMB)",
					pumpStartV310/(1024*1024), raV310/(1024*1024))
				resumeOffset = pumpStartV310
			}
		}

		if resumeOffset > 0 {
			atomic.StoreInt64(&h.lastOff, resumeOffset)
		}

		pumpStart := resumeOffset
		capturedState := sharedState
		safeGo(func() {
			h.nativePump(pumpCtx, pumpReader, pumpStart, capturedState)
		})
	default:
		// If slots are full, it will fall back to per-request slots in Read
		logger.Printf("[MasterSemaphore] Limit reached, %s will use Fallback mode", filepath.Base(h.path))
	}
}

// nativePump reads continuously from the Native pipe and fills raCache.
// sharedState guards against orphan-delete in defer. pumpReader is captured by the caller
// so a concurrent Release cannot nil it out from under the goroutine.
func (h *MkvHandle) nativePump(ctx context.Context, pumpReader *native.NativeReader, startOffset int64, sharedState *NativePumpState) {
	// The master slot must be returned on every exit, including the early return below.
	// It used to be released only by the teardown defer, which the nil-reader return path
	// never reached, leaking one slot per event.
	defer func() {
		if h.hasSlot.Load() {
			select {
			case <-masterDataSemaphore:
			default:
			}
			h.hasSlot.Store(false)
		}
	}()

	if pumpReader == nil {
		logger.Printf("[Pump] reader is nil at startup for %s", filepath.Base(h.path))
		return
	}

	if h.hash == "" {
		// Late hash resolution for handles where Open() didn't complete it.
		if hash, fileID, err := resolveTargetFile(h.url, h.size, h.path); err == nil {
			h.mu.Lock()
			h.hash = hash
			h.fileID = fileID
			h.mu.Unlock()
			logger.Printf("[Pump] Late resolution success: %s", hash[:8])
		} else {
			logger.Printf("[Pump] Warning: hash empty for %s, warmup disabled", filepath.Base(h.path))
		}
	}
	defer func() {
		// V306: Check if pump exited during confirmed healthy playback — this is
		// unexpected and usually indicates an I/O error or cancellation. Log for diagnosis.
		pumpExitedHealthy := false
		if val, ok := playbackRegistry.Load(h.path); ok {
			if ps, ok := val.(*PlaybackState); ok && ps.GetStatus() {
				pumpExitedHealthy = true
			}
		}

		h.mu.Lock()
		// Only delete if our sharedState is still the registered one (prevents pump A's defer from deleting pump B).
		if val, ok := activePumps.Load(h.path); ok && val == sharedState {
			activePumps.Delete(h.path)
			// Purge the adaptive chunk size set at this pump's start (SetPieceLen, main.go:1075):
			// otherwise pieceLens keeps growing with entries for paths no pump is streaming
			// anymore, and a future pump for the same path with a different piece length would
			// see the stale entry via ChunkSize() until a fresh SetPieceLen call overwrites it.
			raCache.pieceLens.Delete(h.path)
		}

		if pumpExitedHealthy {
			logger.Printf("[V306] WARNING: pump goroutine exited during healthy playback for %s — priority preserved via IsHealthy", filepath.Base(h.path))
		}

		// The master slot is released by the defer installed at the top of nativePump.
		pumpReader.Close()
		h.mu.Unlock()
		logger.Printf("[V239] Native Pump Goroutine Ended: %s", filepath.Base(h.path))
	}()

	// V752: starvation watchdog state — sliding 10s window + last revival time.
	var watchSamples []watchSample
	lastRevival := time.Now().Add(-time.Hour)

	chunkSize := raCache.ChunkSize(h.path)

	// Track bytes pumped in this session for the Grace Period Boost
	pumpedBytes := int64(0)
	// Align startOffset to chunk boundary
	offset := (startOffset / chunkSize) * chunkSize

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Re-read chunkSize each iteration for consistency with raCache's own lookups - not
		// because it can change mid-pump. SetPieceLen is called exactly once, at pump start
		// (main.go:1084), only if metadata is already resolved at that moment; if not, this
		// stays at the ReadAheadBase fallback for the pump's entire lifetime. No late-binding path
		// exists today - if one is ever added, chunkKey's offset/chunkSize indexing means a
		// mid-pump size change must purge that path's existing raCache entries first, or stale
		// keys orphan and double-cache.
		chunkSize = raCache.ChunkSize(h.path)

		// Release idle slot: confirmed playback gets 2h, background scans get 45s.
		// Check all handles for this path, not just the pump creator.
		lastAct := time.Time{}
		activeHandles.Range(func(key, value interface{}) bool {
			handle := key.(*MkvHandle)
			if handle.path == h.path {
				handle.mu.Lock()
				if handle.lastActivityTime.After(lastAct) {
					lastAct = handle.lastActivityTime
				}
				handle.mu.Unlock()
			}
			return true
		})
		// Fallback to pump creator's activity if no active handles found
		if lastAct.IsZero() {
			h.mu.Lock()
			lastAct = h.lastActivityTime
			h.mu.Unlock()
		}

		timeoutLimit := 45 * time.Second
		playbackConfirmedOrInferred := false
		if val, ok := playbackRegistry.Load(h.path); ok {
			if ps, ok := val.(*PlaybackState); ok {
				if ps.GetStatus() {
					timeoutLimit = 2 * time.Hour
					playbackConfirmedOrInferred = true
				} else if ps.IsInferredPlayback() {
					timeoutLimit = 10 * time.Minute // V750: Inferred playback
					playbackConfirmedOrInferred = true
				}
			}
		}

		if time.Since(lastAct) > timeoutLimit {
			logger.Printf("[V262] Idle timeout (%v) for %s - yielding slot", timeoutLimit, filepath.Base(h.path))
			return
		}

		// Sync to primary handles only; secondary metadata probes cause false 10GB+ jumps.
		now := time.Now()
		playerOff := int64(0)
		activeHandles.Range(func(key, value interface{}) bool {
			handle := key.(*MkvHandle)
			if handle.path == h.path && handle.isPrimaryHandle.Load() {
				if off := atomic.LoadInt64(&handle.lastOff); off > playerOff {
					playerOff = off
				}
			}
			return true
		})

		// Snap pump to player position when seek gap exceeds budget, aligned to chunk boundary.
		jumpThreshold := int64(gc().ReadAheadBudget)
		if playerOff > offset+jumpThreshold {
			jumpTo := (playerOff / chunkSize) * chunkSize
			if jumpTo < 0 {
				jumpTo = 0
			}
			logger.Printf("[V284] Pump jump: %dMB → %dMB (player at %dMB, gap %dMB)",
				offset/(1024*1024), jumpTo/(1024*1024), playerOff/(1024*1024),
				(playerOff-offset)/(1024*1024))
			offset = jumpTo
			pumpedBytes = 0                 // reset grace period so throttle doesn't fire immediately
			watchSamples = watchSamples[:0] // V752: fresh window after jump — rates across a jump are meaningless
		} else if pumpStrandedAhead(offset, playerOff, jumpThreshold) {
			// V754: the snap above only fires forward, so a pump anchored past the player could
			// never rejoin it — nativePumpChunk's hard limit parked it in a 100ms sleep for the
			// whole session while every read fell through to blocking FetchBlock.
			jumpTo := (playerOff / chunkSize) * chunkSize
			if jumpTo < 0 {
				jumpTo = 0
			}
			logger.Printf("[V754] Pump re-anchor (stranded ahead): %dMB → %dMB (player at %dMB, lead %dMB)",
				offset/(1024*1024), jumpTo/(1024*1024), playerOff/(1024*1024),
				(offset-playerOff)/(1024*1024))
			offset = jumpTo
			pumpedBytes = 0
			watchSamples = watchSamples[:0]
		}

		// V752: starvation watchdog — revive a pump that crawls below the player's
		// consumption rate while its lead is thin, without waiting for a player seek.
		// A blocked read inside nativePumpChunk cannot evaluate here; fetch timeouts
		// (p95/hedge scale) return control to the loop and the next iteration fires.
		watchSamples = append(watchSamples, watchSample{t: now, offset: offset, playerOff: playerOff})
		if len(watchSamples) > 16 {
			watchSamples = watchSamples[len(watchSamples)-16:] // V752: size-bounded ring — sparse crawls must survive in the window
		}
		if len(watchSamples) >= 2 {
			first, lastS := watchSamples[0], watchSamples[len(watchSamples)-1]
			elapsed := lastS.t.Sub(first.t).Seconds()
			if elapsed >= 9 {
				pumpRate := float64(lastS.offset-first.offset) / elapsed
				playerRate := float64(lastS.playerOff-first.playerOff) / elapsed
				if shouldFireStarvationRevival(playbackConfirmedOrInferred, offset-playerOff, pumpRate, playerRate, now.Sub(lastRevival)) {
					// Same core as V286b: Interrupt unblocks the in-flight chunk read and
					// the retry path re-issues it with fresh peer selection; ResetShield
					// clears adaptive state. interruptPending prevents cascade with seeks.
					if ps := sharedState; !ps.interruptPending.Swap(true) {
						// pumpReader, not h.nativeReader: Release nils the field while the pump
						// keeps its captured copy alive, so the field can be nil right here.
						pumpReader.Interrupt()
						torrstor.ResetShield()
						lastRevival = now
						watchSamples = watchSamples[:0]
						logger.Printf("[StarvationWatchdog] Revival: pump %dMB vs player %dMB (rates %.1f/%.1f MB/s) — interrupt+shield reset",
							offset/(1<<20), playerOff/(1<<20), pumpRate/(1<<20), playerRate/(1<<20))
					}
				}
			}
		}

		// Throttle background pump after 64MB grace period.
		if pumpedBytes > 64*1024*1024 {
			isHealthy := false
			if val, ok := playbackRegistry.Load(h.path); ok {
				if ps, ok := val.(*PlaybackState); ok && (ps.GetStatus() || ps.IsInferredPlayback()) {
					isHealthy = true
				}
			}

			if !isHealthy {
				// Throttle background tasks: 150ms delay between 16MB chunks.
				time.Sleep(150 * time.Millisecond)
			}
		}

		stop, nextOffset := h.nativePumpChunk(pumpReader, offset, chunkSize, playerOff)
		if stop {
			// Transient errors (seek interrupt, reconnect, piece timeout): retry until genuine EOF.
			if offset < h.size {
				time.Sleep(200 * time.Millisecond)
				// V286b: reset after each interrupt so subsequent seeks can fire Interrupt()
				// and ResetShield() again. Without this, interruptPending stays true for the
				// pump's entire lifetime after the first seek.
				sharedState.interruptPending.Store(false)
				continue
			}
			return // genuine EOF
		}

		pumpedBytes += (nextOffset - offset)
		offset = nextOffset
	}
}

// nativePumpChunk reads a single chunk from the Native pipe into raCache.
// pumpLeadLimit caps the pump's lead so the player stays inside the raCache window. The cache
// holds the last `budget` bytes the pump wrote, i.e. [pumpOff-budget, pumpOff]; the pump stops
// only once diff EXCEEDS the limit, so its settled lead is limit+chunkSize. With limit == budget
// the player sits a whole chunk below the window and every read on that edge misses (Lucky
// S01E04, 2026-08-22: pump-off pinned at 272MB against a 256MB cache, 40 consecutive stalls).
// Reserving three chunks leaves two of margin after the overshoot. Budget and chunkSize are both
// user-configurable, so the margin is derived, never hardcoded; a budget too small to hold it
// keeps the old behaviour rather than starving the lead.
func pumpLeadLimit(budget, chunkSize int64) int64 {
	if budget <= 0 || chunkSize <= 0 {
		return budget
	}
	slack := 3 * chunkSize
	if slack > budget/2 {
		slack = budget / 2
	}
	lead := budget - slack
	if lead < chunkSize {
		lead = chunkSize
	}
	if lead > budget {
		lead = budget
	}
	return lead
}

func (h *MkvHandle) nativePumpChunk(r *native.NativeReader, offset, chunkSize, playerOff int64) (stop bool, nextOffset int64) {
	// Don't pump beyond file size
	if offset >= h.size {
		return true, offset
	}

	budget := gc().ReadAheadBudget
	leadLimit := pumpLeadLimit(budget, chunkSize)
	diff := offset - playerOff

	if diff > leadLimit {
		// Hard limit reached: Wait for player to advance.
		time.Sleep(100 * time.Millisecond)
		return false, offset
	} else if diff > (leadLimit * 7 / 10) {
		// Soft limit (70%): Slow down gradually.
		sleepMs := (diff - (leadLimit * 7 / 10)) / (1024 * 1024)
		if sleepMs > 50 {
			sleepMs = 50
		}
		if sleepMs > 0 {
			time.Sleep(time.Duration(sleepMs) * time.Millisecond)
		}
	}

	// Playback pressure: the pump's lead over the player has worn thin (opposite end of the same
	// diff/budget signal used to throttle above). Extends tail-hedging into steady-state playback
	// via the same mechanism already used for warmup - see SetPlaybackPressure. Called on every
	// chunk like SetWarmupActive already is; both just store atomics, cheap either way.
	if h.hash != "" {
		if tr := torr.PeekTorrent(h.hash); tr != nil && tr.Torrent != nil {
			tr.Torrent.SetPlaybackPressure(diff < budget*20/100, offset)
		}
	}

	end := offset + chunkSize
	if end > h.size {
		end = h.size
	}

	// Coverage-aware skip-check: must cover the WHOLE [offset, end-1] range, not just probe the
	// first byte. A single-byte probe would still hit after a partial Put (see the drift fix
	// below), letting the pump skip a chunk that's only partially cached and leave a hole.
	// Covered() does the check without copying; this is the common path in steady-state
	// playback, so only pay for Get()'s up-to-16MB defensive copy when DiskWarmup actually
	// needs the bytes.
	if raCache.Covered(h.path, offset, end-1) {
		if h.usesWarmup && warmup.DiskWarmup != nil && h.hash != "" && offset <= warmup.FileSize {
			if data := raCache.Get(h.path, offset, end-1); data != nil {
				warmup.DiskWarmup.WriteChunk(h.hash, h.fileID, data, offset)
			}
		}
		return false, offset + chunkSize
	}

	// Skip warmup zone during initial play (SSD serves 0-80MB); pump jumps ahead to pre-fill raCache.
	// Gated on stateWarmup to avoid skip on resume/seek.
	if h.usesWarmup && warmup.DiskWarmup != nil && h.hash != "" && h.state.Load() == stateWarmup {
		warmupCoverage := warmup.DiskWarmup.GetAvailableRange(h.hash, h.fileID)
		if warmupCoverage >= offset+chunkSize {
			return false, offset + chunkSize
		}
	}

	// Use buffer from pool to reduce allocations
	bufPtr := readBufferPool.Get().(*[]byte)
	defer readBufferPool.Put(bufPtr)

	n, err := r.ReadAt((*bufPtr)[:end-offset], offset)
	if n > 0 {
		raCache.Put(h.path, offset, offset+int64(n)-1, (*bufPtr)[:n])
		if h.usesWarmup && warmup.DiskWarmup != nil && h.hash != "" && offset <= warmup.FileSize {
			warmup.DiskWarmup.WriteChunk(h.hash, h.fileID, (*bufPtr)[:n], offset)
		}
	}

	if err != nil {
		return true, offset + int64(n)
	}

	// A short read (fewer bytes than requested, err==nil) happens when a stream hiccup gets
	// silently converted to (n, nil) upstream (see native.go ReadAt's ErrUnexpectedEOF handling).
	// Continuing from offset+n would leave the pump mid-chunk permanently: every Put/skip-check
	// keys off ChunkSize-aligned offsets, so an unaligned continuation drifts off the chunk grid
	// for the rest of the pump's lifetime, turning every future chunk boundary into a cache miss.
	// Snap forward to the next chunk boundary instead; the small gap this skips is filled on demand
	// by FUSE reads landing in it (native.go's ReadAt handles both small and large forward jumps).
	got := offset + int64(n)
	if int64(n) < end-offset {
		return false, (got/chunkSize + 1) * chunkSize
	}
	return false, got
}

// shouldInterruptForSeek returns true for genuine seeks beyond budget.
// Ignores: new handles (prevOff<=0), Samba header probes (off==0), sequential reads.
func shouldInterruptForSeek(prevOff, off, budget int64) bool {
	if prevOff <= 0 || off == 0 {
		return false
	}
	return off > prevOff+budget || prevOff > off+budget
}

// tailProbeZoneSize returns the adaptive tail-probe zone size for a file of the given size:
// 5%, clamped to [64MB, 2GB]. Shared by the LastSeekOff
// write-site guard, and the ResumeAnchor seed-site guard - a single scanner probe reading MKV
// Cues near the end of a file must never look like a real playback position to any of them.
// promoteIfStreaming marks a handle primary once it has proven it is the player: several
// consecutive sequential reads of streaming size. The pump syncs to primary handles only, and
// during steady playback the real reader is usually an attached (secondary) handle - the
// nominal primary can be an idle probe holding the first reference, so the pump follows the
// wrong offset and the player is served by FetchBlock (Soulm8te, 2026-08-19: 29 attaches, all
// primary=false, only a seek recovered it by forcing a refCount 0->1 reconnect).
//
// Promotion is monotonic and never recomputed per tick, unlike the freshness heuristic reverted
// in 6ae20c9: the pump takes the MAX of the primaries' offsets, so a promotion can only raise
// that value, never drag it backwards - which is what produced the jump/re-anchor thrashing.
// Tail-zone reads never count: an EOF probe must not be able to pull the pump to the end.
// advanceProvesStreaming reports whether one read looks like the player moving forward through
// the file: streaming-sized, sequential from the previous read, and outside the tail probe zone.
// The size floor is a quarter of StreamingThreshold so Synology's 64KB CIFS reads still count.
func advanceProvesStreaming(prevOff, off, readLen, size, tolerance, streamingThreshold int64) bool {
	if prevOff < 0 || off <= prevOff || readLen <= 0 {
		return false
	}
	if off-prevOff > readLen+tolerance {
		return false
	}
	if readLen < streamingThreshold/4 {
		return false
	}
	return off < size-tailProbeZoneSize(size)
}

const primaryProofReads = 3 // consecutive sequential reads before a handle drives the pump

func (h *MkvHandle) promoteIfStreaming(prevOff, off int64, readLen int) {
	if h.isPrimaryHandle.Load() || h.hash == "" || prevOff < 0 {
		return
	}
	if !advanceProvesStreaming(prevOff, off, int64(readLen), h.size, gc().SequentialTolerance, gc().StreamingThreshold) {
		h.seqAdvances.Store(0)
		return
	}
	if h.seqAdvances.Add(1) < primaryProofFor(off, primaryOffsetFor(h.path, h), gc().ReadAheadBudget) {
		return
	}
	// Only during real playback: a background scan reads sequentially too, and outside a
	// confirmed session there is no player for the pump to follow anyway.
	val, ok := playbackRegistry.Load(h.path)
	if !ok {
		return
	}
	ps, ok := val.(*PlaybackState)
	if !ok || !(ps.GetStatus() || ps.IsInferredPlayback()) {
		return
	}
	if becomePrimary(h) {
		logger.Printf("[V284] Handle promoted to primary after %d sequential reads at %dMB: %s",
			h.seqAdvances.Load(), off/(1024*1024), filepath.Base(h.path))
	}
}

func tailProbeZoneSize(size int64) int64 {
	zone := size / 20 // 5%
	if zone < 64*1024*1024 {
		zone = 64 * 1024 * 1024
	}
	if zone > 2*1024*1024*1024 {
		zone = 2 * 1024 * 1024 * 1024
	}
	return zone
}

// shouldServeTailFromSSD reports whether a read at offset off (file size size) can be
// served from the tail warmup file (the last tailWarmupSize bytes). Pure gate for the
// Read() path: ReadTail itself is range-safe (returns 0 outside coverage), so this
// only decides whether the SSD tail is even a candidate. It deliberately does not
// depend on handle state - any first read with off >= warmup.FileSize transitions
// WARMUP→STREAMING (seek/resume detection) before a state-gated tail check could run.
// A resume in the last 16MB was therefore served by network FetchBlock (~16s TTFF on
// a 36.6GB remux, 2026-08-06) although the tail file was complete on SSD.
func shouldServeTailFromSSD(off, size, tailWarmupSize int64) bool {
	if size <= 0 || tailWarmupSize <= 0 {
		return false
	}
	tailStart := size - tailWarmupSize
	if tailStart < 0 {
		tailStart = 0
	}
	return off >= tailStart
}

// shouldRecordResumeOff reports whether a read may update the persisted resume position.
// PlaybackState is per-path, so before the single-primary invariant any handle touching the file
// could write here, and the monotonic guard it used to carry made the value a high-water mark
// that never came back down: a probe reading at 6431MB became the "resume position" for a player
// sitting at 1300MB, and the next session anchored the pump 5GB off (Lucky S01E04, 2026-08-22).
// Gating on the primary handle and dropping the monotonic guard records where the player
// actually is. The 2MB floor and the tail-probe ceiling still exclude header and Cues reads.
func shouldRecordResumeOff(healthy, isPrimary bool, off, size int64) bool {
	if !healthy || !isPrimary || size <= 0 {
		return false
	}
	return off > 2*1024*1024 && off < size-tailProbeZoneSize(size)
}

// resumeAnchorFromPersisted applies the tail-probe guard to a persisted resume
// position. Returns (anchor, zeroed): anchor is a valid pump start (0 when unusable),
// zeroed=true when the value lies in the tail-probe zone (or beyond EOF) and must be
// cleared - a single scanner probe reading MKV Cues near the end of a file must never
// look like a real playback position. Shared by the registry seeding and the DB
// fallback so both paths apply identical semantics.
func resumeAnchorFromPersisted(lastSeekOff, size int64) (anchor int64, zeroed bool) {
	if lastSeekOff > 2*1024*1024 && lastSeekOff < size-tailProbeZoneSize(size) {
		return lastSeekOff, false
	}
	if lastSeekOff >= size-tailProbeZoneSize(size) {
		return 0, true
	}
	return 0, false
}

// shouldInterruptPump decides whether a read should interrupt the pump. Reads served
// from the SSD tail warmup (shouldServeTailFromSSD) never touch the pump - Interrupt()
// + ResetShield() would only stall a pump the read does not need. The old isTailProbe
// gate was unreachable dead state (any read in the tail zone first transitions
// WARMUP→STREAMING), so a live scrub into the last 16MB interrupted the pump even
// though the SSD served the read instantly.
func shouldInterruptPump(prevOff, off, budget int64, tailServed bool) bool {
	if tailServed {
		return false
	}
	return shouldInterruptForSeek(prevOff, off, budget)
}

// primaryProofFor returns how many consecutive sequential reads a challenger must show before it
// may take primary. One landing within the read-ahead budget of the incumbent is the player moving
// normally (its own reopen lands right where it left off) and needs the base proof. One landing
// further away would drag the pump across the file, so it must sustain the streak far longer than
// a short-lived probe can: Lucky S01E04, 2026-08-22 saw handles attach at 5578/6069/6430MB, earn
// primary within 2-3s and be released immediately, each yanking the pump away from a player
// reading 2GB behind. A genuine seek by an open handle promotes through Read()'s seek path
// instead, so it never pays this. The extended proof is also the escape hatch that stops a wrong
// incumbent from wedging primary forever.
func primaryProofFor(challengerOff, incumbentOff, budget int64) int32 {
	if incumbentOff <= 0 || budget <= 0 {
		return primaryProofReads // no incumbent to displace
	}
	d := challengerOff - incumbentOff
	if d < 0 {
		d = -d
	}
	if d <= budget {
		return primaryProofReads
	}
	return primaryProofReads * 4
}

// primaryOffsetFor returns the offset of the current primary handle on path, or 0 if there is none.
func primaryOffsetFor(path string, except *MkvHandle) int64 {
	var off int64
	activeHandles.Range(func(key, _ interface{}) bool {
		other, ok := key.(*MkvHandle)
		if ok && other != except && other.path == path && other.isPrimaryHandle.Load() {
			if o := atomic.LoadInt64(&other.lastOff); o > off {
				off = o
			}
		}
		return true
	})
	return off
}

// becomePrimary makes h the only primary handle for its path, returning false if it already was.
// The pump follows the MAX of the primary offsets, so a probe that earned primary and then went
// idle kept dragging it to a frozen offset (Soulm8te, 2026-08-22: a stale PRI@8697MB anchored the
// pump 7.8GB past a player reading at 1120MB). Demotion is edge-triggered here at promotion time,
// never recomputed per pump tick: re-evaluating relevance every tick is what made the freshness
// heuristic in 6ae20c9 thrash (6517 jump pairs in 19h). Demoted handles lose their sequential
// streak too, so reclaiming primary costs a fresh primaryProofReads run and a frozen handle,
// which by definition stops reading, can never reclaim it at all.
func becomePrimary(h *MkvHandle) bool {
	if !h.isPrimaryHandle.CompareAndSwap(false, true) {
		return false
	}
	activeHandles.Range(func(key, _ interface{}) bool {
		other, ok := key.(*MkvHandle)
		if ok && other != h && other.path == h.path && other.isPrimaryHandle.CompareAndSwap(true, false) {
			other.seqAdvances.Store(0)
		}
		return true
	})
	return true
}

// pumpStrandedAhead reports whether the pump sits so far past the player that it can no longer
// serve it. nativePumpChunk's hard limit already parks a healthy pump at ~budget, so only a lead
// beyond 2x budget means a bad anchor rather than throttling. playerOff == 0 means no primary
// handle is registered right now (handle handoff), and must never drag the pump back to 0.
func pumpStrandedAhead(offset, playerOff, budget int64) bool {
	if playerOff <= 0 || budget <= 0 {
		return false
	}
	return offset > playerOff+2*budget
}

// shouldFireStarvationRevival decides whether the pump loop must self-revive: the
// pump is confirmed playback, its lead over the player is thin (< 16MB), and it is
// advancing below 75% of the player's consumption rate for the observation window.
// This replicates what a manual player seek does via V286b (Interrupt + ResetShield +
// fresh peer selection) without moving the player. Rates are bytes/second averaged
// over the 10s window by the caller. The lead gate protects the healthy hard-limit
// idle (lead = ReadAheadBudget): a pump with a thick lead is deliberately stopped,
// not starved (coincidentally the same 16MB as warmup.TailWarmupSize, but a
// different purpose: the watchdog lead, not the SSD tail coverage). Cooldown
// prevents revival storms.
func shouldFireStarvationRevival(confirmed bool, leadBytes int64, pumpRate, playerRate float64, cooldownAge time.Duration) bool {
	if !confirmed || cooldownAge < 60*time.Second {
		return false
	}
	if playerRate <= 0 || leadBytes >= 16*1024*1024 {
		return false
	}
	return pumpRate < playerRate*0.75
}

// seedResumeAnchor applies the tail-probe guard to a persisted resume position and,
// when valid, anchors the pump start there. ps is optional (nil on the DB fallback
// path). Returns true when the pump start was anchored. Zeroing a poisoned tail-probe
// value is a targeted UPDATE - it must never clobber the previous session's flags.
func (h *MkvHandle) seedResumeAnchor(ps *PlaybackState, lastSeekOff int64) bool {
	if lastSeekOff <= 2*1024*1024 {
		return false
	}
	anchor, zeroed := resumeAnchorFromPersisted(lastSeekOff, h.size)
	if anchor > 0 {
		atomic.StoreInt64(&h.lastOff, anchor)
		logger.Printf("[ResumeAnchor] Seeding pump start from persisted resume position: %.1fMB",
			float64(anchor)/(1<<20))
		return true
	}
	if zeroed {
		// Self-heal: a value in the tail zone (or beyond EOF) is a poisoned tail probe,
		// never a real resume position (a genuine near-end resume is served by the SSD
		// tail warmup, see shouldServeTailFromSSD, so it needs no pump anchor). Zero it
		// in memory + DB so it stops poisoning future pump starts.
		if ps != nil {
			ps.mu.Lock()
			ps.LastSeekOff = 0
			ps.mu.Unlock()
		}
		if stateDB != nil {
			if err := stateDB.UpdatePlaybackSeekOff(h.path, 0); err != nil {
				logger.Printf("[ResumeAnchor] Failed to zero poisoned last_seek_off for %s: %v", filepath.Base(h.path), err)
			} else {
				logger.Printf("[ResumeAnchor] Zeroed poisoned last_seek_off (%.1fMB in tail zone) for %s",
					float64(lastSeekOff)/(1<<20), filepath.Base(h.path))
			}
		}
	}
	return false
}

// forceTorrentWarmupActive marks hash/fileID as warmup-active unconditionally. Used at Open() for
// genuine cold starts (!headReady), before any WriteChunk has happened yet (and thus before
// DiskWarmup's writeWorker has had a chance to record the STARTING state itself), so aggressive
// PEX churn (Task 3) and the hedge watchdog (Task 4) catch the torrent's initial peer-connection
// burst instead of missing it. warmup.OnWarmupStateChange (wired in main() at startup) owns
// turning this back to false once the writeWorker actually observes STARTING/COMPLETED -
// synchronously, from inside processWrite itself, not by polling IsWarmingUp() from here (which
// would race ahead of the async write queue - WriteChunk only enqueues and returns immediately).
func forceTorrentWarmupActive(hash string, fileID int) {
	if warmup.DiskWarmup == nil {
		return
	}
	if tr := torr.PeekTorrent(hash); tr != nil && tr.Torrent != nil {
		tr.Torrent.SetWarmupActive(true, fileID)
	}
}

// safeGo runs a function in a new goroutine with panic recovery.
func safeGo(fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[PANIC] Background goroutine recovered: %v\n%s", r, debug.Stack())
			}
		}()
		fn()
	}()
}

// Compile-time interface checks for MkvHandle
var _ fs.FileReader = (*MkvHandle)(nil)
var _ fs.FileReleaser = (*MkvHandle)(nil)

// cueHandle serves one cue track of a single-file image: the generated header from
// memory, then the image's frames through an ordinary MkvHandle whose file ends
// where the track ends. MkvHandle itself is untouched.
type cueHandle struct {
	inner  *MkvHandle
	header []byte
	base   int64 // where the track's frames start in the torrent file
	length int64 // image bytes the track spans
}

var _ fs.FileReader = (*cueHandle)(nil)
var _ fs.FileReleaser = (*cueHandle)(nil)

// openCueTrack opens the image through the regular path, sized to end at the end of
// the track, and wraps it.
func (n *VirtualMkvNode) openCueTrack(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	length := n.vMeta.Size - int64(len(n.vMeta.SegmentHeader))
	if length <= 0 {
		return nil, 0, syscall.EIO
	}
	inner := *n.vMeta
	inner.Size = n.vMeta.SegmentOffset + length
	inner.SegmentHeader = nil
	fh, fuseFlags, errno := (&VirtualMkvNode{vMeta: &inner, wake: n.wake}).Open(ctx, flags)
	if errno != 0 {
		return nil, 0, errno
	}
	h, ok := fh.(*MkvHandle)
	if !ok {
		return nil, 0, syscall.EIO
	}
	return &cueHandle{inner: h, header: n.vMeta.SegmentHeader, base: n.vMeta.SegmentOffset, length: length}, fuseFlags, 0
}

func (h *cueHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	hdrFrom, hdrTo, innerOff, innerN := vfs.SegmentRead(len(h.header), h.base, h.length, off, len(dest))
	n := copy(dest, h.header[hdrFrom:hdrTo])
	if innerN > 0 {
		want := dest[n : n+innerN]
		res, errno := h.inner.Read(ctx, want, innerOff)
		if errno != 0 {
			// Never the header alone: a short read away from EOF is EOF to the
			// kernel, and a cold torrent would truncate the track for the player.
			return nil, errno
		}
		data, _ := res.Bytes(want)
		got := copy(dest[n:], data)
		res.Done()
		if got < innerN {
			return nil, syscall.EIO
		}
		n += got
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (h *cueHandle) Release(ctx context.Context) syscall.Errno {
	return h.inner.Release(ctx)
}

// shortAwayFromEOF reports whether a read of n bytes into a want-sized buffer at off leaves a
// gap before end of file. Short AT eof is normal; short before it is what the kernel turns
// into a cached zero-filled page.
func shortAwayFromEOF(n, want int, off, size int64) bool {
	return n < want && off+int64(n) < size
}

// Read never hands the kernel a short read away from EOF. FUSE here runs on the kernel page
// cache (no direct_io), and Linux zero-fills the tail of a short read and marks the page
// uptodate — that region would then serve zeros for the life of the mount. Missing bytes are
// refetched; if they still cannot be produced, EIO is returned, which the kernel does not
// cache, so the read is retried instead of a hole being made permanent.
func (h *MkvHandle) Read(fuseCtx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	res, errno := h.readInner(fuseCtx, dest, off)
	if errno != 0 || res == nil {
		return res, errno
	}

	total := res.Size()
	if !shortAwayFromEOF(total, len(dest), off, h.size) {
		return res, 0
	}

	// Short away from EOF: every readInner return site writes into dest, so the bytes
	// already gathered stay valid and only the tail needs filling.
	fuseShortReadCount.Add(1)
	deadline := time.Now().Add(shortReadRefillBudget)

	for attempt := 0; attempt < shortReadRefillAttempts && shortAwayFromEOF(total, len(dest), off, h.size); attempt++ {
		if fuseCtx.Err() != nil {
			return nil, syscall.EINTR
		}
		if time.Now().After(deadline) {
			break // a blocked FUSE read puts smbd in D-state; bail out rather than hang
		}
		sub, subErrno := h.readInner(fuseCtx, dest[total:], off+int64(total))
		if subErrno != 0 || sub == nil {
			break
		}
		m := sub.Size()
		if m == 0 {
			break
		}
		total += m
	}

	if shortAwayFromEOF(total, len(dest), off, h.size) {
		fuseShortReadFailed.Add(1)
		logger.Printf("[ShortRead] Unfillable %d/%d bytes at offset %d for %s - EIO (a partial read would cache zeros)",
			total, len(dest), off, filepath.Base(h.path))
		ttffReadFailed(h.path)
		return nil, syscall.EIO
	}

	fuseShortReadRepaired.Add(1)
	return fuse.ReadResultData(dest[:total]), 0
}

// writeWarmupFromFetch mirrors a fetched block into the SSD warmup files: the head window
// always, the tail only until playback is confirmed (the frozen tail is the discovery snapshot).
// Runs off the read path, from FetchAhead's completion callback.
func writeWarmupFromFetch(h *MkvHandle, off int64, data []byte) {
	if !h.usesWarmup || warmup.DiskWarmup == nil || h.hash == "" || len(data) == 0 {
		return
	}
	if off <= warmup.FileSize {
		warmup.DiskWarmup.WriteChunk(h.hash, h.fileID, data, off)
		return
	}
	if h.size <= warmup.TailWarmupSize || off < h.size-warmup.TailWarmupSize {
		return
	}
	if val, ok := playbackRegistry.Load(h.path); ok {
		ps := val.(*PlaybackState)
		ps.mu.RLock()
		confirmed := !ps.ConfirmedAt.IsZero()
		ps.mu.RUnlock()
		if confirmed {
			return
		}
	}
	warmup.DiskWarmup.WriteTail(h.hash, h.fileID, data, off, h.size)
}

func (h *MkvHandle) readInner(fuseCtx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	now := time.Now()
	timing := &ReadTiming{StartTime: now}
	defer func() {
		timing.TotalTime = time.Since(timing.StartTime)
		globalProfilingStats.RecordRead(timing)
	}()

	if off >= h.size {
		return fuse.ReadResultData(nil), 0
	}

	h.mu.Lock()
	// Late hash recovery: if Open() failed to resolve (metadata lag), retry now.
	if h.hash == "" && h.url != "" {
		if hash, fileID, err := resolveTargetFile(h.url, h.size, h.path); err == nil {
			h.hash = hash
			h.fileID = fileID
			logger.Printf("[LateResolution] Recovered hash for %s: %s", filepath.Base(h.path), h.hash[:8])
			go h.pumpOnce.Do(func() {
				h.startNativePump(h.hash, h.fileID)
			})
		}
	}

	idleTime := now.Sub(h.lastActivityTime)
	isFirstBlock := (off == 0) || (idleTime > time.Duration(gc().WarmStartIdleSeconds)*time.Second)
	h.lastActivityTime = now

	// V750: Update PlaybackState inference tracking
	if val, ok := playbackRegistry.Load(h.path); ok {
		if ps, ok := val.(*PlaybackState); ok {
			ps.mu.Lock()
			ps.ReadCount++
			ps.LastReadAt = now
			// Tail-zone reads (MKV Cues probes) and unconfirmed reads (Plex background scans)
			// must never count as real playback progress here.
			if shouldRecordResumeOff(ps.IsHealthy, h.isPrimaryHandle.Load(), off, h.size) {
				ps.LastSeekOff = off
			}
			ps.mu.Unlock()
		}
	}

	if now.Sub(h.lastGlobalUpdate) > 1*time.Minute {
		globalCleanupManager.UpdateActivity(h.path)
		h.lastGlobalUpdate = now
	}
	h.mu.Unlock()

	prevOff := atomic.LoadInt64(&h.lastOff)
	atomic.StoreInt64(&h.lastOff, off)

	h.promoteIfStreaming(prevOff, off, len(dest))

	// Transition WARMUP→STREAMING on resume (first read >= warmup.FileSize), seek (jump > budget),
	// or plain sequential progression past the warmup zone (no seek ever happens during linear
	// playback from offset 0 - without this case, h.state stayed stuck in stateWarmup for the
	// entire rest of playback, paying GetAvailableRange's uncached filepath.Join/Clean cost on
	// every single read instead of the cheap steady-state path; found via CPU profiling a 4K HDR
	// stream where this uncached path work reached ~29% of total CPU).
	// Checked after SSD path above so initial reads within warmup zone are still served.
	if h.state.Load() == stateWarmup {
		isSeek := false
		naturalProgression := false
		if prevOff == -1 {
			if off >= warmup.FileSize {
				isSeek = true
			}
		} else if off != 0 {
			budget := int64(gc().ReadAheadBudget)
			if off > prevOff+budget || prevOff > off+budget {
				isSeek = true
			} else if off >= warmup.FileSize {
				naturalProgression = true
			}
		}
		if isSeek || naturalProgression {
			h.state.Store(stateStreaming)
			if naturalProgression {
				logger.Printf("[Warmup] Sequential read past warmup zone (off=%dMB): %s→%s.", off/(1024*1024), stateName(stateWarmup), stateName(stateStreaming))
			} else {
				logger.Printf("[Warmup] Seek/Resume detected (off=%dMB): %s→%s.", off/(1024*1024), stateName(stateWarmup), stateName(stateStreaming))
			}

			// WarmLanding on new handle: pre-fetch chunk at resume position while pump catches
			// up. Fires only for new handles (prevOff==-1) since V286b covers existing handles.
			if prevOff == -1 && h.hash != "" {
				warmChunk := raCache.ChunkSize(h.path)
				warmStart := (off / warmChunk) * warmChunk
				// Unified with predictive-prefetch key format to avoid double-fetching the same chunk.
				warmKey := fmt.Sprintf("%s:%d", h.path, warmStart)
				if _, loaded := inFlightPrefetches.LoadOrStore(warmKey, true); !loaded {
					// Non-blocking slot check before spawning: under a saturated semaphore (e.g.
					// a Plex scan burst touching hundreds of files), spawning first and waiting
					// inside the goroutine meant a goroutine was created only to sit blocked for
					// up to 300ms doing nothing. A `default` check costs microseconds and avoids
					// the spawn entirely when there's no room, without blocking this Read() call.
					select {
					case masterDataSemaphore <- struct{}{}:
						goOff, goKey, goHash, goFileID, goSize := warmStart, warmKey, h.hash, h.fileID, h.size
						safeGo(func() {
							defer inFlightPrefetches.Delete(goKey)
							defer func() { <-masterDataSemaphore }()
							fetchEnd := goOff + warmChunk - 1
							if fetchEnd >= goSize {
								fetchEnd = goSize - 1
							}
							if fetchEnd <= goOff {
								return
							}
							bufPtr := readBufferPool.Get().(*[]byte)
							defer readBufferPool.Put(bufPtr)
							limit := int64(len(*bufPtr))
							if fetchEnd-goOff+1 < limit {
								limit = fetchEnd - goOff + 1
							}
							n, err := nativeBridge.FetchBlock(goHash, goFileID, goOff, (*bufPtr)[:limit])
							if err == nil && n > 0 {
								raCache.Put(h.path, goOff, goOff+int64(n)-1, (*bufPtr)[:n])
								logger.Printf("[WarmLanding] Pre-fetched chunk at %dMB for new handle", goOff/(1024*1024))
							}
						})
					default:
						inFlightPrefetches.Delete(warmKey)
					}
				}
			}
		}
	}

	// Interrupt pump on genuine seeks; skip reads served from the SSD tail warmup (the
	// pump must stay alive for the rest of the file, and the tail read is already served).
	// V286b: interruptPending prevents cascade — when multiple handles share a pump,
	// only the first to detect the seek fires Interrupt(); others skip until the pump
	// restarts and resets the flag (preventing the thrash loop seen in the logs).
	budget := int64(gc().ReadAheadBudget)
	seekReader := h.nativeReader // capture: a concurrent Release nils the field
	if seekReader != nil && shouldInterruptPump(prevOff, off, budget, shouldServeTailFromSSD(off, h.size, warmup.TailWarmupSize)) {
		// Promote to primary on a genuine seek once playback is confirmed/inferred - mirrors the
		// refCount 0->1 "primary reconnect" promotion at attach time (main.go:1030): a secondary
		// (attached) handle doing a real seek during confirmed playback IS the player, and needs
		// isPrimaryHandle so the pump loop's playerOff sync and Release's ps.playerOff
		// persistence (see nativePump) pick it up instead of only the original primary handle,
		// which may be an idle stale probe by now.
		if !h.isPrimaryHandle.Load() {
			if val, ok := playbackRegistry.Load(h.path); ok {
				if pbs, ok := val.(*PlaybackState); ok && (pbs.GetStatus() || pbs.IsInferredPlayback()) {
					becomePrimary(h)
				}
			}
		}
		var ps *NativePumpState
		if val, ok := activePumps.Load(h.path); ok {
			ps = val.(*NativePumpState)
		}
		if ps == nil || !ps.interruptPending.Swap(true) {
			seekReader.Interrupt()
			torrstor.ResetShield()
			h.state.Store(stateStreaming)
			logger.Printf("[V286b] Interrupt pump for seek+shield reset: %dMB → %dMB (%s→%s)",
				prevOff/(1024*1024), off/(1024*1024), stateName(stateWarmup), stateName(stateStreaming))

			// Warm landing zone: pre-fetch the first chunk at the seek target while the
			// pump sleeps its 200ms retry. Starts anacrolix piece download ~200ms earlier,
			// reducing FetchBlock blocking time in Read() and preventing smbd D-state.
			if h.hash != "" {
				warmChunk := raCache.ChunkSize(h.path)
				warmStart := (off / warmChunk) * warmChunk
				// Unified with predictive-prefetch key format to avoid double-fetching the same chunk.
				warmKey := fmt.Sprintf("%s:%d", h.path, warmStart)
				if _, loaded := inFlightPrefetches.LoadOrStore(warmKey, true); !loaded {
					// See the matching comment on the other WarmLanding site above: non-blocking
					// slot check before spawning avoids creating a goroutine that just blocks.
					select {
					case masterDataSemaphore <- struct{}{}:
						goOff, goKey, goHash, goFileID, goSize := warmStart, warmKey, h.hash, h.fileID, h.size
						safeGo(func() {
							defer inFlightPrefetches.Delete(goKey)
							defer func() { <-masterDataSemaphore }()
							fetchEnd := goOff + warmChunk - 1
							if fetchEnd >= goSize {
								fetchEnd = goSize - 1
							}
							if fetchEnd <= goOff {
								return
							}
							bufPtr := readBufferPool.Get().(*[]byte)
							defer readBufferPool.Put(bufPtr)
							limit := int64(len(*bufPtr))
							if fetchEnd-goOff+1 < limit {
								limit = fetchEnd - goOff + 1
							}
							n, err := nativeBridge.FetchBlock(goHash, goFileID, goOff, (*bufPtr)[:limit])
							if err == nil && n > 0 {
								raCache.Put(h.path, goOff, goOff+int64(n)-1, (*bufPtr)[:n])
								logger.Printf("[WarmLanding] Pre-fetched chunk at %dMB for seek target", goOff/(1024*1024))
							}
						})
					default:
						inFlightPrefetches.Delete(warmKey)
					}
				}
			}
		}
	}

	// Serve warmup zone from SSD (up to 80MB with boundary chunk); stateWarmup gate skips SSD on resume/seek.
	if h.usesWarmup && warmup.DiskWarmup != nil && h.hash != "" && h.state.Load() == stateWarmup {
		warmupCoverage := warmup.DiskWarmup.GetAvailableRange(h.hash, h.fileID)
		if off < warmupCoverage {
			n, _ := warmup.DiskWarmup.ReadAt(h.hash, h.fileID, dest, off)
			if n > 0 {
				timing.UsedCache = true
				timing.BytesRead = n
				if off == 0 {
					logger.Printf("[DiskWarmup] HIT %s off=0 (%dKB)", filepath.Base(h.path), n/1024)
				}
				atomic.StoreInt64(&h.lastOff, off)

				h.mu.Lock()
				h.lastLen = n
				h.lastTime = now
				h.mu.Unlock()
				ttffRead(h.path, srcWarmupHead, time.Since(now), n, off)
				return fuse.ReadResultData(dest[:n]), 0
			}
		}
	}

	// Serve tail from SSD whenever the read lands in the tail warmup coverage, regardless
	// of handle state: the WARMUP→STREAMING seek/resume transition above runs before any
	// state-gated tail check could - a resume in the last 16MB was served by network
	// FetchBlock instead of the complete SSD tail file (~16s TTFF on a 36.6GB remux,
	// 2026-08-06). ReadTail is range-safe: returns (0,nil) when the tail file's coverage
	// doesn't include the offset.
	if h.usesWarmup && warmup.DiskWarmup != nil && shouldServeTailFromSSD(off, h.size, warmup.TailWarmupSize) {
		n, _ := warmup.DiskWarmup.ReadTail(h.hash, h.fileID, dest, off, h.size)
		if n > 0 {
			timing.UsedCache = true
			timing.BytesRead = n
			h.mu.Lock()
			h.lastLen, h.lastTime = n, now
			h.mu.Unlock()
			ttffRead(h.path, srcWarmupTail, time.Since(now), n, off)
			return fuse.ReadResultData(dest[:n]), 0
		}

		// On SSD tail miss, use stateless FetchBlock to preserve head pump.
		nFetch, err := nativeBridge.FetchBlock(h.hash, h.fileID, off, dest)
		if err == nil && nFetch > 0 {
			if h.usesWarmup && warmup.DiskWarmup != nil {
				warmup.DiskWarmup.WriteTail(h.hash, h.fileID, dest[:nFetch], off, h.size)
			}

			timing.UsedCache = false
			timing.BytesRead = nFetch
			h.mu.Lock()
			h.lastLen, h.lastTime = nFetch, now
			h.mu.Unlock()
			ttffRead(h.path, srcFetchBlock, time.Since(now), nFetch, off)
			return fuse.ReadResultData(dest[:nFetch]), 0
		}
	}
	end := off + int64(len(dest)) - 1

	cacheStart := time.Now()
	if n := raCache.CopyTo(h.path, off, end, dest); n > 0 {
		timing.CacheHitTime = time.Since(cacheStart)
		timing.UsedCache = true
		timing.BytesRead = n

		atomic.StoreInt64(&h.lastOff, off)

		// Predictive prefetch: fetch next chunk if pump is absent or near boundary.
		chunkSize := raCache.ChunkSize(h.path)
		nextChunkStart := (off/chunkSize + 1) * chunkSize

		if (!h.hasSlot.Load() || (nextChunkStart-off < chunkSize/4)) && !raCache.Exists(h.path, nextChunkStart) {
			prefetchKey := fmt.Sprintf("%s:%d", h.path, nextChunkStart)
			if _, loaded := inFlightPrefetches.LoadOrStore(prefetchKey, true); !loaded {
				goStart, goKey, goHash, goFileID := nextChunkStart, prefetchKey, h.hash, h.fileID
				fetchEnd := goStart + chunkSize - 1
				if fetchEnd >= h.size {
					fetchEnd = h.size - 1
				}
				// Non-blocking slot check before spawning: avoids creating a goroutine that just
				// blocks up to 500ms waiting on a saturated semaphore during a Plex scan burst
				// (same reasoning as the WarmLanding sites above).
				if fetchEnd <= goStart {
					inFlightPrefetches.Delete(prefetchKey)
				} else {
					select {
					case masterDataSemaphore <- struct{}{}:
						safeGo(func() {
							defer inFlightPrefetches.Delete(goKey)
							defer func() { <-masterDataSemaphore }()

							if goHash != "" {
								bufPtr := readBufferPool.Get().(*[]byte)
								defer readBufferPool.Put(bufPtr)

								limit := int64(len(*bufPtr))
								if fetchEnd-goStart+1 < limit {
									limit = fetchEnd - goStart + 1
								}

								n, err := nativeBridge.FetchBlock(goHash, goFileID, goStart, (*bufPtr)[:limit])
								if err == nil && n > 0 {
									raCache.Put(h.path, goStart, goStart+int64(n)-1, (*bufPtr)[:n])
								}
							}
							// HTTP Fallback REMOVED
						})
					default:
						inFlightPrefetches.Delete(prefetchKey)
					}
				}
			}
		}

		h.mu.Lock()
		h.lastLen = n
		h.lastTime = now
		h.mu.Unlock()

		ttffRead(h.path, srcRACacheHit, time.Since(cacheStart), n, off)
		return fuse.ReadResultData(dest[:n]), 0
	}

	target := int(end - off + 1)
	isSeq := (off == h.lastOff+int64(h.lastLen)) || (h.lastOff >= 0 && abs(off-(h.lastOff+int64(h.lastLen))) <= gc().SequentialTolerance)
	isStreaming := (len(dest) >= int(gc().StreamingThreshold)) || isSeq
	timing.IsStreaming = isStreaming

	fetchEnd := end
	var fetchSize int64 = int64(target)
	if isStreaming {
		raSize := int64(gc().ReadAheadBase)

		if isFirstBlock {
			raSize = int64(gc().ReadAheadInitial)
		}

		fetchEnd = off + raSize - 1
		if fetchEnd >= h.size {
			fetchEnd = h.size - 1
		}
		fetchSize = fetchEnd - off + 1
	}
	h.mu.Lock()
	h.lastLen, h.lastTime = len(dest), now
	h.mu.Unlock()
	atomic.StoreInt64(&h.lastOff, off)

	if !h.hasSlot.Load() {
		pumpCreationMu.Lock()

		// pumpCreationMu serializes attaches on this path: a concurrent read on this same
		// handle may have attached while we waited, and a double attach would pin the pump in
		// refCount. The hasSlot write itself is guarded by h.mu inside attachToExistingPump.
		if h.hasSlot.Load() {
			pumpCreationMu.Unlock()
			goto pumpSlotResolved
		}
		// Attach to existing pump if one is already running for this path.
		if val, ok := activePumps.Load(h.path); ok {
			ps := val.(*NativePumpState)
			if h.attachToExistingPump(ps) {
				logger.Printf("[V258] Handle ATTACHED to existing active pump (Refs: %d): %s", atomic.LoadInt32(&ps.refCount), filepath.Base(h.path))
			}
		}
		// Unlock early if attached or not needed
		if h.hasSlot.Load() {
			pumpCreationMu.Unlock()
		} else {
			// On-the-fly pump upgrade for confirmed playback with available slot.
			if isStreaming && h.hash != "" {
				if val, ok := playbackRegistry.Load(h.path); ok {
					if ps, ok := val.(*PlaybackState); ok && (ps.GetStatus() || ps.IsInferredPlayback()) {
						select {
						case masterDataSemaphore <- struct{}{}:
							h.hasSlot.Store(true)
							upReader := nativeBridge.NewStreamReader(h.hash, h.fileID, h.size)
							h.nativeReader = upReader
							pumpCtx, pumpCancel := context.WithCancel(context.Background())
							h.pumpCancel = pumpCancel

							sharedState := &NativePumpState{
								cancel:   pumpCancel,
								reader:   upReader,
								path:     h.path,
								refCount: 1,
							}
							activePumps.Store(h.path, sharedState)
							h.pumpState = sharedState
							globalOpenTracker.Inc(h.hash, h.path)

							logger.Printf("[Pump] Upgraded on-the-fly for confirmed playback: %s", filepath.Base(h.path))

							hHash := metainfo.NewHashFromHex(h.hash)
							if t := web.BTS.GetTorrent(hHash); t != nil {
								t.SetAggressiveMode(true, GetEffectiveConcurrencyLimit())
								logger.Printf("[Pump] Aggressive mode enabled on-the-fly for: %s", h.hash[:8])
							}

							upgradedState := sharedState
							safeGo(func() {
								h.nativePump(pumpCtx, upReader, off, upgradedState)
							})
						default:
							// Reserve full, stay in burst mode for now
						}
					}
				}
			}
			pumpCreationMu.Unlock() // Final unlock
		}
	}

pumpSlotResolved:
	// If still no slot (scan or reserve full), acquire a temporary slot for this read
	if !h.hasSlot.Load() {
		select {
		case masterDataSemaphore <- struct{}{}:
			defer func() { <-masterDataSemaphore }()
		case <-fuseCtx.Done():
			return nil, syscall.EINTR
		case <-time.After(30 * time.Second):
			logger.Printf("[MasterSemaphore] Timeout waiting for slot: %s", filepath.Base(h.path))
			return nil, syscall.ETIMEDOUT
		}
	}

	// Rate limiting for non-streaming (metadata) requests only; streaming bypasses to preserve playback priority.
	if !isStreaming {
		rateLimitCtx, rateLimitCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer rateLimitCancel()
		if err := globalRateLimiter.Acquire(rateLimitCtx); err != nil {
			logger.Printf("Rate limit timeout: %v", err)
			return nil, vfs.ToErrno(err)
		}
	}

	// Queueing (semaphore/rate-limiter) is not cache service time — re-stamp.
	cacheStart = time.Now()
	if n := raCache.CopyTo(h.path, off, end, dest); n > 0 {
		atomic.StoreInt64(&h.lastOff, off)
		h.mu.Lock()
		h.lastLen = n
		h.lastTime = now
		h.mu.Unlock()
		ttffRead(h.path, srcRACacheHit, time.Since(cacheStart), n, off)
		return fuse.ReadResultData(dest[:n]), 0
	}

	// FALLBACK: Data Fetch with V265 Retry
	// If cache miss, use FetchAhead. Retry up to 3 times if torrent not ready (async Wake).
	var n int

	// releaseFlight is set when this read leads the singleflight below. The read-ahead window
	// keeps filling after this function returns, so the flight is released from the fill's
	// completion callback (once the cache is populated) instead of on return - a follower woken
	// earlier would find nothing cached and start a duplicate fetch of the same chunk.
	releaseFlight := func() {}
	fetchHandedOff := false

	// Dedup concurrent misses in the same chunk: leader fetches, followers wait and copy.
	if h.hash != "" {
		flightChunk := raCache.ChunkSize(h.path)
		flightKey := fmt.Sprintf("%s:%d", h.path, (off/flightChunk)*flightChunk)
		newFlight := &fetchFlight{done: make(chan struct{})}
		if val, loaded := inFlightFetches.LoadOrStore(flightKey, newFlight); loaded {
			fl := val.(*fetchFlight)
			copyStart := time.Now()
			// The leader re-caches its window as it fills, so poll instead of waiting for the
			// whole window: a follower one FUSE block behind the leader gets its bytes after one
			// fill step, not after the remaining 15MB.
			leaderDone := false
			for {
				if nCopy := raCache.CopyTo(h.path, off, end, dest); nCopy > 0 {
					fetchFlightDedupCount.Add(1)
					timing.UsedCache = true
					timing.BytesRead = nCopy
					atomic.StoreInt64(&h.lastOff, off)
					h.mu.Lock()
					h.lastLen = nCopy
					h.lastTime = now
					h.mu.Unlock()
					ttffRead(h.path, srcRACacheHit, time.Since(copyStart), nCopy, off)
					return fuse.ReadResultData(dest[:nCopy]), 0
				}
				if leaderDone {
					break // leader finished without covering our range
				}
				select {
				case <-fl.done:
					leaderDone = true // one more CopyTo, then fall through
				case <-fuseCtx.Done():
					return nil, syscall.EINTR
				case <-time.After(20 * time.Millisecond):
				}
			}
			// Leader failed or missed our range — fetch directly, don't re-register.
		} else {
			var once sync.Once
			releaseFlight = func() {
				once.Do(func() {
					inFlightFetches.Delete(flightKey)
					close(newFlight.done)
				})
			}
			defer func() {
				if !fetchHandedOff {
					releaseFlight()
				}
			}()
		}
	}

	if h.hash != "" {
		// V753: the very first read of a session can miss raCache/warmup even when
		// headReady was already true at Open() (partial coverage from a prior open),
		// which skips forceTorrentWarmupActive there (main.go ~916, gated on
		// !headReady). That leaves hedgeWatchdog not running yet - playback-pressure
		// hasn't engaged either, since the pump has barely started - so this exact
		// fetch has zero hedge protection. Force it here instead, bounded to the
		// warmup head window: forcing it for a post-idle resume deep in the file
		// (isFirstBlock is also true after WarmStartIdleSeconds) would set
		// warmupActive with no way back to false, since processWrite's off>FileSize
		// early return (warmup.go) means COMPLETED - the only call site that clears
		// it - never fires for writes past the head window.
		if h.usesWarmup && isFirstBlock && off <= warmup.FileSize {
			forceTorrentWarmupActive(h.hash, h.fileID)
		}
		fetchStart := time.Now()

		for attempt := 0; attempt < 3; attempt++ {
			bufPtr := readBufferPool.Get().(*[]byte)

			limit := fetchSize
			if limit > int64(len(*bufPtr)) {
				limit = int64(len(*bufPtr))
			}
			buf := (*bufPtr)[:limit]

			// The window keeps filling after this read returns. Re-cache on every step so the
			// reads queued behind this one find their bytes as they land instead of waiting for
			// the whole window; the buffer goes back to the pool only once the fill is done.
			nFetch, err := nativeBridge.FetchAhead(h.hash, h.fileID, off, buf, dest[:target],
				func(total int, done bool, _ error) {
					if total > 0 {
						raCache.Put(h.path, off, off+int64(total)-1, buf[:total])
					}
					if !done {
						return
					}
					if total > 0 {
						writeWarmupFromFetch(h, off, buf[:total])
					}
					releaseFlight()
					readBufferPool.Put(bufPtr)
				})
			if err == nil && nFetch > 0 {
				n = nFetch
				fetchHandedOff = true
				timing.HTTPFetchTime = time.Since(fetchStart)
				goto DATA_READY
			}
			if attempt < 2 {
				select {
				case <-fuseCtx.Done():
					return nil, syscall.EINTR
				case <-time.After(500 * time.Millisecond):
				}
			}
		}
	}

	// Terminal once the bounded retries are spent. EAGAIN reads as "try again" to
	// a scanner, which then blocks or loops on bytes that are never coming.
	ttffReadFailed(h.path)
	return nil, syscall.EIO

DATA_READY:

	timing.BytesRead = target

	if n > 0 {
		// The bytes are already in dest and the read-ahead window behind them is still
		// filling; raCache.Put and the SSD warmup writes happen in FetchAhead's callback.
		ttffRead(h.path, srcFetchBlock, timing.HTTPFetchTime, n, off)

		// Update sequential detection state
		atomic.StoreInt64(&h.lastOff, off)
		h.mu.Lock()
		h.lastLen = target
		h.mu.Unlock()

		globalCleanupManager.UpdateOffset(h.path, off, target)

		// Prefetch next chunk if in last 25% of current chunk and pump is absent or lagging.
		chunkSize := raCache.ChunkSize(h.path)
		currentChunkIndex := off / chunkSize
		nextChunkStart := (currentChunkIndex + 1) * chunkSize
		distanceToNext := nextChunkStart - off

		maxCached := raCache.MaxCachedOffset(h.path)
		isLagging := maxCached < nextChunkStart

		if isStreaming && (!h.hasSlot.Load() || isLagging) {
			if distanceToNext < chunkSize/4 {
				prefetchKey := fmt.Sprintf("%s:%d", h.path, nextChunkStart)
				if _, loaded := inFlightPrefetches.LoadOrStore(prefetchKey, true); !loaded {
					goStart, goSize, goKey, goHash, goFileID := nextChunkStart, chunkSize, prefetchKey, h.hash, h.fileID
					safeGo(func() {
						defer inFlightPrefetches.Delete(goKey)

						// Check if already in cache to avoid useless work
						if raCache.Exists(h.path, goStart) {
							return // Already cached
						}

						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						if err := globalRateLimiter.Acquire(ctx); err != nil {
							return
						}

						fetchEnd := goStart + goSize - 1
						if fetchEnd >= h.size {
							fetchEnd = h.size - 1
						}
						if fetchEnd <= goStart {
							return
						}

						select {
						case masterDataSemaphore <- struct{}{}:
							defer func() { <-masterDataSemaphore }()
						default:
							return // Skip if pool is saturated
						}

						// Zero-Network Native Prefetch (V227 Phase 7)
						if goHash != "" {
							bufPtr := readBufferPool.Get().(*[]byte)
							defer readBufferPool.Put(bufPtr)

							limit := int64(len(*bufPtr))
							if fetchEnd-goStart+1 < limit {
								limit = fetchEnd - goStart + 1
							}

							n, err := nativeBridge.FetchBlock(goHash, goFileID, goStart, (*bufPtr)[:limit])
							if err == nil && n > 0 {
								raCache.Put(h.path, goStart, goStart+int64(n)-1, (*bufPtr)[:n])
							}
							return
						}

						// HTTP Fallback REMOVED
					})
				}
			}
		}

		return fuse.ReadResultData(dest[:n]), 0
	}

	return fuse.ReadResultData(dest[:0]), 0
}

// ownsPumpState reports whether a releasing handle still owns the pump registered
// for its path. activePumps is keyed by path and the state is replaced wholesale,
// so a handle that outlived its pump would otherwise decrement a count it never
// incremented, driving refCount negative and firing the "last handle closed"
// teardown while a live handle is still reading.
func ownsPumpState(handleState, registered *NativePumpState) bool {
	return handleState != nil && handleState == registered
}

func (h *MkvHandle) Release(fuseCtx context.Context) syscall.Errno {
	ttffReleaseClose(h.path)
	logger.Printf("=== RELEASE VIRTUAL === path=%s", h.path)

	// Delete from activeHandles here, not at the end: the early returns below (probe
	// without slot, replaced pump) would otherwise leak this entry, and both the live
	// handle checks (anyLiveHandleFor*) and cleanup's activePaths trust the map.
	activeHandles.Delete(h)

	if val, ok := activePumps.Load(h.path); ok {
		ps := val.(*NativePumpState)
		// Only primary handles persist position; secondary probes have arbitrary offsets.
		if h.isPrimaryHandle.Load() {
			if pos := atomic.LoadInt64(&h.lastOff); pos > 0 {
				atomic.StoreInt64(&ps.playerOff, pos)
			}
		}
		// Only decrement if this handle acquired a slot; probe/header reads must not decrement.
		if !h.hasSlot.Load() {
			return 0
		}
		globalOpenTracker.Dec(h.hash, h.path)
		if !ownsPumpState(h.pumpState, ps) {
			logger.Printf("[Pump] Stale handle for %s: pump was replaced, refCount left alone", filepath.Base(h.path))
			return 0
		}
		newRefs := atomic.AddInt32(&ps.refCount, -1)
		logger.Printf("[Pump] Release handle for %s (Remaining Refs: %d)", filepath.Base(h.path), newRefs)

		if newRefs <= 0 {
			// V306: If playback is healthy (webhook media.play confirmed), skip grace period.
			// The pump stays alive until media.stop webhook sets IsHealthy=false and kills it.
			// This survives long pauses, buffering gaps, and Apple TV re-reads without
			// killing the pump and causing freeze on resume.
			if pbVal, ok := playbackRegistry.Load(h.path); ok {
				if pbState := pbVal.(*PlaybackState); pbState.IsHealthy {
					logger.Printf("[V306] Healthy playback — pump stays alive (no grace period) for %s", filepath.Base(h.path))
					// Stop any pending grace timer from a previous release cycle
					if oldTimer, ok := pumpTimers.LoadAndDelete(h.path); ok {
						oldTimer.(*time.Timer).Stop()
					}
					goto skipGrace
				}
			}

			// Grace period: 30s for unconfirmed probes/scans.
			graceDuration := 30 * time.Second
			if pbVal, ok := playbackRegistry.Load(h.path); ok {
				if pbState := pbVal.(*PlaybackState); !pbState.ConfirmedAt.IsZero() {
					graceDuration = 90 * time.Second
					if pbState.IsInferredPlayback() {
						graceDuration = 5 * time.Minute // V750
						logger.Printf("[V750] Inferred playback — extended grace (5m) for %s", filepath.Base(h.path))
					}
				} else if pbState.IsInferredPlayback() {
					graceDuration = 5 * time.Minute // V750
					logger.Printf("[V750] Inferred playback — extended grace (5m) for %s", filepath.Base(h.path))
				}
			}
			if oldTimer, ok := pumpTimers.LoadAndDelete(h.path); ok {
				oldTimer.(*time.Timer).Stop()
			}
			var t *time.Timer
			t = time.AfterFunc(graceDuration, func() {
				pumpTimers.CompareAndDelete(h.path, t)
				if val, ok := activePumps.Load(h.path); ok {
					psNow := val.(*NativePumpState)
					if atomic.LoadInt32(&psNow.refCount) <= 0 {
						if psNow.cancel != nil {
							psNow.cancel()
						}
						activePumps.Delete(h.path)
						logger.Printf("[V420] Shared Pump Terminated (Grace Period Expired): %s", filepath.Base(h.path))
					}
				}
			})
			pumpTimers.Store(h.path, t)
			logger.Printf("[V420] Last handle closed: Shared Pump entering %s grace period for %s", graceDuration, filepath.Base(h.path))
		}

	skipGrace:
	}

	// Nil local reference only; pump goroutine owns the reader lifecycle via captured copy.
	h.nativeReader = nil

	// Fast-drop (5s) for scanner probes never confirmed by webhook; 30s otherwise.
	retentionDelay := 30 * time.Second
	lastOffset := atomic.LoadInt64(&h.lastOff)
	isProbeOnly := lastOffset < 2*1024*1024 // < 2MB = probe/scanner, not real playback
	if h.hasWarmup && isProbeOnly {
		if val, ok := playbackRegistry.Load(h.path); ok {
			state := val.(*PlaybackState)
			state.mu.RLock()
			stopped := state.IsStopped
			everConfirmed := !state.ConfirmedAt.IsZero()
			state.mu.RUnlock()
			// Fast-drop only if: explicitly stopped OR never confirmed by any webhook
			if stopped || !everConfirmed {
				retentionDelay = 5 * time.Second
			}
		}
	}

	if oldTimer, ok := priorityTimers.LoadAndDelete(h.path); ok {
		oldTimer.(*time.Timer).Stop()
	}
	var t *time.Timer
	t = time.AfterFunc(retentionDelay, func() {
		priorityTimers.CompareAndDelete(h.path, t)

		// V750: Don't remove priority if pump is still active for this path
		if _, pumpOk := activePumps.Load(h.path); pumpOk {
			logger.Printf("[V750] Priority retained — pump still active for %s", filepath.Base(h.path))
			return
		}

		// V306: Don't remove priority if playback is confirmed healthy — even if
		// pump goroutine exited unexpectedly, the torrent must stay alive for the
		// next Read() to create a new pump without a gap.
		if val, ok := playbackRegistry.Load(h.path); ok {
			if pbState := val.(*PlaybackState); pbState.GetStatus() {
				logger.Printf("[V306] Priority retained — healthy playback confirmed for %s", filepath.Base(h.path))
				return
			}
		}

		// O(1): controlla se il path ha ancora handle aperti prima di disabilitare priority.
		if globalOpenTracker.IsPathOpen(h.path) {
			return
		}

		if val, ok := playbackRegistry.Load(h.path); ok {
			state := val.(*PlaybackState)
			if state.Hash != "" {
				hHash := metainfo.NewHashFromHex(state.Hash)

				// O(1): controlla se altri file dello stesso torrent sono ancora aperti.
				if globalOpenTracker.IsHashOpen(state.Hash) {
					// Another file from the same torrent is still open, keep priority.
					return
				}

				if t := web.BTS.GetTorrent(hHash); t != nil {
					t.IsPriority.Store(false)
					t.SetAggressiveMode(false, 0)

					// Fast-drop scanner handles never confirmed by webhook.
					scannerDrop := false
					if h.hasWarmup && isProbeOnly {
						state.mu.RLock()
						everConfirmed := !state.ConfirmedAt.IsZero()
						state.mu.RUnlock()
						if !state.IsHealthy && !everConfirmed {
							scannerDrop = true
						}
					}

					if scannerDrop {
						t.AddExpiredTime(5 * time.Second)
						logger.Printf("[V272] Scanner fast-drop for Hash %s (5s retention)", state.Hash)
					} else {
						t.AddExpiredTime(30 * time.Second)
						logger.Printf("[NativeBridge] Priority disabled for Hash %s (All handles closed)", state.Hash)
					}
				}
			}
		}
		// Registry entry kept (without priority) for fast webhook-triggered resume.
		// Cleanup handled by GlobalCleanupManager (15 min timeout).
		// playbackRegistry.Delete(h.path)
	})
	priorityTimers.Store(h.path, t)

	return 0
}

// approximateMetadataSize estimates the memory footprint of a Metadata entry
// Used for LRU cache size tracking
func approximateMetadataSize(m *vfs.Metadata) int64 {
	// Approximate size:
	// - URL string: len(URL)
	// - Path string: len(Path)
	// - Size: 8 bytes (int64)
	// - Mtime: 24 bytes (time.Time structure)
	// - Overhead: 64 bytes (pointers, struct alignment)
	return int64(len(m.URL) + len(m.Path) + 8 + 24 + 64)
}

func getOrReadMeta(path string) (*vfs.Metadata, error) {
	var m *vfs.Metadata

	// Check cache first (fast path without lock)
	if val, ok := metaCache.Get(path); ok {
		m = val
	} else {
		// Acquire lock to prevent stampede
		unlock := globalLockManager.Lock(path)
		defer unlock()

		// Double-check cache after acquiring lock
		if val, ok := metaCache.Get(path); ok {
			m = val
		} else {
			fileMeta, err := vfs.ReadMetadataFromFileWithLimits(path, stubSizeLimits(path))
			if err != nil {
				return nil, err
			}

			m = &vfs.Metadata{
				URL:                 fileMeta.URL,
				Size:                fileMeta.Size,
				Mtime:               fileMeta.Mtime,
				Path:                fileMeta.Path,
				ImdbID:              fileMeta.ImdbID,
				ExternalID:          fileMeta.ExternalID,
				ExternalIDNamespace: fileMeta.ExternalIDNamespace,
				Audio:               isAudioSectionPath(path),
			}

			metaCache.Put(path, m, approximateMetadataSize(m))
		}
	}

	return m, nil
}

type RaBuffer struct {
	start, end     int64
	data           []byte
	lastAccess     int64
	sessionID      int64
	responsiveOnly bool // true if written in non-verified mode (responsive shield active)
}

// ReadAheadCache is a 32-shard LRU cache with session-aware eviction.
type ReadAheadCache struct {
	shards    [32]*raShard
	shardMask uint64
	used      int64
	pool      chan []byte // recycled 16MB chunks

	muContext        sync.Mutex
	activePath       string
	currentSessionID int64
	isEvicting       int32 // atomic flag prevents concurrent global evictions

	pieceLens sync.Map // path → int64 adaptive chunk size (aligned to piece boundary)
}

// SetPieceLen stores the aligned chunk size for path derived from piece length.
// Called at pump start once pieceLen is known; both pump and raCache share this value.
func (c *ReadAheadCache) SetPieceLen(path string, pl int64) {
	base := int64(gc().ReadAheadBase)
	if base == 0 {
		base = 16 * 1024 * 1024
	}
	if pl <= 0 {
		c.pieceLens.Delete(path)
		return
	}
	if n := base / pl; n > 0 {
		// pl fits within base: use the largest multiple of pl that's <= base, so chunk
		// boundaries always land on piece boundaries.
		c.pieceLens.Store(path, n*pl)
		return
	}
	// pl > base: chunk boundaries coincide with piece boundaries only at multiples of
	// lcm(base, pl) - with power-of-two pl and base (mandatory in BEP52/v2, merely
	// conventional in BEP3/v1, and the default ReadAheadBase) that's simply every
	// (pl/base)-th boundary. ~25% of the library uses non-power-of-two piece lengths
	// (see recycle() below), for which alignment is rarer still. Either way alignment
	// here is best-effort - reads are byte-range based regardless. Stored explicitly
	// (instead of falling through to ChunkSize's own base fallback) so pieceLens always
	// has an entry once pieceLen is known, and re-reads pick up base config changes at
	// pump start.
	c.pieceLens.Store(path, base)
}

// ChunkSize returns the adaptive chunk size for path (falls back to ReadAheadBase).
func (c *ReadAheadCache) ChunkSize(path string) int64 {
	if v, ok := c.pieceLens.Load(path); ok {
		return v.(int64)
	}
	base := int64(gc().ReadAheadBase)
	if base == 0 {
		base = 16 * 1024 * 1024
	}
	return base
}

type raShard struct {
	mu      sync.RWMutex
	buffers map[string]*RaBuffer
	order   []string
	total   int64
}

func newReadAheadCache() *ReadAheadCache {
	// Static 32-buffer default: newReadAheadCache runs at package init (var raCache),
	// BEFORE config is loaded - gc() is nil here, so nothing config-dependent may be
	// touched. The pool is re-anchored to the budget once config exists via
	// AnchorPoolToConfig() (called from main after loading).
	c := &ReadAheadCache{
		shardMask: 31,
		pool:      make(chan []byte, 32), // Cap at 32 chunks (512MB max pool)
	}
	for i := range c.shards {
		c.shards[i] = &raShard{
			buffers: make(map[string]*RaBuffer),
		}
	}
	return c
}

// AnchorPoolToConfig re-sizes the recycled-buffer pool to the configured budget. Idle buffers
// are only ever needed to absorb eviction/overwrite storms, whose size scales with how much
// live content the budget allows - previously a fixed 32 (512MB max pool) regardless of
// budget: wasteful at 128MB (8 live chunks, 512MB parked), and at 512MB (32 live chunks) the
// 32-cap was 1:1 with live content, leaving no margin for the 32-shard concurrent-pop burst
// (every Put pops one buffer before its eviction recycle lands - with all shards popping
// simultaneously the pool must hold live-count buffers or allocations churn).
// clamp(budget/base*2, 16, 64): 2x live content so the margin holds across the whole range
// (256MB->32, 512MB->64), floor 16 to avoid allocation churn on tiny budgets, ceiling 64 to
// bound parking. The ceiling is nearly free: the pool only ever retains buffers that were
// already allocated, it never pre-allocates.
func (c *ReadAheadCache) AnchorPoolToConfig() {
	base := gc().ReadAheadBase
	if base <= 0 {
		base = 16 * 1024 * 1024
	}
	budget := gc().ReadAheadBudget
	if budget <= 0 {
		budget = 256 * 1024 * 1024
	}
	poolCap := int(budget / base * 2)
	if poolCap < 16 {
		poolCap = 16
	}
	if poolCap > 64 {
		poolCap = 64
	}
	if poolCap == cap(c.pool) {
		return
	}
	logger.Printf("[RaCache] Pool cap anchored to budget: %d buffers (%dMB max, budget %dMB, base %dMB)",
		poolCap, poolCap*int(base)/1024/1024, budget/1024/1024, base/1024/1024)
	c.pool = make(chan []byte, poolCap)
}

func (c *ReadAheadCache) getShard(path string) *raShard {
	return c.shards[xxhash.Sum64String(path)&c.shardMask]
}

// allocChunk allocates a chunk buffer with capacity rounded up to base so recycle() can pool
// it regardless of the file's adaptive chunk size. Sizes above base (shouldn't happen for
// pump chunks) fall back to an exact, unpoolable allocation.
func allocChunk(size, base int64) []byte {
	if size < base {
		return make([]byte, size, base)
	}
	return make([]byte, size)
}

func (c *ReadAheadCache) recycle(b []byte) {
	chunkSize := int(16 * 1024 * 1024)
	if gc().ReadAheadBase > 0 {
		chunkSize = int(gc().ReadAheadBase)
	}
	// Match on cap, not len: adaptive chunks (see SetPieceLen) have len < base for torrents
	// whose piece length doesn't divide base evenly (~25% of the library uses non-power-of-two
	// piece lengths), but Put always allocates them with cap == base so they stay poolable.
	if cap(b) == chunkSize {
		select {
		case c.pool <- b[:chunkSize]:
		default:
			// Pool full, let GC handle it
		}
	}
}

// MaxCachedOffset returns the highest cached byte end for a given path.
func (c *ReadAheadCache) MaxCachedOffset(p string) int64 {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	prefix := p + ":"
	var maxEnd int64
	for key, b := range s.buffers {
		if strings.HasPrefix(key, prefix) && b.end > maxEnd {
			maxEnd = b.end
		}
	}
	return maxEnd

}

// chunkKey returns a compound key using the per-path adaptive chunk size.
func (c *ReadAheadCache) chunkKey(path string, offset int64) string {
	return fmt.Sprintf("%s:%d", path, offset/c.ChunkSize(path))
}

// SwitchContext increments SessionID on path change to invalidate stale data.
func (c *ReadAheadCache) SwitchContext(newPath string) {
	c.muContext.Lock()
	pathChanged := newPath != c.activePath
	if pathChanged {
		c.activePath = newPath
		// Increment session ID: All old buffers (with old ID) become "stale" instantly
		c.currentSessionID++
	}
	activePath := c.activePath
	activeSessionID := c.currentSessionID
	c.muContext.Unlock()

	if pathChanged {
		safeGo(func() {
			c.triggerGlobalEviction(activePath, activeSessionID)
		})
	}
}

// Covered reports whether the cache fully covers [off, end] without copying any bytes —
// use this for presence/coverage checks on the hot path; call Get separately only once the
// actual data is needed, to avoid an up-to-16MB defensive copy on every check.
func (c *ReadAheadCache) Covered(p string, off, end int64) bool {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := c.chunkKey(p, off)
	if b, ok := s.buffers[key]; ok && off >= b.start && off <= b.end {
		if end <= b.end {
			atomic.StoreInt64(&b.lastAccess, time.Now().UnixNano())
			return true
		}
		if b2, ok2 := s.buffers[c.chunkKey(p, end)]; ok2 && b2.start == b.end+1 && b2.end >= end {
			atomic.StoreInt64(&b.lastAccess, time.Now().UnixNano())
			atomic.StoreInt64(&b2.lastAccess, time.Now().UnixNano())
			return true
		}
	}
	return false
}

func (c *ReadAheadCache) Get(p string, off, end int64) []byte {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := c.chunkKey(p, off)
	if b, ok := s.buffers[key]; ok && off >= b.start && off <= b.end {
		atomic.StoreInt64(&b.lastAccess, time.Now().UnixNano())
		if end <= b.end {
			// Defensive copy: pool evicts buffers immediately; sub-slice reference causes use-after-free.
			src := b.data[off-b.start : end-b.start+1]
			out := make([]byte, len(src))
			copy(out, src)
			return out
		}
		// Cross-boundary read: stitch two adjacent chunks to avoid FetchBlock on chunk boundary straddles.
		if b2, ok2 := s.buffers[c.chunkKey(p, end)]; ok2 && b2.start == b.end+1 && b2.end >= end {
			atomic.StoreInt64(&b2.lastAccess, time.Now().UnixNano())
			out := make([]byte, end-off+1)
			n1 := copy(out, b.data[off-b.start:])
			copy(out[n1:], b2.data[:end-b2.start+1])
			return out
		}
	}
	return nil
}

// Exists checks if a chunk is present in cache without allocating.
func (c *ReadAheadCache) Exists(p string, off int64) bool {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := c.chunkKey(p, off)
	_, found := s.buffers[key]
	return found
}

// CopyTo copies data directly into dest, avoiding an intermediate allocation in the FUSE Read hot path.
func (c *ReadAheadCache) CopyTo(p string, off, end int64, dest []byte) int {
	s := c.getShard(p)
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := c.chunkKey(p, off)
	if b, ok := s.buffers[key]; ok && off >= b.start && off <= b.end {
		atomic.StoreInt64(&b.lastAccess, time.Now().UnixNano())
		if end <= b.end {
			// Fast path: entirely within one chunk.
			src := b.data[off-b.start : end-b.start+1]
			return copy(dest, src)
		}
		// Cross-boundary read: same logic as Get().
		if b2, ok2 := s.buffers[c.chunkKey(p, end)]; ok2 && b2.start == b.end+1 && b2.end >= end {
			atomic.StoreInt64(&b2.lastAccess, time.Now().UnixNano())
			n1 := copy(dest, b.data[off-b.start:])
			n2 := copy(dest[n1:], b2.data[:end-b2.start+1])
			return n1 + n2
		}
	}
	return 0
}

func (c *ReadAheadCache) Put(p string, start, end int64, d []byte) {
	c.muContext.Lock()
	sessID := c.currentSessionID
	// Active path uses current session ID; stale pumps use sessID=0 so eviction can identify them.
	if p != c.activePath {
		sessID = 0
	}
	c.muContext.Unlock()

	shard := c.getShard(p)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	key := c.chunkKey(p, start)

	dataSize := int64(len(d))

	// Pooled buffers all have cap == ReadAheadBase; adaptive chunk sizes are always <= base
	// (guaranteed by SetPieceLen), so a pooled buffer resliced to dataSize serves any chunk.
	// Fresh allocations get cap rounded up to base too, so recycle() can pool them later —
	// previously a size mismatch both discarded the pooled buffer and allocated an
	// unpoolable one, permanently draining the pool on non-power-of-two-piece torrents.
	poolCap := int64(gc().ReadAheadBase)
	if poolCap == 0 {
		poolCap = 16 * 1024 * 1024
	}
	var dataCopy []byte
	select {
	case buf := <-c.pool:
		if int64(cap(buf)) >= dataSize {
			dataCopy = buf[:dataSize]
		} else {
			// Buffer predates a smaller base config; too small now, let GC take it.
			dataCopy = allocChunk(dataSize, poolCap)
		}
	default:
		dataCopy = allocChunk(dataSize, poolCap)
	}
	copy(dataCopy, d)

	globalLimit := gc().ReadAheadBudget
	if globalLimit <= 0 {
		globalLimit = 256 * 1024 * 1024 // Fail-safe default
	}

	// 1. Account for overwrite
	if old, ok := shard.buffers[key]; ok {
		shard.total -= int64(len(old.data))
		atomic.AddInt64(&c.used, -int64(len(old.data)))
		c.recycle(old.data)
		// Promote to MRU on overwrite to prevent premature eviction of recently refreshed chunks.
		for i, k := range shard.order {
			if k == key {
				shard.order = append(shard.order[:i], shard.order[i+1:]...)
				break
			}
		}
	}
	shard.order = append(shard.order, key)

	// 2. Add new data
	shard.total += dataSize
	newUsed := atomic.AddInt64(&c.used, dataSize)
	responsiveOnly := torrstor.IsResponsive()
	shard.buffers[key] = &RaBuffer{start, end, dataCopy, time.Now().UnixNano(), sessID, responsiveOnly}

	// Evict from this shard while over budget; keep last item to avoid evicting the chunk just added.
	for newUsed > globalLimit && len(shard.order) > 1 {
		v := shard.order[0]
		shard.order = shard.order[1:]
		if old, ok := shard.buffers[v]; ok {
			evictedSize := int64(len(old.data))
			shard.total -= evictedSize
			delete(shard.buffers, v)
			newUsed = atomic.AddInt64(&c.used, -evictedSize)
			c.recycle(old.data)
		}
	}

	// Local shard exhausted: trigger global eviction to free stale data from other shards.
	if newUsed > globalLimit && len(shard.order) <= 1 {
		c.muContext.Lock()
		ap := c.activePath
		sid := c.currentSessionID
		c.muContext.Unlock()
		safeGo(func() {
			c.triggerGlobalEviction(ap, sid)
		})
	}

	if newUsed > globalLimit && len(shard.order) == 0 {
		// If we reach here, it means even after global eviction we are over budget.
		// However, to allow the new stream to start, we must NOT drop the only chunk it has.
		// So we do nothing and allow a tiny over-budget for the first few chunks.
		logger.Printf("[RaCache] Budget full (%d MB), allowing active chunk to persist for handoff: %s", newUsed/(1024*1024), filepath.Base(p))
	}

	// Periodic compaction of order slice
	if len(shard.order) > 100 && len(shard.order) > len(shard.buffers)*2 {
		newOrder := make([]string, 0, len(shard.buffers))
		for _, v := range shard.order {
			if _, exists := shard.buffers[v]; exists {
				newOrder = append(newOrder, v)
			}
		}
		shard.order = newOrder
	}
}

// Stats returns read-ahead cache statistics for metrics endpoint
func (c *ReadAheadCache) Stats() (totalBytes int64, activeBytes int64, entries int) {
	totalBytes = atomic.LoadInt64(&c.used)

	now := time.Now().UnixNano()
	staleThreshold := (120 * time.Second).Nanoseconds()

	for _, shard := range c.shards {
		shard.mu.RLock()
		entries += len(shard.buffers)
		for _, buf := range shard.buffers {
			if now-atomic.LoadInt64(&buf.lastAccess) < staleThreshold {
				activeBytes += int64(len(buf.data))
			}
		}
		shard.mu.RUnlock()
	}

	return totalBytes, activeBytes, entries
}

// triggerGlobalEviction removes stale session data and old chunks from all shards.
func (c *ReadAheadCache) triggerGlobalEviction(activePath string, activeSessionID int64) {
	// Single-flight: skip if already evicting.
	if !atomic.CompareAndSwapInt32(&c.isEvicting, 0, 1) {
		return
	}
	defer atomic.StoreInt32(&c.isEvicting, 0)

	now := time.Now().UnixNano()
	staleThreshold := (120 * time.Second).Nanoseconds()

	evictShard := func(s *raShard) {
		var newOrder []string
		for _, key := range s.order {
			keep := true
			if buf, ok := s.buffers[key]; ok {
				// 1. Session ID Check (Fastest)
				if buf.sessionID != activeSessionID && !strings.HasPrefix(key, activePath+":") {
					keep = false
				} else {
					threshold := staleThreshold
					if strings.HasPrefix(key, activePath+":") {
						threshold = (60 * time.Second).Nanoseconds()
					}

					lastAcc := atomic.LoadInt64(&buf.lastAccess)
					if now-lastAcc > threshold {
						keep = false
					}
				}

				if !keep {
					size := int64(len(buf.data))
					s.total -= size
					delete(s.buffers, key)
					atomic.AddInt64(&c.used, -size)
					c.recycle(buf.data)
				}
			}

			if keep {
				newOrder = append(newOrder, key)
			}
		}
		s.order = newOrder
	}

	skipped := 0
	for _, s := range c.shards {
		if !s.mu.TryLock() {
			skipped++
			continue
		}
		evictShard(s)
		s.mu.Unlock()
	}

	// If all shards were busy, force blocking eviction on first shard to prevent budget overflow.
	if skipped == len(c.shards) {
		s := c.shards[0]
		s.mu.Lock()
		evictShard(s)
		s.mu.Unlock()
	}
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// exactMatchEntry is one playbackRegistry entry: a virtual path and its state.
type exactMatchEntry struct {
	path  string
	state *PlaybackState
}

// exactMatchKeys carries the webhook-side keys used by pass-1 matching.
type exactMatchKeys struct {
	imdbID     string
	identities webhookIdentityKeys
	basenames  []string
}

// findExactMatch selects the pass-1 match among the registry entries.
//
// The hash suffix is deliberately NOT a key: every episode of a season pack shares
// the same hash8 in its filename (HashPrefix = hash[:8]), so a suffix-only match can
// bind the wrong episode — e.g. the media.stop of E01 killing the pump of E02 that
// is actually playing. The filename (physical identity) wins over the IMDB ID
// (metadata identity), which also protects against an IMDB id polluted by a
// previous mis-matched media.play.
func findExactMatch(keys exactMatchKeys, entries []exactMatchEntry) (string, *PlaybackState) {
	for _, e := range entries {
		if e.state == nil {
			continue
		}
		base := filepath.Base(e.path)
		for _, want := range keys.basenames {
			if want == base {
				return e.path, e.state
			}
		}
	}
	if keys.imdbID != "" {
		for _, e := range entries {
			if e.state == nil {
				continue
			}
			if imdb := e.state.GetImdbID(); imdb != "" && imdb == keys.imdbID {
				return e.path, e.state
			}
		}
	}
	// Audio identity, most specific first: a session registered with the payload's
	// recording id beats one that only shares its release group, which in turn beats
	// one sharing the artist. Within one rank the most recent sign of life wins — the
	// coarser the shared id, the weaker that tiebreak is, which is why an exact
	// recording match is preferred over a recent artist match.
	if !keys.identities.empty() {
		bestRank := -1
		var bestPath string
		var bestState *PlaybackState
		var bestAt time.Time
		for _, e := range entries {
			if e.state == nil {
				continue
			}
			id, ns := e.state.GetExternalIdentity()
			if id == "" {
				continue
			}
			if ns != "" && !strings.EqualFold(ns, "musicbrainz") {
				continue
			}
			rank, ok := keys.identities.rank(id)
			if !ok {
				continue
			}
			sign := e.state.LastSignOfLife()
			if rank > bestRank || (rank == bestRank && (bestState == nil || sign.After(bestAt))) {
				bestPath, bestState, bestAt, bestRank = e.path, e.state, sign, rank
			}
		}
		if bestState != nil {
			return bestPath, bestState
		}
	}
	return "", nil
}

// snapshotPlaybackEntries copies the registry into a slice: both pass-1 scans within
// one call see the same entries and order, independent of concurrent registry writes.
func snapshotPlaybackEntries() []exactMatchEntry {
	entries := make([]exactMatchEntry, 0, 16)
	playbackRegistry.Range(func(key, value interface{}) bool {
		entries = append(entries, exactMatchEntry{path: key.(string), state: value.(*PlaybackState)})
		return true
	})
	return entries
}

// anyLiveHandleFor reports whether a FUSE handle for path is still registered. It reads
// activeHandles (the live source of truth) instead of globalOpenTracker on purpose: the
// tracker is not decremented when media.stop deletes the pump before Release, so it can
// latch a stale count and would block stops forever.
func anyLiveHandleFor(path string) bool {
	found := false
	activeHandles.Range(func(key, _ interface{}) bool {
		if key.(*MkvHandle).path == path {
			found = true
			return false
		}
		return true
	})
	return found
}

// anyLiveHandleForHashExcept is anyLiveHandleForHash excluding one path: the file being
// stopped still has its own handle open at webhook time and must not count as a sibling.
func anyLiveHandleForHashExcept(hash, excludePath string) bool {
	if hash == "" {
		return false
	}
	found := false
	activeHandles.Range(func(key, _ interface{}) bool {
		h := key.(*MkvHandle)
		if h.path == excludePath {
			return true
		}
		h.mu.Lock()
		hashOf := h.hash
		h.mu.Unlock()
		if hashOf == hash {
			found = true
			return false
		}
		return true
	})
	return found
}

// shouldDropTorrentPriority reports whether a media.stop may deactivate priority for
// the torrent: never while a file other than the one being stopped still has an open
// handle on the same hash (season-pack episodes share it, so priority-off would hit
// the live stream).
func shouldDropTorrentPriority(hash, excludePath string) bool {
	return hash != "" && !anyLiveHandleForHashExcept(hash, excludePath)
}

// handlePlexWebhook gestisce i messaggi in arrivo dal server Plex
func handlePlexWebhook(w http.ResponseWriter, r *http.Request) {
	logger.Printf("[PLEX] Webhook connection from %s", r.RemoteAddr)

	var payloadStr string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
		if err != nil {
			http.Error(w, "Bad request", 400)
			return
		}
		payloadStr = string(body)
	} else {
		// A form post is a form post: the plugin may send multipart or the older
		// application/x-www-form-urlencoded, and ParseMultipartForm reports the
		// latter as ErrNotMultipart after ParseForm has already filled the values.
		//
		// The cap matters for the urlencoded branch: ParseForm reads that body with
		// ReadAll, unbounded, and this process shares a 2200MB limit with the engine.
		//
		// ParseForm runs first on purpose: ParseMultipartForm swallows its error for a
		// urlencoded body and reports ErrNotMultipart instead, so an oversized post
		// would otherwise be read until the cap and then answer 200.
		//
		// The cap matches the multipart limit below rather than undercutting it: this
		// reader wraps the body before either branch, so a smaller value here would
		// reject a Plex webhook whose multipart carries a thumbnail, and a rejected
		// webhook is a playback session that never gets confirmed.
		r.Body = http.MaxBytesReader(w, r.Body, webhookMaxBody)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad request", 400)
			return
		}
		if err := r.ParseMultipartForm(webhookMaxBody); err != nil && !errors.Is(err, http.ErrNotMultipart) {
			http.Error(w, "Bad request", 400)
			return
		}
		payloadStr = r.FormValue("payload")
	}
	if payloadStr == "" {
		// Silent 200s make a misconfigured plugin a mystery: say so once per event.
		logger.Printf("[PLEX] Webhook from %s carried no payload", r.RemoteAddr)
		return
	}

	// Logga il payload per debug (limitato ai primi 500 caratteri per non intasare)
	displayPayload := payloadStr
	if len(displayPayload) > 500 {
		displayPayload = displayPayload[:500] + "..."
	}
	logger.Printf("[DEBUG] Webhook received: %s", displayPayload)

	var payload struct {
		Event    string `json:"event"`
		Metadata struct {
			Title              string `json:"title"`
			GrandparentTitle   string `json:"grandparentTitle"` // for TV series
			Year               int    `json:"year"`
			LibrarySectionType string `json:"librarySectionType"`
			Media              []struct {
				Part []struct {
					File string `json:"file"`
				} `json:"Part"`
			} `json:"Media"`
		} `json:"Metadata"`
	}

	// Sanitize empty numeric fields produced by Jellyfin templates (e.g. "year":, → "year":0,)
	sanitized := reEmptyNumber.ReplaceAllString(payloadStr, `"$1":0`)
	if err := json.Unmarshal([]byte(sanitized), &payload); err != nil {
		return
	}

	// Normalize Jellyfin event names to Plex-style
	switch payload.Event {
	case "PlaybackStart":
		payload.Event = "media.play"
	case "PlaybackStop":
		payload.Event = "media.stop"
	case "PlaybackProgress":
		payload.Event = "media.resume"
	}

	// Normalize Jellyfin ItemType values to Plex-style ("Movie"→"movie", "Episode"→"show")
	// and Plex music section types ("artist", "album") to one audio marker. Audio is
	// accepted so the identity pass can run, but its fuzzy name fallbacks stay behind:
	// an album id is shared by every track, and a name-similarity guess would be worse
	// than no match.
	audioWebhook := false
	switch payload.Metadata.LibrarySectionType {
	case "Movie":
		payload.Metadata.LibrarySectionType = "movie"
	case "Episode":
		payload.Metadata.LibrarySectionType = "show"
	case "artist", "album", "music", "Audio":
		payload.Metadata.LibrarySectionType = "music"
		audioWebhook = true
	case "audiobook":
		audioWebhook = true
	}

	switch payload.Metadata.LibrarySectionType {
	case "movie", "show", "music", "audiobook":
	default:
		return
	}

	if payload.Event == "media.play" || payload.Event == "media.resume" {
		targetTitle := strings.ToLower(payload.Metadata.Title)
		seriesTitle := strings.ToLower(payload.Metadata.GrandparentTitle)
		targetYear := payload.Metadata.Year

		logger.Printf("[DEBUG] Webhook for '%s' / '%s' (%d). Current registry:", targetTitle, seriesTitle, targetYear)
		playbackRegistry.Range(func(key, value interface{}) bool {
			logger.Printf("  - Registered: %s", key.(string))
			return true
		})

		// Two-pass matching: exact first (filename, IMDB), fuzzy only as fallback.
		// The hash suffix is not a key: season-pack episodes share it (HashPrefix =
		// hash[:8]), so a suffix-only match can bind the wrong episode.

		// Extract IMDB ID via regex on raw payload (struct unmarshal would cause UnmarshalTypeError
		// due to Plex sending both lowercase "guid" and capital "Guid" fields).
		webhookImdbID := ""
		if m := reImdbID.FindStringSubmatch(payloadStr); len(m) > 1 {
			webhookImdbID = m[1]
		}
		identities := webhookIdentityKeysFromPayload(sanitized)

		// Pass 1: Exact matches only (filename, IMDB ID).
		var basenames []string
		for _, m := range payload.Metadata.Media {
			for _, p := range m.Part {
				if b := filepath.Base(p.File); b != "" && b != "." {
					basenames = append(basenames, b)
				}
			}
		}
		exactMatch, exactState := findExactMatch(
			exactMatchKeys{imdbID: webhookImdbID, identities: identities, basenames: basenames},
			snapshotPlaybackEntries(),
		)

		// Pass 1c: IMDB bootstrap — if webhookImdbID is available but no state has it yet,
		// find the unique registered path of the matching library type with empty ImdbID.
		// One-time bootstrap: saves webhookImdbID into state so future sessions match via 0a.
		if exactMatch == "" && webhookImdbID != "" {
			sectionDir := ""
			switch payload.Metadata.LibrarySectionType {
			case "show":
				sectionDir = "/tv/"
			case "movie":
				sectionDir = "/movies/"
			}
			if sectionDir != "" {
				var bootPath string
				var bootState *PlaybackState
				bootCount := 0
				playbackRegistry.Range(func(key, value interface{}) bool {
					path := key.(string)
					state := value.(*PlaybackState)
					if strings.Contains(path, sectionDir) && state.GetImdbID() == "" {
						bootPath = path
						bootState = state
						bootCount++
					}
					return true
				})
				if bootCount == 1 {
					exactMatch = bootPath
					exactState = bootState
				}
			}
		}

		// Pass 2: Fuzzy matches only if no exact match found. Never for audio: the
		// identity pass above is the precise match, and a name guess on a music payload
		// could confirm a different track of the same album.
		if exactMatch == "" && !audioWebhook {
			var bestMatch string
			var bestState *PlaybackState
			bestLevel := 0 // higher = better match

			playbackRegistry.Range(func(key, value interface{}) bool {
				path := key.(string)
				filename := strings.ToLower(filepath.Base(path))
				level := 0

				// Tentativo 2: Match per Titolo completo (dot and underscore separators)
				if targetTitle != "" && (strings.Contains(filename, strings.ReplaceAll(targetTitle, " ", ".")) ||
					strings.Contains(filename, strings.ReplaceAll(targetTitle, " ", "_"))) {
					level = 3
				}

				// Tentativo 3: Match per Titolo Serie (TV Shows)
				if level == 0 && seriesTitle != "" {
					if strings.Contains(filename, strings.ReplaceAll(seriesTitle, " ", ".")) ||
						strings.Contains(filename, strings.ReplaceAll(seriesTitle, " ", "_")) {
						level = 2
					} else {
						words := strings.Fields(seriesTitle)
						if len(words) > 0 && len(words[0]) > 4 {
							cleanWord := strings.TrimRight(words[0], ":.,;!?")
							if len(cleanWord) > 4 && strings.Contains(filename, cleanWord) {
								level = 1
							}
						}
					}
				}

				// Tentativo 4: Match per prima parola + anno (first word > 3 chars to avoid "the", "a")
				if level == 0 && len(strings.Fields(targetTitle)) > 0 {
					firstWord := strings.Fields(targetTitle)[0]
					if len(firstWord) > 3 && strings.Contains(filename, firstWord) {
						if targetYear > 0 && strings.Contains(filename, strconv.Itoa(targetYear)) {
							level = 1
						}
					}
				}

				if level > bestLevel {
					bestLevel = level
					bestMatch = path
					bestState = value.(*PlaybackState)
				}
				return true
			})
			exactMatch = bestMatch
			exactState = bestState
		}

		if exactMatch != "" && exactState != nil {
			exactState.SetHealthy(true)
			exactState.mu.Lock()
			exactState.IsStopped = false
			// Cache webhookImdbID into state for fast IMDB matching in future sessions.
			// A pass-1 match is precise (filename or IMDB ID) and cannot cross-pollinate
			// episodes of a season pack; the pass-1c/fuzzy fallbacks are less precise.
			if exactState.ImdbID == "" && webhookImdbID != "" {
				exactState.ImdbID = webhookImdbID
				logger.Printf("[PLEX] IMDB ID cached for future matching: %s → %s", filepath.Base(exactMatch), webhookImdbID)
			}
			exactState.mu.Unlock()
			logger.Printf("[PLEX] Playback confirmed by webhook for: %s", filepath.Base(exactMatch))

			if exactState.Hash != "" {
				h := metainfo.NewHashFromHex(exactState.Hash)
				if t := web.BTS.GetTorrent(h); t != nil {
					t.IsPriority.Store(true)
					t.SetAggressiveMode(true, GetEffectiveConcurrencyLimit())
					logger.Printf("[PLEX] High Priority + Aggressive Mode for: %s", exactState.Hash)
				}
			}
		}
	} else if payload.Event == "media.stop" {
		targetTitle := strings.ToLower(payload.Metadata.Title)
		seriesTitle := strings.ToLower(payload.Metadata.GrandparentTitle)
		targetYear := payload.Metadata.Year

		stopImdbID := ""
		if m := reImdbID.FindStringSubmatch(payloadStr); len(m) > 1 {
			stopImdbID = m[1]
		}
		stopIdentities := webhookIdentityKeysFromPayload(sanitized)

		var basenames []string
		for _, m := range payload.Metadata.Media {
			for _, p := range m.Part {
				if b := filepath.Base(p.File); b != "" && b != "." {
					basenames = append(basenames, b)
				}
			}
		}

		// Pass 1: Exact matches (filename, IMDB ID) — never the shared hash suffix.
		stopMatch, stopState := findExactMatch(
			exactMatchKeys{imdbID: stopImdbID, identities: stopIdentities, basenames: basenames},
			snapshotPlaybackEntries(),
		)

		stopMatchFromFuzzy := false

		// Pass 2: Fuzzy matches only if no exact match, and never for audio (see the
		// play path): a name guess must not stop the wrong track.
		if stopMatch == "" && !audioWebhook {
			bestLevel := 0
			playbackRegistry.Range(func(key, value interface{}) bool {
				path := key.(string)
				filename := strings.ToLower(filepath.Base(path))
				level := 0

				if targetTitle != "" && (strings.Contains(filename, strings.ReplaceAll(targetTitle, " ", ".")) ||
					strings.Contains(filename, strings.ReplaceAll(targetTitle, " ", "_"))) {
					level = 3
				}

				if level == 0 && seriesTitle != "" {
					if strings.Contains(filename, strings.ReplaceAll(seriesTitle, " ", ".")) ||
						strings.Contains(filename, strings.ReplaceAll(seriesTitle, " ", "_")) {
						level = 2
					} else {
						words := strings.Fields(seriesTitle)
						if len(words) > 0 && len(words[0]) > 4 {
							cleanWord := strings.TrimRight(words[0], ":.,;!?")
							if len(cleanWord) > 4 && strings.Contains(filename, cleanWord) {
								level = 1
							}
						}
					}
				}

				if level == 0 && len(strings.Fields(targetTitle)) > 0 {
					firstWord := strings.Fields(targetTitle)[0]
					if len(firstWord) > 3 && strings.Contains(filename, firstWord) {
						if targetYear > 0 && strings.Contains(filename, strconv.Itoa(targetYear)) {
							level = 1
						}
					}
				}

				if level > bestLevel {
					bestLevel = level
					stopMatch = path
					stopState = value.(*PlaybackState)
					stopMatchFromFuzzy = true
				}
				return true
			})
		}

		// Only a basename match (pass 1) is the file by construction. An IMDB-only match
		// is precise on this deployment (episode-level IDs) but only when the registry
		// holds exactly one entry with that ID; otherwise it is a guess like fuzzy.
		matchByBasename := false
		if stopMatch != "" {
			base := filepath.Base(stopMatch)
			for _, b := range basenames {
				if b == base {
					matchByBasename = true
					break
				}
			}
		}
		imdbUnique := false
		if stopMatch != "" && !matchByBasename && !stopMatchFromFuzzy && stopImdbID != "" {
			n := 0
			playbackRegistry.Range(func(_, v interface{}) bool {
				ps, ok := v.(*PlaybackState)
				if !ok || ps == nil {
					return true
				}
				if ps.GetImdbID() == stopImdbID {
					n++
				}
				return n <= 1
			})
			imdbUnique = n == 1
		}
		// An audio identity match is exact: the matched state's registered id is one
		// of the ids the webhook carried, at the best rank available. It is trusted
		// like a basename even though sibling tracks can share a coarser id, because
		// the identity pass preferred the session with the most specific match.
		matchByIdentity := false
		if stopMatch != "" && stopState != nil && !stopIdentities.empty() {
			id, ns := stopState.GetExternalIdentity()
			if id != "" && (ns == "" || strings.EqualFold(ns, "musicbrainz")) {
				_, matchByIdentity = stopIdentities.rank(id)
			}
		}

		// A fuzzy (pass 2) match is a name-similarity guess; an IMDB-only match is only
		// trusted when unique. If such a match still has a live handle it is likely a
		// sibling episode being read, so a late or spurious stop must not kill it.
		// Basename and unique-IMDB matches are honored even with the handle open:
		// ignoring them would leave the pump alive until the 2h idle timeout (V262).
		if stopMatch != "" && stopState != nil && !matchByBasename && !matchByIdentity && !imdbUnique && anyLiveHandleFor(stopMatch) {
			logger.Printf("[PLEX] STOP ignored for %s: fuzzy or ambiguous match with handle still open", filepath.Base(stopMatch))
		} else if stopMatch != "" && stopState != nil {
			stopState.mu.Lock()
			stopState.IsStopped = true
			stopState.mu.Unlock()
			stopState.SetHealthy(false) // persists with IsStopped=true
			logger.Printf("[PLEX] STOP applied for: %s (Event: %s)", filepath.Base(stopMatch), payload.Event)

			if val, ok := activePumps.Load(stopMatch); ok {
				ps := val.(*NativePumpState)
				if ps.cancel != nil {
					ps.cancel()
				}
				activePumps.Delete(stopMatch)
				logger.Printf("[PLEX] STOP: force-terminated pump for %s", filepath.Base(stopMatch))
			}

			torrstor.ResetShield()
			logger.Printf("[AdaptiveShield] Shield reset on media.stop")

			// Pull the rest of the tail window now that the pump is gone: piece requests at
			// the file end would otherwise compete with the sequential download front. Ready
			// for the next open, which is when the MKV Cues probe needs it.
			if warmup.DiskWarmup != nil {
				if v, ok := tailFillTargets.Load(stopMatch); ok {
					t := v.(tailFillTarget)
					warmup.DiskWarmup.EnsureTail(t.hash, t.fileID, t.size)
				}
			}

			// Deactivate Core Priority unless a sibling episode of the same torrent is
			// still live (shared hash): priority/aggressive off and the 30s expiry would
			// hit the stream that is actually playing.
			if shouldDropTorrentPriority(stopState.Hash, stopMatch) {
				h := metainfo.NewHashFromHex(stopState.Hash)
				if t := web.BTS.GetTorrent(h); t != nil {
					t.IsPriority.Store(false)
					t.SetAggressiveMode(false, 0) // Back to normal download priority
					t.AddExpiredTime(30 * time.Second)
					logger.Printf("[PLEX] STOP detected. Grace period 30s for: %s", stopState.Hash)
				}
			}
		}
	}
	w.WriteHeader(200)
}

// V750: Persist a PlaybackState to SQLite (best-effort)
func savePlaybackStateToDB(ps *PlaybackState) {
	if stateDB == nil {
		return
	}
	ps.mu.RLock()
	rec := &metadb.PlaybackRecord{
		Path:         ps.Path,
		Hash:         ps.Hash,
		ImdbID:       ps.ImdbID,
		ExternalID:   ps.ExternalID,
		ExternalIDNS: ps.ExternalIDNamespace,
		OpenedAt:     ps.OpenedAt,
		ConfirmedAt:  ps.ConfirmedAt,
		IsHealthy:    ps.IsHealthy,
		IsStopped:    ps.IsStopped,
		LastReadAt:   ps.LastReadAt,
		ReadCount:    ps.ReadCount,
		LastSeekOff:  ps.LastSeekOff,
	}
	ps.mu.RUnlock()
	if err := stateDB.SavePlaybackState(rec); err != nil {
		logger.Printf("[V750] Failed to persist playback state for %s: %v", filepath.Base(rec.Path), err)
	}
}

// V750: Restore PlaybackState from SQLite at boot
func restorePlaybackStates(db *metadb.DB) {
	records, err := db.LoadPlaybackStates(4 * time.Hour)
	if err != nil {
		logger.Printf("[V750] Failed to load playback states: %v", err)
		return
	}
	restored := 0
	priorityApplied := 0
	for _, rec := range records {
		if rec.IsHealthy && !rec.ConfirmedAt.IsZero() {
			ps := &PlaybackState{
				Path:                rec.Path,
				Hash:                rec.Hash,
				ImdbID:              rec.ImdbID,
				ExternalID:          rec.ExternalID,
				ExternalIDNamespace: rec.ExternalIDNS,
				OpenedAt:            rec.OpenedAt,
				ConfirmedAt:         rec.ConfirmedAt,
				IsHealthy:           true,
				ReadCount:           rec.ReadCount,
				LastSeekOff:         rec.LastSeekOff,
				LastReadAt:          rec.LastReadAt,
			}
			playbackRegistry.Store(rec.Path, ps)
			// Restore GoStorm priority
			if rec.Hash != "" {
				hHash := metainfo.NewHashFromHex(rec.Hash)
				if t := web.BTS.GetTorrent(hHash); t != nil {
					t.IsPriority.Store(true)
					t.SetAggressiveMode(true, GetEffectiveConcurrencyLimit())
					priorityApplied++
					logger.Printf("[V750] Priority RESTORED from DB: %s", filepath.Base(rec.Path))
				}
			}
			restored++
		}
	}
	logger.Printf("[V750] Restored %d/%d PlaybackStates, %d priorities reapplied", restored, len(records), priorityApplied)
}

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

func main() {
	checkHardwareSupport()

	var dbPath string
	flag.StringVar(&dbPath, "path", "", "path to database and config")
	flag.Parse()

	// Default --path to the directory containing the binary (portable install)
	if dbPath == "" {
		if exe, err := os.Executable(); err == nil {
			dbPath = filepath.Dir(exe)
		}
	}
	if dbPath != "" {
		if settings.Args == nil {
			settings.Args = &settings.ExecArgs{}
		}
		settings.Args.Path = dbPath
		// settings.Path is what the GoStorm layer actually joins against for config.db,
		// the settings JSON, trackers.txt, accs.db and the blocklist fallback. It was never
		// assigned, so all of those resolved relative to the working directory: under
		// systemd that happens to be the install dir, but in Docker it is the image's
		// WORKDIR, outside every mounted volume - so torrents and GoStorm settings were
		// lost on "docker rm". Migrate anything already written there before switching.
		settings.Path = dbPath
		settings.MigrateFromWorkdir(dbPath)
	}

	source, mount := flag.Arg(0), flag.Arg(1)

	cfg := config.LoadConfig()

	if dbPath != "" {
		// If dbPath is a directory, use it as RootPath; if a file, use its parent.
		if fi, err := os.Stat(dbPath); err == nil && fi.IsDir() {
			cfg.RootPath = dbPath
		} else {
			cfg.RootPath = filepath.Dir(dbPath)
		}
	} else {
		// Default to /home/pi if no flag provided (for backward compat)
		cfg.RootPath = "/home/pi"
	}

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

	// Must precede server.Start(): the engine reads the blocklist while configuring
	// its client, and a flag set afterwards would leave it unloaded until a refresh.
	if err := torrutils.SetBlockListFilter(gc().BlockListFilter); err != nil {
		logger.Printf("[BlockList] %v — loading the list unfiltered", err)
	}
	torrutils.SetBlockListEnabled(gc().BlockListEnabled)

	go func() {
		logger.Println("Starting Embedded GoStorm Engine...")
		server.Start() // Starts Web Server on 8090 and Engine
	}()
	// Give engine a moment to init (hash maps etc)
	time.Sleep(2 * time.Second)

	warmup.InitDiskWarmup(gc().DiskWarmupQuotaGB)
	// Fires synchronously from DiskWarmup's single writeWorker goroutine at the exact moment a
	// head warmup fetch starts/completes - avoids the race where a caller checking IsWarmingUp()
	// right after WriteChunk() returns could read stale state, since WriteChunk only enqueues and
	// returns immediately (see OnWarmupStateChange doc comment in internal/warmup/warmup.go).
	warmup.OnWarmupStateChange = func(hash string, fileID int, active bool) {
		if tr := torr.PeekTorrent(hash); tr != nil && tr.Torrent != nil {
			tr.Torrent.SetWarmupActive(active, fileID)
		}
	}
	// Lets the warmup package pull the tail window itself (EnsureTail); it cannot import
	// the bridge, so the dependency is injected the same way as the callback above.
	warmup.TailFetch = func(hash string, fileID int, off int64, buf []byte) (int, error) {
		if nativeBridge == nil {
			return 0, fmt.Errorf("native bridge unavailable")
		}
		return nativeBridge.FetchBlock(hash, fileID, off, buf)
	}
	go registry.StartRegistryWatchdog(backgroundStopChan)
	go natpmp.NatpmpLoop(backgroundStopChan, gc().NatPMP, logger)

	masterDataSemaphore = make(chan struct{}, gc().MasterConcurrencyLimit)
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
	// init gc() was nil, so newReadAheadCache kept the static 32-buffer default).
	raCache.AnchorPoolToConfig()

	catalog.SetRetryDefaults(gc().MaxRetryAttempts, time.Duration(gc().RetryDelayMS)*time.Millisecond)

	nativeBridge = native.NewNativeClient()

	go tuner.Start(context.Background())

	if gc().BlockListEnabled && gc().BlockListURL != "" {
		startBlockListLoop(gc().BlockListURL)
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

	// V1.7.1: Optional SQLite State DB for unified persistence.
	if gc().EnableStateDB {
		dbPath := gc().StateDBPath
		if dbPath == "" {
			dbPath = filepath.Join(GetStateDir(), "tiramisu.db")
		}
		var err error
		stateDB, err = metadb.New(dbPath, logger)
		if err != nil {
			logger.Printf("WARNING: Failed to open StateDB: %v (falling back to JSON)", err)
			stateDB = nil
		} else {
			// Migrate JSON files if needed
			if err := stateDB.MigrateFromJSON(GetStateDir()); err != nil {
				logger.Printf("WARNING: StateDB migration failed: %v (falling back to JSON)", err)
				stateDB.Close()
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
				logger.Printf("[StateDB] Active: %s", dbPath)

				// V304 ban persistence: restore prior bans (30-day TTL) and persist new ones.
				if ips, err := stateDB.LoadV304Bans(30 * 24 * time.Hour); err != nil {
					logger.Printf("WARNING: Failed to load V304 bans: %v", err)
				} else if len(ips) > 0 {
					torrent.V304LoadBans(ips)
					logger.Printf("[V304] Restored %d persisted peer bans", len(ips))
				}
				// Reachability outcomes: the sync engines read the counter to tell a dead
				// release from a slow one before dropping a title.
				failDB := stateDB
				native.ReachabilityOutcome = func(hash string, resolved bool) {
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
				torrent.V304SetOnBan(func(ip string) {
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

	globalTorrentRemover = NewTorrentRemover(nativeBridge, logger)
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
		raPercent := float64(raTotal) / float64(raBudget) * 100
		raActivePercent := float64(raActive) / float64(raBudget) * 100
		raStalePercent := float64(raStale) / float64(raBudget) * 100

		natPort := atomic.LoadInt64(&natpmp.CurrentNatPort)

		warmupBuckets := warmup.WarmupDurationBucketCounts()
		warmupBucketsJSON := fmt.Sprintf("[%d,%d,%d,%d,%d,%d,%d,%d]",
			warmupBuckets[0], warmupBuckets[1], warmupBuckets[2], warmupBuckets[3],
			warmupBuckets[4], warmupBuckets[5], warmupBuckets[6], warmupBuckets[7])

		// Task 4: hedge counters live per-Torrent on the fork - sum trigger count across active
		// torrents, report circuit breaker as open if any active torrent currently has it tripped.
		var hedgeTriggerTotal int64
		deadlinePieces := 0
		hedgeCircuitOpenAny := false
		for _, tr := range torr.ListActiveTorrent() {
			if tr.Torrent == nil {
				continue
			}
			hedgeTriggerTotal += tr.Torrent.HedgeTriggerCount()
			deadlinePieces += tr.Torrent.PieceDeadlineCount()
			if tr.Torrent.HedgeCircuitOpen() {
				hedgeCircuitOpenAny = true
			}
		}
		// Peer view split by transport - conn slots are capped, so a transport is only worth
		// the share of useful bytes it returns for the share of slots it takes.
		peerStats := torr.CollectPeerTransportStats()

		shortStream, shortFetch := native.ShortReadCounts()
		repairedStream, unfilledStream := native.ShortReadRepairCounts()

		fmt.Fprintf(w, `{"version":"%s", "config_source":"%s", "uptime":"%s", "cache_entries":%d, "cache_size_mb":%.2f, "cleanup_hashes":%d, "cleanup_offsets":%d, "cleanup_activities":%d, "locks_total":%d, "master_concurrency_limit":%d, "negative_cache_entries":%d, "fullpack_cache_entries":%d, "streaming_threshold_kb":%d, "config_preload_workers":%d, "max_conns_per_host":%d, "read_ahead_total_bytes":%d, "read_ahead_active_bytes":%d, "read_ahead_stale_bytes":%d, "read_ahead_entries":%d, "read_ahead_budget":%d, "read_ahead_percent":%.2f, "read_ahead_active_percent":%.2f, "read_ahead_stale_percent":%.2f, "natpmp_port":%d, "latest_version":"%s", "update_available":%t, "warmup_duration_buckets_lt_2_5_10_15_30_60_120_gte120s":%s, "hedge_trigger_count":%d, "hedge_circuit_open":%t, "fetch_singleflight_dedup":%d, "peer_eject_count":%d, "v304_banned_peers":%d, "ip_blocklist_rejections":%d, "ip_blocklist_ips":%d, "fuse_short_reads":%d, "fuse_short_reads_repaired":%d, "fuse_short_reads_failed":%d, "short_read_stream":%d, "short_read_fetch":%d, "short_read_repaired":%d, "short_read_unfilled":%d, "peer_conns_tcp":%d, "peer_conns_utp":%d, "peer_rated_tcp":%d, "peer_rated_utp":%d, "peer_useful_bps_tcp":%.0f, "peer_useful_bps_utp":%.0f, "peer_eject_count_utp":%d, "peer_churn_count":%d, "peer_churn_count_utp":%d, "deadline_pieces":%d, "audio_namespace_state":"%s", "audio_namespace_entries":%d}`,
			AppVersion,
			gc().ConfigPath,
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
			updater.LatestVersion(), updater.UpdateAvailable(),
			warmupBucketsJSON,
			hedgeTriggerTotal, hedgeCircuitOpenAny, fetchFlightDedupCount.Load(), peerStats.EjectTotal, torr.V304BannedCount(),
			torrent.IPBlocklistRejections(), torrent.IPBlocklistDistinctIPs(),
			fuseShortReadCount.Load(), fuseShortReadRepaired.Load(), fuseShortReadFailed.Load(), shortStream, shortFetch, repairedStream, unfilledStream,
			peerStats.ConnsTCP, peerStats.ConnsUTP, peerStats.RatedTCP, peerStats.RatedUTP,
			peerStats.UsefulBpsTCP, peerStats.UsefulBpsUTP,
			peerStats.EjectUTP, peerStats.ChurnTotal, peerStats.ChurnUTP, deadlinePieces,
			audioNamespaceState(), audioNamespaceEntries())
	})

	// Which blocklist ranges are actually rejecting peers. The aggregate counters say
	// how much is blocked; this says who, which is the only way to tell a list that
	// catches monitoring outfits from one quietly eating ordinary peers.
	http.HandleFunc("/metrics/blocklist", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"enabled":      gc().BlockListEnabled,
			"url":          gc().BlockListURL,
			"distinct_ips": torrent.IPBlocklistDistinctIPs(),
			"rejections":   torrent.IPBlocklistRejections(),
			"top_ranges":   torrent.IPBlocklistTopRanges(25),
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

	http.HandleFunc("/metrics/ttff", func(w http.ResponseWriter, r *http.Request) {
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

	http.HandleFunc("/control", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(settingsHTML)
	})

	http.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(gc())
			return
		}
		if r.Method == "POST" {
			var newCfg config.Config
			if err := json.NewDecoder(r.Body).Decode(&newCfg); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			// Update file
			data, _ := json.MarshalIndent(newCfg, "", "  ")
			if err := os.WriteFile(gc().ConfigPath, data, 0644); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			// Reload in memory (V1.4.0 Live Update)
			oldEnabled := gc().BlockListEnabled
			oldURL := gc().BlockListURL
			oldFilter := gc().BlockListFilter
			cfg := config.LoadConfig()
			globalConfig.Store(&cfg)
			prowlarrClient = prowlarr.NewClient(gc().Prowlarr)

			newEnabled := gc().BlockListEnabled
			newURL := gc().BlockListURL
			torrutils.SetBlockListEnabled(newEnabled)
			newFilter := gc().BlockListFilter
			if err := torrutils.SetBlockListFilter(newFilter); err != nil {
				logger.Printf("[BlockList] %v — loading the list unfiltered", err)
			}
			// The filter changes which ranges the same file yields, so re-read it rather
			// than waiting for the next download.
			if newEnabled && newFilter != oldFilter {
				if list, err := torrutils.ReadBlockedIP(); err == nil {
					torr.SetIPBlocklist(list)
					logger.Printf("[BlockList] Filter changed, reloaded from the existing file")
				} else {
					logger.Printf("[BlockList] WARNING: filter changed but reload failed: %v", err)
				}
			}
			switch {
			case newEnabled && (!oldEnabled || newURL != oldURL):
				// was off -> on, or URL changed while staying on: (re)start with fresh URL
				startBlockListLoop(newURL)
			case !newEnabled && oldEnabled:
				// was on -> off: stop future refreshes and clear the ranges already loaded
				// into the engine, otherwise the file keeps banning peers until a restart.
				stopBlockListLoop()
				torr.SetIPBlocklist(nil)
				logger.Printf("[BlockList] Disabled: cleared from running engine")
			}
			logger.Printf("[Config] Updated via Dashboard API")
			w.WriteHeader(200)
		}
	})

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
		streams, err := prowlarrClient.FetchTorrents(imdbID, contentType, title, year)
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

	// Same-origin proxy to the GoStorm settings API. The Control Panel must not
	// build an absolute URL of its own: behind a reverse proxy the GoStorm port is
	// not exposed, and an http:// call from an https:// page is blocked as mixed
	// content, which left the whole GoStorm section empty and unsaveable.
	http.HandleFunc("/api/gostorm/settings", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		base := gc().GoStormBaseURL
		if base == "" {
			http.Error(w, "gostorm_url not configured", http.StatusServiceUnavailable)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), "POST",
			strings.TrimRight(base, "/")+"/settings", bytes.NewReader(body))
		if err != nil {
			http.Error(w, "build request: "+err.Error(), http.StatusInternalServerError)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err != nil {
			http.Error(w, "gostorm unreachable: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})

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
			}),
			"tv": engines.NewTVSyncer(engines.TVSyncerConfig{
				GoStormURL:      gc().GoStormBaseURL,
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
				InvalidatePath:  invalidateSyncRemovedPath,
			}),
			"music": engines.NewMusicSyncEngine(engines.MusicSyncConfig{
				PlexURL:      gc().Plex.URL,
				PlexToken:    gc().Plex.Token,
				PlexMusicLib: strconv.Itoa(gc().Plex.MusicLibraryID),
				LibraryURL:   fmt.Sprintf("http://127.0.0.1:%d", gc().MetricsPort),
				StateDir:     GetStateDir(),
				LogsDir:      logsDir,
				ProwlarrCfg:  gc().Prowlarr,
				Discovery:    gc().MusicDiscovery,
			}),
			"watchlist": engines.NewWatchlistSyncer(engines.WatchlistSyncerConfig{
				GoStormURL:      gc().GoStormBaseURL,
				DB:              stateDB,
				AudioRegistry:   audioOwnershipRegistry(),
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

		sched := scheduler.New(schedCfg, syncers, statePath)

		http.HandleFunc("/api/scheduler/status", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(sched.Status())
		})
		http.HandleFunc("/api/scheduler/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", 405)
				return
			}
			path := strings.TrimPrefix(r.URL.Path, "/api/scheduler/")
			if strings.HasSuffix(path, "/stop") {
				name := strings.TrimSuffix(path, "/stop")
				if err := sched.StopJob(name); err != nil {
					http.Error(w, err.Error(), 409)
					return
				}
				w.WriteHeader(http.StatusAccepted)
				return
			}
			name := strings.TrimSuffix(path, "/run")
			if err := sched.TriggerRun(name); err != nil {
				http.Error(w, err.Error(), 409)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		})

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
			GoStorm:    engines.NewGoStormClient(gc().GoStormBaseURL),
			Registry:   registry,
			// Without this the drop path sees only movie and tv stubs, and removing
			// the last one for a shared hash takes the torrent out from under any
			// audio projection still backed by it.
			//
			// audioOwnershipRegistry(), not a raw stateDB value: on a boot where
			// EnableStateDB is true but the DB failed to open, the raw value is nil
			// and this path would permit exactly that drop.
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
			ActiveSession:  ttffActiveByHash,
			InvalidatePath: invalidateSyncRemovedPath,
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
			Blacklist: func(path, hash string) {
				if globalTorrentRemover == nil || len(hash) != 40 {
					return
				}
				globalTorrentRemover.addToBlacklist(hash, globalTorrentRemover.deriveTitleFromPath(path))
			},
			Logger:       logger,
			MediaServer:  mediaserver.New(gc().MediaServerType, gc().Plex.URL, gc().Plex.Token),
			MovieSection: gc().Plex.LibraryID,
			TVSection:    gc().Plex.TVLibraryID,
			MusicSection: gc().Plex.MusicLibraryID,
		})
		libHandler := library.NewHandler(libMgr)
		http.HandleFunc("/api/library/add", libHandler.Add)
		http.HandleFunc("/api/library/remove", libHandler.Remove)
		http.HandleFunc("/api/library/list", libHandler.List)
		http.HandleFunc("/api/library/inspect", libHandler.Inspect)
	}

	// Health Monitor + Dashboard (Fase 5)
	logsDir := gc().LogDir
	monCollector := collector.New(
		"http://127.0.0.1:8090",
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
	safeGo(func() {
		monCollector.Run(backgroundStopChan)
	})
	logger.Printf("[Dashboard] enabled at :%d/dashboard", gc().MetricsPort)

	go http.ListenAndServe(fmt.Sprintf(":%d", gc().MetricsPort), nil)

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
			MaxBackground:           gc().ConcurrencyLimit,
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

	go smbdWatchdog()
	go ttffCleanupLoop()

	server.Wait()
}

// smbdWatchdog detects smbd processes stuck in D-state (uninterruptible FUSE I/O).
// Level 1 (3 hits / 180s): interrupt all pumps. Level 2 (10 hits / 600s): graceful restart.
func smbdWatchdog() {
	const checkInterval = 60 * time.Second
	const unblockThreshold = 3  // 180s - Emergency unblock (interrupt all pumps)
	const restartThreshold = 10 // 600s - Full restart (persistent stall)
	consecutiveHits := 0

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for range ticker.C {
		if countDStateSmbd() > 0 {
			consecutiveHits++
			logger.Printf("[Watchdog] D-state smbd detected (%d/%d)", consecutiveHits, restartThreshold)

			// Level 1 (3 consecutive hits): soft-interrupt all pumps to unblock hung FUSE reads.
			// Only closes pipe readers — pumps stay alive and will retry on next FUSE read.
			// Preserves pump survival during temporary peer shortages (swarm may recover).
			if consecutiveHits == unblockThreshold {
				logger.Printf("[Watchdog] D-state persistent for %ds — triggering soft interrupt (pumps stay alive)",
					consecutiveHits*int(checkInterval/time.Second))
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

			// Level 2 (10 consecutive hits): graceful restart.
			if consecutiveHits >= restartThreshold {
				logger.Printf("[Watchdog] D-state STILL persistent for %ds — triggering graceful restart",
					consecutiveHits*int(checkInterval/time.Second))
				restartRequested.Store(true)
				syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
				return
			}
		} else {
			if consecutiveHits > 0 {
				logger.Printf("[Watchdog] D-state cleared after %d hit(s) (Max: %d)", consecutiveHits, restartThreshold)
			}
			consecutiveHits = 0
		}
	}
}

// countDStateSmbd returns the number of smbd processes in D-state (uninterruptible sleep).
func countDStateSmbd() int {
	// ps -eo stat,comm: STAT column starts with D for uninterruptible sleep
	out, err := exec.Command("ps", "-eo", "stat,comm").Output()
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range bytes.Split(out, []byte("\n")) {
		// Match lines where STAT starts with D and command is smbd
		fields := bytes.Fields(line)
		if len(fields) >= 2 && fields[0][0] == 'D' && string(fields[1]) == "smbd" {
			count++
		}
	}
	return count
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

// startBlockListLoop (re)starts the background blocklist refresh loop for the given URL,
// stopping any previously running loop first (used for both startup and runtime toggles).
func startBlockListLoop(urlStr string) {
	if urlStr == "" {
		return
	}
	blockListMu.Lock()
	if blockListStop != nil {
		close(blockListStop)
	}
	stop := make(chan struct{})
	blockListStop = stop
	blockListMu.Unlock()

	safeGo(func() {
		updateBlockList(urlStr)
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				updateBlockList(gc().BlockListURL)
			case <-stop:
				return
			case <-backgroundStopChan:
				return
			}
		}
	})
}

// stopBlockListLoop signals the running blocklist refresh loop (if any) to exit.
func stopBlockListLoop() {
	blockListMu.Lock()
	if blockListStop != nil {
		close(blockListStop)
		blockListStop = nil
	}
	blockListMu.Unlock()
}

// updateBlockList downloads and updates the BitTorrent blocklist
func updateBlockList(urlStr string) {
	if urlStr == "" {
		return
	}

	// Write next to the rest of the state, not next to the binary: in a container the
	// binary lives in the image layer, so the list was re-downloaded on every start.
	destDir := settings.Path
	if destDir == "" {
		exePath, err := os.Executable()
		if err != nil {
			logger.Printf("[BlockList] Error getting executable path: %v", err)
			return
		}
		destDir = filepath.Dir(exePath)
	}
	destPath := filepath.Join(destDir, "blocklist")

	// A sidecar records which URL produced the file on disk. Age alone is not enough:
	// changing the URL in the Control Panel used to keep serving the previous list for
	// up to 24h, restart included, while the log claimed everything was fine.
	urlPath := destPath + ".url"
	prevURL := ""
	if b, err := os.ReadFile(urlPath); err == nil {
		prevURL = strings.TrimSpace(string(b))
	}
	if info, err := os.Stat(destPath); err == nil && prevURL == urlStr {
		if time.Since(info.ModTime()) < 24*time.Hour {
			logger.Printf("[BlockList] Existing blocklist is recent, skipping update")
			return
		}
	}
	if prevURL != "" && prevURL != urlStr {
		logger.Printf("[BlockList] URL changed, refreshing now")
	}

	logger.Printf("[BlockList] Updating from %s...", urlStr)

	// A bare http.Get has no deadline: a stalled connection (e.g. a VPN flap mid-download)
	// hangs this goroutine forever. Since this call runs before the loop's select, closing
	// blockListStop can't interrupt it either — a timeout is the only way out.
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(urlStr)
	if err != nil {
		logger.Printf("[BlockList] Download error: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logger.Printf("[BlockList] Download failed: status %d", resp.StatusCode)
		return
	}

	// Detect gzip from the magic bytes, not the URL: iblocklist and friends serve a
	// gzip stream from a query string (...&archiveformat=gz), and a suffix test would
	// save the compressed bytes verbatim - a blocklist with zero usable ranges and no
	// error anywhere.
	buffered := bufio.NewReader(resp.Body)
	var reader io.Reader = buffered
	if magic, err := buffered.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(buffered)
		if err != nil {
			logger.Printf("[BlockList] Gzip error: %v", err)
			return
		}
		defer gz.Close()
		reader = gz
	}

	// Write to a temp file and rename atomically, so a download that dies mid-copy
	// (network drop, process kill) can't leave destPath truncated and destroy the
	// last known-good blocklist.
	tmpPath := destPath + ".tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		logger.Printf("[BlockList] File create error: %v", err)
		return
	}

	n, err := io.Copy(out, reader)
	if err != nil {
		out.Close()
		os.Remove(tmpPath)
		logger.Printf("[BlockList] File write error: %v", err)
		return
	}
	if err := out.Close(); err != nil {
		os.Remove(tmpPath)
		logger.Printf("[BlockList] File close error: %v", err)
		return
	}
	// Refuse to install something with no usable ranges. iblocklist serves a captcha page
	// to anything that looks like a browser, and a downloaded HTML page would otherwise
	// replace a working blocklist with an empty one, silently.
	if ranges := torrutils.CountRanges(tmpPath); ranges == 0 {
		os.Remove(tmpPath)
		logger.Printf("[BlockList] Refused: %d bytes downloaded but no usable ranges — keeping the previous list", n)
		return
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		logger.Printf("[BlockList] File rename error: %v", err)
		return
	}
	if err := os.WriteFile(urlPath, []byte(urlStr), 0644); err != nil {
		logger.Printf("[BlockList] WARNING: could not record the source URL: %v", err)
	}

	logger.Printf("[BlockList] Updated successfully: %d bytes saved to %s", n, destPath)

	if list, err := torrutils.ReadBlockedIP(); err == nil {
		torr.SetIPBlocklist(list)
		logger.Printf("[BlockList] Live-reloaded into running engine")
	} else {
		logger.Printf("[BlockList] WARNING: downloaded but failed to reload live: %v", err)
	}
}

// GetStateDir returns the centralized state directory path.
func GetStateDir() string {
	if gc().RootPath == "" {
		return "/home/pi/STATE"
	}
	return filepath.Join(gc().RootPath, "STATE")
}

// --- InodeMap globals & wrappers ---

var globalInodeMap *vfs.InodeMap

func InitGlobalInodeMap(stateDir string, logger *log.Logger) error {
	savePath := vfs.GetDefaultInodeMapPath(stateDir)
	dir := filepath.Dir(savePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create inode map directory: %w", err)
	}
	globalInodeMap = vfs.NewInodeMap(savePath, &vfsLogger{logger})
	return nil
}

// finishGlobalInodeMapInit loads the map and starts the saver. Split from
// InitGlobalInodeMap so the load runs once, after StateDB had a chance to attach:
// loading JSON first would report an empty map on every migrated install.
func finishGlobalInodeMapInit(logger *log.Logger) {
	if globalInodeMap == nil {
		return
	}
	if !globalInodeMap.HasDB() {
		if err := globalInodeMap.LoadFromDisk(); err != nil {
			logger.Printf("WARNING: Failed to load inode map: %v (falling back to filename hash)", err)
		}
		files, dirs, _, _ := GetInodeMapStats()
		logger.Printf("InodeMap: Initialized with %d files, %d dirs from %s", files, dirs, vfs.GetDefaultInodeMapPath(GetStateDir()))
	}
	globalInodeMap.StartBackgroundSaver()
}

func ShutdownGlobalInodeMap() {
	if globalInodeMap != nil {
		globalInodeMap.Stop()
	}
}

func getFileInodeFromMap(fullPath string) uint64 {
	if globalInodeMap == nil {
		return hashFilenameToInode(filepath.Base(fullPath)) & vfs.InodeFileMask
	}
	if inode := globalInodeMap.GetFileInode(fullPath); inode != 0 {
		return inode
	}
	if inode := globalInodeMap.GetFileInodeByName(filepath.Base(fullPath)); inode != 0 {
		return inode
	}
	return hashFilenameToInode(filepath.Base(fullPath)) & vfs.InodeFileMask
}

// publishAudioProjectionsLive wires a committed audio batch into the live VFS. Every
// inode is registered before the namespace gains the paths, exactly as startup
// reconciliation does, so a Readdir that lands before the first Lookup emits the
// content-derived inode instead of a basename-derived fallback (or, through the basename
// map, the inode of a different projection that happens to share the basename). The batch
// lands in one namespace update, so no reader observes half an album.
func publishAudioProjectionsLive(ps []library.AudioProjection) {
	for _, p := range ps {
		if globalInodeMap != nil {
			full := filepath.Join(physicalSourcePath, string(p.Section), filepath.FromSlash(p.VirtualPath))
			globalInodeMap.AddFile(full, vfs.SegmentInodeHash(p.Hash, p.CueTrack), p.FileIndex)
		}
	}
	if globalAudioNamespace != nil {
		globalAudioNamespace.AddProjections(ps)
	}
}

func getDirInodeFromMap(relativePath string) uint64 {
	if globalInodeMap == nil {
		return vfs.GenerateDirInode(relativePath)
	}
	return globalInodeMap.GetDirInode(relativePath)
}

func addFileToInodeMap(fullPath, url string) uint64 {
	if globalInodeMap == nil {
		return 0
	}
	hash, index := vfs.ExtractHashAndIndex(url)
	if hash == "" {
		return 0
	}
	return globalInodeMap.AddFile(fullPath, hash, index)
}

// addMetaToInodeMap registers a virtual file's inode. A cue track joins its track to
// the key, since the tracks of an image share one torrent file; every other file,
// video included, goes through addFileToInodeMap unchanged.
func addMetaToInodeMap(fullPath string, meta *vfs.Metadata) uint64 {
	if meta.SegmentTrack <= 0 {
		return addFileToInodeMap(fullPath, meta.URL)
	}
	if globalInodeMap == nil {
		return 0
	}
	hash, index := vfs.ExtractHashAndIndex(meta.URL)
	if hash == "" {
		return 0
	}
	return globalInodeMap.AddFile(fullPath, vfs.SegmentInodeHash(hash, meta.SegmentTrack), index)
}

func GetInodeMapStats() (files, dirs, hits, misses int64) {
	if globalInodeMap == nil {
		return 0, 0, 0, 0
	}
	return globalInodeMap.Stats()
}

// vfsLogger adapts *log.Logger to the vfs.Logger interface.
type vfsLogger struct{ logger *log.Logger }

func (l *vfsLogger) Printf(format string, args ...interface{}) {
	l.logger.Printf(format, args...)
}

// pathUsesSSDWarmup reports whether a projection at this physical path takes the
// movie/TV head/tail SSD warmup. A path outside every known section keeps the
// existing behavior; only the audio sections opt out.
func pathUsesSSDWarmup(path string) bool {
	section, ok := library.SectionForPath(physicalSourcePath, path)
	if !ok {
		return true
	}
	return library.UsesSSDWarmup(section)
}

// classifyVFSPath is the single dispatch decision every FUSE entry point shares:
// Readdir, Lookup, Getattr and Open must agree about what is a virtual file, or a
// scanner sees names it cannot open. physicalSourcePath is the effective root,
// which is the CLI argument when one was given.
// globalAudioNamespace is the committed audio namespace the VFS dispatches on.
// Extension alone must not publish a projection: a stub the registry disowned,
// or one nobody ever committed, would otherwise be listed and served.
var globalAudioNamespace = library.NewAudioNamespace()

func classifyVFSPath(fullPath string) library.VFSClass {
	return library.ClassifyProjection(physicalSourcePath, fullPath, globalAudioNamespace)
}

// stubSizeLimits picks the size guard for a stub. Audio drops the movie-sized
// floor: a valid track or book part is legitimately small.
// Size limits follow the file format, not ownership: reading a stub must work
// even for a projection the registry has disowned, or recovery could not see it.
func stubSizeLimits(fullPath string) vfs.SizeLimits {
	if library.ClassifyPath(physicalSourcePath, fullPath).Audio {
		return vfs.AudioSizeLimits
	}
	return vfs.VideoSizeLimits
}

// audioOwnershipRegistry is what every drop guard consults. The nil case means
// audio is genuinely not configured; when the state DB was expected but could not
// be opened, an unavailable registry is returned instead so a tolerated
// persistence failure cannot become permission to delete shared torrents.
func audioOwnershipRegistry() library.AudioRegistry {
	if stateDB != nil {
		return stateDB
	}
	if gc().EnableStateDB {
		return library.UnavailableAudioRegistry{Err: errStateDBUnavailable}
	}
	return nil
}

// audioNamespaceState reports the audio namespace readiness to external observers:
// "Ready" only once reconciliation published a coherent committed set, never during
// a partial rebuild (spec 8).
func audioNamespaceState() string {
	if globalAudioNamespace == nil {
		return "Uninitialized"
	}
	return globalAudioNamespace.State().String()
}

func audioNamespaceEntries() int {
	if globalAudioNamespace == nil {
		return 0
	}
	return globalAudioNamespace.Len()
}

// audioProjectionRegistry returns the projection registry only when it is really
// open: a typed-nil *metadb.DB would satisfy the interface and then panic.
func audioProjectionRegistry() library.AudioProjectionRegistry {
	if stateDB == nil {
		return nil
	}
	return stateDB
}

// audioRemovalRegistry mirrors audioProjectionRegistry for the removal half of the
// registry, so a boot whose DB failed to open cannot half-enable the endpoint.
func audioRemovalRegistry() library.AudioRemovalRegistry {
	if stateDB == nil {
		return nil
	}
	return stateDB
}

// unpublishAudioProjectionLive drops a removed projection from the live VFS and clears
// the caches that still answer for it, before its stub is unlinked.
func unpublishAudioProjectionLive(p library.AudioProjection) {
	if globalAudioNamespace != nil {
		globalAudioNamespace.Remove(p.Path())
	}
	invalidateSyncRemovedPath(filepath.Join(physicalSourcePath, string(p.Section), filepath.FromSlash(p.VirtualPath)))
}

var errStateDBUnavailable = errors.New("state DB is enabled but was not opened")

// audioReadyWait bounds how long a VFS operation waits for reconciliation. The
// wait exists because reconciliation always finishes; the bound exists because
// "always" is a claim about code that could regress. Without it, a builder that
// never runs would hang the mount for the life of the process, and a hung mount
// is harder to diagnose than an I/O error.
const audioReadyWait = 2 * time.Minute

// audioWaitServable blocks until an audio directory may be served, and reports
// the errno to fail with when it may not.
//
// Movies and tv return immediately in every state: they are read straight from
// disk and must never wait on the audio registry.
//
// This replaced a bare syscall.EAGAIN. Pass 4 read the scanner sources: GNU ls
// reports and stops, Navidrome retries once then returns the entries it has
// (none), and Audiobookshelf converts the error to an empty slice and then marks
// existing items missing. The errno chosen to avoid an apparent-empty library
// produced exactly that on two supported scanners. Blocking is what a network
// filesystem does, and scanners tolerate slow far better than empty.
func audioWaitServable(ctx context.Context, dir string) syscall.Errno {
	section, ok := library.SectionForPath(physicalSourcePath, filepath.Join(dir, "x"))
	if !ok || !library.IsAudioSection(section) {
		return 0
	}
	waitCtx, cancel := context.WithTimeout(ctx, audioReadyWait)
	defer cancel()

	err := globalAudioNamespace.WaitServable(waitCtx)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, library.ErrAudioReconcileFailed):
		// Terminal. Never ENOENT: a scanner reads absence as a deletion.
		return syscall.EIO
	case ctx.Err() != nil:
		// The caller gave up, not us. EINTR says interrupted rather than failed.
		return syscall.EINTR
	default:
		// Our own deadline expired, so reconciliation never terminated.
		logger.Printf("AUDIO: readiness wait expired after %s for %s", audioReadyWait, dir)
		return syscall.EIO
	}
}

// audioProjectionMeta builds an audio file's metadata from its committed registry
// row instead of the physical stub. The registry is authoritative for audio
// (spec §5.1), and the stream URL is recomputed from (hash, file_index) rather
// than read back from the stub, which spec §5.2 calls a second potentially stale
// source of truth. A stub swapped underneath therefore cannot change what is
// served, and a stub with an unparseable URL cannot make a committed row
// unreadable.
func audioProjectionMeta(fullPath string, p library.AudioProjection) *vfs.Metadata {
	return &vfs.Metadata{
		URL:                 fmt.Sprintf("%s/stream?link=%s&index=%d&play", strings.TrimRight(gc().GoStormBaseURL, "/"), p.Hash, p.FileIndex),
		Size:                p.Size,
		Mtime:               time.Unix(0, p.MtimeNS),
		Path:                fullPath,
		ExternalID:          p.ExternalID,
		ExternalIDNamespace: p.ExternalIDNamespace,
		Audio:               true,
		SegmentTrack:        p.CueTrack,
		SegmentOffset:       p.ByteOffset,
		SegmentHeader:       p.Header,
	}
}

// committedAudioMeta returns registry-derived metadata when the namespace owns
// the path. ok is false for video and for anything uncommitted.
func committedAudioMeta(fullPath string) (*vfs.Metadata, bool) {
	p, ok := library.CommittedProjectionFor(physicalSourcePath, fullPath, globalAudioNamespace)
	if !ok {
		return nil, false
	}
	return audioProjectionMeta(fullPath, p), true
}

// stubMeta returns a virtual file's metadata. Audio comes from its committed
// registry row; video keeps reading its physical stub exactly as before.
func stubMeta(fullPath string) (*vfs.Metadata, error) {
	if meta, ok := committedAudioMeta(fullPath); ok {
		return meta, nil
	}
	return getOrReadMeta(fullPath)
}
