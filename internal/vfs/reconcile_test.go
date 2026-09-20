package vfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"tiramisu/internal/metadb"
)

type audioProjectionSourceFake struct {
	bySection map[string][]metadb.AudioProjection
	err       error
}

func (f *audioProjectionSourceFake) CommittedAudioProjections(section string) ([]metadb.AudioProjection, error) {
	if f.err != nil {
		return nil, f.err
	}
	if section == "" {
		var all []metadb.AudioProjection
		for _, name := range []string{"music", "audiobooks"} {
			all = append(all, f.bySection[name]...)
		}
		return all, nil
	}
	return append([]metadb.AudioProjection(nil), f.bySection[section]...), nil
}

func testAudioProjection(section, virtualPath, hash string, index int, size int64) metadb.AudioProjection {
	return metadb.AudioProjection{
		Section:         section,
		VirtualPath:     virtualPath,
		PortablePathKey: strings.ToLower(virtualPath),
		Hash:            hash,
		FileIndex:       index,
		SourcePath:      "release/" + filepath.Base(virtualPath),
		Size:            size,
		MtimeNS:         1700000000000000001,
		Title:           filepath.Base(virtualPath),
		Magnet:          "magnet:?xt=urn:btih:" + hash,
		State:           metadb.AudioCommitted,
		TxnID:           "txn-" + hash + fmt.Sprint(index),
		StagingName:     ".stage-" + hash,
		CreatedAtNS:     1700000000000000002,
		UpdatedAtNS:     1700000000000000003,
	}
}

func writeJSONStub(t *testing.T, root string, p metadb.AudioProjection, size int64) string {
	t.Helper()
	path := filepath.Join(root, p.Section, filepath.FromSlash(p.VirtualPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create stub directory: %v", err)
	}
	content := fmt.Sprintf(`{"url":"http://127.0.0.1/stream?link=%s&index=%d&play","size":%d,"magnet":"%s","imdb":"tt1234567"}`,
		p.Hash, p.FileIndex, size, p.Magnet)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write stub %q: %v", path, err)
	}
	return path
}

func writeRawStub(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create stub directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write stub %q: %v", path, err)
	}
}

func newTestInodeMap(t *testing.T) *InodeMap {
	t.Helper()
	return NewInodeMap(filepath.Join(t.TempDir(), "inode-map.json"), nil)
}

func TestReadMetadataFromFileWithLimits_SizeContracts(t *testing.T) {
	// N1, N2, N4: audio accepts positive small files while video retains its
	// established floor; both policies retain the positive-size and maximum bounds.
	tests := []struct {
		name      string
		size      int64
		limits    SizeLimits
		wantValid bool
	}{
		{"N1 audio accepts one byte", 1, AudioSizeLimits, true},
		{"N1 audio accepts one KiB", 1024, AudioSizeLimits, true},
		{"N1 audio accepts realistic four MiB track", 4 * 1024 * 1024, AudioSizeLimits, true},
		{"N1 audio accepts exact movie floor minus one", MinFileSize - 1, AudioSizeLimits, true},
		{"N1 audio accepts exact movie floor", MinFileSize, AudioSizeLimits, true},
		{"N2 video rejects one byte", 1, VideoSizeLimits, false},
		{"N2 video rejects one KiB", 1024, VideoSizeLimits, false},
		{"N2 video rejects realistic four MiB track", 4 * 1024 * 1024, VideoSizeLimits, false},
		{"N2 video rejects exact movie floor minus one", MinFileSize - 1, VideoSizeLimits, false},
		{"N2 video accepts exact movie floor", MinFileSize, VideoSizeLimits, true},
		{"N4 audio rejects zero", 0, AudioSizeLimits, false},
		{"N4 audio rejects negative", -1, AudioSizeLimits, false},
		{"N4 audio rejects above maximum", MaxFileSize + 1, AudioSizeLimits, false},
		{"N4 video rejects zero", 0, VideoSizeLimits, false},
		{"N4 video rejects negative", -1, VideoSizeLimits, false},
		{"N4 video rejects above maximum", MaxFileSize + 1, VideoSizeLimits, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stub.flac")
			writeRawStub(t, path, fmt.Sprintf(`{"url":"https://example.invalid/stream","size":%d,"imdb":"tt1234567"}`, tc.size))
			got, err := ReadMetadataFromFileWithLimits(path, tc.limits)
			if !tc.wantValid {
				if !errors.Is(err, ErrInvalidSize) {
					t.Fatalf("error = %v, want errors.Is(_, ErrInvalidSize)", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("read metadata: %v", err)
			}
			if got.Size != tc.size {
				t.Errorf("size = %d, want %d", got.Size, tc.size)
			}
		})
	}

	t.Run("N1 legacy audio stub uses audio limits", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "legacy.flac")
		writeRawStub(t, path, "https://example.invalid/legacy\n1024\nunused\ntt7654321\n")
		got, err := ReadMetadataFromFileWithLimits(path, AudioSizeLimits)
		if err != nil {
			t.Fatalf("read legacy audio metadata: %v", err)
		}
		if got.Size != 1024 || got.URL != "https://example.invalid/legacy" || got.Path != path || got.ImdbID != "tt7654321" {
			t.Errorf("legacy metadata = %#v, want exact URL, size, path, and IMDb ID", got)
		}
	})
}

func TestReadMetadataFromFile_VideoCompatibility(t *testing.T) {
	// N3, P5: the old entry point remains the video-policy entry point for both
	// supported formats, including the established extracted fields.
	tests := []struct {
		name    string
		content func(int64) string
		url     string
		imdb    string
	}{
		{
			name: "N3 JSON format",
			content: func(size int64) string {
				return fmt.Sprintf(`{"url":" https://example.invalid/json ","size":%d,"magnet":"m","imdb":"tt1111111"}`, size)
			},
			url:  "https://example.invalid/json",
			imdb: "tt1111111",
		},
		{
			name: "N3 legacy line format",
			content: func(size int64) string {
				return fmt.Sprintf("http://example.invalid/legacy\n%d\nunused\ntt2222222\n", size)
			},
			url:  "http://example.invalid/legacy",
			imdb: "tt2222222",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			validPath := filepath.Join(t.TempDir(), "valid.mkv")
			writeRawStub(t, validPath, tc.content(MinFileSize))
			got, err := ReadMetadataFromFile(validPath)
			if err != nil {
				t.Fatalf("read exact-floor video metadata: %v", err)
			}
			if got.URL != tc.url || got.Size != MinFileSize || got.Path != validPath || got.ImdbID != tc.imdb {
				t.Errorf("metadata = %#v, want URL %q, size %d, path %q, IMDb %q", got, tc.url, MinFileSize, validPath, tc.imdb)
			}

			invalidPath := filepath.Join(t.TempDir(), "too-small.mkv")
			writeRawStub(t, invalidPath, tc.content(MinFileSize-1))
			if _, err := ReadMetadataFromFile(invalidPath); !errors.Is(err, ErrInvalidSize) {
				t.Errorf("below-floor error = %v, want errors.Is(_, ErrInvalidSize)", err)
			}
		})
	}

	t.Run("P5 existing inode methods retain content identity", func(t *testing.T) {
		im := newTestInodeMap(t)
		path := filepath.Join(t.TempDir(), "movies", "movie.mkv")
		got := im.AddFile(path, "ABCDEF", 7)
		want := GenerateFileInode("abcdef", 7)
		if got != want || im.GetFileInode(path) != want || im.GetFileInodeByName("movie.mkv") != want {
			t.Errorf("existing inode behavior changed: AddFile=%d full=%d basename=%d want=%d", got, im.GetFileInode(path), im.GetFileInodeByName("movie.mkv"), want)
		}
	})
}

func TestReconcileAudio_MixedHealthBothSectionsAndDeterministicResults(t *testing.T) {
	// N5, N7-N10, P2, P4, Q4, Q5: one pass classifies each committed
	// projection independently, spans both section roots, and never repairs disk.
	root := t.TempDir()
	healthy := testAudioProjection("music", "Artist/Album/02 - Healthy.flac", "hash-z", 2, 4096)
	missing := testAudioProjection("music", "Artist/Album/03 - Missing.flac", "hash-a", 3, 8192)
	mismatch := testAudioProjection("music", "Artist/Album/04 - Mismatch.flac", "hash-z", 4, 16384)
	audiobook := testAudioProjection("audiobooks", "Author/Book/01 - Opening.m4b", "hash-m", 1, 32768)
	missingBook := testAudioProjection("audiobooks", "Author/Book/02 - Missing.m4b", "hash-b", 2, 65536)
	mismatchBook := testAudioProjection("audiobooks", "Author/Book/03 - Mismatch.m4b", "hash-y", 3, 131072)
	staged := testAudioProjection("music", "Artist/Album/05 - Staged.flac", "hash-staged", 5, 1024)
	staged.State = metadb.AudioStaged

	healthyPath := writeJSONStub(t, root, healthy, healthy.Size)
	mismatchPath := writeJSONStub(t, root, mismatch, mismatch.Size+1)
	audiobookPath := writeJSONStub(t, root, audiobook, audiobook.Size)
	mismatchBookPath := writeJSONStub(t, root, mismatchBook, mismatchBook.Size+1)
	stagedPath := writeJSONStub(t, root, staged, staged.Size)
	mismatchBefore, err := os.ReadFile(mismatchPath)
	if err != nil {
		t.Fatal(err)
	}

	src := &audioProjectionSourceFake{bySection: map[string][]metadb.AudioProjection{
		"music":      {mismatch, missing, healthy},
		"audiobooks": {mismatchBook, audiobook, missingBook},
	}}
	im := newTestInodeMap(t)
	got, err := ReconcileAudio(src, im, root)
	if err != nil {
		t.Fatalf("ReconcileAudio: %v", err)
	}
	if got.Registered != 2 {
		t.Errorf("N5 Registered = %d, want 2 healthy committed stubs", got.Registered)
	}
	if want := []string{"audiobooks/Author/Book/02 - Missing.m4b", "music/Artist/Album/03 - Missing.flac"}; !reflect.DeepEqual(got.MissingStub, want) {
		t.Errorf("N7 MissingStub = %v, want %v", got.MissingStub, want)
	}
	if want := []string{"audiobooks/Author/Book/03 - Mismatch.m4b", "music/Artist/Album/04 - Mismatch.flac"}; !reflect.DeepEqual(got.SizeMismatch, want) {
		t.Errorf("N8 SizeMismatch = %v, want %v", got.SizeMismatch, want)
	}
	if want := []string{"hash-a", "hash-b", "hash-m", "hash-y", "hash-z"}; !reflect.DeepEqual(got.ReferencedHashes, want) {
		t.Errorf("N9/Q4 ReferencedHashes = %v, want sorted distinct %v", got.ReferencedHashes, want)
	}
	for _, check := range []struct {
		name string
		path string
		p    metadb.AudioProjection
	}{
		{"N5 music full physical path", healthyPath, healthy},
		{"N10 Q5 nested audiobook path under audiobook section", audiobookPath, audiobook},
	} {
		t.Run(check.name, func(t *testing.T) {
			if inode := im.GetFileInode(check.path); inode != GenerateFileInode(check.p.Hash, check.p.FileIndex) {
				t.Errorf("full-path inode = %d, want content inode %d", inode, GenerateFileInode(check.p.Hash, check.p.FileIndex))
			}
		})
	}
	for name, path := range map[string]string{
		"N7 missing projection":     filepath.Join(root, missing.Section, filepath.FromSlash(missing.VirtualPath)),
		"N7 missing audiobook":      filepath.Join(root, missingBook.Section, filepath.FromSlash(missingBook.VirtualPath)),
		"N8 mismatching projection": mismatchPath,
		"N8 mismatching audiobook":  mismatchBookPath,
		"P2 staged projection":      stagedPath,
	} {
		if inode := im.GetFileInode(path); inode != 0 {
			t.Errorf("%s inode = %d, want zero", name, inode)
		}
	}
	if _, err := os.Stat(filepath.Join(root, missing.Section, filepath.FromSlash(missing.VirtualPath))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("P4 missing stub was created or stat failed unexpectedly: %v", err)
	}
	mismatchAfter, err := os.ReadFile(mismatchPath)
	if err != nil {
		t.Fatalf("P4 mismatching stub disappeared: %v", err)
	}
	if !reflect.DeepEqual(mismatchAfter, mismatchBefore) {
		t.Error("P4 mismatching stub was modified")
	}
	if _, err := os.Stat(healthyPath); err != nil {
		t.Errorf("P4 healthy stub was removed: %v", err)
	}

	second, err := ReconcileAudio(src, im, root)
	if err != nil {
		t.Fatalf("second ReconcileAudio: %v", err)
	}
	if !reflect.DeepEqual(second, got) {
		t.Errorf("Q4 repeated result differs:\nfirst:  %#v\nsecond: %#v", got, second)
	}
}

func TestReconcileAudio_BasenameCollisionsKeepFullPathIdentity(t *testing.T) {
	// P1 and N6: the ambiguous basename cache must never substitute for the two
	// independently addressable full-path identities.
	tests := []struct {
		name string
		a    metadb.AudioProjection
		b    metadb.AudioProjection
	}{
		{
			name: "P1 different hashes",
			a:    testAudioProjection("music", "Artist/Album One/01 - Intro.flac", "hash-one", 1, 1000),
			b:    testAudioProjection("music", "Artist/Album Two/01 - Intro.flac", "hash-two", 1, 1000),
		},
		{
			name: "P1 same hash different file indexes",
			a:    testAudioProjection("music", "Artist/Album One/01 - Intro.flac", "shared-hash", 1, 1000),
			b:    testAudioProjection("music", "Artist/Album Two/01 - Intro.flac", "shared-hash", 2, 1000),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			pathA := writeJSONStub(t, root, tc.a, tc.a.Size)
			pathB := writeJSONStub(t, root, tc.b, tc.b.Size)
			src := &audioProjectionSourceFake{bySection: map[string][]metadb.AudioProjection{"music": {tc.a, tc.b}}}
			im := newTestInodeMap(t)
			result, err := ReconcileAudio(src, im, root)
			if err != nil {
				t.Fatalf("ReconcileAudio: %v", err)
			}
			if result.Registered != 2 {
				t.Fatalf("Registered = %d, want 2", result.Registered)
			}
			inodeA, inodeB := im.GetFileInode(pathA), im.GetFileInode(pathB)
			wantA := GenerateFileInode(tc.a.Hash, tc.a.FileIndex)
			wantB := GenerateFileInode(tc.b.Hash, tc.b.FileIndex)
			if inodeA != wantA || inodeB != wantB || inodeA == 0 || inodeB == 0 || inodeA == inodeB {
				t.Errorf("full-path inodes = (%d, %d), want distinct content identities (%d, %d)", inodeA, inodeB, wantA, wantB)
			}
			byName := im.GetFileInodeByName("01 - Intro.flac")
			if byName != inodeA && byName != inodeB {
				t.Errorf("ambiguous basename inode = %d, want either %d or %d", byName, inodeA, inodeB)
			}

			fresh := newTestInodeMap(t)
			if _, err := ReconcileAudio(src, fresh, root); err != nil {
				t.Fatalf("N6 repeat reconciliation: %v", err)
			}
			if fresh.GetFileInode(pathA) != inodeA || fresh.GetFileInode(pathB) != inodeB {
				t.Errorf("N6 fresh-map inodes changed: got (%d, %d), want (%d, %d)", fresh.GetFileInode(pathA), fresh.GetFileInode(pathB), inodeA, inodeB)
			}
		})
	}
}

func TestReconcileAudio_RealRegistryRestartStateFilterAndReadOnly(t *testing.T) {
	// N6, P2, P3: prove the interface against *metadb.DB, including persistent
	// committed identity, lifecycle filtering, and no registry mutation.
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "audio.db")
	committed := testAudioProjection("music", "Artist/Album/Track.flac", "restart-hash", 7, 7777)
	staged := testAudioProjection("music", "Artist/Album/Staged.flac", "staged-hash", 8, 8888)
	removing := testAudioProjection("audiobooks", "Author/Book/Removing.m4b", "removing-hash", 9, 9999)
	writeJSONStub(t, root, committed, committed.Size)
	writeJSONStub(t, root, staged, staged.Size)
	writeJSONStub(t, root, removing, removing.Size)

	db, err := metadb.New(dbPath, nil)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	defer func() { _ = db.Close() }()
	for _, p := range []metadb.AudioProjection{committed, staged, removing} {
		if err := db.StageAudioProjections(p.TxnID, []metadb.AudioProjection{p}); err != nil {
			t.Fatalf("stage %s: %v", p.VirtualPath, err)
		}
	}
	if _, err := db.CommitAudioProjections(committed.TxnID, committed.UpdatedAtNS); err != nil {
		t.Fatalf("commit projection: %v", err)
	}
	if _, err := db.CommitAudioProjections(removing.TxnID, removing.UpdatedAtNS); err != nil {
		t.Fatalf("commit removing projection: %v", err)
	}
	if ok, err := db.MarkAudioProjectionRemoving(removing.Section, removing.VirtualPath, removing.UpdatedAtNS+1); err != nil || !ok {
		t.Fatalf("mark projection removing: ok=%v err=%v", ok, err)
	}

	before := registryProjectionSnapshot(t, db)
	im1 := newTestInodeMap(t)
	result1, err := ReconcileAudio(db, im1, root)
	if err != nil {
		t.Fatalf("first reconciliation: %v", err)
	}
	committedPath := filepath.Join(root, committed.Section, filepath.FromSlash(committed.VirtualPath))
	firstInode := im1.GetFileInode(committedPath)
	if result1.Registered != 1 || firstInode != GenerateFileInode(committed.Hash, committed.FileIndex) {
		t.Errorf("committed reconciliation = result %#v inode %d", result1, firstInode)
	}
	if im1.GetFileInode(filepath.Join(root, staged.Section, filepath.FromSlash(staged.VirtualPath))) != 0 ||
		im1.GetFileInode(filepath.Join(root, removing.Section, filepath.FromSlash(removing.VirtualPath))) != 0 {
		t.Error("P2 staged or removing projection was registered")
	}
	after := registryProjectionSnapshot(t, db)
	if !reflect.DeepEqual(after, before) {
		t.Errorf("P3 registry changed during reconciliation:\nbefore: %#v\nafter:  %#v", before, after)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close registry: %v", err)
	}

	reopened, err := metadb.New(dbPath, nil)
	if err != nil {
		t.Fatalf("reopen registry: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	im2 := newTestInodeMap(t)
	result2, err := ReconcileAudio(reopened, im2, root)
	if err != nil {
		t.Fatalf("reconcile after restart: %v", err)
	}
	if result2.Registered != 1 || im2.GetFileInode(committedPath) != firstInode {
		t.Errorf("N6 restart identity changed: result=%#v inode=%d want=%d", result2, im2.GetFileInode(committedPath), firstInode)
	}
}

func registryProjectionSnapshot(t *testing.T, db *metadb.DB) []metadb.AudioProjection {
	t.Helper()
	var out []metadb.AudioProjection
	for _, state := range []metadb.AudioProjectionState{metadb.AudioStaged, metadb.AudioCommitted, metadb.AudioRemoving} {
		rows, err := db.AudioProjectionsByState(state)
		if err != nil {
			t.Fatalf("snapshot registry state %q: %v", state, err)
		}
		out = append(out, rows...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func TestReconcileAudio_MalformedStubsDoNotAbortLaterProjection(t *testing.T) {
	// Q2: existing but unusable stubs are reported, never registered, and do not
	// prevent a later healthy projection from being processed.
	root := t.TempDir()
	invalidJSON := testAudioProjection("music", "A/01 Invalid JSON.flac", "bad-json", 1, 100)
	invalidURL := testAudioProjection("music", "A/02 Invalid URL.flac", "bad-url", 2, 200)
	healthy := testAudioProjection("music", "A/03 Healthy.flac", "good", 3, 300)
	writeRawStub(t, filepath.Join(root, invalidJSON.Section, filepath.FromSlash(invalidJSON.VirtualPath)), "{not-json")
	writeRawStub(t, filepath.Join(root, invalidURL.Section, filepath.FromSlash(invalidURL.VirtualPath)), `{"url":"file:///tmp/source","size":200}`)
	healthyPath := writeJSONStub(t, root, healthy, healthy.Size)
	src := &audioProjectionSourceFake{bySection: map[string][]metadb.AudioProjection{"music": {invalidJSON, invalidURL, healthy}}}
	im := newTestInodeMap(t)
	result, err := ReconcileAudio(src, im, root)
	if err != nil {
		t.Fatalf("Q2 reconciliation aborted: %v", err)
	}
	if result.Registered != 1 || im.GetFileInode(healthyPath) != GenerateFileInode(healthy.Hash, healthy.FileIndex) {
		t.Errorf("Q2 later healthy projection not registered: result=%#v inode=%d", result, im.GetFileInode(healthyPath))
	}
	bad := append(append([]string(nil), result.MissingStub...), result.SizeMismatch...)
	sort.Strings(bad)
	wantBad := []string{"music/A/01 Invalid JSON.flac", "music/A/02 Invalid URL.flac"}
	if !reflect.DeepEqual(bad, wantBad) {
		t.Errorf("Q2 malformed classifications = %v, want %v", bad, wantBad)
	}
	for _, p := range []metadb.AudioProjection{invalidJSON, invalidURL} {
		path := filepath.Join(root, p.Section, filepath.FromSlash(p.VirtualPath))
		if inode := im.GetFileInode(path); inode != 0 {
			t.Errorf("Q2 malformed stub %q inode = %d, want zero", p.VirtualPath, inode)
		}
	}
}

func TestReconcileAudio_RejectsStubIdentityMismatchAndContinues(t *testing.T) {
	// N11: a stable virtual path must never be registered when its stub addresses
	// a different torrent object, while exact and later healthy rows still land.
	registryHash := strings.Repeat("a", 40)
	otherHash := strings.Repeat("b", 40)
	exactHash := strings.Repeat("c", 40)
	laterHash := strings.Repeat("d", 40)
	tests := []struct {
		name          string
		stubHash      string
		stubFileIndex int
	}{
		{
			name:          "N11 mismatching hash is rejected while later exact identities register",
			stubHash:      otherHash,
			stubFileIndex: 1,
		},
		{
			name:          "N11 mismatching file index is rejected while later exact identities register",
			stubHash:      registryHash,
			stubFileIndex: 99,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			mismatch := testAudioProjection("music", "Identity/01 - Mismatch.flac", registryHash, 1, 4096)
			exact := testAudioProjection("music", "Identity/02 - Exact.flac", exactHash, 2, 8192)
			later := testAudioProjection("music", "Identity/03 - Later Healthy.flac", laterHash, 3, 16384)
			mismatchPath := filepath.Join(root, mismatch.Section, filepath.FromSlash(mismatch.VirtualPath))
			writeRawStub(t, mismatchPath, fmt.Sprintf(
				`{"url":"http://127.0.0.1/stream?link=%s&index=%d&play","size":%d}`,
				tc.stubHash, tc.stubFileIndex, mismatch.Size,
			))
			exactPath := writeJSONStub(t, root, exact, exact.Size)
			laterPath := writeJSONStub(t, root, later, later.Size)
			src := &audioProjectionSourceFake{bySection: map[string][]metadb.AudioProjection{
				"music": {mismatch, exact, later},
			}}
			im := newTestInodeMap(t)

			result, err := ReconcileAudio(src, im, root)
			if err != nil {
				t.Fatalf("N11 reconciliation aborted: %v", err)
			}
			if result.Registered != 2 {
				t.Errorf("N11 Registered = %d, want 2 exact-identity stubs", result.Registered)
			}
			bad := append(append([]string(nil), result.MissingStub...), result.SizeMismatch...)
			if want := []string{"music/Identity/01 - Mismatch.flac"}; !reflect.DeepEqual(bad, want) {
				t.Errorf("N11 identity mismatch classifications = %v, want %v", bad, want)
			}
			if inode := im.GetFileInode(mismatchPath); inode != 0 {
				t.Errorf("N11 identity-mismatching stub inode = %d, want zero", inode)
			}
			if inode := im.GetFileInode(exactPath); inode != GenerateFileInode(exact.Hash, exact.FileIndex) {
				t.Errorf("N11 exact-identity stub inode = %d, want %d", inode, GenerateFileInode(exact.Hash, exact.FileIndex))
			}
			if inode := im.GetFileInode(laterPath); inode != GenerateFileInode(later.Hash, later.FileIndex) {
				t.Errorf("N11 later healthy stub inode = %d, want %d", inode, GenerateFileInode(later.Hash, later.FileIndex))
			}
		})
	}
}

func TestReconcileAudio_BoundariesAndErrors(t *testing.T) {
	t.Run("Q1 empty registry yields an empty result", func(t *testing.T) {
		got, err := ReconcileAudio(&audioProjectionSourceFake{}, newTestInodeMap(t), t.TempDir())
		if err != nil {
			t.Fatalf("empty reconciliation: %v", err)
		}
		if got == nil || got.Registered != 0 || len(got.MissingStub) != 0 || len(got.SizeMismatch) != 0 || len(got.ReferencedHashes) != 0 {
			t.Errorf("empty result = %#v, want non-nil zero result with empty slices", got)
		}
	})

	t.Run("Q3 registry read error is returned", func(t *testing.T) {
		want := errors.New("registry unavailable")
		got, err := ReconcileAudio(&audioProjectionSourceFake{err: want}, newTestInodeMap(t), t.TempDir())
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want errors.Is(_, registry error)", err)
		}
		if got != nil && (got.Registered != 0 || len(got.MissingStub) != 0 || len(got.SizeMismatch) != 0 || len(got.ReferencedHashes) != 0) {
			t.Errorf("Q3 registry failure returned misleading partial result: %#v", got)
		}
	})

	t.Run("P6 nil source does not panic", func(t *testing.T) {
		assertNoPanicAndNoWork(t, func() (*AudioReconcileResult, error) {
			return ReconcileAudio(nil, newTestInodeMap(t), t.TempDir())
		})
	})

	t.Run("P6 nil inode map does not panic", func(t *testing.T) {
		assertNoPanicAndNoWork(t, func() (*AudioReconcileResult, error) {
			return ReconcileAudio(&audioProjectionSourceFake{}, nil, t.TempDir())
		})
	})
}

func assertNoPanicAndNoWork(t *testing.T, call func() (*AudioReconcileResult, error)) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("panicked: %v", recovered)
		}
	}()
	got, err := call()
	if err == nil && got != nil && (got.Registered != 0 || len(got.MissingStub) != 0 || len(got.SizeMismatch) != 0 || len(got.ReferencedHashes) != 0) {
		t.Errorf("nil dependency returned successful non-empty work: %#v", got)
	}
}
