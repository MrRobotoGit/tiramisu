package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tiramisu/internal/engine"
	"tiramisu/internal/streamer"
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

// NativeClient is the in-process bridge to the engine.
type NativeClient struct {
	eng           *engine.Engine
	src           Source
	wakeSemaphore chan struct{} // V239: limit concurrent Wake calls (max 25), fail-fast

	fillMu sync.Mutex
	fills  map[fillKey]map[*streamer.Stream]struct{} // FetchAhead windows still filling
	waits  map[fillKey]map[*fillWait]struct{}        // followers blocked on a window
}

// NewNativeClient bridges e; streams are scheduled by st.
func NewNativeClient(e *engine.Engine, st *streamer.Streamer) *NativeClient {
	c := &NativeClient{eng: e, wakeSemaphore: make(chan struct{}, 25)}
	if e != nil && st != nil {
		c.src = EngineSource(e, st)
	}
	return c
}

// NewWithSource bridges a Source without an engine (simulators): Wake and the housekeeping
// calls report no engine.
func NewWithSource(src Source) *NativeClient {
	return &NativeClient{src: src, wakeSemaphore: make(chan struct{}, 25)}
}

var errNoEngine = errors.New("bridge: no engine")

// ReachabilityOutcome reports whether a release answered. Injected by main so this
// package stays unaware of the state DB; nil when no DB is wired.
//
// Wake acquits when the metainfo had to be waited for and arrived (peers are there right
// now); it never condemns. The session layer acquits on bytes served from the swarm and
// condemns a whole playback session that ends with a failed read and nothing served.
var ReachabilityOutcome func(hash string, resolved bool)

// ActivePeers counts the live connections of a hash, 0 when the torrent is not hydrated.
// Never TotalPeers: it counts peers re-injected from the DB (dead-swarm detector).
func (c *NativeClient) ActivePeers(hash string) int {
	if c.eng == nil {
		return 0
	}
	t := c.eng.Get(hash)
	if t == nil {
		return 0
	}
	return t.ActivePeers()
}

// UnchokeSettle is how long until the torrent's choking peers reach their usual unchoke wait.
func (c *NativeClient) UnchokeSettle(hash string) time.Duration {
	if c.eng == nil {
		return 0
	}
	if t := c.eng.Get(hash); t != nil {
		return t.UnchokeSettle()
	}
	return 0
}

// metadataWait bounds the wait for the metainfo in Wake (T11).
var metadataWait = 45 * time.Second

// Wake activates a torrent (DB-only -> resident) and waits for its metainfo. Admission is
// fail-fast at 25 (a scan storm must not exhaust FUSE threads); ctx bounds the call.
func (c *NativeClient) Wake(ctx context.Context, magnetUrl string, fileIdx int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case c.wakeSemaphore <- struct{}{}:
		defer func() { <-c.wakeSemaphore }()
	default:
		return fmt.Errorf("wake semaphore exhausted (system busy)")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.eng == nil {
		return errNoEngine
	}
	// Add reuses a resident torrent and pushes its expiry forward, so the expiry ticker cannot
	// close it between this wake and the first read (9a97585).
	t, err := c.eng.Add(magnetUrl, engine.Meta{})
	if err != nil {
		return fmt.Errorf("add torrent error: %w", err)
	}
	hash := t.Hash()
	hadInfo := t.Store() != nil
	wctx, cancel := context.WithTimeout(ctx, metadataWait)
	defer cancel()
	if _, err := c.eng.WaitFiles(wctx, hash); err != nil {
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			// Not reported: the Open's session condemns once when it closes.
			log.Printf("[NativeBridge] Metadata timeout for %s", hash)
			return fmt.Errorf("torrent metadata timeout (%v): %s", metadataWait, hash)
		default:
			return fmt.Errorf("torrent closed while waiting for metadata: %s", hash)
		}
	}
	if !hadInfo && ReachabilityOutcome != nil {
		ReachabilityOutcome(hash, true) // the metainfo came from the swarm during this wait
	}
	log.Printf("[NativeBridge] Metadata ready for %s (piece=%dKB)", hash, t.Store().PieceLen()/1024)
	return nil
}

// CleanupHashes is kept for the cleanup manager: the engine answers "resident?" itself, so
// there is no local set to prune any more.
func (c *NativeClient) CleanupHashes() int { return 0 }

// NewStreamReader creates the pump's reader for a torrent file.
func (c *NativeClient) NewStreamReader(hash string, fileID int, totalSize int64) *NativeReader {
	return &NativeReader{c: c, hash: hash, fileID: fileID, lastActivity: time.Now()}
}

// NativeReader is the pump's reader: one open-ended stream, reconnected on seek or failure.
type NativeReader struct {
	c                *NativeClient
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
	timedOut         atomic.Bool // the per-read bound closed the pipe (set by the reading goroutine)
	closing          atomic.Bool // Close closed the pipe: no reconnect
	pipeReaderAtomic atomic.Pointer[io.PipeReader]
	pieceLen         atomic.Int64
	stream           atomic.Pointer[streamer.Stream] // the open stream (nil under StreamSource)
	bitrate          atomic.Int64                    // film bytes/s of content, carried to every stream
}

func (r *NativeReader) SetPieceLen(n int64) { r.pieceLen.Store(n) }

// SetPlayerLead tells the open stream how far the pump runs ahead of the player, so its stall
// risk counts the bytes already pumped.
func (r *NativeReader) SetPlayerLead(n int64) {
	if r == nil {
		return
	}
	if s := r.stream.Load(); s != nil {
		s.SetLead(n)
	}
}

// SetBitrate gives the stream the film's bytes per second of content (size / duration), which
// its stall risk uses as consumption; kept for the streams a reconnect or a seek opens later.
func (r *NativeReader) SetBitrate(n int64) {
	if r == nil || n <= 0 {
		return
	}
	r.bitrate.Store(n)
	if s := r.stream.Load(); s != nil {
		s.SetBitrate(n)
	}
}

// ErrInterrupted is returned by ReadAt when the pipe was closed by Interrupt().
var ErrInterrupted = fmt.Errorf("interrupted by seek")

// ErrReadTimeout ends a pump ReadAt that ran past pumpReadBound.
var ErrReadTimeout = errors.New("bridge: read bound exceeded")

// pumpReadBound caps one pump ReadAt end to end: twice the STRICT idle timeout at the default
// 15s, so the idle error stays the normal exit, and well under the smbd D-state watchdog
// (180s: 3 polls of 60s) that interrupts every pump (T7, T14).
var pumpReadBound = 60 * time.Second

// Short reads reported as success: a stream ending early (EOF of the range) maps to (n, nil).
var shortReadStream atomic.Int64 // ReadAt: sequential, smart-seek and hard-seek paths
var shortReadFetch atomic.Int64  // FetchAhead, FetchBlockBlocking and PrefetchBlock

// ShortReadCounts returns the number of short reads reported as success, by path.
func ShortReadCounts() (stream, fetch int64) {
	return shortReadStream.Load(), shortReadFetch.Load()
}

// Outcome of ReadAt's fill loop, per call that needed it.
var shortReadRepaired atomic.Int64
var shortReadUnfilled atomic.Int64

// ShortReadRepairCounts returns the fill-loop outcomes.
func ShortReadRepairCounts() (repaired, unfilled int64) {
	return shortReadRepaired.Load(), shortReadUnfilled.Load()
}

// readAtFillAttempts bounds the reconnects behind one ReadAt.
const readAtFillAttempts = 4

// ReadAt fills p completely or reports why not: a partial chunk in the pump's cache turns
// every later read of its missing tail into a blocking fetch.
func (r *NativeReader) ReadAt(p []byte, off int64) (int, error) {
	deadline := time.Now().Add(pumpReadBound)
	total, resets := 0, 0
	var err error
	for total < len(p) {
		var n int
		n, err = r.readAtOnce(p[total:], off+int64(total), deadline)
		total += n
		// EOF with data is success: the pump retries any error at the same offset.
		if err == io.EOF && total > 0 {
			err = nil
			break
		}
		if err != nil || total >= len(p) {
			break
		}
		if resets >= readAtFillAttempts {
			break
		}
		if n == 0 && resets > 0 {
			break // a freshly started stream produced nothing: genuine end of file
		}
		resets++
		r.resetStream()
	}
	// A seek aborts the fill deliberately; counting it would poison the metric.
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

// readFull reads p from the current pipe within the end-to-end bound.
func (r *NativeReader) readFull(p []byte, deadline time.Time) (int, error) {
	return r.bounded(deadline, func(pr *io.PipeReader) (int, error) { return io.ReadFull(pr, p) })
}

// bounded runs read on the current pipe, closing it if the end-to-end bound passes first.
func (r *NativeReader) bounded(deadline time.Time, read func(*io.PipeReader) (int, error)) (int, error) {
	pr := r.pipeReader
	d := time.Until(deadline)
	if d <= 0 {
		r.timedOut.Store(true)
		return 0, ErrReadTimeout
	}
	// The token makes a timer firing as read returns a no-op for the next read on this pipe.
	var mu sync.Mutex
	finished, fired := false, false
	stop := readTimer(d, func() {
		mu.Lock()
		defer mu.Unlock()
		if !finished {
			fired = true
			pr.Close()
		}
	})
	n, err := read(pr)
	stop()
	mu.Lock()
	finished = true
	timedOut := fired
	mu.Unlock()
	if timedOut {
		r.timedOut.Store(true)
		return n, ErrReadTimeout
	}
	return n, err
}

// readTimer runs f after d unless stopped; a var so a test can fire it at a chosen moment.
var readTimer = func(d time.Duration, f func()) (stop func() bool) { return time.AfterFunc(d, f).Stop }

func (r *NativeReader) readAtOnce(p []byte, off int64, deadline time.Time) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return 0, io.ErrClosedPipe
	}
	r.lastActivity = time.Now()

	// V286: check for interrupt BEFORE any operation
	if r.interrupted.Swap(false) {
		r.closeStream()
		return 0, ErrInterrupted
	}

	// 1. Sequential match
	if r.pipeReader != nil && off == r.offset {
		n, err = r.readFull(p, deadline)
		r.offset += int64(n)
		// V286-fix: an interrupt wins over EOF, else a closed pipe would look like the end of
		// the stream and corrupt the offset.
		if n, err := r.stopped(n); err != nil {
			return n, err
		}
		if err == nil || err == io.EOF || err == io.ErrUnexpectedEOF {
			if err != nil && n < len(p) {
				shortReadStream.Add(1)
			}
			return n, nil
		}
		log.Printf("[NativeReader] Sequential Read Error: %v - Attempting Transparent Reconnect at offset %d", err, off)
	}

	// 2. Smart seek: a small forward gap is cheaper to discard than to reconnect.
	if r.pipeReader != nil && off > r.offset && off-r.offset < 2*1024*1024 {
		skip := off - r.offset
		_, errSkip := r.bounded(deadline, func(pr *io.PipeReader) (int, error) {
			m, err := io.CopyN(io.Discard, pr, skip)
			return int(m), err
		})
		if errSkip == nil {
			r.offset = off
			n, err = r.readFull(p, deadline)
			r.offset += int64(n)
			if n, err := r.stopped(n); err != nil {
				return n, err
			}
			if err == nil || err == io.EOF || err == io.ErrUnexpectedEOF {
				if err != nil && n < len(p) {
					shortReadStream.Add(1)
				}
				return n, nil
			}
			log.Printf("[NativeReader] Smart Seek Read Error: %v - Attempting Transparent Reconnect at offset %d", err, off)
		}
		if n, err := r.stopped(0); err != nil {
			return n, err
		}
	}

	// V286: final check before the hard seek
	if n, err := r.stopped(0); err != nil {
		return n, err
	}

	// 3. Hard seek (errors, large seeks, first read)
	if r.pipeReader != nil {
		r.closeStream()
	}
	if err := startStreamFn(r, off); err != nil {
		return 0, err
	}
	n, err = r.readFull(p, deadline)
	r.offset += int64(n)
	if n, err := r.stopped(n); err != nil {
		return n, err
	}
	if err == io.ErrUnexpectedEOF {
		if n < len(p) {
			shortReadStream.Add(1)
		}
		return n, nil
	}
	return n, err
}

// stopped closes the stream and reports why the last pipe read must not be continued: an
// interrupt (its bytes are dropped), a Close, or the end-to-end bound (its bytes are valid).
func (r *NativeReader) stopped(n int) (int, error) {
	var err error
	switch {
	case r.interrupted.Swap(false):
		n, err = 0, ErrInterrupted
	case r.closing.Load():
		n, err = 0, io.ErrClosedPipe
	case r.timedOut.Swap(false):
		err = ErrReadTimeout
	default:
		return n, nil
	}
	r.closeStream()
	return n, err
}

// Interrupt unblocks a blocked ReadAt by closing the pipe reader; ReadAt then returns
// ErrInterrupted exactly once.
func (r *NativeReader) Interrupt() {
	if r == nil {
		return // the handle was released; the pump keeps its own captured copy
	}
	r.interrupted.Store(true)
	if pr := r.pipeReaderAtomic.Load(); pr != nil {
		pr.Close()
	}
}

// startStreamFn opens the stream; a var so the reconnect path can be exercised without a
// live torrent.
var startStreamFn = (*NativeReader).startStream

// StreamSource, when set, replaces the streamer as the byte source of every stream (pump
// reader, FetchAhead, FetchBlockBlocking, PrefetchBlock): it writes [offset, offset+length) of the file into w,
// length -1 meaning to the end. The FUSE-layer simulator uses it. nil in production.
var StreamSource func(ctx context.Context, hash string, fileID int, offset, length int64, w io.Writer) error

func (r *NativeReader) startStream(off int64) error {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := r.c.startSource(ctx, r.hash, r.fileID, off, -1, streamer.KindStream, pw)
	if err != nil {
		cancel()
		pr.Close()
		return err
	}
	if b := r.bitrate.Load(); b > 0 && st != nil {
		st.SetBitrate(b)
	}
	r.stream.Store(st)
	r.pipeReader, r.pipeWriter, r.offset, r.cancelFunc = pr, pw, off, cancel
	r.pipeReaderAtomic.Store(pr)
	return nil
}

func (r *NativeReader) closeStream() {
	r.stream.Store(nil)
	// V460: cancel first, so the stream stops and releases its priorities promptly.
	if r.cancelFunc != nil {
		r.cancelFunc()
		r.cancelFunc = nil
	}
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
	// Unblock a ReadAt blocked in the pipe before taking the lock it holds.
	r.closing.Store(true)
	if pr := r.pipeReaderAtomic.Load(); pr != nil {
		pr.Close()
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

// fetchBlockTimeout bounds a demand read end to end (V283, T4), under the smbd watchdog.
var fetchBlockTimeout = 8 * time.Second

// FetchTimeout bounds one fetch of the read path (a FetchAhead fill included).
func FetchTimeout() time.Duration { return fetchBlockTimeout }

// streamRangeFn opens [offset, offset+length) into pw and closes pw when it ends; a var so
// FetchAhead can be exercised without a torrent.
var streamRangeFn = (*NativeClient).streamRange

func (c *NativeClient) streamRange(ctx context.Context, hash string, fileID int, offset, length int64, pw *io.PipeWriter) (*streamer.Stream, error) {
	return c.startSource(ctx, hash, fileID, offset, length, streamer.KindDemand, pw)
}

// The background fill reports progress in steps from fetchFillFirstStep doubling to
// fetchFillStep: the reads right after a miss need small first steps (a79ca91).
var (
	fetchFillFirstStep = 256 << 10
	fetchFillStep      = 4 << 20
)

// FetchAhead fills buf with [offset, offset+len(buf)) but returns as soon as the first
// len(dest) bytes are copied into dest (d046a84). buf belongs to FetchAhead until onFill
// reports done; onFill(n, false, nil) fires as the window grows, and exactly once with
// done=true, also when the head fails or a callback panics.
func (c *NativeClient) FetchAhead(hash string, fileID int, offset int64, buf, dest []byte, onFill func(n int, done bool, err error)) (int, error) {
	head := min(len(dest), len(buf))
	if head <= 0 {
		onFill(0, true, nil)
		return 0, fmt.Errorf("empty read")
	}

	pr, pw := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), fetchBlockTimeout)

	s, err := streamRangeFn(c, ctx, hash, fileID, offset, int64(len(buf)), pw)
	if err != nil {
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

	unregister := c.addFill(hash, fileID, s)
	go func() {
		finished := false
		finish := func(n int, err error) {
			finished = true
			onFill(n, true, err)
		}
		defer func() {
			unregister()
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
		if s != nil {
			s.Demote() // the caller has its bytes: the window is a prefetch until a follower blocks
		}
		ready <- headResult{n, err}

		if err != nil || n < head {
			pr.Close()
			finish(n, err)
			return
		}

		total := n
		step := min(fetchFillFirstStep, fetchFillStep)
		for total < len(buf) {
			end := min(total+step, len(buf))
			step = min(step*2, fetchFillStep)
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

// FetchBlockBlocking reads exactly [offset, offset+len(p)), one attempt bounded by
// fetchBlockTimeout, for a caller that blocks a FUSE read on it (pinned ahead of everything):
// only the SSD-tail miss. A range ending early is (n, nil) and counted; the timeout returns
// the bytes it got.
func (c *NativeClient) FetchBlockBlocking(hash string, fileID int, offset int64, p []byte) (int, error) {
	return c.fetchBlock(hash, fileID, offset, p, streamer.KindDemand)
}

// PrefetchBlock is FetchBlockBlocking for a read nobody waits on (WarmLanding, predictive
// prefetch, TailFetch): its pieces are raised at Readahead and never outrank a blocked read.
func (c *NativeClient) PrefetchBlock(hash string, fileID int, offset int64, p []byte) (int, error) {
	return c.fetchBlock(hash, fileID, offset, p, streamer.KindPrefetch)
}

func (c *NativeClient) fetchBlock(hash string, fileID int, offset int64, p []byte, kind streamer.Kind) (int, error) {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), fetchBlockTimeout)
	defer cancel()
	if _, err := c.startSource(ctx, hash, fileID, offset, int64(len(p)), kind, pw); err != nil {
		pr.Close()
		return 0, err
	}
	// V283-fix: a stalled stream that never closes the pipe must not hang the read.
	go func() {
		<-ctx.Done()
		pr.Close()
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

// ListTorrents lists every stored torrent plus the hydrated ones not stored yet, with live
// stats for the hydrated (autoremove resolves an 8-char suffix through it).
func (c *NativeClient) ListTorrents() ([]TorrentStats, error) {
	if c.eng == nil {
		return nil, errNoEngine
	}
	refs, err := c.eng.ListRefs()
	if err != nil {
		return nil, err
	}
	out := make([]TorrentStats, 0, len(refs))
	for _, ref := range refs {
		ts := TorrentStats{Hash: ref.Hash, Title: ref.Title}
		if t := c.eng.Get(ref.Hash); t != nil {
			st := t.Status()
			ts.DownloadSpeed, ts.TotalPeers, ts.ActivePeers, ts.Downloaded = st.DownloadSpeed, st.TotalPeers, st.ActivePeers, st.LoadedSize
		}
		out = append(out, ts)
	}
	return out, nil
}

// RemoveTorrent removes the torrent and its row, then the SSD warmup files of the hash.
func (c *NativeClient) RemoveTorrent(hash string) error {
	if c.eng == nil {
		return errNoEngine
	}
	if err := c.eng.Remove(hash); err != nil {
		return err
	}
	if warmup.DiskWarmup != nil && hash != "" {
		warmup.DiskWarmup.RemoveHash(strings.ToLower(hash))
	}
	return nil
}
