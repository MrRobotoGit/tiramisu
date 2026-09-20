package main

// Tests for slice audio-vfs-authoritative-lookup.
//
// They drive the real FUSE node tree (VirtualMkvRoot -> VirtualDirNode ->
// VirtualMkvNode) through go-fuse's in-process raw bridge: fs.NewNodeFS
// initialises inodes and dispatches Lookup/GetAttr/Open exactly as the kernel
// loop would, but nothing is mounted and no request leaves the process.
// nativeBridge stays nil, so Open never reaches GoStorm.
//
// Everything here mutates package-main globals (physicalSourcePath, the audio
// namespace, the metadata cache, the inode map, config), so no test in this file
// may run in parallel. newVFSEnv restores every global it touches.

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"tiramisu/internal/cache"
	"tiramisu/internal/config"
	"tiramisu/internal/library"
	"tiramisu/internal/lockmgr"
	"tiramisu/internal/vfs"
)

// Committed identity "A", the substitute "B", a later replacement "C", and the
// video identities. All are canonical 40-hex infohashes, lowercase.
const (
	hashA  = "0123456789abcdef0123456789abcdef01234567"
	hashB  = "fedcba9876543210fedcba9876543210fedcba98"
	hashC  = "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"
	hashV1 = "1111222233334444555566667777888899990000"
	hashV2 = "aaaabbbbccccddddeeeeffff0000111122223333"

	idxA = 3
	idxB = 5
	idxC = 7

	// rowSize is deliberately not a round number and is not any stub's size
	// unless a case says so.
	rowSize int64 = 4194305
)

var (
	rowMtime  = time.Date(2021, 3, 4, 5, 6, 7, 500_000_000, time.UTC)
	stubMtime = time.Date(2019, 1, 2, 3, 4, 5, 0, time.UTC)
)

// streamURL is the URL shape real stubs carry. The host never resolves.
func streamURL(hash string, idx int) string {
	return fmt.Sprintf("http://tiramisu.invalid/stream?link=%s&index=%d&play", hash, idx)
}

func jsonStub(url string, size int64) string {
	return fmt.Sprintf(`{"url":%q,"size":%d,"magnet":"","imdb":""}`, url, size)
}

func writeFile(t testing.TB, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// vfsEnv is one isolated VFS: a temp physical source tree, an empty audio
// namespace, a fresh inode map and metadata cache, and an initialised node tree.
type vfsEnv struct {
	t      *testing.T
	root   string
	ns     *library.AudioNamespace
	inodes *vfs.InodeMap
	rfs    fuse.RawFileSystem
}

func newVFSEnv(t *testing.T) *vfsEnv {
	t.Helper()

	prevRoot, prevNS, prevInodes := physicalSourcePath, globalAudioNamespace, globalInodeMap
	prevMeta, prevLocks, prevBridge := metaCache, globalLockManager, nativeBridge
	prevDirs := globalDirCache
	prevCfg := globalConfig.Load()

	root := t.TempDir()
	e := &vfsEnv{
		t:      t,
		root:   root,
		ns:     library.NewAudioNamespace(),
		inodes: vfs.NewInodeMap(filepath.Join(t.TempDir(), "inode_map.json"), &vfsLogger{logger}),
	}
	t.Cleanup(func() {
		// A regression that blocks a call forever must not also strand its goroutine
		// on the next test's globals: release every waiter before restoring them.
		e.ns.MarkUnavailable()
		physicalSourcePath, globalAudioNamespace, globalInodeMap = prevRoot, prevNS, prevInodes
		metaCache, globalLockManager, nativeBridge = prevMeta, prevLocks, prevBridge
		globalDirCache = prevDirs
		globalConfig.Store(prevCfg)
	})

	physicalSourcePath = root
	globalAudioNamespace = e.ns
	globalInodeMap = e.inodes
	globalDirCache = vfs.NewDirCache(time.Hour) // long enough that only an explicit Delete evicts
	metaCache = cache.NewLRUCache(1<<20, time.Hour)
	globalLockManager = lockmgr.NewLockManager(time.Hour)
	nativeBridge = nil // Open must never contact GoStorm from a unit test.
	globalConfig.Store(&config.Config{FuseBlockSize: 1 << 20, LogLevel: "INFO"})

	e.remount()
	return e
}

// remount discards every kernel-side inode and starts a fresh node tree, as after
// a FUSE restart. The global caches survive it, the way they would in-process.
func (e *vfsEnv) remount() {
	e.rfs = fs.NewNodeFS(&VirtualMkvRoot{sourcePath: e.root}, nil)
}

func (e *vfsEnv) publish(rows ...library.AudioProjection) { e.ns.Publish(rows) }

func (e *vfsEnv) evictMetadataCache() {
	metaCache = cache.NewLRUCache(1<<20, time.Hour)
}

// phys returns the physical path of a section-relative path.
func (e *vfsEnv) phys(section library.Section, rel string) string {
	return filepath.Join(e.root, string(section), filepath.FromSlash(rel))
}

type lookupResult struct {
	nodeID uint64
	out    fuse.EntryOut
	status fuse.Status
}

// lookup walks rel (slash-separated, starting at the source root) one component
// at a time through the real Lookup implementations.
func (e *vfsEnv) lookup(rel string) lookupResult { return e.lookupCancellable(nil, rel) }

// lookupCancellable is lookup with the kernel's request-cancellation channel.
func (e *vfsEnv) lookupCancellable(cancel <-chan struct{}, rel string) lookupResult {
	id := uint64(1)
	var res lookupResult
	for _, comp := range strings.Split(rel, "/") {
		res = e.lookupIn(cancel, id, comp)
		if res.status != fuse.OK {
			return res
		}
		id = res.nodeID
	}
	return res
}

// lookupIn is one Lookup below an already-resolved directory node.
func (e *vfsEnv) lookupIn(cancel <-chan struct{}, parent uint64, name string) lookupResult {
	var res lookupResult
	res.status = e.rfs.Lookup(cancel, &fuse.InHeader{NodeId: parent}, name, &res.out)
	if res.status == fuse.OK {
		res.nodeID = res.out.NodeId
	}
	return res
}

func (e *vfsEnv) getattr(t *testing.T, id uint64) fuse.AttrOut {
	t.Helper()
	var out fuse.AttrOut
	st := e.rfs.GetAttr(nil, &fuse.GetAttrIn{InHeader: fuse.InHeader{NodeId: id}}, &out)
	if st != fuse.OK {
		t.Fatalf("GetAttr(node %d) status = %v, want OK", id, st)
	}
	return out
}

// open runs the real VirtualMkvNode.Open and returns the handle it registered.
func (e *vfsEnv) open(t *testing.T, id uint64, physical string) (*MkvHandle, fuse.Status) {
	t.Helper()
	t.Cleanup(func() {
		activeHandles.Range(func(k, _ any) bool {
			if h, ok := k.(*MkvHandle); ok && h.path == physical {
				activeHandles.Delete(k)
			}
			return true
		})
		playbackRegistry.Delete(physical)
		sessions.Delete(physical)
	})

	var out fuse.OpenOut
	st := e.rfs.Open(nil, &fuse.OpenIn{InHeader: fuse.InHeader{NodeId: id}, Flags: syscall.O_RDONLY}, &out)
	if st != fuse.OK {
		return nil, st
	}
	var found *MkvHandle
	activeHandles.Range(func(k, _ any) bool {
		if h, ok := k.(*MkvHandle); ok && h.path == physical {
			found = h
			return false
		}
		return true
	})
	return found, st
}

func audioRow(rel, hash string, idx int, size int64, mtime time.Time) library.AudioProjection {
	return library.AudioProjection{
		Section:     library.SectionMusic,
		VirtualPath: rel,
		Hash:        hash,
		FileIndex:   idx,
		Size:        size,
		MtimeNS:     mtime.UnixNano(),
	}
}

func committedA(rel string) library.AudioProjection {
	return audioRow(rel, hashA, idxA, rowSize, rowMtime)
}

// committedProblems lists every way a Lookup result deviates from serving the
// committed row. It returns strings rather than failing so goroutines can use it.
func (e *vfsEnv) committedProblems(res lookupResult, want library.AudioProjection, physical string) []string {
	if res.status != fuse.OK {
		return []string{fmt.Sprintf("Lookup status = %v, want OK (committed audio row must resolve)", res.status)}
	}
	var problems []string
	wantIno := vfs.GenerateFileInode(want.Hash, want.FileIndex)
	if res.out.Ino != wantIno {
		problems = append(problems, fmt.Sprintf("inode = %#x, want %#x (GenerateFileInode(%s, %d))", res.out.Ino, wantIno, want.Hash, want.FileIndex))
	}
	if got := e.inodes.GetFileInode(physical); got != 0 && got != wantIno {
		problems = append(problems, fmt.Sprintf("inode map binds the path to %#x, want %#x or no binding", got, wantIno))
	}
	if res.out.Mode&syscall.S_IFMT != syscall.S_IFREG {
		problems = append(problems, fmt.Sprintf("mode = %#o, want a regular file", res.out.Mode))
	}
	if int64(res.out.Size) != want.Size {
		problems = append(problems, fmt.Sprintf("size = %d, want committed %d", res.out.Size, want.Size))
	}
	if wantSec := uint64(time.Unix(0, want.MtimeNS).Unix()); res.out.Mtime != wantSec {
		problems = append(problems, fmt.Sprintf("mtime = %d, want committed %d", res.out.Mtime, wantSec))
	}
	return problems
}

func (e *vfsEnv) requireCommitted(t *testing.T, step string, res lookupResult, want library.AudioProjection, physical string) {
	t.Helper()
	for _, p := range e.committedProblems(res, want, physical) {
		t.Errorf("%s: %s", step, p)
	}
	if res.status != fuse.OK {
		return
	}
	// The node the kernel would later GetAttr must agree with the Lookup answer.
	at := e.getattr(t, res.nodeID)
	if int64(at.Size) != want.Size {
		t.Errorf("%s: GetAttr size = %d, want committed %d", step, at.Size, want.Size)
	}
	if wantSec := uint64(time.Unix(0, want.MtimeNS).Unix()); at.Mtime != wantSec {
		t.Errorf("%s: GetAttr mtime = %d, want committed %d", step, at.Mtime, wantSec)
	}
}

// stubStates are the physical stub conditions that must not change what a
// committed audio row resolves to. prepare puts the stub on disk (and may seed
// caches) before the first Lookup, so nothing has cached the committed identity.
var stubStates = []struct {
	name    string
	prepare func(e *vfsEnv, phys string, t *testing.T)
}{
	{"control: stub agrees with the committed row", func(e *vfsEnv, phys string, t *testing.T) {
		writeFile(t, phys, jsonStub(streamURL(hashA, idxA), rowSize), rowMtime)
	}},
	{"A1 cold cache: valid same-size stub for another torrent", func(e *vfsEnv, phys string, t *testing.T) {
		writeFile(t, phys, jsonStub(streamURL(hashB, idxB), rowSize), stubMtime)
	}},
	{"A2 stub size and mtime differ from the committed row", func(e *vfsEnv, phys string, t *testing.T) {
		writeFile(t, phys, jsonStub(streamURL(hashB, idxB), rowSize+1234), stubMtime)
	}},
	{"A2 stub for the same torrent but a different size", func(e *vfsEnv, phys string, t *testing.T) {
		writeFile(t, phys, jsonStub(streamURL(hashA, idxA), rowSize*2), stubMtime)
	}},
	{"A1 stale metadata-cache entry for another torrent", func(e *vfsEnv, phys string, t *testing.T) {
		writeFile(t, phys, jsonStub(streamURL(hashA, idxA), rowSize), rowMtime)
		metaCache.Put(phys, &vfs.Metadata{URL: streamURL(hashB, idxB), Size: rowSize + 9, Mtime: stubMtime, Path: phys}, 256)
	}},
	{"E2 placeholder URL with no link=", func(e *vfsEnv, phys string, t *testing.T) {
		writeFile(t, phys, jsonStub("http://tiramisu.invalid/placeholder", rowSize), stubMtime)
	}},
	{"A3 stub content is not parseable", func(e *vfsEnv, phys string, t *testing.T) {
		writeFile(t, phys, "this is not a stub\x00\xff", stubMtime)
	}},
	{"A3 stub file is empty", func(e *vfsEnv, phys string, t *testing.T) {
		writeFile(t, phys, "", stubMtime)
	}},
	{"A3 stub URL has an unsupported scheme", func(e *vfsEnv, phys string, t *testing.T) {
		writeFile(t, phys, jsonStub("magnet:?xt=urn:btih:"+hashB, rowSize), stubMtime)
	}},
	{"A3 stub size is outside the guard", func(e *vfsEnv, phys string, t *testing.T) {
		writeFile(t, phys, jsonStub(streamURL(hashB, idxB), 0), stubMtime)
	}},
}

// A1, A2, I2, E2, A3: whatever the physical stub says, Lookup of a committed
// audio path answers with the row's identity, size and mtime.
func TestAudioLookup_CommittedRowIsAuthoritative(t *testing.T) {
	const rel = "Artist/Album/01 Track_01234567.flac"
	for _, tc := range stubStates {
		t.Run(tc.name, func(t *testing.T) {
			e := newVFSEnv(t)
			phys := e.phys(library.SectionMusic, rel)
			e.publish(committedA(rel))
			tc.prepare(e, phys, t)

			res := e.lookup("music/" + rel)
			e.requireCommitted(t, "Lookup", res, committedA(rel), phys)
		})
	}
}

// I1, I2, A1: once published, the binding survives stub replacement, cache
// eviction and a restart of the node tree. Only a later publication changes it.
func TestAudioLookup_IdentitySurvivesStubReplacementAndEviction(t *testing.T) {
	const rel = "Artist/Album/01 Track_01234567.flac"
	e := newVFSEnv(t)
	phys := e.phys(library.SectionMusic, rel)
	want := committedA(rel)
	e.publish(want)
	lookup := func() lookupResult { return e.lookup("music/" + rel) }

	writeFile(t, phys, jsonStub(streamURL(hashA, idxA), rowSize), rowMtime)
	e.requireCommitted(t, "first lookup with the matching stub", lookup(), want, phys)

	writeFile(t, phys, jsonStub(streamURL(hashB, idxB), rowSize), stubMtime)
	e.requireCommitted(t, "same-size stub for B replaces it, metadata cache still warm", lookup(), want, phys)

	e.evictMetadataCache()
	e.requireCommitted(t, "metadata cache evicted, B stub on disk", lookup(), want, phys)

	e.remount()
	e.requireCommitted(t, "node tree rebuilt, B stub on disk", lookup(), want, phys)

	writeFile(t, phys, "garbage", stubMtime)
	e.evictMetadataCache()
	e.requireCommitted(t, "stub replaced by unparseable content and cache evicted", lookup(), want, phys)
}

// I1: a later publication is the only thing that rebinds a path, and it takes
// effect even though an earlier identity was already served and cached.
func TestAudioLookup_LaterPublicationReplacesIdentity(t *testing.T) {
	const rel = "Artist/Album/02 Track_89abcdef.flac"
	e := newVFSEnv(t)
	phys := e.phys(library.SectionMusic, rel)
	lookup := func() lookupResult { return e.lookup("music/" + rel) }
	writeFile(t, phys, jsonStub(streamURL(hashA, idxA), rowSize), rowMtime)

	first := committedA(rel)
	e.publish(first)
	e.requireCommitted(t, "before republication", lookup(), first, phys)

	second := audioRow(rel, hashC, idxC, rowSize+4096, rowMtime.Add(48*time.Hour))
	e.publish(second)
	e.requireCommitted(t, "after republication with a new identity", lookup(), second, phys)

	e.publish() // the row is gone from the committed set
	if res := lookup(); res.status != fuse.ENOENT {
		t.Errorf("after the row left the committed set: Lookup status = %v, want ENOENT", res.status)
	}
}

// A3, I2: Open resolves the torrent from the committed A/i identity, not from the
// physical stub, including when the stub is malformed.
func TestAudioOpen_UsesCommittedIdentity(t *testing.T) {
	const rel = "Artist/Album/03 Track_01234567.flac"
	for _, tc := range stubStates {
		t.Run(tc.name, func(t *testing.T) {
			e := newVFSEnv(t)
			phys := e.phys(library.SectionMusic, rel)
			want := committedA(rel)
			e.publish(want)
			tc.prepare(e, phys, t)

			res := e.lookup("music/" + rel)
			if res.status != fuse.OK {
				t.Fatalf("Lookup status = %v, want OK: a committed audio file must stay readable", res.status)
			}
			h, st := e.open(t, res.nodeID, phys)
			if st != fuse.OK {
				t.Fatalf("Open status = %v, want OK", st)
			}
			if h == nil {
				t.Fatal("Open succeeded but registered no MkvHandle for the physical path")
			}

			// The read path re-derives identity from the handle URL when a pump has
			// to be rescued late, so the URL itself must carry the committed A/i.
			if gotHash, gotIdx := vfs.ExtractHashAndIndex(h.url); gotHash != want.Hash || gotIdx != want.FileIndex {
				t.Errorf("handle URL identity = (%q, %d), want committed (%q, %d); url=%q", gotHash, gotIdx, want.Hash, want.FileIndex, h.url)
			}
			if !strings.Contains(strings.ToLower(h.magnet), want.Hash) {
				t.Errorf("handle magnet %q does not carry committed hash %s", h.magnet, want.Hash)
			}
			if leaked := strings.ToLower(h.magnet + " " + h.url); strings.Contains(leaked, hashB) {
				t.Errorf("handle references the substituted torrent %s: magnet=%q url=%q", hashB, h.magnet, h.url)
			}
			if h.size != want.Size {
				t.Errorf("handle size = %d, want committed %d", h.size, want.Size)
			}

			v, ok := playbackRegistry.Load(phys)
			if !ok {
				t.Fatal("Open registered no playback state for the physical path")
			}
			ps := v.(*PlaybackState)
			ps.mu.RLock()
			gotHash := ps.Hash
			ps.mu.RUnlock()
			if gotHash != want.Hash {
				t.Errorf("playback state hash = %q, want committed %q", gotHash, want.Hash)
			}
		})
	}
}

// I1, I2, A1 under -race: Lookups racing a stub that is replaced and evicted
// underneath them all answer with the committed identity.
func TestAudioLookup_ConcurrentLookupsKeepCommittedIdentity(t *testing.T) {
	const rel = "Artist/Album/04 Track_01234567.flac"
	const workers, rounds = 8, 25
	e := newVFSEnv(t)
	phys := e.phys(library.SectionMusic, rel)
	want := committedA(rel)
	e.publish(want)
	writeFile(t, phys, jsonStub(streamURL(hashB, idxB), rowSize), stubMtime)

	problems := make([][]string, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				problems[w] = append(problems[w], e.committedProblems(e.lookup("music/"+rel), want, phys)...)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		tmp := phys + ".swap"
		for i := 0; i < rounds; i++ {
			body := jsonStub(streamURL(hashB, idxB), rowSize)
			if i%2 == 1 {
				body = "garbage"
			}
			if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
				t.Errorf("write swap stub: %v", err)
				return
			}
			if err := os.Rename(tmp, phys); err != nil { // atomic: readers never see a torn stub
				t.Errorf("rename swap stub: %v", err)
				return
			}
			metaCache.Delete(phys)
		}
	}()
	wg.Wait()

	seen := make(map[string]int)
	for _, ps := range problems {
		for _, p := range ps {
			seen[p]++
		}
	}
	for p, n := range seen {
		t.Errorf("%s (seen in %d of %d lookups)", p, n, workers*rounds)
	}
}

// E1: an audio extension is not enough to be exposed. Without a committed row
// for exactly this section and path, Lookup says ENOENT even though a
// perfectly valid stub sits on disk.
func TestAudioLookup_UncommittedAudioIsNotExposed(t *testing.T) {
	cases := []struct {
		name    string
		section library.Section
		rel     string
		rows    func(rel string) []library.AudioProjection
	}{
		{"E1 namespace published empty", library.SectionMusic, "Artist/Album/05 Track_01234567.flac",
			func(string) []library.AudioProjection { return nil }},
		{"E1 row committed for a different path", library.SectionMusic, "Artist/Album/05 Track_01234567.flac",
			func(string) []library.AudioProjection {
				return []library.AudioProjection{committedA("Artist/Album/06 Other_01234567.flac")}
			}},
		{"E1 row committed under the other audio section", library.SectionAudiobooks, "Author/Book/Part 01_01234567.m4b",
			func(rel string) []library.AudioProjection { return []library.AudioProjection{committedA(rel)} }}, // Section music
		// Was "never published" while Lookup answered ENOENT for it. An unpublished
		// namespace now blocks (V6), so the no-registry answer that is genuine
		// absence is the Unavailable state (V7).
		{"E1 namespace unavailable (no registry)", library.SectionMusic, "Artist/Album/05 Track_01234567.flac", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newVFSEnv(t)
			phys := e.phys(tc.section, tc.rel)
			writeFile(t, phys, jsonStub(streamURL(hashA, idxA), rowSize), rowMtime)
			if tc.rows != nil {
				e.publish(tc.rows(tc.rel)...)
			} else {
				e.ns.MarkUnavailable()
			}

			res := e.lookup(string(tc.section) + "/" + tc.rel)
			if res.status != fuse.ENOENT {
				t.Errorf("Lookup status = %v, want ENOENT for an uncommitted audio path", res.status)
			}
			if got := e.inodes.GetFileInode(phys); got != 0 {
				t.Errorf("inode map bound an uncommitted audio path to %#x", got)
			}
		})
	}

	t.Run("directories inside audio sections still resolve", func(t *testing.T) {
		e := newVFSEnv(t)
		rel := "Artist/Album/05 Track_01234567.flac"
		writeFile(t, e.phys(library.SectionMusic, rel), jsonStub(streamURL(hashA, idxA), rowSize), rowMtime)
		e.publish(committedA(rel))
		res := e.lookup("music/Artist/Album")
		if res.status != fuse.OK {
			t.Fatalf("Lookup of a directory status = %v, want OK", res.status)
		}
		if res.out.Mode&syscall.S_IFMT != syscall.S_IFDIR {
			t.Errorf("directory mode = %#o, want S_IFDIR", res.out.Mode)
		}
	})
}

// A4: movie/TV virtual files keep deriving identity, size and mtime from their
// physical stub, and audio registry state, even a hostile row for the very same
// path, does not redefine them.
func TestVideoLookupAndOpen_StillDerivedFromPhysicalStub(t *testing.T) {
	const videoSize1, videoSize2 = int64(250_000_000), int64(300_000_000)
	cases := []struct {
		name    string
		section library.Section
		rel     string
	}{
		{"movie stub", library.SectionMovies, "Film (2020)/Film_12345678.mkv"},
		{"tv stub", library.SectionTV, "Show/Season 01/Show_S01E01_12345678.mkv"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newVFSEnv(t)
			phys := e.phys(tc.section, tc.rel)
			full := string(tc.section) + "/" + tc.rel

			// A music row, plus a row squatting on the video's own section and path.
			e.publish(
				committedA("Artist/Album/07 Track_01234567.flac"),
				library.AudioProjection{Section: tc.section, VirtualPath: tc.rel, Hash: hashC, FileIndex: 9, Size: 12345, MtimeNS: rowMtime.UnixNano()},
			)

			url1 := streamURL(hashV1, 2)
			writeFile(t, phys, jsonStub(url1, videoSize1), stubMtime)
			res := e.lookup(full)
			if res.status != fuse.OK {
				t.Fatalf("Lookup status = %v, want OK", res.status)
			}
			wantIno := vfs.GenerateFileInode(hashV1, 2)
			if res.out.Ino != wantIno {
				t.Errorf("inode = %#x, want %#x derived from the stub URL", res.out.Ino, wantIno)
			}
			if int64(res.out.Size) != videoSize1 {
				t.Errorf("size = %d, want the stub's %d", res.out.Size, videoSize1)
			}
			if res.out.Mtime != uint64(stubMtime.Unix()) {
				t.Errorf("mtime = %d, want the stub file's %d", res.out.Mtime, stubMtime.Unix())
			}
			if got := e.inodes.GetFileInode(phys); got != wantIno {
				t.Errorf("inode map binds the video path to %#x, want %#x", got, wantIno)
			}

			h, st := e.open(t, res.nodeID, phys)
			if st != fuse.OK || h == nil {
				t.Fatalf("Open status = %v handle=%v, want OK with a handle", st, h)
			}
			if h.url != url1 {
				t.Errorf("handle URL = %q, want the stub URL %q unchanged", h.url, url1)
			}
			if want := "magnet:?xt=urn:btih:" + hashV1; h.magnet != want {
				t.Errorf("handle magnet = %q, want %q", h.magnet, want)
			}
			if h.size != videoSize1 {
				t.Errorf("handle size = %d, want %d", h.size, videoSize1)
			}
			v, ok := playbackRegistry.Load(phys)
			if !ok {
				t.Fatal("Open registered no playback state")
			}
			ps := v.(*PlaybackState)
			ps.mu.RLock()
			gotHash := ps.Hash
			ps.mu.RUnlock()
			if gotHash != hashV1 {
				t.Errorf("playback state hash = %q, want %q", gotHash, hashV1)
			}

			// Video is not pinned by anything: a rewritten stub is followed, as before.
			url2 := streamURL(hashV2, 4)
			writeFile(t, phys, jsonStub(url2, videoSize2), rowMtime)
			e.evictMetadataCache()
			e.remount()
			res = e.lookup(full)
			if res.status != fuse.OK {
				t.Fatalf("Lookup after the stub was rewritten: status = %v, want OK", res.status)
			}
			if want := vfs.GenerateFileInode(hashV2, 4); res.out.Ino != want {
				t.Errorf("inode after rewrite = %#x, want %#x from the new stub", res.out.Ino, want)
			}
			if int64(res.out.Size) != videoSize2 {
				t.Errorf("size after rewrite = %d, want the new stub's %d", res.out.Size, videoSize2)
			}
			if res.out.Mtime != uint64(rowMtime.Unix()) {
				t.Errorf("mtime after rewrite = %d, want the new stub file's %d", res.out.Mtime, rowMtime.Unix())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Slice audio-readiness-vfs.
//
// Audio directory operations block until reconciliation publishes (settled
// design). That makes a hang the worst possible regression, so every call under
// test runs on its own goroutine behind a guard: one that must return is given
// vfsGuard, one that must block is watched for vfsBlockedWindow. A regression
// therefore fails the test instead of hanging the run, and newVFSEnv's cleanup
// releases any waiter a failing test left behind.
// ---------------------------------------------------------------------------

const (
	// vfsGuard bounds any call expected to return. Generous for -race on a Pi-class
	// CI box, still far below the test binary's own timeout.
	vfsGuard = 3 * time.Second
	// vfsBlockedWindow is how long a call expected to block is watched. A call that
	// wrongly returns does so in microseconds, so this only has to be non-trivial.
	vfsBlockedWindow = 150 * time.Millisecond
)

// asyncCall runs fn on a goroutine and hands its result back through guards.
// val and have belong to the test goroutine alone.
type asyncCall[T any] struct {
	result chan T
	cancel chan struct{}
	exited chan struct{}
	once   sync.Once
	val    T
	have   bool
}

func startCall[T any](t *testing.T, fn func(cancel <-chan struct{}) T) *asyncCall[T] {
	t.Helper()
	c := &asyncCall[T]{result: make(chan T, 1), cancel: make(chan struct{}), exited: make(chan struct{})}
	t.Cleanup(c.abort)
	go func() {
		defer close(c.exited)
		c.result <- fn(c.cancel)
	}()
	return c
}

// abort delivers the kernel's request cancellation. Safe to call repeatedly.
func (c *asyncCall[T]) abort() { c.once.Do(func() { close(c.cancel) }) }

func (c *asyncCall[T]) within(d time.Duration) (T, bool) {
	if c.have {
		return c.val, true
	}
	select {
	case v := <-c.result:
		c.val, c.have = v, true
		return v, true
	case <-time.After(d):
		var zero T
		return zero, false
	}
}

func (c *asyncCall[T]) mustBlock(t *testing.T, what string) {
	t.Helper()
	if v, done := c.within(vfsBlockedWindow); done {
		t.Fatalf("%s returned %+v instead of blocking until reconciliation publishes", what, v)
	}
}

func (c *asyncCall[T]) mustReturn(t *testing.T, what string) T {
	t.Helper()
	v, ok := c.within(vfsGuard)
	if !ok {
		c.abort()
		t.Fatalf("%s did not return within %v: it is blocked", what, vfsGuard)
	}
	return v
}

func (c *asyncCall[T]) mustExit(t *testing.T, what string) {
	t.Helper()
	select {
	case <-c.exited:
	case <-time.After(vfsGuard):
		t.Fatalf("%s: goroutine still running %v after it returned", what, vfsGuard)
	}
}

// namespaceWaiters counts goroutines currently parked in AudioNamespace.WaitServable.
func namespaceWaiters() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "(*AudioNamespace).WaitServable")
}

// requireWaitersAtMost fails if a blocked reader is still parked after its call
// returned: the leak V4 forbids.
func requireWaitersAtMost(t *testing.T, atMost int, what string) {
	t.Helper()
	deadline := time.NewTimer(vfsGuard)
	defer deadline.Stop()
	for namespaceWaiters() > atMost {
		select {
		case <-deadline.C:
			t.Errorf("%s: %d goroutine(s) still blocked in WaitServable, want at most %d", what, namespaceWaiters(), atMost)
			return
		default:
			runtime.Gosched()
		}
	}
}

type readdirResult struct {
	names []string
	errno syscall.Errno
}

func (r readdirResult) String() string {
	return fmt.Sprintf("names=%v errno=%v (%d)", r.names, r.errno, int(r.errno))
}

// String keeps a failed "must block" assertion readable: the status is the point.
func (r lookupResult) String() string { return fmt.Sprintf("status=%v", r.status) }

// readdirNames runs the real VirtualDirNode.Readdir with the same context type the
// FUSE bridge hands it, and drains the stream it returns.
func readdirNames(cancel <-chan struct{}, physical string) readdirResult {
	d := &VirtualDirNode{physicalPath: physical}
	stream, errno := d.Readdir(&fuse.Context{Cancel: cancel})
	if errno != 0 {
		return readdirResult{errno: errno}
	}
	var res readdirResult
	for stream.HasNext() {
		de, errno := stream.Next()
		if errno != 0 {
			return readdirResult{names: res.names, errno: errno}
		}
		res.names = append(res.names, de.Name)
	}
	stream.Close()
	sort.Strings(res.names)
	return res
}

func readdirAsync(t *testing.T, physical string) *asyncCall[readdirResult] {
	t.Helper()
	return startCall(t, func(cancel <-chan struct{}) readdirResult { return readdirNames(cancel, physical) })
}

// audioFixture is a music/ directory holding one stub the registry will commit and
// one it will not. Files only: directory entries in an empty audio tree are not
// something the requirements pin.
type audioFixture struct {
	dir  string
	name string // the committed stub
	row  library.AudioProjection
}

func (e *vfsEnv) audioFixture(t *testing.T) audioFixture {
	t.Helper()
	fx := audioFixture{dir: filepath.Join(e.root, "music"), name: "Track_01234567.flac"}
	fx.row = committedA(fx.name)
	writeFile(t, filepath.Join(fx.dir, fx.name), jsonStub(streamURL(hashA, idxA), rowSize), rowMtime)
	writeFile(t, filepath.Join(fx.dir, "Other_89abcdef.flac"), jsonStub(streamURL(hashB, idxB), rowSize), stubMtime)
	return fx
}

// audioDirNode resolves the music/ directory node while the namespace is Ready, so
// a test can then move to another state without depending on how the section
// directory itself is looked up. It leaves the namespace Ready.
func (e *vfsEnv) audioDirNode(t *testing.T, fx audioFixture) uint64 {
	t.Helper()
	e.publish(fx.row)
	res := e.lookup("music")
	if res.status != fuse.OK {
		t.Fatalf("setup: Lookup(music) status = %v, want OK", res.status)
	}
	return res.nodeID
}

// setState drives the namespace into s using only the public library API.
func (e *vfsEnv) setState(t *testing.T, s library.NamespaceState, rows ...library.AudioProjection) {
	t.Helper()
	switch s {
	case library.Unreconciled:
		e.ns.MarkUnready()
	case library.Ready:
		e.ns.Publish(rows)
	case library.Unavailable:
		e.ns.MarkUnavailable()
	case library.Failed:
		e.ns.MarkFailed(errors.New("registry read failed"))
	}
	if got := e.ns.State(); got != s {
		t.Fatalf("setup: State() = %v, want %v", got, s)
	}
}

var allNamespaceStates = []library.NamespaceState{library.Unreconciled, library.Ready, library.Unavailable, library.Failed}

// V1: Readdir of an audio directory blocks while Unreconciled and, once Publish
// runs on another goroutine, returns the committed entries: neither EAGAIN nor an
// empty success.
func TestAudioReaddir_BlocksUntilPublishThenListsCommitted(t *testing.T) {
	e := newVFSEnv(t)
	fx := e.audioFixture(t)

	call := readdirAsync(t, fx.dir)
	call.mustBlock(t, "Readdir(music) while Unreconciled")

	e.publish(fx.row)
	got := call.mustReturn(t, "Readdir(music) after Publish")
	if got.errno != 0 {
		t.Fatalf("Readdir after Publish errno = %v (%d), want 0", got.errno, int(got.errno))
	}
	if want := []string{fx.name}; !equalStrings(got.names, want) {
		t.Errorf("Readdir after Publish = %v, want exactly the committed %v", got.names, want)
	}
}

// V1: the blocked reader takes whatever terminal state reconciliation ends in.
func TestAudioReaddir_BlockedCallFollowsTheTerminalState(t *testing.T) {
	cases := []struct {
		name      string
		release   func(e *vfsEnv)
		wantErrno syscall.Errno
	}{
		{"failure while blocked surfaces as EIO", func(e *vfsEnv) { e.ns.MarkFailed(errors.New("boom")) }, syscall.EIO},
		{"unavailable while blocked serves an empty listing", func(e *vfsEnv) { e.ns.MarkUnavailable() }, 0},
		{"empty publication while blocked serves an empty listing", func(e *vfsEnv) { e.publish() }, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newVFSEnv(t)
			fx := e.audioFixture(t)
			call := readdirAsync(t, fx.dir)
			call.mustBlock(t, "Readdir(music) while Unreconciled")

			tc.release(e)
			got := call.mustReturn(t, "Readdir(music) after the state changed")
			if got.errno != tc.wantErrno {
				t.Fatalf("errno = %v (%d), want %v (%d)", got.errno, int(got.errno), tc.wantErrno, int(tc.wantErrno))
			}
			if len(got.names) != 0 {
				t.Errorf("listing = %v, want empty: nothing is committed", got.names)
			}
		})
	}
}

// V2: no authority source means an empty audio tree, served at once. An install
// with a music/ directory and the state DB disabled must never hang.
func TestAudioReaddir_UnavailableServesEmptyWithoutBlocking(t *testing.T) {
	e := newVFSEnv(t)
	fx := e.audioFixture(t) // valid stubs on disk, none committed
	e.setState(t, library.Unavailable)

	got := readdirAsync(t, fx.dir).mustReturn(t, "Readdir(music) in Unavailable")
	if got.errno != 0 {
		t.Fatalf("errno = %v (%d), want 0: Unavailable is a legitimately empty tree, not an error", got.errno, int(got.errno))
	}
	if len(got.names) != 0 {
		t.Errorf("listing = %v, want empty: no registry means no committed projection", got.names)
	}
}

// V3: a failed reconciliation is a terminal I/O error. Not EAGAIN (a retryable
// error scanners read as "empty"), not an empty success.
func TestAudioReaddir_FailedReturnsEIO(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"failure with a cause", errors.New("registry read failed")},
		{"failure without a cause", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newVFSEnv(t)
			fx := e.audioFixture(t)
			e.ns.MarkFailed(tc.err)

			got := readdirAsync(t, fx.dir).mustReturn(t, "Readdir(music) in Failed")
			if got.errno != syscall.EIO {
				t.Errorf("errno = %v (%d), want EIO (%d)", got.errno, int(got.errno), int(syscall.EIO))
			}
			if len(got.names) != 0 {
				t.Errorf("listing = %v, want none alongside an error", got.names)
			}
		})
	}
}

// V4 (+V10 for the dir cache): a Readdir cancelled while blocked returns promptly
// with an errno, leaves no goroutine parked, and caches nothing, so the eventual
// real listing is not shadowed by a partial one.
func TestAudioReaddir_CancelledWhileBlockedReturnsPromptly(t *testing.T) {
	t.Run("cancelled after it started blocking", func(t *testing.T) {
		e := newVFSEnv(t)
		fx := e.audioFixture(t)
		before := namespaceWaiters()

		call := readdirAsync(t, fx.dir)
		call.mustBlock(t, "Readdir(music) while Unreconciled")
		call.abort()

		got := call.mustReturn(t, "cancelled Readdir(music)")
		if got.errno == 0 {
			t.Errorf("cancelled Readdir returned success with %v, want a non-zero errno", got.names)
		}
		call.mustExit(t, "cancelled Readdir")
		requireWaitersAtMost(t, before, "after cancelled Readdir")
		if entries, cached := globalDirCache.Get(fx.dir); cached {
			t.Errorf("cancelled Readdir left %d entries in the dir cache", len(entries))
		}

		e.publish(fx.row)
		after := readdirAsync(t, fx.dir).mustReturn(t, "Readdir(music) after Publish")
		if after.errno != 0 || !equalStrings(after.names, []string{fx.name}) {
			t.Errorf("Readdir after Publish = %v errno %v, want [%s] errno 0", after.names, after.errno, fx.name)
		}
	})

	t.Run("already cancelled before it started", func(t *testing.T) {
		e := newVFSEnv(t)
		fx := e.audioFixture(t)
		before := namespaceWaiters()

		cancelled := make(chan struct{})
		close(cancelled)
		call := startCall(t, func(<-chan struct{}) readdirResult { return readdirNames(cancelled, fx.dir) })

		got := call.mustReturn(t, "Readdir(music) with an already-cancelled request")
		if got.errno == 0 {
			t.Errorf("returned success with %v, want a non-zero errno", got.names)
		}
		requireWaitersAtMost(t, before, "after already-cancelled Readdir")
	})
}

// readdirViaBridge is the kernel's sequence for listing a directory: OpenDir, then
// ReadDir, which is where the stream, and therefore the errno, is created.
func (e *vfsEnv) readdirViaBridge(cancel <-chan struct{}, id uint64) fuse.Status {
	var open fuse.OpenOut
	if st := e.rfs.OpenDir(cancel, &fuse.OpenIn{InHeader: fuse.InHeader{NodeId: id}}, &open); st != fuse.OK {
		return st
	}
	defer e.rfs.ReleaseDir(&fuse.ReleaseIn{InHeader: fuse.InHeader{NodeId: id}, Fh: open.Fh})
	buf := make([]byte, 4096)
	return e.rfs.ReadDir(cancel, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: id}, Fh: open.Fh, Size: uint32(len(buf))}, fuse.NewDirEntryList(buf, 0))
}

// V1-V3 as the kernel sees them: the status that goes back on the wire.
func TestAudioReaddir_KernelPathCarriesTheSameErrnos(t *testing.T) {
	t.Run("Failed is EIO on the wire", func(t *testing.T) {
		e := newVFSEnv(t)
		fx := e.audioFixture(t)
		id := e.audioDirNode(t, fx)
		e.setState(t, library.Failed)

		got := startCall(t, func(c <-chan struct{}) fuse.Status { return e.readdirViaBridge(c, id) }).mustReturn(t, "ReadDir in Failed")
		if got != fuse.EIO {
			t.Errorf("status = %v, want EIO", got)
		}
	})

	t.Run("Unavailable is an OK empty listing on the wire", func(t *testing.T) {
		e := newVFSEnv(t)
		fx := e.audioFixture(t)
		id := e.audioDirNode(t, fx)
		e.setState(t, library.Unavailable)

		got := startCall(t, func(c <-chan struct{}) fuse.Status { return e.readdirViaBridge(c, id) }).mustReturn(t, "ReadDir in Unavailable")
		if got != fuse.OK {
			t.Errorf("status = %v, want OK", got)
		}
	})

	t.Run("Unreconciled blocks, then Publish releases it with OK", func(t *testing.T) {
		e := newVFSEnv(t)
		fx := e.audioFixture(t)
		id := e.audioDirNode(t, fx)
		e.setState(t, library.Unreconciled)

		call := startCall(t, func(c <-chan struct{}) fuse.Status { return e.readdirViaBridge(c, id) })
		call.mustBlock(t, "ReadDir while Unreconciled")
		e.publish(fx.row)
		if got := call.mustReturn(t, "ReadDir after Publish"); got != fuse.OK {
			t.Errorf("status = %v, want OK", got)
		}
	})
}

// V5: Readdir of movies and tv never blocks and never fails on account of audio
// readiness, in every state. Video is read straight from disk, as before.
func TestVideoReaddir_NeverBlocksOrConsultsReadiness(t *testing.T) {
	for _, section := range []library.Section{library.SectionMovies, library.SectionTV} {
		for _, state := range allNamespaceStates {
			t.Run(fmt.Sprintf("%s/%v", section, state), func(t *testing.T) {
				e := newVFSEnv(t)
				dir := filepath.Join(e.root, string(section))
				const stub = "Title_12345678.mkv"
				writeFile(t, filepath.Join(dir, stub), jsonStub(streamURL(hashV1, 2), 250_000_000), stubMtime)
				e.setState(t, state, committedA("Track_01234567.flac"))

				got := readdirAsync(t, dir).mustReturn(t, fmt.Sprintf("Readdir(%s) in %v", section, state))
				if got.errno != 0 {
					t.Fatalf("errno = %v (%d), want 0: video must not depend on audio readiness", got.errno, int(got.errno))
				}
				if want := []string{stub}; !equalStrings(got.names, want) {
					t.Errorf("listing = %v, want %v read from disk", got.names, want)
				}
			})
		}
	}
}

// V6: Lookup of an audio path blocks while Unreconciled and resolves after Publish.
// The ENOENT it used to answer is exactly what a scanner reads as a deletion.
func TestAudioLookup_BlocksUntilPublishThenResolves(t *testing.T) {
	e := newVFSEnv(t)
	fx := e.audioFixture(t)
	dir := e.audioDirNode(t, fx)
	e.setState(t, library.Unreconciled)

	call := startCall(t, func(c <-chan struct{}) lookupResult { return e.lookupIn(c, dir, fx.name) })
	call.mustBlock(t, "Lookup(music/"+fx.name+") while Unreconciled")

	e.publish(fx.row)
	res := call.mustReturn(t, "Lookup after Publish")
	for _, p := range e.committedProblems(res, fx.row, filepath.Join(fx.dir, fx.name)) {
		t.Errorf("Lookup after Publish: %s", p)
	}
}

// V6: the blocked Lookup takes whatever the reconciliation ends in. A path the
// published set does not contain is genuinely absent, and only then ENOENT.
func TestAudioLookup_BlockedCallFollowsTheTerminalState(t *testing.T) {
	cases := []struct {
		name    string
		release func(e *vfsEnv)
		want    fuse.Status
	}{
		{"failure while blocked is EIO", func(e *vfsEnv) { e.ns.MarkFailed(errors.New("boom")) }, fuse.EIO},
		{"unavailable while blocked is genuine absence", func(e *vfsEnv) { e.ns.MarkUnavailable() }, fuse.ENOENT},
		{"empty publication while blocked is genuine absence", func(e *vfsEnv) { e.publish() }, fuse.ENOENT},
		{"publication of other paths while blocked is genuine absence", func(e *vfsEnv) {
			e.publish(committedA("Someone_Else_00000000.flac"))
		}, fuse.ENOENT},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newVFSEnv(t)
			fx := e.audioFixture(t)
			dir := e.audioDirNode(t, fx)
			e.setState(t, library.Unreconciled)

			call := startCall(t, func(c <-chan struct{}) lookupResult { return e.lookupIn(c, dir, fx.name) })
			call.mustBlock(t, "Lookup while Unreconciled")

			tc.release(e)
			if res := call.mustReturn(t, "Lookup after the state changed"); res.status != tc.want {
				t.Errorf("status = %v, want %v", res.status, tc.want)
			}
		})
	}
}

// V7: with no registry an audio path is genuinely absent, answered at once.
func TestAudioLookup_UnavailableAnswersENOENTWithoutBlocking(t *testing.T) {
	e := newVFSEnv(t)
	fx := e.audioFixture(t)
	dir := e.audioDirNode(t, fx)
	e.setState(t, library.Unavailable)

	for _, name := range []string{fx.name, "Other_89abcdef.flac", "Missing_00000000.flac"} {
		res := startCall(t, func(c <-chan struct{}) lookupResult { return e.lookupIn(c, dir, name) }).
			mustReturn(t, "Lookup("+name+") in Unavailable")
		if res.status != fuse.ENOENT {
			t.Errorf("Lookup(%s) status = %v, want ENOENT", name, res.status)
		}
	}
}

// V8: a failed reconciliation is EIO for every audio name, never ENOENT, so no
// scanner can read the failure as the library having been deleted.
func TestAudioLookup_FailedReturnsEIONotENOENT(t *testing.T) {
	for _, cause := range []error{errors.New("registry read failed"), nil} {
		t.Run(fmt.Sprintf("cause=%v", cause), func(t *testing.T) {
			e := newVFSEnv(t)
			fx := e.audioFixture(t)
			dir := e.audioDirNode(t, fx)
			e.ns.MarkFailed(cause)

			for _, name := range []string{fx.name, "Other_89abcdef.flac", "Missing_00000000.flac"} {
				res := startCall(t, func(c <-chan struct{}) lookupResult { return e.lookupIn(c, dir, name) }).
					mustReturn(t, "Lookup("+name+") in Failed")
				if res.status != fuse.EIO {
					t.Errorf("Lookup(%s) status = %v, want EIO", name, res.status)
				}
			}
		})
	}
}

// V9: Lookup of a movie or tv path never blocks and never fails on account of
// audio readiness, in every state, and still reads its metadata from the stub.
func TestVideoLookup_NeverBlocksOrConsultsReadiness(t *testing.T) {
	const videoSize = int64(250_000_000)
	cases := []struct {
		section library.Section
		rel     string
	}{
		{library.SectionMovies, "Film (2020)/Film_12345678.mkv"},
		{library.SectionTV, "Show/Season 01/Show_S01E01_12345678.mkv"},
	}
	for _, tc := range cases {
		for _, state := range allNamespaceStates {
			t.Run(fmt.Sprintf("%s/%v", tc.section, state), func(t *testing.T) {
				e := newVFSEnv(t)
				writeFile(t, e.phys(tc.section, tc.rel), jsonStub(streamURL(hashV1, 2), videoSize), stubMtime)
				e.setState(t, state, committedA("Track_01234567.flac"))

				full := string(tc.section) + "/" + tc.rel
				res := startCall(t, func(c <-chan struct{}) lookupResult { return e.lookupCancellable(c, full) }).
					mustReturn(t, fmt.Sprintf("Lookup(%s) in %v", full, state))
				if res.status != fuse.OK {
					t.Fatalf("status = %v, want OK: video must not depend on audio readiness", res.status)
				}
				if want := vfs.GenerateFileInode(hashV1, 2); res.out.Ino != want {
					t.Errorf("inode = %#x, want %#x derived from the stub URL", res.out.Ino, want)
				}
				if int64(res.out.Size) != videoSize {
					t.Errorf("size = %d, want the stub's %d", res.out.Size, videoSize)
				}
				if res.out.Mtime != uint64(stubMtime.Unix()) {
					t.Errorf("mtime = %d, want the stub file's %d", res.out.Mtime, stubMtime.Unix())
				}
			})
		}
	}
}

// V10: a Lookup cancelled while blocked returns promptly, is not mistaken for
// absence, and leaves nothing behind in the metadata cache, the dir cache or the
// inode map, so the next Lookup after Publish resolves cleanly.
func TestAudioLookup_CancelledWhileBlockedLeavesNoPartialState(t *testing.T) {
	e := newVFSEnv(t)
	fx := e.audioFixture(t)
	dir := e.audioDirNode(t, fx)
	e.setState(t, library.Unreconciled)
	phys := filepath.Join(fx.dir, fx.name)
	before := namespaceWaiters()

	call := startCall(t, func(c <-chan struct{}) lookupResult { return e.lookupIn(c, dir, fx.name) })
	call.mustBlock(t, "Lookup while Unreconciled")
	call.abort()

	res := call.mustReturn(t, "cancelled Lookup")
	if res.status == fuse.OK {
		t.Errorf("cancelled Lookup returned OK, want an error")
	}
	if res.status == fuse.ENOENT {
		t.Errorf("cancelled Lookup returned ENOENT: an interrupted wait is not an absence, and a negative entry could be cached")
	}
	call.mustExit(t, "cancelled Lookup")
	requireWaitersAtMost(t, before, "after cancelled Lookup")

	if _, cached := metaCache.Get(phys); cached {
		t.Errorf("cancelled Lookup left a metadata-cache entry for %s", phys)
	}
	if _, cached := globalDirCache.Get(fx.dir); cached {
		t.Errorf("cancelled Lookup left a dir-cache entry for %s", fx.dir)
	}
	if got := e.inodes.GetFileInode(phys); got != 0 {
		t.Errorf("cancelled Lookup bound %s to inode %#x", phys, got)
	}

	e.publish(fx.row)
	after := startCall(t, func(c <-chan struct{}) lookupResult { return e.lookupIn(c, dir, fx.name) }).
		mustReturn(t, "Lookup after Publish")
	for _, p := range e.committedProblems(after, fx.row, phys) {
		t.Errorf("Lookup after Publish: %s", p)
	}
	listing := readdirAsync(t, fx.dir).mustReturn(t, "Readdir after Publish")
	if listing.errno != 0 || !equalStrings(listing.names, []string{fx.name}) {
		t.Errorf("Readdir after Publish = %v errno %v, want [%s] errno 0", listing.names, listing.errno, fx.name)
	}
}

// V11 (behaviour of the provider the Library API must be given): a boot where the
// state DB was expected but did not open yields a registry that fails every
// ownership question, so a drop is denied rather than permitted.
func TestAudioOwnershipRegistry_DeniesDropsWhenStateDBIsExpectedButMissing(t *testing.T) {
	prevDB, prevCfg := stateDB, globalConfig.Load()
	t.Cleanup(func() { stateDB = prevDB; globalConfig.Store(prevCfg) })
	stateDB = nil
	globalConfig.Store(&config.Config{EnableStateDB: true})

	reg := audioOwnershipRegistry()
	if reg == nil {
		t.Fatal("audioOwnershipRegistry() = nil with EnableStateDB and no DB: a drop would be permitted")
	}
	ok, err := library.MayDropTorrent(reg, hashA)
	if ok {
		t.Error("MayDropTorrent permitted a drop while audio ownership could not be checked")
	}
	if !errors.Is(err, errStateDBUnavailable) {
		t.Errorf("MayDropTorrent error = %v, want it to wrap errStateDBUnavailable", err)
	}
}

// V12: with audio genuinely not configured the provider yields a true nil, so
// drops stay permitted. Fail-closed must not become fail-never (and a typed nil
// wrapped in the interface would silently do exactly that).
func TestAudioOwnershipRegistry_PermitsDropsWhenAudioIsDisabled(t *testing.T) {
	prevDB, prevCfg := stateDB, globalConfig.Load()
	t.Cleanup(func() { stateDB = prevDB; globalConfig.Store(prevCfg) })
	stateDB = nil
	globalConfig.Store(&config.Config{EnableStateDB: false})

	reg := audioOwnershipRegistry()
	if reg != nil {
		t.Fatalf("audioOwnershipRegistry() = %#v with the state DB disabled, want a true nil interface", reg)
	}
	ok, err := library.MayDropTorrent(reg, hashA)
	if err != nil || !ok {
		t.Errorf("MayDropTorrent(nil registry) = (%v, %v), want (true, nil)", ok, err)
	}
}

// V11 (wiring): main() builds the Library API manager inline, so the only
// hermetic way to see what it is given is to read its source. Every
// library.New(library.Config{...}) in package main must set AudioRegistry from
// audioOwnershipRegistry(), never from a raw stateDB-derived value, which is nil
// exactly when the DB failed to open and so permits the drop.
func TestLibraryManager_IsBuiltWithTheFailClosedAudioRegistry(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	sites := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 || !isSelector(call.Fun, "library", "New") {
				return true
			}
			lit, ok := call.Args[0].(*ast.CompositeLit)
			if !ok || !isSelector(lit.Type, "library", "Config") {
				return true
			}
			sites++
			pos := fset.Position(call.Pos())
			var value ast.Expr
			for _, el := range lit.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "AudioRegistry" {
						value = kv.Value
					}
				}
			}
			if value == nil {
				t.Errorf("%s: library.New has no AudioRegistry: drops are never checked against audio ownership", pos)
				return true
			}
			if c, ok := value.(*ast.CallExpr); !ok || !isIdent(c.Fun, "audioOwnershipRegistry") {
				t.Errorf("%s: library.New AudioRegistry is not audioOwnershipRegistry(): a raw value is nil when the DB failed to open, which permits the drop", pos)
			}
			return true
		})
	}
	if sites == 0 {
		t.Fatal("found no library.New(library.Config{...}) in package main: the Library API is no longer wired here")
	}
}

func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == name && isIdent(sel.X, pkg)
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
