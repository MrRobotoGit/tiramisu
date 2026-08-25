package native

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"log"
	"sync"
	"sync/atomic"
	"time"
	"tiramisu/internal/gostorm/torr"
	"tiramisu/internal/gostorm/torr/state"
	apiUtils "tiramisu/internal/gostorm/web/api/utils"
	"tiramisu/internal/warmup"
)

// TorrentStats is used by NativeClient methods to report torrent status.
type TorrentStats struct {
	Hash          string  `json:"hash"`
	Title         string  `json:"title"`
	DownloadSpeed float64 `json:"download_speed"`
	TotalPeers    int     `json:"total_peers"`
	ActivePeers   int     `json:"active_peers"`
	Downloaded    int64   `json:"downloaded"`
}

// NativeClient abstracts direct calls to the internal GoStorm instance
// eliminating HTTP overhead for metadata operations.
type NativeClient struct {
	// Stateless client
	activeHashes  sync.Map      // Map[string]bool - Fast lookup for active torrents
	wakeSemaphore chan struct{} // V239: Limit concurrent Wake calls (max 10)
}

// NewNativeClient creates a new native bridge client
func NewNativeClient() *NativeClient {
	return &NativeClient{
		wakeSemaphore: make(chan struct{}, 25), // Max 25 concurrent Wake operations
	}
}

// Wake triggers the start of a torrent (Ghost -> Active) entirely in-memory
// Synchronous & Deduplicated.
func (c *NativeClient) Wake(magnetUrl string, fileIdx int) error {
	// V239-Semaphore: Guard against "Thread Exhaustion" during massive scans
	select {
	case c.wakeSemaphore <- struct{}{}:
		defer func() { <-c.wakeSemaphore }()
	default:
		// Fail-Fast: If >10 Opens are pending, we drop the request to save the filesystem.
		// Player will retry, or fail this specific file, but FUSE remains alive.
		return fmt.Errorf("wake semaphore exhausted (system busy)")
	}
	// 1. Parse Magnet/Link to get hash
	spec, err := apiUtils.ParseLink(magnetUrl)
	if err != nil {
		return fmt.Errorf("parse link error: %w", err)
	}
	hash := spec.InfoHash.HexString()

	// 2. Dedup: Check if already active (optimization)
	var t *torr.Torrent
	if _, ok := c.activeHashes.Load(hash); ok {
		// V255: Use PeekTorrent to check RAM only, not re-activate from DB.
		if existing := torr.PeekTorrent(hash); existing != nil && existing.Torrent != nil {
			t = existing
			// V265: If we have an existing torrent, we fall through to the metadata check
			// instead of returning nil, to ensure Open waits if metadata isn't ready.
		} else {
			// If not in core but in our map, remove it and proceed to add
			c.activeHashes.Delete(hash)
		}
	}

	// 3. Synchronous Wakeup
	if t == nil {
		// Add/Start Torrent via Internal API
		var err error
		t, err = torr.AddTorrent(spec, "", "", "", "")
		if err != nil {
			return fmt.Errorf("add torrent error: %w", err)
		}
	}

	// Wait for metadata
	if t != nil {
		if t.Torrent != nil && t.Torrent.Info() == nil {
			// Metadata NOT ready yet - wait with 45s timeout (Resilience)
			timer := time.NewTimer(45 * time.Second)
			defer timer.Stop()

			select {
			case <-t.Torrent.GotInfo():
				// Metadata ready — fall through to log below
			case <-timer.C:
				log.Printf("[NativeBridge] Metadata timeout for %s", hash)
				return fmt.Errorf("torrent metadata timeout (45s): %s", hash)
			}
		}
		pieceLenKB := 0
		if t.Torrent != nil {
			if info := t.Torrent.Info(); info != nil {
				pieceLenKB = int(info.PieceLength) / 1024
			}
		}
		log.Printf("[NativeBridge] Metadata ready for %s (piece=%dKB)", hash, pieceLenKB)
		// V255: Save metadata to DB immediately so next Wake() skips GotInfo() wait.
		// Note: ForceSaveTorrentToDB at torrent expiry captures the full peer swarm
		// safely (no streaming active). The previous 90s delayed goroutine was
		// removed (V306) because it fired during active playback, causing cl._mu
		// contention (PeerConns snapshot) that briefly stalled the pump.
		torr.SaveTorrentToDB(t)

		// Optimistic active update
		c.activeHashes.Store(hash, true)
	}

	return nil
}

// CleanupHashes removes hashes from the local map that are no longer present in the GoStorm core.
// Prevents memory leaks in long-running sessions.
func (c *NativeClient) CleanupHashes() int {
	removed := 0
	c.activeHashes.Range(func(key, value interface{}) bool {
		hash := key.(string)
		// V255: Use PeekTorrent to avoid re-activating expired torrents from DB.
		// GetTorrent() would re-activate DB-only entries, causing infinite loops.
		// Check Torrent handle (nil = DB-only or not found, non-nil = active in engine).
		t := torr.PeekTorrent(hash)
		if t == nil || t.Torrent == nil {
			c.activeHashes.Delete(hash)
			removed++
		}
		return true
	})
	return removed
}

// NewStreamReader creates a new stateful hybrid reader for a torrent file.
func (c *NativeClient) NewStreamReader(hash string, fileID int, totalSize int64) *NativeReader {
	return &NativeReader{
		hash:         hash,
		fileID:       fileID,
		lastActivity: time.Now(),
	}
}

// NativeReader implements a hybrid stateful/stateless reader for Torrent files.
type NativeReader struct {
	mu               sync.Mutex
	hash             string
	fileID           int
	offset           int64
	pipeReader       *io.PipeReader
	pipeWriter       *io.PipeWriter
	cancelFunc       context.CancelFunc
	closed           bool
	lastActivity     time.Time
	interrupted      atomic.Bool // V286: set by Interrupt(), cleared by next startStream
	pipeReaderAtomic atomic.Pointer[io.PipeReader]
	pieceLen         atomic.Int64
}

func (r *NativeReader) SetPieceLen(n int64) { r.pieceLen.Store(n) }

// ErrInterrupted is returned by ReadAt when the pipe was closed by Interrupt().
var ErrInterrupted = fmt.Errorf("interrupted by seek")

// Short reads reported as success. io.ReadFull returns ErrUnexpectedEOF when the stream ends
// early — a stalled pipe closed by FetchBlock's 8s timeout, or a hiccup mid-stream — and every
// site below maps that to (n, nil). The caller cannot tell a partial read from a complete one,
// and with FUSE on the kernel page cache (no direct_io) a short read makes the kernel zero-fill
// the rest of the page and mark it uptodate: that region then serves zeros for good.
var shortReadStream atomic.Int64 // ReadAt: sequential, smart-seek and hard-seek paths
var shortReadFetch atomic.Int64  // FetchBlock: the stateless path behind the FUSE fallback

// ShortReadCounts returns the number of short reads reported as success, by path.
func ShortReadCounts() (stream, fetch int64) {
	return shortReadStream.Load(), shortReadFetch.Load()
}

// Outcome of ReadAt's fill loop, per call that needed it: repaired = p was filled after a
// reconnect, unfilled = it stayed short and the caller sees a gap.
var shortReadRepaired atomic.Int64
var shortReadUnfilled atomic.Int64

// ShortReadRepairCounts returns the fill-loop outcomes.
func ShortReadRepairCounts() (repaired, unfilled int64) {
	return shortReadRepaired.Load(), shortReadUnfilled.Load()
}

// readAtFillAttempts bounds the reconnects behind one ReadAt. Each readAtOnce that has to
// hard-seek pays a stream restart, so this trades a bounded stall against a short chunk.
const readAtFillAttempts = 4

// ReadAt implements io.ReaderAt. It fills p completely: a partial read leaves the pump
// caching a chunk shorter than the reader asked for, and every later read spanning the
// missing tail misses the read-ahead cache and pays a blocking FetchBlock (seconds, against
// ~1ms for a cache hit). Stopping short is what turns a dead pipe into a stall downstream.
func (r *NativeReader) ReadAt(p []byte, off int64) (int, error) {
	total, resets := 0, 0
	var err error
	for total < len(p) {
		var n int
		n, err = r.readAtOnce(p[total:], off+int64(total))
		total += n
		// End of file is not a failure: the pump's caller treats any error as "retry this
		// same offset", which at the last chunk would spin forever instead of finishing.
		if err == io.EOF && total > 0 {
			err = nil
			break
		}
		if err != nil || total >= len(p) {
			break
		}
		// The stream ended before p was full. On the sequential path a dead pipe keeps
		// returning (0, nil), so it has to be dropped explicitly to force a reconnect.
		if resets >= readAtFillAttempts {
			break
		}
		if n == 0 && resets > 0 {
			break // a freshly started stream produced nothing: genuine end of file
		}
		resets++
		r.resetStream()
	}

	// A seek aborts the fill deliberately; counting it as unfilled would poison the very
	// metric used to judge whether the reconnect works.
	if resets > 0 && err != ErrInterrupted {
		if total == len(p) {
			shortReadRepaired.Add(1)
		} else {
			shortReadUnfilled.Add(1)
		}
	}
	return total, err
}

// resetStream drops the current pipe so the next readAtOnce takes the hard-seek path.
func (r *NativeReader) resetStream() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pipeReader != nil {
		r.closeStream()
	}
}

func (r *NativeReader) readAtOnce(p []byte, off int64) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return 0, io.ErrClosedPipe
	}

	r.lastActivity = time.Now()

	// V286: Check for interrupt BEFORE any operation
	if r.interrupted.Swap(false) {
		r.closeStream()
		return 0, ErrInterrupted
	}

	// 1. Sequential Match
	if r.pipeReader != nil && off == r.offset {
		n, err = io.ReadFull(r.pipeReader, p)
		r.offset += int64(n)

		// V286-fix: check interrupted BEFORE EOF — Close() closes pipeWriter without
		// lock (deadlock fix), so it can cause io.EOF/ErrUnexpectedEOF that would
		// otherwise mask an in-progress seek and silently corrupt the read offset.
		if r.interrupted.Swap(false) {
			r.closeStream()
			return 0, ErrInterrupted
		}

		if err == nil || err == io.EOF || err == io.ErrUnexpectedEOF {
			if err != nil && n < len(p) {
				shortReadStream.Add(1)
			}
			return n, nil
		}

		// V257: Resilience Fix - If pipe fails, attempt one transparent reconnect
		log.Printf("[NativeReader] Sequential Read Error: %v - Attempting Transparent Reconnect at offset %d", err, off)
	}

	// 2. Smart Seek
	if r.pipeReader != nil && off > r.offset && off-r.offset < 2*1024*1024 {
		skip := off - r.offset
		_, errSkip := io.CopyN(io.Discard, r.pipeReader, skip)
		if errSkip == nil {
			r.offset = off
			n, err = io.ReadFull(r.pipeReader, p)
			r.offset += int64(n)
			if err == nil || err == io.EOF || err == io.ErrUnexpectedEOF {
				if err != nil && n < len(p) {
					shortReadStream.Add(1)
				}
				return n, nil
			}
			log.Printf("[NativeReader] Smart Seek Read Error: %v - Attempting Transparent Reconnect at offset %d", err, off)
		}

		if r.interrupted.Swap(false) {
			r.closeStream()
			return 0, ErrInterrupted
		}
	}

	// V286: Final interrupt check before Hard Seek
	if r.interrupted.Swap(false) {
		if r.pipeReader != nil {
			r.closeStream()
		}
		return 0, ErrInterrupted
	}

	// 4. Hard Seek (Recovery Path for errors or large seeks)
	if r.pipeReader != nil {
		r.closeStream()
	}

	if err := startStreamFn(r, off); err != nil {
		return 0, err
	}

	n, err = io.ReadFull(r.pipeReader, p)
	r.offset += int64(n)
	if err == io.ErrUnexpectedEOF {
		if n < len(p) {
			shortReadStream.Add(1)
		}
		return n, nil
	}
	return n, err
}

// V286: Interrupt unblocks a blocked ReadAt by closing the pipe reader.
// Sets interrupted flag so ReadAt returns ErrInterrupted reliably.
func (r *NativeReader) Interrupt() {
	if r == nil {
		return // the handle was released; the pump keeps its own captured copy
	}
	r.interrupted.Store(true)
	if pr := r.pipeReaderAtomic.Load(); pr != nil {
		pr.Close() // Reader side close is enough to unblock ReadFull
	}
}

// startStreamFn opens the stream; a var so the reconnect path can be exercised without a
// live torrent, the same injection seam used for warmup.TailFetch.
var startStreamFn = (*NativeReader).startStream

func (r *NativeReader) startStream(off int64) error {
	// V255: Use PeekTorrent to avoid extending expiry timer on every Hard Seek.
	t := torr.PeekTorrent(r.hash)
	if t == nil || t.Torrent == nil {
		return fmt.Errorf("torrent not found")
	}

	pr, pw := io.Pipe()
	r.pipeReader = pr
	r.pipeReaderAtomic.Store(pr)
	r.pipeWriter = pw
	r.offset = off

	// V460: Use a context that we can cancel explicitly on Close()
	ctx, cancel := context.WithCancel(context.Background())
	r.cancelFunc = cancel

	// Create request with our explicit context
	req, _ := http.NewRequestWithContext(ctx, "GET", "/stream", nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", off))

	resp := &PipeResponseWriter{
		writer: pw,
		header: make(http.Header),
	}

	go func() {
		defer pw.Close()
		if err := t.Stream(r.fileID, req, resp); err != nil {
			log.Printf("[NativeReader] Stream error at off=%dMB fileID=%d hash=%s: %v",
				off/(1024*1024), r.fileID, r.hash[:8], err)
		}
	}()

	return nil
}

func (r *NativeReader) closeStream() {
	// V460: Cancel context FIRST to trigger GoStorm exit
	if r.cancelFunc != nil {
		r.cancelFunc()
		r.cancelFunc = nil
	}

	// Small delay to allow context propagation? No, BoltDB/RAM is fast.

	if r.pipeReader != nil {
		r.pipeReaderAtomic.Store(nil)
		r.pipeReader.Close()
		r.pipeReader = nil
	}
	if r.pipeWriter != nil {
		r.pipeWriter.Close()
		r.pipeWriter = nil
	}
}

func (r *NativeReader) Close() error {
	// Close pipeWriter BEFORE lock to unblock io.ReadFull in ReadAt goroutine
	if pw := r.pipeWriter; pw != nil {
		pw.Close()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.closeStream()
	return nil
}

func (r *NativeReader) IsIdle(d time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Since(r.lastActivity) > d
}

// fetchBlockTimeout bounds a stateless fetch. 3 retries x 8s = 27s max FUSE block, under the
// 60s smbd D-state watchdog.
const fetchBlockTimeout = 8 * time.Second

// streamRangeFn opens a byte stream for [offset, offset+length) into pw and closes pw when the
// stream ends. A var so FetchAhead can be exercised without a live torrent, the same injection
// seam used by startStreamFn.
var streamRangeFn = streamRange

func streamRange(ctx context.Context, hash string, fileID int, offset, length int64, pw *io.PipeWriter) error {
	t := torr.PeekTorrent(hash)
	if t == nil || t.Torrent == nil {
		return fmt.Errorf("torrent not found")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", "/stream", nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	resp := &PipeResponseWriter{writer: pw, header: make(http.Header)}
	go func() {
		defer pw.Close()
		t.Stream(fileID, req, resp)
	}()
	return nil
}

// fetchFillStep is how much the background fill gathers before reporting progress. Sized around
// a torrent piece: smaller steps report nothing earlier (data lands piece by piece) and only
// multiply the caller's re-cache work.
var fetchFillStep = 4 << 20

// FetchAhead fills buf with [offset, offset+len(buf)) but returns as soon as the first
// len(dest) bytes are copied into dest. A blocking FUSE read needs one FUSE block, not the
// whole read-ahead window: waiting for the window multiplied every cache miss by the ratio
// between them (1MB asked, 16MB awaited) and is what made a miss a multi-second stall.
//
// buf belongs to FetchAhead until onFill reports done — the background fill keeps writing into
// it — so the caller must not recycle buf before then, and may only read buf[:n] for the n it
// was last handed. onFill(n, false, nil) fires as the window grows, so the caller can widen its
// cache entry and let the reads behind this one land while the tail is still arriving; it fires
// exactly once with done=true, including when the head itself fails.
func (c *NativeClient) FetchAhead(hash string, fileID int, offset int64, buf, dest []byte, onFill func(n int, done bool, err error)) (int, error) {
	head := len(dest)
	if head > len(buf) {
		head = len(buf)
	}
	if head <= 0 {
		onFill(0, true, nil)
		return 0, fmt.Errorf("empty read")
	}

	pr, pw := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), fetchBlockTimeout)

	if err := streamRangeFn(ctx, hash, fileID, offset, int64(len(buf)), pw); err != nil {
		cancel()
		pw.Close()
		pr.Close()
		onFill(0, true, err)
		return 0, err
	}

	// Unblock the reads below if the stream stalls without closing the pipe.
	go func() {
		<-ctx.Done()
		pr.Close()
	}()

	type headResult struct {
		n   int
		err error
	}
	ready := make(chan headResult, 1)

	go func() {
		// The caller's onFill releases the buffer and unblocks the reads dedup'd behind this
		// one; a panic escaping it must not leave them waiting for a done that never comes.
		finished := false
		finish := func(n int, err error) {
			finished = true
			onFill(n, true, err)
		}
		defer func() {
			cancel()
			if r := recover(); r != nil {
				log.Printf("[NativeReader] FetchAhead fill panicked at off=%d: %v", offset, r)
				if !finished {
					onFill(0, true, fmt.Errorf("fill panic: %v", r))
				}
			}
		}()
		n, err := io.ReadFull(pr, buf[:head])
		if err == io.ErrUnexpectedEOF || (err == io.EOF && n > 0) {
			shortReadFetch.Add(1)
			err = nil
		}
		if err == nil && n > 0 {
			copy(dest, buf[:n])
		}
		ready <- headResult{n, err}

		if err != nil || n < head {
			pr.Close()
			finish(n, err)
			return
		}

		total := n
		for total < len(buf) {
			end := total + fetchFillStep
			if end > len(buf) {
				end = len(buf)
			}
			m, stepErr := io.ReadFull(pr, buf[total:end])
			total += m
			if stepErr != nil {
				pr.Close()
				if stepErr == io.ErrUnexpectedEOF || stepErr == io.EOF {
					if total < len(buf) {
						shortReadFetch.Add(1)
					}
					stepErr = nil
				}
				finish(total, stepErr)
				return
			}
			if total < len(buf) {
				onFill(total, false, nil)
			}
		}
		pr.Close()
		finish(total, nil)
	}()

	r := <-ready
	return r.n, r.err
}

// FetchBlock performs an atomic, stateless read from the Torrent Core.
func (c *NativeClient) FetchBlock(hash string, fileID int, offset int64, p []byte) (int, error) {
	// V255: Use PeekTorrent — same reasoning as startStream above.
	t := torr.PeekTorrent(hash)
	if t == nil || t.Torrent == nil {
		return 0, fmt.Errorf("torrent not found")
	}

	pr, pw := io.Pipe()
	// V283: 8s timeout (was 30s). 6 retries × 30s = 180s FUSE block → smbd D-state.
	// 3 retries × 8s = 27s max → under 60s watchdog threshold.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", "/stream", nil)
	endRange := offset + int64(len(p)) - 1
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, endRange))

	resp := &PipeResponseWriter{
		writer: pw,
		header: make(http.Header),
	}

	go func() {
		defer pw.Close()
		t.Stream(fileID, req, resp)
	}()

	// V283-Fix: Ensure io.ReadFull is unblocked if context expires (8s timeout).
	// Previously, if t.Stream() stalled without closing the pipe, FetchBlock would hang forever.
	go func() {
		<-ctx.Done()
		pr.Close() // Force unblock io.ReadFull on timeout/cancel
	}()

	n, err := io.ReadFull(pr, p)
	pr.Close()

	if err == io.ErrUnexpectedEOF {
		if n < len(p) {
			shortReadFetch.Add(1)
		}
		return n, nil
	}

	return n, err
}

// ListTorrents returns all torrents
func (c *NativeClient) ListTorrents() ([]TorrentStats, error) {
	list := torr.ListTorrent()
	result := make([]TorrentStats, 0, len(list))

	for _, t := range list {
		if t != nil {
			result = append(result, *convertStatusToStats(t.Status()))
		}
	}
	return result, nil
}

// RemoveTorrent removes a torrent from the server
func (c *NativeClient) RemoveTorrent(hash string) error {
	torr.RemTorrent(hash)
	// V272: Clean up disk warmup files for this hash
	if warmup.DiskWarmup != nil && hash != "" {
		warmup.DiskWarmup.RemoveHash(hash)
	}
	return nil
}

// PipeResponseWriter bridges GoStorm's HTTP responses to our Go pipe.
type PipeResponseWriter struct {
	writer *io.PipeWriter
	header http.Header
}

func (w *PipeResponseWriter) Header() http.Header         { return w.header }
func (w *PipeResponseWriter) Write(p []byte) (int, error) { return w.writer.Write(p) }
func (w *PipeResponseWriter) WriteHeader(statusCode int)  {}

// convertStatusToStats maps internal TorrentStatus to our local TorrentStats struct
func convertStatusToStats(st *state.TorrentStatus) *TorrentStats {
	if st == nil {
		return nil
	}

	return &TorrentStats{
		Hash:          st.Hash,
		Title:         st.Title,
		DownloadSpeed: st.DownloadSpeed,
		TotalPeers:    st.TotalPeers,
		ActivePeers:   st.ActivePeers,
		Downloaded:    st.LoadedSize,
	}
}
