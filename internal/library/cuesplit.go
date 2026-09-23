package library

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"tiramisu/internal/audio/cue"
	"tiramisu/internal/audio/flacsplit"
)

// ErrCueSplit marks a cue track the engine cannot cut: no sheet names the image,
// the sheet lacks the track, or the image is not a FLAC it can read.
var ErrCueSplit = errors.New("library: cannot cut the cue track")

// ErrCueUnavailable marks a cue sheet or image the torrent could not deliver in
// time: the add is refused rather than filed unsplit, and a retry can succeed.
var ErrCueUnavailable = errors.New("library: cue sheet or image not available yet")

// maxCueBytes bounds a cue sheet read: real sheets are a few KB.
const maxCueBytes = 256 << 10

// cueSplitTimeout bounds the torrent reads one add spends finding track boundaries.
const cueSplitTimeout = 3 * time.Minute

// cueTrackAllowance is the extra time one requested track may spend finding its
// boundaries: a box set needs far more probes than a three-track album.
const cueTrackAllowance = 5 * time.Second

func cueSplitBudget(tracks int) time.Duration {
	return cueSplitTimeout + time.Duration(tracks)*cueTrackAllowance
}

// cueSegment is where one cue track lives in its image, and the header it gets.
type cueSegment struct {
	Offset int64
	Length int64
	Header []byte
}

// Size is the projection's size: the header plus the image bytes.
func (s cueSegment) Size() int64 { return int64(len(s.Header)) + s.Length }

// cueCatalog is a torrent's cue sheets, read once per add: the image expansion and
// the cut match against the same parse.
type cueCatalog struct {
	sheets []*cue.Sheet
	unread bool
}

// cueSegments cuts every requested cue track, keyed by request index. All tracks of
// one image are cut from one read of its cue sheet and STREAMINFO, and a boundary a
// track shares with the next is searched once.
func (m *Manager) cueSegments(ctx context.Context, hash string, files []FileStat, reqs []AudioFileRequest, sources []ResolvedSource, cat *cueCatalog) (map[int]cueSegment, error) {
	cueTracks := 0
	for _, req := range reqs {
		if req.CueTrack != 0 {
			cueTracks++
		}
	}
	ctx, cancel := context.WithTimeout(ctx, cueSplitBudget(cueTracks))
	defer cancel()
	out := map[int]cueSegment{}
	type image struct {
		im       *flacsplit.Image
		boundary boundaryFinder
		sheet    *cue.Sheet
		file     *cue.File
	}
	images := map[int]*image{}
	for i, req := range reqs {
		if req.CueTrack == 0 {
			continue
		}
		src := sources[i]
		img := images[src.FileIndex]
		if img == nil {
			if cat == nil {
				cat = m.loadCueSheets(ctx, hash, files)
			}
			sheet, file, err := findCueSheet(cat, files, src.SourcePath)
			if err != nil {
				return nil, err
			}
			im, err := flacsplit.Open(m.sourceReader(ctx, hash, src.FileIndex, src.Size), src.Size)
			if err != nil {
				return nil, cutError(src.SourcePath, 0, err)
			}
			img = &image{im: im, boundary: memoBoundary(im), sheet: sheet, file: file}
			images[src.FileIndex] = img
		}
		seg, err := cutTrack(img.im, img.boundary, img.sheet, img.file, req)
		if err != nil {
			return nil, cutError(src.SourcePath, req.CueTrack, err)
		}
		out[i] = seg
	}
	return out, nil
}

// boundaryFinder finds the frame at or after a sample; *flacsplit.Image implements it.
type boundaryFinder interface {
	Boundary(sample int64) (int64, int64, error)
}

// boundaryFunc adapts a function to boundaryFinder.
type boundaryFunc func(sample int64) (int64, int64, error)

func (f boundaryFunc) Boundary(sample int64) (int64, int64, error) { return f(sample) }

// memoBoundary caches each sample: a track ends where the next begins, and a box set
// would otherwise search every internal boundary twice.
func memoBoundary(f boundaryFinder) boundaryFinder {
	type point struct{ off, at int64 }
	cache := map[int64]point{}
	return boundaryFunc(func(sample int64) (int64, int64, error) {
		if p, ok := cache[sample]; ok {
			return p.off, p.at, nil
		}
		off, at, err := f.Boundary(sample)
		if err != nil {
			return 0, 0, err
		}
		cache[sample] = point{off, at}
		return off, at, nil
	})
}

// cutError tells a cut the image cannot support from a read the torrent did not
// deliver in time: the first is final, the second worth retrying.
func cutError(source string, track int, err error) error {
	if errors.Is(err, flacsplit.ErrNotFLAC) || errors.Is(err, flacsplit.ErrBoundaryNotFound) ||
		errors.Is(err, flacsplit.ErrHeaderTooLarge) || errors.Is(err, errNoSuchTrack) {
		return fmt.Errorf("%w: %s track %d: %v", ErrCueSplit, source, track, err)
	}
	return fmt.Errorf("%w: %s track %d: %v", ErrCueUnavailable, source, track, err)
}

var errNoSuchTrack = errors.New("the cue sheet has no such track")

// cutTrack finds a track's frame range and builds its header. The track ends where
// the next begins, so tracks stay contiguous and nothing is lost between them.
func cutTrack(im *flacsplit.Image, boundary boundaryFinder, sheet *cue.Sheet, file *cue.File, req AudioFileRequest) (cueSegment, error) {
	k := -1
	for i, t := range file.Tracks {
		if t.Number == req.CueTrack {
			k = i
			break
		}
	}
	if k < 0 {
		return cueSegment{}, errNoSuchTrack
	}
	rate := im.Info.Rate
	startSample := file.Tracks[k].StartSample(rate)
	endSample := im.Info.TotalSamples
	if k+1 < len(file.Tracks) {
		endSample = file.Tracks[k+1].StartSample(rate)
	}
	startOff, startAt, err := boundary.Boundary(startSample)
	if err != nil {
		return cueSegment{}, err
	}
	endOff, endAt, err := boundary.Boundary(endSample)
	if err != nil {
		return cueSegment{}, err
	}
	if endOff <= startOff || endAt <= startAt {
		return cueSegment{}, errors.New("the track holds no frame")
	}
	header, err := im.Header(endAt-startAt, trackTags(sheet, file.Tracks[k], req.Tags))
	if err != nil {
		return cueSegment{}, err
	}
	return cueSegment{Offset: startOff, Length: endOff - startOff, Header: header}, nil
}

// trackTags returns the caller's tags in a stable order, or the cue sheet's own
// when the caller supplied none.
func trackTags(sheet *cue.Sheet, track cue.Track, supplied map[string]string) []flacsplit.Tag {
	if len(supplied) > 0 {
		keys := make([]string, 0, len(supplied))
		for k := range supplied {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		tags := make([]flacsplit.Tag, 0, len(keys))
		for _, k := range keys {
			tags = append(tags, flacsplit.Tag{Key: strings.ToUpper(k), Value: supplied[k]})
		}
		return tags
	}
	performer := track.Performer
	if performer == "" {
		performer = sheet.Performer
	}
	var tags []flacsplit.Tag
	for _, t := range []flacsplit.Tag{
		{Key: "TITLE", Value: track.Title},
		{Key: "ARTIST", Value: performer},
		{Key: "ALBUM", Value: sheet.Title},
		{Key: "TRACKNUMBER", Value: strconv.Itoa(track.Number)},
	} {
		if t.Value != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

// findCueSheet returns the sheet naming the image among a torrent's parsed sheets.
func findCueSheet(cat *cueCatalog, files []FileStat, imagePath string) (*cue.Sheet, *cue.File, error) {
	if sheet, file, ok := matchCueSheet(cat.sheets, files, imagePath); ok {
		return sheet, file, nil
	}
	if cat.unread {
		return nil, nil, fmt.Errorf("%w: a cue sheet of %s could not be read", ErrCueUnavailable, imagePath)
	}
	return nil, nil, fmt.Errorf("%w: no cue sheet names %s", ErrCueSplit, imagePath)
}

// loadCueSheets reads and parses every cue sheet of a torrent once.
func (m *Manager) loadCueSheets(ctx context.Context, hash string, files []FileStat) *cueCatalog {
	sheets, unread := m.cueSheets(ctx, hash, files)
	return &cueCatalog{sheets: sheets, unread: unread}
}

// cueSheets reads and parses every cue sheet of a torrent, in torrent order, with
// its directory remembered. A sheet that does not parse is skipped, so a second
// encoding of the same sheet can still serve; unread reports a sheet the torrent did
// not deliver, which is not the same answer as "no sheet".
func (m *Manager) cueSheets(ctx context.Context, hash string, files []FileStat) (sheets []*cue.Sheet, unread bool) {
	for _, f := range files {
		if !strings.EqualFold(path.Ext(f.Path), ".cue") || f.Length <= 0 || f.Length > maxCueBytes {
			continue
		}
		data := make([]byte, f.Length)
		if n, err := m.sourceReader(ctx, hash, f.ID, f.Length).ReadAt(data, 0); (err != nil && !errors.Is(err, io.EOF)) || int64(n) < f.Length {
			unread = true
			continue
		}
		sheet, err := cue.Parse(data)
		if err != nil {
			continue
		}
		sheet.Dir = path.Dir(f.Path)
		sheets = append(sheets, sheet)
	}
	return sheets, unread
}

// expandCueImages is the pipeline step every caller goes through: a request for a
// whole FLAC that a cue sheet cuts into tracks becomes one request per track, named
// "NN - Title" beside the requested path and carrying the caller's identity. Only
// folders shaped like an image (no more FLACs than cue sheets) are examined: a
// per-track rip keeps one cue beside twelve FLACs and is left alone. A sheet the
// torrent has not delivered refuses the add instead of filing the album as one long
// track; requests that already name a cue track pass as they are.
func (m *Manager) expandCueImages(ctx context.Context, hash string, files []FileStat, reqs []AudioFileRequest, sources []ResolvedSource) ([]AudioFileRequest, []ResolvedSource, *cueCatalog, error) {
	dirs := imageDirs(files)
	var cat *cueCatalog
	token, _ := HashSuffixToken(hash)
	outReqs := make([]AudioFileRequest, 0, len(reqs))
	outSrcs := make([]ResolvedSource, 0, len(sources))
	for i, req := range reqs {
		src := sources[i]
		if req.CueTrack != 0 || !strings.EqualFold(path.Ext(src.SourcePath), ".flac") || !dirs[path.Dir(src.SourcePath)] {
			outReqs, outSrcs = append(outReqs, req), append(outSrcs, src)
			continue
		}
		if cat == nil {
			cctx, cancel := context.WithTimeout(ctx, cueSplitTimeout)
			cat = m.loadCueSheets(cctx, hash, files)
			cancel()
		}
		_, file, ok := matchCueSheet(cat.sheets, files, src.SourcePath)
		if !ok {
			if cat.unread {
				return nil, nil, nil, fmt.Errorf("%w: a cue sheet of %s could not be read", ErrCueUnavailable, src.SourcePath)
			}
			outReqs, outSrcs = append(outReqs, req), append(outSrcs, src)
			continue
		}
		if len(file.Tracks) < 2 {
			outReqs, outSrcs = append(outReqs, req), append(outSrcs, src)
			continue
		}
		dir := path.Dir(req.Path)
		for _, t := range file.Tracks {
			title := t.Title
			if title == "" {
				title = fmt.Sprintf("Track %02d", t.Number)
			}
			name := fmt.Sprintf("%s_%s.flac", SafeComponent(fmt.Sprintf("%02d - %s", t.Number, title)), token)
			if dir != "." && dir != "" {
				name = dir + "/" + name
			}
			outReqs = append(outReqs, AudioFileRequest{
				SourcePath: req.SourcePath, Path: name, CueTrack: t.Number,
				ExternalID: req.ExternalID, ExternalIDNamespace: req.ExternalIDNamespace,
			})
			outSrcs = append(outSrcs, src)
		}
	}
	return outReqs, outSrcs, cat, nil
}

// imageDirs reports the directories shaped like a FLAC image rather than a track
// set: no more FLACs than cue sheets, as a single-file rip or one image per disc
// has. A per-track rip carries one cue beside twelve FLACs and is not one.
func imageDirs(files []FileStat) map[string]bool {
	type counts struct{ flac, cue int }
	byDir := map[string]*counts{}
	for _, f := range files {
		ext := path.Ext(f.Path)
		if !strings.EqualFold(ext, ".flac") && !strings.EqualFold(ext, ".cue") {
			continue
		}
		c := byDir[path.Dir(f.Path)]
		if c == nil {
			c = &counts{}
			byDir[path.Dir(f.Path)] = c
		}
		if strings.EqualFold(ext, ".flac") {
			c.flac++
		} else {
			c.cue++
		}
	}
	dirs := map[string]bool{}
	for dir, c := range byDir {
		if c.flac > 0 && c.flac <= c.cue {
			dirs[dir] = true
		}
	}
	return dirs
}

// matchCueSheet picks the sheet naming the image: one in the image's directory
// first, then any. When nothing names it, a lone image and a single-FILE sheet in the
// same directory belong together: rippers mangle names (a "¡" saved as ".") but not
// which file sits beside which.
func matchCueSheet(sheets []*cue.Sheet, files []FileStat, imagePath string) (*cue.Sheet, *cue.File, bool) {
	dir := path.Dir(imagePath)
	ordered := append([]*cue.Sheet(nil), sheets...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Dir == dir && ordered[j].Dir != dir })
	for _, sheet := range ordered {
		if file, ok := sheet.FileFor(imagePath); ok {
			return sheet, file, true
		}
	}
	images := 0
	for _, f := range files {
		if path.Dir(f.Path) == dir && strings.EqualFold(path.Ext(f.Path), ".flac") {
			images++
		}
	}
	if images != 1 {
		return nil, nil, false
	}
	var match *cue.Sheet
	for _, sheet := range sheets {
		if sheet.Dir == dir && len(sheet.Files) == 1 && len(sheet.Files[0].Tracks) > 0 {
			if match != nil {
				return nil, nil, false
			}
			match = sheet
		}
	}
	if match == nil {
		return nil, nil, false
	}
	return match, &match.Files[0], true
}

// sourceReader reads a torrent file through GoStorm's stream endpoint with ranged
// requests: pieces download on demand, so a boundary costs a few pieces, not the
// album. The size lets a short answer be told apart from the end of the file.
func (m *Manager) sourceReader(ctx context.Context, hash string, index int, size int64) io.ReaderAt {
	return &rangeReader{ctx: ctx, url: m.streamURL(hash, index), size: size}
}

type rangeReader struct {
	ctx  context.Context
	url  string
	size int64
}

// errRangeShort marks a range the torrent delivered only in part: retryable, unlike
// a boundary search that exhausted its probes.
var errRangeShort = errors.New("range read: fewer bytes than the file still holds")

var rangeClient = &http.Client{Timeout: 2 * time.Minute}

func (r *rangeReader) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off >= r.size {
		return 0, io.EOF
	}
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+int64(len(p))-1))
	resp, err := rangeClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusRequestedRangeNotSatisfiable:
		// The range starts inside the file, so the engine refusing it is a
		// temporary answer, not the end of the file.
		return 0, fmt.Errorf("range read %d+%d: status %d", off, len(p), resp.StatusCode)
	default:
		return 0, fmt.Errorf("range read %d+%d: status %d", off, len(p), resp.StatusCode)
	}
	n, err := io.ReadFull(resp.Body, p)
	switch {
	case err == nil:
		return n, nil
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		// A read that stops before the end of the file is a swarm that went away;
		// only one that reaches the end is the end.
		if off+int64(n) < r.size {
			return n, fmt.Errorf("%w: %d of %d bytes at %d of %d", errRangeShort, n, len(p), off, r.size)
		}
		return n, io.EOF
	default:
		return n, err
	}
}
