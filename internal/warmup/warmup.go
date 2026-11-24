package warmup

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FileSize is the per-file head cache cap. Set at init from config, default 64 MB.
var FileSize int64 = 64 * 1024 * 1024

const (
	TailWarmupSize int64 = 16 * 1024 * 1024        // V265: 16 MB tail (Cues/seek index)
	warmupQuota    int64 = 32 * 1024 * 1024 * 1024 // fallback default 32 GB (overridden by config)
	warmupSuffix         = ".warmup"
	tailSuffix           = ".warmup-tail"    // V265: separate file for tail
	tailDoneSuffix       = ".ok"             // next to a tail file: the window was complete and synced
	denseMarker          = ".dense-migrated" // one-shot marker: head cache rebuilt for the density invariant
	warmupWriteBuf       = 16 * 1024 * 1024  // 16 MB — matches pump chunk size
	handleIdleMax        = 30 * time.Second  // close idle file handles after 30s
	missingTTL           = 10 * time.Second  // negative-cache lifetime, matches sizeCache
	// headReadyFloor is the least covered bytes for a head warmup file to count as
	// warm. A file holding a few bytes is the residue of an aborted attempt (38 bytes
	// in production), and treating it as warm sends Open down the warm path and
	// falsifies any "this title has a warmup" reading.
	headReadyFloor = 1 << 20
)

var diskQuotaGB int64

// DiskWarmup is the global instance, nil when disabled.
var DiskWarmup *DiskWarmupCache

// OnWarmupStateChange, if set, is called synchronously from the single writeWorker goroutine at
// the exact moment a head warmup fetch starts (active=true) or completes (active=false) - wired
// up once at startup (main.go) to signal the anacrolix-torrent fork's Torrent.SetWarmupActive.
// Deliberately synchronous, not polled: WriteChunk() only enqueues onto writeCh and returns
// immediately, so a caller checking IsWarmingUp() right after WriteChunk() returns can race ahead
// of this worker actually processing the write - this callback fires precisely when the state
// itself transitions, eliminating that race by construction.
var OnWarmupStateChange func(hash string, fileID int, active bool)

var warmupDurationBuckets [8]atomic.Int64 // <2s,<5s,<10s,<15s,<30s,<60s,<120s,>=120s

func recordWarmupDuration(d time.Duration) {
	s := d.Seconds()
	idx := 7
	switch {
	case s < 2:
		idx = 0
	case s < 5:
		idx = 1
	case s < 10:
		idx = 2
	case s < 15:
		idx = 3
	case s < 30:
		idx = 4
	case s < 60:
		idx = 5
	case s < 120:
		idx = 6
	}
	warmupDurationBuckets[idx].Add(1)
}

// WarmupDurationBucketCounts returns a snapshot of the bucket counts for /metrics.
func WarmupDurationBucketCounts() [8]int64 {
	var out [8]int64
	for i := range warmupDurationBuckets {
		out[i] = warmupDurationBuckets[i].Load()
	}
	return out
}

// V261: sync.Pool for write buffers — avoids 16MB heap allocs per chunk.
var warmupWritePool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, warmupWriteBuf)
		return &buf
	},
}

type warmupWrite struct {
	hash   string
	fileID int
	buf    *[]byte // pooled buffer pointer — returned to warmupWritePool after write
	len    int     // actual data length within buf
	off    int64
}

// cachedHandle holds an open file descriptor with last-access tracking.
type cachedHandle struct {
	f            *os.File
	lastUsedNano atomic.Int64 // atomic to prevent race with handleReaper
	closed       atomic.Bool  // set by closeHandle before f.Close() so writeWorker can detect stale handles
}

// V264: sizeEntry stores the cached size of a warmup file with TTL.
type sizeEntry struct {
	size      int64
	updatedAt time.Time
}

// tailSpan is a half-open [start,end) byte range within a tail warmup file.
type tailSpan struct{ start, end int64 }

// V265: tailRange tracks which ranges of a tail warmup file were actually written.
// Writes land at whatever offsets the client probes, so the file is sparse: a single
// watermark would mark unwritten holes as covered, and reading a hole returns zeros
// with err==nil - served to the player as if it were real MKV data.
type tailRange struct {
	mu     sync.Mutex
	ranges []tailSpan // disjoint, merged, sorted by start
}

// add merges [start,end) into the covered set.
func (tr *tailRange) add(start, end int64) {
	if end <= start {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.ranges = append(tr.ranges, tailSpan{start, end})
	sort.Slice(tr.ranges, func(i, j int) bool { return tr.ranges[i].start < tr.ranges[j].start })
	merged := tr.ranges[:1]
	for _, s := range tr.ranges[1:] {
		last := &merged[len(merged)-1]
		if s.start <= last.end {
			if s.end > last.end {
				last.end = s.end
			}
			continue
		}
		merged = append(merged, s)
	}
	tr.ranges = merged
}

// covers reports whether all of [start,end) was written. Spans are disjoint and merged,
// so a covered request always falls inside a single span.
func (tr *tailRange) covers(start, end int64) bool {
	if end <= start {
		return false
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	i := sort.Search(len(tr.ranges), func(i int) bool { return tr.ranges[i].end > start })
	return i < len(tr.ranges) && tr.ranges[i].start <= start && tr.ranges[i].end >= end
}

// complete reports full coverage of the tail window.
func (tr *tailRange) complete() bool { return tr.covers(0, TailWarmupSize) }

// DiskWarmupCache persists the first 128MB of each streamed file to SSD.
type DiskWarmupCache struct {
	dir          string
	mu           sync.Mutex // protects quota enforcement
	totalSize    int64      // V288: Tracked total size of all warmup files in bytes
	missing      sync.Map   // path -> time.Time (negative cache for missing files)
	handles      sync.Map   // path -> *cachedHandle (cached file descriptors)
	sizeCache    sync.Map   // V264: path -> sizeEntry (cached file sizes with TTL)
	tailCoverage sync.Map   // V265: path -> *tailRange (written range tracking)
	tailFills    sync.Map   // path -> true while a sequential tail fill is running
	warmupStarts sync.Map   // path -> time.Time (STARTING timestamp, for duration histogram)
	touched      sync.Map   // path -> time.Time of the last mtime refresh on read
	mids         sync.Map   // mid path -> *midFile (coverage, from the .cov file)
	midWriting   sync.Map   // mid path -> true while a piece is written to it
	writeCh      chan warmupWrite

	headMax    int64 // head cap; 0 = FileSize
	quotaBytes int64 // 0 = diskQuotaGB, else warmupQuota
	idxMu      sync.Mutex
	idx        atomic.Pointer[warmIndex] // nil until ServeIndex
	stop       chan struct{}
	stopOnce   sync.Once
	wg         sync.WaitGroup
}

// headCap is the head size of this cache.
func (d *DiskWarmupCache) headCap() int64 {
	if d.headMax > 0 {
		return d.headMax
	}
	return FileSize
}

func (d *DiskWarmupCache) quota() int64 {
	if d.quotaBytes > 0 {
		return d.quotaBytes
	}
	if diskQuotaGB > 0 {
		return diskQuotaGB * 1024 * 1024 * 1024
	}
	return warmupQuota
}

var logf = log.New(os.Stdout, "[DiskWarmup] ", log.LstdFlags)

// InitDiskWarmup creates the global warmup cache in dir when useDisk is set (the engine
// settings UseDisk and TorrentsSavePath, read once at start).
func InitDiskWarmup(quotaGB int64, useDisk bool, dir string) {
	diskQuotaGB = quotaGB
	FileSize = 64 * 1024 * 1024 // default, overridden by config
	if !useDisk {
		return
	}
	if dir == "" || dir == "/" {
		return
	}
	if DiskWarmup = New(dir, FileSize, quotaGB); DiskWarmup != nil {
		logf.Printf("[DiskWarmup] Active — dir=%s quota=%dGB warmup=%dMB", dir, quotaGB, FileSize/1024/1024)
	}
}

// New opens a warmup cache in dir with the head size and quota given (0 = the defaults) and
// starts its writer and reaper; nil if dir cannot be created.
func New(dir string, headSize, quotaGB int64) *DiskWarmupCache {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil
	}
	d := &DiskWarmupCache{
		dir:        dir,
		writeCh:    make(chan warmupWrite, 32),
		headMax:    headSize,
		quotaBytes: quotaGB << 30,
		stop:       make(chan struct{}),
	}

	// One-shot migration: head files written before the density invariant may hold holes
	// that read back as zeros, and nothing on disk can tell them apart from dense ones.
	// Head warmup is the library-wide instant-start cache, so this must not repeat on
	// every boot - the marker makes it a single cold start per file, once.
	migrated := filepath.Join(dir, denseMarker)
	dropHeads := false
	if _, err := os.Stat(migrated); os.IsNotExist(err) {
		dropHeads = true
	}

	if total, dropped, residues, err := d.scanExisting(dropHeads); err == nil {
		atomic.StoreInt64(&d.totalSize, total)
		logf.Printf("[DiskWarmup] Initial size: %.1fGB (dropped %d stale cache files, %d residues)", float64(total)/(1<<30), dropped, residues)
	}

	if dropHeads {
		if f, err := os.Create(migrated); err == nil {
			f.Close()
			logf.Printf("[DiskWarmup] Head cache rebuilt for the write-density invariant (one-shot)")
		}
	}

	d.wg.Add(2)
	go d.writeWorker()
	go d.handleReaper()
	return d
}

// Close stops the writer and the reaper and closes the open files (tests).
func (d *DiskWarmupCache) Close() {
	if d == nil || d.stop == nil {
		return
	}
	d.stopOnce.Do(func() { close(d.stop) })
	d.wg.Wait()
	d.handles.Range(func(k, _ any) bool { d.closeHandle(k.(string)); return true })
}

// scanExisting walks the cache at startup. A tail stays only with its completion marker
// (written after the full window was synced) and is restored as fully covered; any other
// tail goes, since nothing on disk proves which of its ranges are real. Head files go when
// the one-shot density migration runs, and a head below the ready floor goes as a residue.
func (d *DiskWarmupCache) scanExisting(dropHeads bool) (total int64, dropped, residues int, err error) {
	dir := d.dir
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, 0, err
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, midSuffix+covSuffix) {
			if !fileExists(filepath.Join(dir, strings.TrimSuffix(name, covSuffix))) {
				os.Remove(filepath.Join(dir, name)) // orphan coverage
			}
			continue
		}
		if strings.HasSuffix(name, midSuffix) {
			total += d.loadMid(filepath.Join(dir, name))
			continue
		}
		if strings.HasSuffix(name, tailSuffix+tailDoneSuffix) {
			if _, serr := os.Stat(filepath.Join(dir, strings.TrimSuffix(name, tailDoneSuffix))); serr != nil {
				os.Remove(filepath.Join(dir, name)) // orphan marker
			}
			continue
		}
		if strings.HasSuffix(name, tailSuffix) {
			path := filepath.Join(dir, name)
			if info, ierr := e.Info(); ierr == nil && info.Size() == TailWarmupSize && fileExists(path+tailDoneSuffix) {
				d.tailRangeFor(path).add(0, TailWarmupSize)
				total += TailWarmupSize
				continue
			}
			os.Remove(path + tailDoneSuffix)
			if os.Remove(path) == nil {
				dropped++
			}
			continue
		}
		if dropHeads && strings.HasSuffix(name, warmupSuffix) {
			if os.Remove(filepath.Join(dir, name)) == nil {
				dropped++
			}
			continue
		}
		if strings.HasSuffix(name, warmupSuffix) {
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			if info.Size() < headReadyFloor {
				if os.Remove(filepath.Join(dir, name)) == nil {
					residues++
				}
				continue
			}
			total += info.Size()
		}
	}
	return total, dropped, residues, nil
}

// touchInterval bounds mtime refreshes on read: eviction orders by mtime, minutes are enough.
const touchInterval = time.Minute

// touch marks path as used now, so quota eviction follows reads and not only the last write.
func (d *DiskWarmupCache) touch(path string) {
	now := time.Now()
	if v, ok := d.touched.Load(path); ok && now.Sub(v.(time.Time)) < touchInterval {
		return
	}
	d.touched.Store(path, now)
	os.Chtimes(path, now, now)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// removeTail deletes a tail file with its completion marker and forgets its coverage.
func (d *DiskWarmupCache) removeTail(path string) {
	d.tailCoverage.Delete(path)
	os.Remove(path + tailDoneSuffix)
}

// markTailDone syncs a complete tail and writes its marker, so the next start keeps it.
func (d *DiskWarmupCache) markTailDone(path string, ch *cachedHandle) {
	if fileExists(path + tailDoneSuffix) {
		return
	}
	if err := ch.f.Sync(); err != nil {
		return
	}
	if f, err := os.Create(path + tailDoneSuffix); err == nil {
		f.Sync()
		f.Close()
		d.indexAdd(path)
	}
}

func (d *DiskWarmupCache) handleReaper() {
	defer d.wg.Done()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-ticker.C:
			d.reapIdle(handleIdleMax)
		}
	}
}

// reapIdle closes handles idle longer than max and drops the head files that never
// grew past the ready floor. Without the drop the residue of an aborted attempt (38
// bytes in production) lives until quota pressure, passes for warm coverage, and
// falsifies every "it has a warmup" reading.
func (d *DiskWarmupCache) reapIdle(max time.Duration) {
	now := time.Now()
	d.handles.Range(func(key, val interface{}) bool {
		path := key.(string)
		ch := val.(*cachedHandle)
		if now.UnixNano()-ch.lastUsedNano.Load() <= max.Nanoseconds() {
			return true
		}
		// V2.0: Use LoadAndDelete + double-check to prevent TOCTOU with getHandle.
		// If getHandle updated lastUsedNano between Range and delete, re-insert.
		if actual, loaded := d.handles.LoadAndDelete(path); loaded {
			ac := actual.(*cachedHandle)
			if now.UnixNano()-ac.lastUsedNano.Load() < max.Nanoseconds() {
				d.handles.Store(path, ac)
				return true
			}
			ac.closed.Store(true)
			ac.f.Close()
			d.warmupStarts.Delete(path)
			d.dropResidue(path)
		}
		return true
	})
}

// dropResidue removes a head file that stayed below the ready floor: the shape of an
// aborted attempt, not a warmup worth keeping. Tail files are left alone, since their
// readiness already comes from in-memory coverage instead of file size.
func (d *DiskWarmupCache) dropResidue(path string) {
	if !strings.HasSuffix(path, warmupSuffix) {
		return
	}
	// A new attempt may have reopened the file since the reaper decided on the old
	// idle handle: unlinking it now would lose the warmup being written.
	if _, ok := d.handles.Load(path); ok {
		return
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Size() >= headReadyFloor {
		return
	}
	logf.Printf("[DiskWarmup] DROPPED %s (%.1fKB): incomplete head warmup below the ready floor",
		filepath.Base(path), float64(fi.Size())/(1<<10))
	d.sizeCache.Delete(path)
	if os.Remove(path) == nil {
		// Written bytes were counted against the quota, so the drop gives them back
		// instead of leaving the drift to the next full walk in enforceQuotaLocked.
		atomic.AddInt64(&d.totalSize, -fi.Size())
		d.indexRemove(path)
	}
	d.missing.Store(path, time.Now())
}

func (d *DiskWarmupCache) getHandle(path string) (*cachedHandle, error) {
	if val, ok := d.handles.Load(path); ok {
		ch := val.(*cachedHandle)
		ch.lastUsedNano.Store(time.Now().UnixNano())
		return ch, nil
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	ch := &cachedHandle{f: f}
	ch.lastUsedNano.Store(time.Now().UnixNano())
	if actual, loaded := d.handles.LoadOrStore(path, ch); loaded {
		f.Close()
		existing := actual.(*cachedHandle)
		existing.lastUsedNano.Store(time.Now().UnixNano())
		return existing, nil
	}
	return ch, nil
}

// storeNewHandle caches the handle of a file just created, keeping one a concurrent getHandle
// stored first.
func (d *DiskWarmupCache) storeNewHandle(path string, f *os.File) {
	ch := &cachedHandle{f: f}
	ch.lastUsedNano.Store(time.Now().UnixNano())
	if prev, loaded := d.handles.LoadOrStore(path, ch); loaded {
		prev.(*cachedHandle).lastUsedNano.Store(time.Now().UnixNano())
		f.Close()
	}
}

func (d *DiskWarmupCache) closeHandle(path string) {
	if val, ok := d.handles.LoadAndDelete(path); ok {
		ch := val.(*cachedHandle)
		ch.closed.Store(true) // mark before Close so writeWorker can detect stale handle
		ch.f.Close()
	}
	// Aborted warmups (eviction, hash removal, idle close) never reach the
	// completion branch in processWrite that LoadAndDeletes warmupStarts —
	// clear it here so the entry doesn't leak forever under a rotating cache.
	d.warmupStarts.Delete(path)
}

func (d *DiskWarmupCache) writeWorker() {
	defer d.wg.Done()
	for {
		select {
		case <-d.stop:
			return
		case w := <-d.writeCh:
			d.processWrite(w.hash, w.fileID, (*w.buf)[:w.len], w.off)
			warmupWritePool.Put(w.buf)
		}
	}
}

func (d *DiskWarmupCache) WriteChunk(hash string, fileID int, data []byte, off int64) {
	if off > d.headCap() || d.writeCh == nil {
		return
	}

	bufPtr := warmupWritePool.Get().(*[]byte)
	if len(*bufPtr) < len(data) {
		warmupWritePool.Put(bufPtr)
		buf := make([]byte, len(data))
		copy(buf, data)
		bufPtr = &buf
	} else {
		copy(*bufPtr, data)
	}

	select {
	case d.writeCh <- warmupWrite{hash, fileID, bufPtr, len(data), off}:
	default:
		warmupWritePool.Put(bufPtr)
	}
}

func (d *DiskWarmupCache) processWrite(hash string, fileID int, data []byte, off int64) {
	FileSize := d.headCap()
	if off > FileSize {
		return
	}
	// Only truncate chunks that straddle the boundary from below.
	// Chunks starting AT FileSize are written in full (the boundary chunk).
	if off < FileSize && off+int64(len(data)) > FileSize {
		data = data[:FileSize-off]
	}

	path := d.filePath(hash, fileID)

	if val, ok := d.sizeCache.Load(path); ok {
		if entry := val.(sizeEntry); entry.size > FileSize {
			return
		}
	} else if fi, err := os.Stat(path); err == nil && fi.Size() > FileSize {
		d.sizeCache.Store(path, sizeEntry{size: fi.Size(), updatedAt: time.Now()})
		return
	}

	// Head files must stay dense: GetAvailableRange reports their on-disk size as
	// contiguous coverage from 0 (pump start offset, warmup gates, ReadAt), and a hole
	// below that size reads back as zeros, not as a short read. A write that would leave
	// one is dropped - the sequential pump rewrites that range in order anyway.
	prevSize := d.headSize(path)
	if off > prevSize {
		return
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		if off != 0 {
			return // removed under this write: [0, off) would be a hole reported as covered
		}
		os.MkdirAll(d.dir, 0755)

		d.mu.Lock()
		d.enforceQuotaLocked(FileSize)
		d.mu.Unlock()
		d.missing.Delete(path)

		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			logf.Printf("[DiskWarmup] Error creating file: %v", err)
			return
		}
		d.storeNewHandle(path, f)
		d.indexAdd(path)
		d.warmupStarts.Store(path, time.Now())
		logf.Printf("[DiskWarmup] STARTING %s at offset %d", filepath.Base(path), off)
		if OnWarmupStateChange != nil {
			OnWarmupStateChange(hash, fileID, true)
		}
	}

	ch, err := d.getHandle(path)
	if err != nil {
		return
	}
	if ch.closed.Load() {
		logf.Printf("[DiskWarmup] Write skipped: handle closed for %s", filepath.Base(path))
		return
	}

	n, err := ch.f.WriteAt(data, off)
	if err != nil {
		logf.Printf("[DiskWarmup] WriteAt error for %s: %v", filepath.Base(path), err)
		return
	}

	// The file demonstrably exists now, so no stale negative-cache entry may outlive it.
	d.missing.Delete(path)

	// A write overlapping already-covered bytes must not shrink the recorded coverage.
	currentSize := off + int64(n)
	if currentSize > prevSize {
		atomic.AddInt64(&d.totalSize, currentSize-prevSize)
	} else {
		currentSize = prevSize
	}

	d.sizeCache.Store(path, sizeEntry{size: currentSize, updatedAt: time.Now()})

	if off+int64(n) >= FileSize {
		if startVal, ok := d.warmupStarts.LoadAndDelete(path); ok {
			recordWarmupDuration(time.Since(startVal.(time.Time)))
		}
		logf.Printf("[DiskWarmup] COMPLETED %s", filepath.Base(path))
		if OnWarmupStateChange != nil {
			OnWarmupStateChange(hash, fileID, false)
		}
	}
}

func (d *DiskWarmupCache) filePath(hash string, fileID int) string {
	return filepath.Join(d.dir, hash+"-"+strconv.Itoa(fileID)+warmupSuffix)
}

func (d *DiskWarmupCache) tailPath(hash string, fileID int) string {
	return filepath.Join(d.dir, hash+"-"+strconv.Itoa(fileID)+tailSuffix)
}

// GetAvailableRange returns the contiguous bytes cached from offset 0. Callers treat the
// result as coverage (pump start offset, warmup gates, ReadAt clamp), which holds only
// because processWrite refuses writes that would leave a hole.
func (d *DiskWarmupCache) GetAvailableRange(hash string, fileID int) int64 {
	path := d.filePath(hash, fileID)
	if val, ok := d.missing.Load(path); ok {
		// The negative cache must expire: a file absent at one Open is routinely created
		// moments later, and a permanent entry hides a complete warmup file for the rest
		// of the process lifetime (headReady false -> slow resolve path on every Open).
		since := time.Since(val.(time.Time))
		if since < missingTTL {
			return 0
		}
		d.missing.Delete(path)
		if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
			logf.Printf("[DiskWarmup] Negative cache hid an existing warmup file (%.1fMB, %s) for %s",
				float64(fi.Size())/(1<<20), since.Truncate(time.Second), filepath.Base(path))
		}
	}

	if val, ok := d.sizeCache.Load(path); ok {
		entry := val.(sizeEntry)
		if time.Since(entry.updatedAt) < 10*time.Second {
			return entry.size
		}
	}

	fi, err := os.Stat(path)
	if err != nil {
		d.missing.Store(path, time.Now())
		return 0
	}

	if fi.Size() > d.headCap()+(16*1024*1024) {
		logf.Printf("[DiskWarmup] CORRUPT CACHE detected (Size: %.1fMB > 128MB) for %s. Removing.", float64(fi.Size())/(1<<20), hash[:8])
		d.closeHandle(path)
		d.sizeCache.Delete(path)
		os.Remove(path)
		d.indexRemove(path)
		d.missing.Store(path, time.Now())
		return 0
	}

	d.sizeCache.Store(path, sizeEntry{size: fi.Size(), updatedAt: time.Now()})
	return fi.Size()
}

// HeadReady reports whether the head warmup covers enough to be worth treating the
// file as warm. GetAvailableRange keeps reporting the raw coverage: the pump and the
// read clamps want the number, the Open decision wants the floor.
func (d *DiskWarmupCache) HeadReady(hash string, fileID int) bool {
	if d == nil {
		return false
	}
	return d.GetAvailableRange(hash, fileID) >= headReadyFloor
}

// TailReady reports whether the whole tail window for hash/fileID is on SSD, i.e. Open
// can serve end-of-file probes without the network. Coverage is tracked in memory only:
// the on-disk size of a sparse file says nothing about which ranges hold real data.
func (d *DiskWarmupCache) TailReady(hash string, fileID int) bool {
	if d == nil {
		return false
	}
	val, ok := d.tailCoverage.Load(d.tailPath(hash, fileID))
	return ok && val.(*tailRange).complete()
}

// tailRangeFor returns the coverage tracker for path, creating it if absent.
func (d *DiskWarmupCache) tailRangeFor(path string) *tailRange {
	if val, ok := d.tailCoverage.Load(path); ok {
		return val.(*tailRange)
	}
	val, _ := d.tailCoverage.LoadOrStore(path, &tailRange{})
	return val.(*tailRange)
}

// headSize returns the bytes currently covered in a head warmup file, 0 when absent.
func (d *DiskWarmupCache) headSize(path string) int64 {
	if val, ok := d.sizeCache.Load(path); ok {
		return val.(sizeEntry).size
	}
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}

func (d *DiskWarmupCache) ReadAt(hash string, fileID int, buf []byte, off int64) (int, error) {
	if off > d.headCap() {
		return 0, nil
	}
	path := d.filePath(hash, fileID)

	ch, err := d.getHandle(path)
	if err != nil {
		return 0, nil
	}

	availSize := d.GetAvailableRange(hash, fileID)
	if off >= availSize {
		return 0, nil
	}

	if avail := availSize - off; int64(len(buf)) > avail {
		buf = buf[:avail]
	}

	n, err := ch.f.ReadAt(buf, off)
	if n > 0 {
		d.touch(path)
	}
	return n, err
}

func (d *DiskWarmupCache) WriteTail(hash string, fileID int, data []byte, absoluteOffset, fileSize int64) {
	path := d.tailPath(hash, fileID)

	tailStart := fileSize - TailWarmupSize
	if tailStart < 0 {
		tailStart = 0
	}
	if absoluteOffset < tailStart {
		return
	}

	tailLen := fileSize - tailStart
	relOffset := absoluteOffset - tailStart
	if relOffset >= tailLen {
		return
	}
	if relOffset+int64(len(data)) > tailLen {
		data = data[:tailLen-relOffset]
	}
	if len(data) == 0 {
		return
	}

	tr := d.tailRangeFor(path)
	if tr.complete() {
		return
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		os.MkdirAll(d.dir, 0755)
		d.missing.Delete(path)

		// Kept across restarts, so the whole window counts against the quota from the start.
		d.mu.Lock()
		d.enforceQuotaLocked(tailLen)
		d.mu.Unlock()
		// O_EXCL: a concurrent first write that lost the race must not truncate the winner's bytes.
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0644)
		if err == nil {
			atomic.AddInt64(&d.totalSize, tailLen)
			d.storeNewHandle(path, f)
			logf.Printf("[DiskWarmup] TAIL STARTING %s at relOffset %d", filepath.Base(path), relOffset)
		} else if !os.IsExist(err) {
			return
		}
	}

	ch, err := d.getHandle(path)
	if err != nil {
		return
	}
	if ch.closed.Load() {
		logf.Printf("[DiskWarmup] Write skipped (tail): handle closed for %s", filepath.Base(path))
		return
	}

	n, _ := ch.f.WriteAt(data, relOffset)
	if n <= 0 {
		return
	}
	d.sizeCache.Store(path, sizeEntry{size: relOffset + int64(n), updatedAt: time.Now()})
	tr.add(relOffset, relOffset+int64(n))
	if tr.complete() {
		d.markTailDone(path, ch)
	}
}

func (d *DiskWarmupCache) ReadTail(hash string, fileID int, buf []byte, absoluteOffset, fileSize int64) (int, error) {
	tailStart := fileSize - TailWarmupSize
	if tailStart < 0 {
		tailStart = 0
	}
	if absoluteOffset < tailStart {
		return 0, nil
	}

	tailLen := fileSize - tailStart
	relOffset := absoluteOffset - tailStart
	path := d.tailPath(hash, fileID)

	// Clamp to the tail window: a buffer overhanging EOF still hits if the bytes it can
	// actually return were written.
	readEnd := relOffset + int64(len(buf))
	if readEnd > tailLen {
		readEnd = tailLen
	}

	// Miss unless this exact range was written. No fallback on the file's on-disk size:
	// the file is sparse, and an unwritten hole reads back as zeros, not as a short read.
	val, ok := d.tailCoverage.Load(path)
	if !ok || !val.(*tailRange).covers(relOffset, readEnd) {
		return 0, nil
	}

	ch, err := d.getHandle(path)
	if err != nil {
		return 0, nil
	}

	n, err := ch.f.ReadAt(buf, relOffset)
	if n > 0 {
		d.touch(path)
	}
	return n, err
}

func (d *DiskWarmupCache) RemoveHash(hash string) {
	entries, _ := os.ReadDir(d.dir)
	prefix := hash + "-"
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, midSuffix) {
			d.removeMid(filepath.Join(d.dir, name))
			continue
		}
		if strings.HasPrefix(name, prefix) && (strings.HasSuffix(name, warmupSuffix) || strings.HasSuffix(name, tailSuffix)) {
			fullPath := filepath.Join(d.dir, name)

			if fi, err := e.Info(); err == nil {
				atomic.AddInt64(&d.totalSize, -fi.Size())
			}

			d.closeHandle(fullPath)
			d.sizeCache.Delete(fullPath)
			d.touched.Delete(fullPath)
			d.removeTail(fullPath)
			os.Remove(fullPath)
			d.indexRemove(fullPath)
		}
	}
}

// enforceQuotaLocked frees room for needed bytes: mid files first, oldest first, then heads and
// tails by mtime.
func (d *DiskWarmupCache) enforceQuotaLocked(needed int64) { d.evictLocked(needed, false) }

// roomForMidLocked frees room for a mid piece from other mids only: pieces kept for friends never
// push out the library's heads and tails. False when the mids cannot make enough room.
func (d *DiskWarmupCache) roomForMidLocked(needed int64) bool { return d.evictLocked(needed, true) }

// evictLocked frees room for needed bytes, mids first (only mids if onlyMids); true if it did.
func (d *DiskWarmupCache) evictLocked(needed int64, onlyMids bool) bool {
	quota := d.quota()

	totalSize := atomic.LoadInt64(&d.totalSize)
	if totalSize+needed <= quota {
		return true
	}

	entries, _ := os.ReadDir(d.dir)
	type wFile struct {
		path    string
		size    int64
		modTime int64
	}
	var files, mids []wFile
	var diskTotal int64
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, midSuffix) {
			path := filepath.Join(d.dir, name)
			if info, err := e.Info(); err == nil {
				var n int64 // a sparse file: its coverage, not its apparent size
				if v, ok := d.mids.Load(path); ok {
					n = v.(*midFile).cov.size()
				}
				mids = append(mids, wFile{path, n, info.ModTime().UnixNano()})
				diskTotal += n
			}
			continue
		}
		if !strings.HasSuffix(name, warmupSuffix) && !strings.HasSuffix(name, tailSuffix) {
			continue
		}
		if info, err := e.Info(); err == nil {
			files = append(files, wFile{filepath.Join(d.dir, name), info.Size(), info.ModTime().Unix()})
			diskTotal += info.Size()
		}
	}

	atomic.StoreInt64(&d.totalSize, diskTotal)
	if diskTotal+needed <= quota {
		return true
	}

	sort.Slice(mids, func(i, j int) bool { return mids[i].modTime < mids[j].modTime })
	for _, fi := range mids {
		if diskTotal+needed <= quota {
			break
		}
		if _, ok := d.midWriting.Load(fi.path); ok {
			continue
		}
		if d.evictMid(fi.path) {
			diskTotal -= fi.size
		}
	}
	if onlyMids {
		atomic.StoreInt64(&d.totalSize, diskTotal)
		return diskTotal+needed <= quota
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime < files[j].modTime })
	for _, fi := range files {
		if diskTotal+needed <= quota {
			break
		}
		// A live handle means a reader or writer is on it now: never evict from under it.
		if v, ok := d.handles.Load(fi.path); ok && time.Since(time.Unix(0, v.(*cachedHandle).lastUsedNano.Load())) < handleIdleMax {
			continue
		}
		d.touched.Delete(fi.path)
		d.closeHandle(fi.path)
		d.sizeCache.Delete(fi.path)
		d.removeTail(fi.path)
		os.Remove(fi.path)
		d.indexRemove(fi.path)
		diskTotal -= fi.size
	}
	atomic.StoreInt64(&d.totalSize, diskTotal)
	return diskTotal+needed <= quota
}

// TailFetch fetches bytes from the torrent. Wired at startup to the native bridge: the
// warmup package cannot import it, so the dependency is injected like OnWarmupStateChange.
var TailFetch func(hash string, fileID int, off int64, buf []byte) (int, error)

const tailFillChunk = 1024 * 1024 // per-request size of the sequential fill

// tailFillPacing spaces out the fill so it stays a background trickle; a var so tests
// don't have to wait it out.
var tailFillPacing = 250 * time.Millisecond

// maxTailFills bounds the fills fetching at once; the rest wait their turn.
const maxTailFills = 2

var tailFillSlots = make(chan struct{}, maxTailFills)

// TailFillYield, when set, stops a fill whose file is back in playback (checked per chunk).
var TailFillYield func(hash string, fileID int) bool

// TailIdleDelay is how long a closed file must stay idle before EnsureTailWhenIdle fills its tail.
// Below the 30s pump grace: the pump's stream keeps the torrent resident until then.
var TailIdleDelay = 10 * time.Second

// EnsureTailWhenIdle fills the tail after TailIdleDelay if idle still holds: a library scan or a
// player that sends no media.stop leaves the file closed without anything else starting the fill.
func (d *DiskWarmupCache) EnsureTailWhenIdle(hash string, fileID int, fileSize int64, idle func() bool) {
	if d == nil || d.TailReady(hash, fileID) {
		return
	}
	time.AfterFunc(TailIdleDelay, func() {
		if idle() {
			d.EnsureTail(hash, fileID, fileSize)
		}
	})
}

// TailPaused is the error TailFetch returns while speculative reads are paused (a stream at risk
// of stalling): the fill waits and retries instead of giving up. Wired at startup.
var TailPaused error

// tailPausedRetry spaces the retries while paused; tailPausedMax bounds the wait.
var tailPausedRetry, tailPausedMax = 2 * time.Second, 15 * time.Minute

// EnsureTail fills the whole tail window sequentially in the background so TailReady can
// actually become true. Opportunistic WriteTail only records the ranges a client happened
// to probe, which leaves the MKV Cues index one network round trip away on every open.
//
// Call this only while the file is idle (playback stopped): piece requests at the end of a
// partially downloaded file compete with the sequential download front and stall the
// playhead (observed 2026-08-12).
func (d *DiskWarmupCache) EnsureTail(hash string, fileID int, fileSize int64) {
	if d == nil || TailFetch == nil || hash == "" || fileSize <= 0 {
		return
	}
	if d.TailReady(hash, fileID) {
		return
	}
	path := d.tailPath(hash, fileID)
	if _, busy := d.tailFills.LoadOrStore(path, true); busy {
		return
	}

	// Read once: tests swap them.
	fetch, pacing, yield := TailFetch, tailFillPacing, TailFillYield
	paused, pausedRetry, pausedMax := TailPaused, tailPausedRetry, tailPausedMax
	go func() {
		defer d.tailFills.Delete(path)
		tailFillSlots <- struct{}{}
		defer func() { <-tailFillSlots }()

		tailStart := fileSize - TailWarmupSize
		if tailStart < 0 {
			tailStart = 0
		}
		tailLen := fileSize - tailStart
		tr := d.tailRangeFor(path)

		started := time.Now()
		var fetched int64
		buf := make([]byte, tailFillChunk)
		for rel := int64(0); rel < tailLen; {
			end := rel + tailFillChunk
			if end > tailLen {
				end = tailLen
			}
			if tr.covers(rel, end) {
				rel = end
				continue
			}
			if yield != nil && yield(hash, fileID) {
				logf.Printf("[DiskWarmup] Tail fill paused at %.1f/%.1fMB for %s: file back in playback",
					float64(rel)/(1<<20), float64(tailLen)/(1<<20), filepath.Base(path))
				return
			}
			n, err := fetch(hash, fileID, tailStart+rel, buf[:end-rel])
			if paused != nil && errors.Is(err, paused) && time.Since(started) < pausedMax {
				time.Sleep(pausedRetry)
				continue
			}
			if err != nil || n <= 0 {
				logf.Printf("[DiskWarmup] Tail fill stopped at %.1f/%.1fMB for %s: %v",
					float64(rel)/(1<<20), float64(tailLen)/(1<<20), filepath.Base(path), err)
				return
			}
			d.WriteTail(hash, fileID, buf[:n], tailStart+rel, fileSize)
			fetched += int64(n)
			rel += int64(n)
			time.Sleep(pacing)
		}
		if d.TailReady(hash, fileID) {
			logf.Printf("[DiskWarmup] TAIL COMPLETE %s (%.1fMB fetched in %s)",
				filepath.Base(path), float64(fetched)/(1<<20), time.Since(started).Truncate(time.Second))
		}
	}()
}

// tailFillRunning reports whether a sequential fill is in flight (test helper).
func (d *DiskWarmupCache) tailFillRunning(hash string, fileID int) bool {
	_, busy := d.tailFills.Load(d.tailPath(hash, fileID))
	return busy
}
