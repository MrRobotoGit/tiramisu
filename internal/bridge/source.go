package bridge

import (
	"context"
	"errors"
	"io"
	"sync"

	"tiramisu/internal/engine"
	"tiramisu/internal/streamer"
)

var (
	ErrNotFound = errors.New("torrent not found")
	ErrNoInfo   = errors.New("torrent has no metadata yet")
	ErrNoFile   = errors.New("file not in torrent")
)

// Source opens ordered streams; release must be called once the stream is closed.
type Source interface {
	Open(hash string, fileID int, off, length int64, kind streamer.Kind) (s *streamer.Stream, release func(), err error)
}

// engineSource streams the hydrated torrents of an engine. A read never activates a torrent
// (V255): it looks up RAM only and fails fast, without side effects, when the torrent is not
// resident or has no metadata yet.
type engineSource struct {
	e  *engine.Engine
	st *streamer.Streamer
}

// EngineSource is the production Source.
func EngineSource(e *engine.Engine, st *streamer.Streamer) Source { return engineSource{e: e, st: st} }

func (s engineSource) Open(hash string, fileID int, off, length int64, kind streamer.Kind) (*streamer.Stream, func(), error) {
	t := s.e.Get(hash)
	if t == nil {
		return nil, nil, ErrNotFound
	}
	ts, ctl := t.Store(), t.Pieces()
	if ts == nil || ctl == nil {
		return nil, nil, ErrNoInfo
	}
	var file *engine.File
	for _, f := range t.Files() {
		if f.ID == fileID {
			file = &f
			break
		}
	}
	if file == nil {
		return nil, nil, ErrNoFile
	}
	release, ok := t.Acquire() // the torrent does not expire while the stream exists (T15)
	if !ok {
		return nil, nil, ErrNotFound
	}
	str, err := s.st.Torrent(ts, ctl).Open(streamer.File{Offset: file.Offset, Length: file.Length}, off, length, kind)
	if err != nil {
		release()
		return nil, nil, err
	}
	return str, release, nil
}

// streamBlock is the largest single pipe write, as the 1MiB bufio of the old chain.
const streamBlock = 1 << 20

var blockPool = sync.Pool{New: func() any { b := make([]byte, streamBlock); return &b }}

// copyStream writes s into w in order. Each write returns only once the reader took it, so
// the stream advances at the consumer's pace. EOF ends cleanly; any error is returned and
// never written as bytes.
func copyStream(ctx context.Context, s *streamer.Stream, w io.Writer) error {
	bp := blockPool.Get().(*[]byte)
	defer blockPool.Put(bp)
	for {
		n, err := s.Read(ctx, *bp)
		if n > 0 {
			if _, werr := w.Write((*bp)[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func kindOf(length int64) streamer.Kind {
	if length < 0 {
		return streamer.KindStream // the pump's open-ended stream
	}
	return streamer.KindDemand
}

// WriteStream writes [off, off+length) of the file into w (length -1: to the end): the source
// behind every pipe, with the signature of StreamSource so a simulator can install it.
func (c *NativeClient) WriteStream(ctx context.Context, hash string, fileID int, off, length int64, w io.Writer) error {
	return c.WriteStreamAs(ctx, hash, fileID, off, length, kindOf(length), w)
}

// WriteStreamAs is WriteStream with an explicit consumer kind (a simulator that knows which
// calls are prefetches).
func (c *NativeClient) WriteStreamAs(ctx context.Context, hash string, fileID int, off, length int64, kind streamer.Kind, w io.Writer) error {
	s, release, err := c.openAs(hash, fileID, off, length, kind)
	if err != nil {
		return err
	}
	defer release()
	defer s.Close()
	return copyStream(ctx, s, w)
}

func (c *NativeClient) openAs(hash string, fileID int, off, length int64, kind streamer.Kind) (*streamer.Stream, func(), error) {
	if c == nil || c.src == nil {
		return nil, nil, ErrNotFound
	}
	return c.src.Open(hash, fileID, off, length, kind)
}

// startSource opens the stream synchronously (a missing torrent fails at once) and copies it
// into pw in the background; pw is closed with the stream's outcome. The stream is nil under
// StreamSource.
func (c *NativeClient) startSource(ctx context.Context, hash string, fileID int, off, length int64, kind streamer.Kind, pw *io.PipeWriter) (*streamer.Stream, error) {
	if StreamSource != nil {
		go func() { pw.CloseWithError(StreamSource(ctx, hash, fileID, off, length, pw)) }()
		return nil, nil
	}
	s, release, err := c.openAs(hash, fileID, off, length, kind)
	if err != nil {
		return nil, err
	}
	go func() {
		defer release()
		defer s.Close()
		pw.CloseWithError(copyStream(ctx, s, pw))
	}()
	return s, nil
}

type fillKey struct {
	hash   string
	fileID int
}

// fillWait is a follower blocked on byte off: it holds every window of its file covering off,
// also the ones registered after it started waiting.
type fillWait struct {
	off   int64
	dones []func()
}

// addFill registers a FetchAhead window for AwaitFill; the returned func removes it.
func (c *NativeClient) addFill(hash string, fileID int, s *streamer.Stream) func() {
	if s == nil {
		return func() {}
	}
	k := fillKey{hash, fileID}
	c.fillMu.Lock()
	if c.fills == nil {
		c.fills = map[fillKey]map[*streamer.Stream]struct{}{}
	}
	if c.fills[k] == nil {
		c.fills[k] = map[*streamer.Stream]struct{}{}
	}
	c.fills[k][s] = struct{}{}
	for w := range c.waits[k] {
		w.dones = append(w.dones, s.Await(w.off))
	}
	c.fillMu.Unlock()
	return func() {
		c.fillMu.Lock()
		delete(c.fills[k], s)
		if len(c.fills[k]) == 0 {
			delete(c.fills, k)
		}
		c.fillMu.Unlock()
	}
}

// AwaitFill is called by a read blocked on byte off of the file while a FetchAhead window
// fills (the singleflight follower): the windows holding off, now or registered later, are
// demands again until done.
func (c *NativeClient) AwaitFill(hash string, fileID int, off int64) (done func()) {
	k := fillKey{hash, fileID}
	w := &fillWait{off: off}
	c.fillMu.Lock()
	if c.waits == nil {
		c.waits = map[fillKey]map[*fillWait]struct{}{}
	}
	if c.waits[k] == nil {
		c.waits[k] = map[*fillWait]struct{}{}
	}
	c.waits[k][w] = struct{}{}
	for s := range c.fills[k] {
		w.dones = append(w.dones, s.Await(off))
	}
	c.fillMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.fillMu.Lock()
			delete(c.waits[k], w)
			if len(c.waits[k]) == 0 {
				delete(c.waits, k)
			}
			dones := w.dones
			c.fillMu.Unlock()
			for _, d := range dones {
				d()
			}
		})
	}
}

// FillsInFlight counts the FetchAhead windows still filling: their onFill(done) has not
// returned yet.
func (c *NativeClient) FillsInFlight() int {
	c.fillMu.Lock()
	defer c.fillMu.Unlock()
	n := 0
	for _, m := range c.fills {
		n += len(m)
	}
	return n
}
