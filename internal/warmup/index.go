package warmup

import (
	"cmp"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SpanKind tells which warm file holds a Span.
type SpanKind int

const (
	Head SpanKind = iota + 1
	Tail
	Mid
)

// Span is a warm range of one file of a torrent on SSD, file-relative [Start, End). Path holds
// the range from its offset PathOff.
type Span struct {
	FileID     int
	Kind       SpanKind
	Start, End int64
	Path       string
	PathOff    int64
}

// warmIndex maps each warm infohash to its warm files; EachHash reads an immutable snapshot.
type warmIndex struct {
	mu    sync.Mutex
	files map[[20]byte]map[string]struct{}
	snap  atomic.Pointer[[][20]byte]
}

// warmName parses "<hash>-<id>.warmup", "<hash>-<id>.warmup-tail" and "<hash>-<id>.warmup-mid".
func warmName(name string) (ih [20]byte, id int, kind SpanKind, ok bool) {
	switch {
	case strings.HasSuffix(name, midSuffix):
		kind, name = Mid, strings.TrimSuffix(name, midSuffix)
	case strings.HasSuffix(name, tailSuffix):
		kind, name = Tail, strings.TrimSuffix(name, tailSuffix)
	case strings.HasSuffix(name, warmupSuffix):
		kind, name = Head, strings.TrimSuffix(name, warmupSuffix)
	default:
		return
	}
	h, n, found := strings.Cut(name, "-")
	if !found || len(h) != 40 {
		return
	}
	if _, err := hex.Decode(ih[:], []byte(h)); err != nil {
		return
	}
	id, err := strconv.Atoi(n)
	if err != nil || id < 0 {
		return
	}
	return ih, id, kind, true
}

func (x *warmIndex) publishLocked() {
	out := make([][20]byte, 0, len(x.files))
	for ih := range x.files {
		out = append(out, ih)
	}
	x.snap.Store(&out)
}

// ServeIndex builds the in-memory index of warm infohashes; until then EachHash yields nothing.
func (d *DiskWarmupCache) ServeIndex() {
	if d.idx.Load() != nil {
		return
	}
	d.idxMu.Lock()
	defer d.idxMu.Unlock()
	if d.idx.Load() != nil {
		return
	}
	x := &warmIndex{files: map[[20]byte]map[string]struct{}{}}
	x.mu.Lock()
	defer x.mu.Unlock()
	d.idx.Store(x) // before the scan: a change racing it lands in the map either way
	entries, _ := os.ReadDir(d.dir)
	for _, e := range entries {
		name := e.Name()
		ih, _, kind, ok := warmName(name)
		if !ok || kind == Tail && !fileExists(filepath.Join(d.dir, name+tailDoneSuffix)) {
			continue
		}
		if _, loaded := d.mids.Load(filepath.Join(d.dir, name)); kind == Mid && !loaded {
			continue
		}
		x.addLocked(ih, filepath.Join(d.dir, name))
	}
	x.publishLocked()
}

func (x *warmIndex) addLocked(ih [20]byte, path string) {
	fs := x.files[ih]
	if fs == nil {
		fs = map[string]struct{}{}
		x.files[ih] = fs
	}
	fs[path] = struct{}{}
}

// indexAdd records a warm file; nothing without ServeIndex.
func (d *DiskWarmupCache) indexAdd(path string) {
	x := d.idx.Load()
	if x == nil {
		return
	}
	ih, _, _, ok := warmName(filepath.Base(path))
	if !ok {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if _, had := x.files[ih][path]; had {
		return
	}
	x.addLocked(ih, path)
	x.publishLocked()
}

// indexRemove forgets a warm file; nothing without ServeIndex.
func (d *DiskWarmupCache) indexRemove(path string) {
	x := d.idx.Load()
	if x == nil {
		return
	}
	ih, _, _, ok := warmName(filepath.Base(path))
	if !ok {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	fs := x.files[ih]
	if _, had := fs[path]; !had {
		return
	}
	delete(fs, path)
	if len(fs) == 0 {
		delete(x.files, ih)
		x.publishLocked()
	}
}

// EachHash calls f with every warm infohash until f returns false; false if f stopped it.
func (d *DiskWarmupCache) EachHash(f func(ih []byte) bool) bool {
	x := d.idx.Load()
	if x == nil {
		return true
	}
	p := x.snap.Load()
	if p == nil {
		return true
	}
	for _, ih := range *p {
		if !f(ih[:]) {
			return false
		}
	}
	return true
}

// Spans lists hash's warm ranges in file order (heads, complete tails, mid ranges); fileLen gives a
// file's length by ID (0 = unknown). Needs ServeIndex.
func (d *DiskWarmupCache) Spans(hash string, fileLen func(fileID int) int64) []Span {
	x := d.idx.Load()
	var ih [20]byte
	if x == nil || len(hash) != 40 {
		return nil
	}
	if _, err := hex.Decode(ih[:], []byte(hash)); err != nil {
		return nil
	}
	x.mu.Lock()
	paths := make([]string, 0, len(x.files[ih]))
	for p := range x.files[ih] {
		paths = append(paths, p)
	}
	x.mu.Unlock()
	var out []Span
	for _, p := range paths {
		_, id, kind, _ := warmName(filepath.Base(p))
		fl := fileLen(id)
		switch kind {
		case Head:
			n := d.GetAvailableRange(hash, id)
			if fl > 0 {
				n = min(n, fl)
			}
			if n > 0 {
				out = append(out, Span{FileID: id, Kind: Head, End: n, Path: p})
			}
		case Tail:
			if fl > 0 && d.TailReady(hash, id) {
				tl := min(TailWarmupSize, fl)
				out = append(out, Span{FileID: id, Kind: Tail, Start: fl - tl, End: fl, Path: p})
			}
		case Mid:
			for _, r := range d.midSpans(p) {
				end := r.end
				if fl > 0 {
					end = min(end, fl)
				}
				if end > r.start {
					out = append(out, Span{FileID: id, Kind: Mid, Start: r.start, End: end, Path: p, PathOff: r.start})
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b Span) int {
		if a.FileID != b.FileID {
			return a.FileID - b.FileID
		}
		if a.Start != b.Start {
			return cmp.Compare(a.Start, b.Start)
		}
		return int(a.Kind - b.Kind)
	})
	return out
}

// Invalidate deletes one warm file that failed a piece hash.
func (d *DiskWarmupCache) Invalidate(hash string, fileID int, kind SpanKind) {
	switch kind {
	case Head:
		path := d.filePath(hash, fileID)
		d.closeHandle(path)
		if fi, err := os.Stat(path); err == nil && os.Remove(path) == nil {
			atomic.AddInt64(&d.totalSize, -fi.Size())
		}
		d.sizeCache.Delete(path)
		d.touched.Delete(path)
		d.missing.Store(path, time.Now())
		d.indexRemove(path)
	case Mid:
		d.removeMid(d.midPath(hash, fileID))
	case Tail:
		path := d.tailPath(hash, fileID)
		d.closeHandle(path)
		d.removeTail(path)
		if fi, err := os.Stat(path); err == nil && os.Remove(path) == nil {
			atomic.AddInt64(&d.totalSize, -fi.Size())
		}
		d.sizeCache.Delete(path)
		d.touched.Delete(path)
		d.indexRemove(path)
	}
	logf.Printf("[DiskWarmup] INVALIDATED %s-%d (kind %d): a piece failed its hash", hash, fileID, kind)
}
