package bt

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// WarmSpan is a run of torrent bytes [Off, Off+Len) held at FileOff in Path.
type WarmSpan struct {
	Off, Len, FileOff int64
	Path              string
	Key               any
}

var (
	// ErrFriendsOnlyHeld: the client holds a friends-only torrent for this hash; drop it and retry.
	ErrFriendsOnlyHeld = errors.New("bt: hash held by a friends-only torrent")
	errHeld            = errors.New("bt: hash already held")
	errReadOnly        = errors.New("bt: friends-only storage is read-only")
	errNotWarm         = errors.New("bt: piece not on SSD")
	errBadPiece        = errors.New("bt: warm piece failed its hash")
)

// InfoFiles decodes an info dictionary: its files in engine order, piece length and piece count.
func InfoFiles(infoBytes []byte) ([]File, int64, int, error) {
	info, err := metainfo.MetaInfo{InfoBytes: infoBytes}.UnmarshalInfo()
	if err != nil {
		return nil, 0, 0, err
	}
	var out []File
	var off int64
	for _, fi := range info.UpvertedFiles() {
		out = append(out, File{Path: strings.Join(append([]string{info.BestName()}, fi.BestPath()...), "/"), Length: fi.Length, Offset: off})
		off += fi.Length
	}
	return out, info.PieceLength, info.NumPieces(), nil
}

// AddFriendsOnly loads ih as a friends-only torrent over spans; it fails if the client already
// holds ih. onBad reports a span whose piece failed its hash, once, off the client lock; verdicts,
// if set, skips pieces already verified in the same files.
func (c *Client) AddFriendsOnly(ih string, infoBytes []byte, spans []WarmSpan, onBad func(key any), verdicts *VerdictCache) (*Torrent, error) {
	var h metainfo.Hash
	if err := h.FromHexString(ih); err != nil {
		return nil, err
	}
	if metainfo.HashBytes(infoBytes) != h {
		return nil, fmt.Errorf("bt: info bytes do not hash to %s", ih)
	}
	t, isNew := c.cl.AddTorrentOpt(torrent.AddTorrentOpts{InfoHash: h, InfoBytes: infoBytes, Storage: newWarmStore(spans, onBad).withVerdicts(verdicts), FriendsOnly: true})
	if !isNew {
		return nil, errHeld
	}
	if t.Info() == nil {
		_ = c.drop(t)
		return nil, errors.New("bt: friends-only torrent without info")
	}
	return &Torrent{t: t, c: c}, nil
}

// FriendsOnly reports whether t serves proven friends only.
func (t *Torrent) FriendsOnly() bool { return t.t.FriendsOnly() }

// warmStore serves pieces wholly covered by spans, read-only, each SHA1-verified at its first read.
type warmStore struct {
	spans    []WarmSpan // by Off
	onBad    func(any)
	verdicts *VerdictCache
	stamps   []fileStamp // per span, taken off the client lock

	verifications atomic.Int32 // tests
}

func newWarmStore(spans []WarmSpan, onBad func(any)) *warmStore {
	s := append([]WarmSpan(nil), spans...)
	sort.Slice(s, func(i, j int) bool { return s[i].Off < s[j].Off })
	return &warmStore{spans: s, onBad: onBad}
}

const (
	pieceUnverified int32 = iota
	pieceGood
	pieceBad
)

type warmTorrent struct {
	ws     *warmStore
	info   *metainfo.Info
	pieces []warmPiece

	filesMu sync.Mutex
	files   map[string]*os.File
	closed  bool

	reported sync.Map // span key -> struct{}
}

type warmPiece struct {
	declared bool
	state    atomic.Int32
	mu       sync.Mutex // one verification at a time
}

// withVerdicts stamps the span files (size, mtime) for c; nil keeps no verdicts.
func (s *warmStore) withVerdicts(c *VerdictCache) *warmStore {
	if c == nil {
		return s
	}
	s.verdicts, s.stamps = c, make([]fileStamp, len(s.spans))
	for i, sp := range s.spans {
		s.stamps[i] = stampOf(sp.Path)
	}
	return s
}

func (s *warmStore) OpenTorrent(info *metainfo.Info, _ metainfo.Hash) (storage.TorrentImpl, error) {
	wt := &warmTorrent{ws: s, info: info, pieces: make([]warmPiece, info.NumPieces()), files: map[string]*os.File{}}
	for i := range wt.pieces {
		p := info.Piece(i)
		wt.pieces[i].declared = s.covers(p.Offset(), p.Offset()+p.Length())
		if wt.pieces[i].declared && s.verdicts.good(s.touching(p), i) {
			wt.pieces[i].state.Store(pieceGood)
		}
	}
	return storage.TorrentImpl{
		Piece: func(p metainfo.Piece) storage.PieceImpl { return warmPieceImpl{wt, p} },
		Close: wt.close,
	}, nil
}

// covers reports whether the union of the spans holds all of [from, to).
func (s *warmStore) covers(from, to int64) bool {
	for _, sp := range s.spans {
		if sp.Off > from {
			return false
		}
		if end := sp.Off + sp.Len; end > from {
			from = end
		}
		if from >= to {
			return true
		}
	}
	return false
}

func (wt *warmTorrent) close() error {
	wt.filesMu.Lock()
	defer wt.filesMu.Unlock()
	wt.closed = true
	for _, f := range wt.files {
		f.Close()
	}
	wt.files = nil
	return nil
}

func (wt *warmTorrent) file(path string) (*os.File, error) {
	wt.filesMu.Lock()
	defer wt.filesMu.Unlock()
	if wt.closed {
		return nil, os.ErrClosed
	}
	if f := wt.files[path]; f != nil {
		return f, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	wt.files[path] = f
	return f, nil
}

// readAt reads torrent bytes [off, off+len(b)) from the spans; keys lists the spans touched.
func (wt *warmTorrent) readAt(b []byte, off int64) (keys []any, err error) {
	for _, sp := range wt.ws.spans {
		if len(b) == 0 {
			break
		}
		end := sp.Off + sp.Len
		if end <= off || sp.Off > off {
			continue
		}
		n := min(int64(len(b)), end-off)
		f, err := wt.file(sp.Path)
		if err != nil {
			return keys, err
		}
		if _, err := f.ReadAt(b[:n], sp.FileOff+off-sp.Off); err != nil {
			return keys, err
		}
		keys = append(keys, sp.Key)
		b, off = b[n:], off+n
	}
	if len(b) != 0 {
		return keys, errNotWarm
	}
	return keys, nil
}

var verifyBufs = sync.Pool{New: func() any { b := make([]byte, 256<<10); return &b }}

// spanReaderAt reads torrent offsets from the spans and records the span keys touched.
type spanReaderAt struct {
	wt   *warmTorrent
	keys []any
}

func (r *spanReaderAt) ReadAt(b []byte, off int64) (int, error) {
	keys, err := r.wt.readAt(b, off)
	r.keys = append(r.keys, keys...)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

// verifyInto hashes piece i once, streaming it, and copies the piece bytes at [off, off+len(b))
// into b on the way; a failure reports each span it touched, once per span. done=false means the
// piece was already verified good and b was not filled.
func (wt *warmTorrent) verifyInto(i int, b []byte, off int64) (done bool, err error) {
	wp := &wt.pieces[i]
	if s := wp.state.Load(); s != pieceUnverified {
		if s == pieceBad {
			return false, errBadPiece
		}
		return false, nil
	}
	wp.mu.Lock()
	defer wp.mu.Unlock()
	switch wp.state.Load() {
	case pieceGood:
		return false, nil
	case pieceBad:
		return false, errBadPiece
	}
	wt.ws.verifications.Add(1)
	p := wt.info.Piece(i)
	src := &spanReaderAt{wt: wt}
	sec := io.NewSectionReader(src, p.Offset(), p.Length())
	bp := verifyBufs.Get().(*[]byte)
	defer verifyBufs.Put(bp)
	buf := *bp
	h := sha1.New()
	for pos := int64(0); pos < p.Length(); {
		n, err := sec.Read(buf[:min(int64(len(buf)), p.Length()-pos)])
		if n > 0 {
			h.Write(buf[:n])
			if lo, hi := max(pos, off), min(pos+int64(n), off+int64(len(b))); lo < hi {
				copy(b[lo-off:hi-off], buf[lo-pos:hi-pos])
			}
			pos += int64(n)
		}
		if err != nil && pos < p.Length() {
			return false, err // I/O, not a verdict: try again next time
		}
	}
	if want := p.Hash(); bytes.Equal(h.Sum(nil), want[:]) {
		wp.state.Store(pieceGood)
		wt.ws.verdicts.record(wt.ws.touching(p), i)
		return true, nil
	}
	wp.state.Store(pieceBad)
	if f := wt.ws.onBad; f != nil {
		for _, k := range src.keys {
			if _, dup := wt.reported.LoadOrStore(k, struct{}{}); !dup {
				go f(k)
			}
		}
	}
	return false, errBadPiece
}

type warmPieceImpl struct {
	wt *warmTorrent
	p  metainfo.Piece
}

func (x warmPieceImpl) ReadAt(b []byte, off int64) (int, error) {
	i := x.p.Index()
	if !x.wt.pieces[i].declared {
		return 0, errNotWarm
	}
	n := min(int64(len(b)), x.p.Length()-off)
	if n <= 0 {
		return 0, nil
	}
	done, err := x.wt.verifyInto(i, b[:n], off)
	if err != nil {
		return 0, err
	}
	if !done {
		if _, err := x.wt.readAt(b[:n], x.p.Offset()+off); err != nil {
			return 0, err
		}
	}
	return int(n), nil
}

func (warmPieceImpl) WriteAt([]byte, int64) (int, error) { return 0, errReadOnly }
func (warmPieceImpl) MarkComplete() error                { return errReadOnly }

func (x warmPieceImpl) MarkNotComplete() error {
	x.wt.pieces[x.p.Index()].state.Store(pieceBad)
	return nil
}

func (x warmPieceImpl) Completion() storage.Completion {
	wp := &x.wt.pieces[x.p.Index()]
	return storage.Completion{Complete: wp.declared && wp.state.Load() != pieceBad, Ok: true}
}

// WholePieces counts the pieces of a torrent of length bytes that spans hold whole.
func WholePieces(spans []WarmSpan, pieceLen, length int64) int {
	if pieceLen <= 0 {
		return 0
	}
	s := newWarmStore(spans, nil)
	n := 0
	for off := int64(0); off < length; off += pieceLen {
		if s.covers(off, min(off+pieceLen, length)) {
			n++
		}
	}
	return n
}

// DropFriendsOnly drops the friends-only torrent holding ih, if any.
func (c *Client) DropFriendsOnly(ih string) {
	var h metainfo.Hash
	if h.FromHexString(ih) != nil {
		return
	}
	if t, ok := c.cl.Torrent(h); ok && t.FriendsOnly() {
		_ = c.drop(t)
	}
}

// Interested reports whether a connected peer is interested in t's pieces.
func (t *Torrent) Interested() bool {
	for _, p := range t.t.PeerSnapshots() {
		if p.Interested {
			return true
		}
	}
	return false
}

// fileStamp identifies a warm file's content by inode, size and mtime; zero when it cannot be read.
// The inode tells a file deleted and written again within one mtime tick from the old one.
type fileStamp struct {
	ino         uint64
	size, mtime int64
}

func stampOf(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	st := fileStamp{size: fi.Size(), mtime: fi.ModTime().UnixNano()}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		st.ino = uint64(sys.Ino)
	}
	return st
}

type stampedPath struct {
	path  string
	stamp fileStamp
}

// touching lists the stamped span files holding part of p; nil without verdicts.
func (s *warmStore) touching(p metainfo.Piece) []stampedPath {
	if s.verdicts == nil {
		return nil
	}
	var out []stampedPath
	from, to := p.Offset(), p.Offset()+p.Length()
	for i, sp := range s.spans {
		if sp.Off < to && sp.Off+sp.Len > from {
			out = append(out, stampedPath{sp.Path, s.stamps[i]})
		}
	}
	return out
}

// VerdictCache remembers the pieces verified good in each warm file while its size and mtime hold,
// so a served torrent opened again does not hash them again.
type VerdictCache struct {
	mu    sync.Mutex
	files map[string]*fileVerdicts
}

type fileVerdicts struct {
	stamp fileStamp
	good  map[int]struct{}
}

func (c *VerdictCache) good(files []stampedPath, piece int) bool {
	if c == nil || len(files) == 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range files {
		v := c.files[f.path]
		if f.stamp == (fileStamp{}) || v == nil || v.stamp != f.stamp {
			return false
		}
		if _, ok := v.good[piece]; !ok {
			return false
		}
	}
	return true
}

func (c *VerdictCache) record(files []stampedPath, piece int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.files == nil {
		c.files = map[string]*fileVerdicts{}
	}
	newFile := false
	for _, f := range files {
		if f.stamp == (fileStamp{}) {
			continue
		}
		v := c.files[f.path]
		if v == nil || v.stamp != f.stamp {
			newFile = newFile || v == nil
			v = &fileVerdicts{stamp: f.stamp, good: map[int]struct{}{}}
			c.files[f.path] = v
		}
		v.good[piece] = struct{}{}
	}
	c.mu.Unlock()
	if newFile {
		c.prune()
	}
}

// prune forgets files that changed or are gone; run when a new file is first recorded. The files
// are stat'ed off the lock.
func (c *VerdictCache) prune() {
	c.mu.Lock()
	snap := make(map[string]fileStamp, len(c.files))
	for path, v := range c.files {
		snap[path] = v.stamp
	}
	c.mu.Unlock()
	var stale []string
	for path, st := range snap {
		if stampOf(path) != st {
			stale = append(stale, path)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, path := range stale {
		if v := c.files[path]; v != nil && v.stamp == snap[path] { // not re-recorded meanwhile
			delete(c.files, path)
		}
	}
}
