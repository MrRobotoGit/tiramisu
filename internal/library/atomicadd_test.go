package library

import (
	"context"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"tiramisu/internal/metadb"
)

const (
	atomicAddHash       = "0123456789abcdef0123456789abcdef01234567"
	atomicAddBase32Hash = "abcdefghijklmnopqrstuvwxyz234567"
)

type atomicEngineCall struct {
	method  string
	magnet  string
	title   string
	hash    string
	maxWait int
}

type atomicGoStormFake struct {
	mu sync.Mutex

	addHash   string
	addErr    error
	info      *TorrentStats
	infoErr   error
	torrents  []TorrentStats
	listErr   error
	removeErr error
	calls     []atomicEngineCall
}

func (f *atomicGoStormFake) AddTorrent(_ context.Context, magnet, title string) (string, error) {
	f.record(atomicEngineCall{method: "AddTorrent", magnet: magnet, title: title})
	return f.addHash, f.addErr
}

func (f *atomicGoStormFake) GetTorrentInfo(_ context.Context, hash string, maxWait int) (*TorrentStats, error) {
	f.record(atomicEngineCall{method: "GetTorrentInfo", hash: hash, maxWait: maxWait})
	return f.info, f.infoErr
}

func (f *atomicGoStormFake) RemoveTorrent(_ context.Context, hash string) error {
	f.record(atomicEngineCall{method: "RemoveTorrent", hash: hash})
	return f.removeErr
}

func (f *atomicGoStormFake) ListTorrents(context.Context) ([]TorrentStats, error) {
	f.record(atomicEngineCall{method: "ListTorrents"})
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]TorrentStats(nil), f.torrents...), f.listErr
}

func (f *atomicGoStormFake) record(call atomicEngineCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *atomicGoStormFake) callsFor(method string) []atomicEngineCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var calls []atomicEngineCall
	for _, call := range f.calls {
		if call.method == method {
			calls = append(calls, call)
		}
	}
	return calls
}

type atomicRegistryCall struct {
	method string
	txnID  string
	rows   []metadb.AudioProjection
}

type atomicRegistryFake struct {
	mu sync.Mutex

	rows      map[string]metadb.AudioProjection
	staged    map[string][]metadb.AudioProjection
	stageErr  error
	commitErr error
	calls     []atomicRegistryCall
	onCommit  func(string, []metadb.AudioProjection)
}

var _ AudioProjectionRegistry = (*atomicRegistryFake)(nil)

func newAtomicRegistryFake(initial ...metadb.AudioProjection) *atomicRegistryFake {
	f := &atomicRegistryFake{
		rows:   make(map[string]metadb.AudioProjection),
		staged: make(map[string][]metadb.AudioProjection),
	}
	for _, row := range initial {
		f.rows[atomicProjectionKey(row.Section, row.VirtualPath)] = row
	}
	return f
}

func (f *atomicRegistryFake) GetAudioProjection(section, virtualPath string) (*metadb.AudioProjection, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[atomicProjectionKey(section, virtualPath)]
	if !ok {
		return nil, false, nil
	}
	copy := row
	return &copy, true, nil
}

func (f *atomicRegistryFake) AudioProjectionBySource(hash string, fileIndex int) (*metadb.AudioProjection, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if row.Hash == hash && row.FileIndex == fileIndex {
			copy := row
			return &copy, true, nil
		}
	}
	return nil, false, nil
}

func (f *atomicRegistryFake) AudioProjectionByPortableKey(section, portableKey string) (*metadb.AudioProjection, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if row.Section == section && row.PortablePathKey == portableKey {
			copy := row
			return &copy, true, nil
		}
	}
	return nil, false, nil
}

func (f *atomicRegistryFake) StageAudioProjections(txnID string, rows []metadb.AudioProjection) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, atomicRegistryCall{method: "stage", txnID: txnID, rows: cloneAtomicRows(rows)})
	if f.stageErr != nil {
		return f.stageErr
	}
	staged := cloneAtomicRows(rows)
	for i := range staged {
		staged[i].State = metadb.AudioStaged
		staged[i].TxnID = txnID
	}
	f.staged[txnID] = staged
	return nil
}

func (f *atomicRegistryFake) CommitAudioProjections(txnID string, _ int64) (int, error) {
	f.mu.Lock()
	rows := cloneAtomicRows(f.staged[txnID])
	f.calls = append(f.calls, atomicRegistryCall{method: "commit", txnID: txnID, rows: cloneAtomicRows(rows)})
	hook := f.onCommit
	commitErr := f.commitErr
	f.mu.Unlock()

	if hook != nil {
		hook(txnID, cloneAtomicRows(rows))
	}
	if commitErr != nil {
		return 0, commitErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range rows {
		row.State = metadb.AudioCommitted
		row.TxnID = ""
		f.rows[atomicProjectionKey(row.Section, row.VirtualPath)] = row
	}
	delete(f.staged, txnID)
	return len(rows), nil
}

func (f *atomicRegistryFake) RollbackAudioProjections(txnID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := f.staged[txnID]
	f.calls = append(f.calls, atomicRegistryCall{method: "rollback", txnID: txnID, rows: cloneAtomicRows(rows)})
	delete(f.staged, txnID)
	return len(rows), nil
}

func (f *atomicRegistryFake) AudioHashReferenced(hash string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if row.Hash == hash {
			return true, nil
		}
	}
	for _, rows := range f.staged {
		for _, row := range rows {
			if row.Hash == hash {
				return true, nil
			}
		}
	}
	return false, nil
}

func (f *atomicRegistryFake) callsFor(method string) []atomicRegistryCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var calls []atomicRegistryCall
	for _, call := range f.calls {
		if call.method == method {
			call.rows = cloneAtomicRows(call.rows)
			calls = append(calls, call)
		}
	}
	return calls
}

func (f *atomicRegistryFake) allCalls() []atomicRegistryCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := make([]atomicRegistryCall, len(f.calls))
	for i, call := range f.calls {
		calls[i] = call
		calls[i].rows = cloneAtomicRows(call.rows)
	}
	return calls
}

func (f *atomicRegistryFake) committedRows() []metadb.AudioProjection {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := make([]metadb.AudioProjection, 0, len(f.rows))
	for _, row := range f.rows {
		if row.State == metadb.AudioCommitted {
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Section != rows[j].Section {
			return rows[i].Section < rows[j].Section
		}
		return rows[i].VirtualPath < rows[j].VirtualPath
	})
	return rows
}

func (f *atomicRegistryFake) stagedRows() []metadb.AudioProjection {
	f.mu.Lock()
	defer f.mu.Unlock()
	var rows []metadb.AudioProjection
	for _, batch := range f.staged {
		rows = append(rows, batch...)
	}
	return cloneAtomicRows(rows)
}

func atomicProjectionKey(section, virtualPath string) string {
	return section + "\x00" + virtualPath
}

func cloneAtomicRows(rows []metadb.AudioProjection) []metadb.AudioProjection {
	return append([]metadb.AudioProjection(nil), rows...)
}

type atomicFixture struct {
	root        string
	musicRoot   string
	bookRoot    string
	engine      *atomicGoStormFake
	registry    *atomicRegistryFake
	manager     *Manager
	invalidated []string
	mu          sync.Mutex
}

func newAtomicFixture(t *testing.T, files []FileStat, initial ...metadb.AudioProjection) *atomicFixture {
	t.Helper()
	root := t.TempDir()
	musicRoot := filepath.Join(root, string(SectionMusic))
	bookRoot := filepath.Join(root, string(SectionAudiobooks))
	moviesRoot := filepath.Join(root, string(SectionMovies))
	tvRoot := filepath.Join(root, string(SectionTV))
	for _, dir := range []string{musicRoot, bookRoot, moviesRoot, tvRoot} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatalf("Mkdir(%q): %v", dir, err)
		}
	}

	engine := &atomicGoStormFake{
		addHash: atomicAddHash,
		info: &TorrentStats{
			Hash:      atomicAddHash,
			FileStats: append([]FileStat(nil), files...),
		},
	}
	registry := newAtomicRegistryFake(initial...)
	f := &atomicFixture{
		root:      root,
		musicRoot: musicRoot,
		bookRoot:  bookRoot,
		engine:    engine,
		registry:  registry,
	}
	f.manager = New(Config{
		MoviesDir:        moviesRoot,
		TVDir:            tvRoot,
		GoStormURL:       "http://gostorm.invalid/base/",
		GoStorm:          engine,
		AudioRoot:        root,
		AudioRegistry:    registry,
		AudioProjections: registry,
		InvalidatePath: func(path string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.invalidated = append(f.invalidated, path)
		},
		Logger: log.New(io.Discard, "", 0),
	})
	return f
}

func (f *atomicFixture) invalidations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.invalidated...)
}

func TestAddAudio_RejectsHashMagnetMismatch_B2(t *testing.T) {
	source := FileStat{ID: 1, Path: "Release/01.flac", Length: 4 << 20}
	requestPath := "Artist/Album/01_01234567.flac"

	t.Run("B2a_a_disagreeing_magnet_is_a_400_with_no_engine_call", func(t *testing.T) {
		f := newAtomicFixture(t, []FileStat{source})
		_, err := f.manager.AddAudio(context.Background(), AddRequest{
			Type:   "music",
			Hash:   atomicAddHash,
			Magnet: BuildMagnet(atomicAddBase32Hash, "Other Release", DefaultTrackers()),
			Title:  "An Album",
			Files:  []AudioFileRequest{{SourcePath: source.Path, Path: requestPath}},
		})
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
			t.Fatalf("AddAudio() error = %v, want 400", err)
		}
		if !strings.Contains(apiErr.Message, "hash_magnet_mismatch") {
			t.Errorf("message = %q, want hash_magnet_mismatch", apiErr.Message)
		}
		for _, method := range []string{"AddTorrent", "GetTorrentInfo", "ListTorrents", "RemoveTorrent"} {
			if calls := f.engine.callsFor(method); len(calls) != 0 {
				t.Errorf("%s calls = %+v, want none", method, calls)
			}
		}
	})

	t.Run("B2b_the_same_hash_in_base32_is_not_a_mismatch", func(t *testing.T) {
		raw, err := hex.DecodeString(atomicAddHash)
		if err != nil {
			t.Fatal(err)
		}
		sameBase32 := strings.ToLower(base32.StdEncoding.EncodeToString(raw))
		f := newAtomicFixture(t, []FileStat{source})
		_, err = f.manager.AddAudio(context.Background(), AddRequest{
			Type:   "music",
			Hash:   atomicAddHash,
			Magnet: BuildMagnet(sameBase32, "An Album", DefaultTrackers()),
			Title:  "An Album",
			Files:  []AudioFileRequest{{SourcePath: source.Path, Path: requestPath}},
		})
		if err != nil {
			t.Fatalf("AddAudio() error = %v, want nil for equivalent spellings", err)
		}
		if calls := f.engine.callsFor("AddTorrent"); len(calls) != 1 {
			t.Fatalf("AddTorrent calls = %+v, want 1", calls)
		}
	})
}

// H6: when cleanup cannot prove it removed this request's files, the rows stay staged
// for startup recovery instead of being rolled back: the rows are the only proof of
// what the leftovers belong to.
func TestAddAudio_CleanupFailureLeavesRowsStaged_H6(t *testing.T) {
	source := FileStat{ID: 1, Path: "Release/01.flac", Length: 4 << 20}
	requestPath := "Artist/Album/01_01234567.flac"
	f := newAtomicFixture(t, []FileStat{source})
	f.registry.commitErr = errors.New("commit failed")
	// Before the commit error fires, replace the published file with a different
	// object so the rollback's identity check refuses to unlink it.
	f.registry.onCommit = func(_ string, rows []metadb.AudioProjection) {
		if len(rows) != 1 {
			t.Errorf("rows at commit = %d, want 1", len(rows))
			return
		}
		final := filepath.Join(f.musicRoot, filepath.FromSlash(rows[0].VirtualPath))
		replacement := filepath.Join(f.musicRoot, "replacement.tmp")
		if err := os.WriteFile(replacement, []byte("not this request's file"), 0o644); err != nil {
			t.Errorf("write replacement: %v", err)
			return
		}
		if err := os.Rename(replacement, final); err != nil {
			t.Errorf("replace published file: %v", err)
		}
	}

	_, err := f.manager.AddAudio(context.Background(), AddRequest{
		Type:  "music",
		Hash:  atomicAddHash,
		Title: "An Album",
		Files: []AudioFileRequest{{SourcePath: source.Path, Path: requestPath}},
	})
	if err == nil {
		t.Fatal("AddAudio() error = nil, want a cleanup failure")
	}
	if !strings.Contains(err.Error(), "cleanup incomplete") {
		t.Errorf("error = %q, want it to report incomplete cleanup", err)
	}
	if rollbacks := f.registry.callsFor("rollback"); len(rollbacks) != 0 {
		t.Errorf("rollback calls = %+v, want none while the transaction stays staged", rollbacks)
	}
	assertFileContent(t, filepath.Join(f.musicRoot, filepath.FromSlash(requestPath)), []byte("not this request's file"))
}

// B2 follow-up: when GoStorm reports junk instead of the hash it added, the cleanup
// must drop the torrent under the spelling the engine knows (canonical hex), not the
// request's: a base32 magnet would otherwise leak the hydrated torrent.
func TestAddAudio_MalformedEngineHashDropsCanonicalSpelling_B2(t *testing.T) {
	source := FileStat{ID: 1, Path: "Release/01.flac", Length: 4 << 20}
	f := newAtomicFixture(t, []FileStat{source})
	f.engine.addHash = "not-a-hash"

	response, err := f.manager.AddAudio(context.Background(), AddRequest{
		Type:   "music",
		Magnet: BuildMagnet(atomicAddBase32Hash, "An Album", DefaultTrackers()),
		Title:  "An Album",
		Files:  []AudioFileRequest{{SourcePath: source.Path, Path: "Artist/Album/01_01234567.flac"}},
	})
	assertAtomicStatus(t, response, err, http.StatusBadGateway)

	wantKey := canonicalHashKey(atomicAddBase32Hash)
	removes := f.engine.callsFor("RemoveTorrent")
	if len(removes) != 1 || removes[0].hash != wantKey {
		t.Fatalf("RemoveTorrent calls = %+v, want exactly one with %q", removes, wantKey)
	}
}

func TestAddAudio_OneFilePublishesAtomically_E1_E3_E8_E18_E19_E20(t *testing.T) {
	source := FileStat{ID: 7, Path: "Release/Disc 1/01 - Track.flac", Length: 34_567_890}
	requestPath := "Artist/Album/01 - Track_01234567.flac"
	magnet := "magnet:?xt=urn:btih:" + atomicAddHash + "&dn=Caller+Magnet"
	f := newAtomicFixture(t, []FileStat{source})

	var commitObservationErr error
	f.registry.onCommit = func(_ string, rows []metadb.AudioProjection) {
		finalPath := filepath.Join(f.musicRoot, filepath.FromSlash(requestPath))
		info, err := os.Stat(finalPath)
		if err != nil || !info.Mode().IsRegular() {
			commitObservationErr = fmt.Errorf("E8 final path was not a regular file before registry commit: info = %v, error = %v", info, err)
			return
		}
		files, err := atomicRegularFiles(f.musicRoot)
		if err != nil {
			commitObservationErr = fmt.Errorf("E8 inspect pre-commit tree: %w", err)
			return
		}
		if !reflect.DeepEqual(files, []string{finalPath}) {
			commitObservationErr = fmt.Errorf("E8 regular files before commit = %v, want only final path %q", files, finalPath)
			return
		}
		staging, err := atomicDotLeadingPaths(f.musicRoot)
		if err != nil {
			commitObservationErr = fmt.Errorf("E8 inspect staging leftovers: %w", err)
			return
		}
		if len(staging) != 0 {
			commitObservationErr = fmt.Errorf("E8 staging leftovers before commit = %v, want none", staging)
			return
		}
		if len(rows) != 1 || rows[0].Section != string(SectionMusic) || rows[0].VirtualPath != requestPath ||
			rows[0].Hash != atomicAddHash || rows[0].FileIndex != source.ID || rows[0].SourcePath != source.Path {
			commitObservationErr = fmt.Errorf("E8 rows at commit = %+v, want exactly the created projection", rows)
		}
	}

	response, err := f.manager.AddAudio(context.Background(), AddRequest{
		Type:   "music",
		Magnet: magnet,
		Title:  "  An Album  ",
		Files: []AudioFileRequest{{
			SourcePath: source.Path,
			Path:       requestPath,
		}},
	})
	if err != nil {
		t.Fatalf("AddAudio() error = %v, want nil", err)
	}
	if commitObservationErr != nil {
		t.Fatal(commitObservationErr)
	}
	if response == nil {
		t.Fatal("AddAudio() response = nil, want non-nil")
	}

	t.Run("E1_one_file_is_created_and_staged_then_committed_once", func(t *testing.T) {
		if len(response.Files) != 1 || response.Files[0].State != AudioProjectionCreated {
			t.Fatalf("response Files = %#v, want one created file", response.Files)
		}
		stages := f.registry.callsFor("stage")
		commits := f.registry.callsFor("commit")
		if len(stages) != 1 || len(stages[0].rows) != 1 {
			t.Fatalf("stage calls = %#v, want one call with one row", stages)
		}
		if len(commits) != 1 || stages[0].txnID == "" || commits[0].txnID != stages[0].txnID {
			t.Fatalf("commit calls = %#v after stages %#v, want same transaction committed once", commits, stages)
		}
		calls := f.registry.allCalls()
		if len(calls) != 2 || calls[0].method != "stage" || calls[1].method != "commit" {
			t.Errorf("registry call order = %#v, want stage then commit", calls)
		}
		if len(f.registry.callsFor("rollback")) != 0 {
			t.Errorf("rollback called after successful add")
		}
		row := stages[0].rows[0]
		if row.Section != string(SectionMusic) || row.VirtualPath != requestPath || row.Hash != atomicAddHash ||
			row.SourcePath != source.Path || row.FileIndex != source.ID || row.Size != source.Length {
			t.Errorf("staged row = %+v, want exact section/path/source identity", row)
		}
	})

	finalPath := filepath.Join(f.musicRoot, filepath.FromSlash(requestPath))
	t.Run("E3_stub_contains_engine_stream_URL_source_size_and_magnet", func(t *testing.T) {
		stub := readAtomicStub(t, finalPath)
		wantURL := "http://gostorm.invalid/base/stream?link=" + atomicAddHash + "&index=7&play"
		if stub.URL != wantURL || stub.Size != source.Length || stub.Magnet != magnet {
			t.Errorf("stub = %+v, want URL %q, size %d, magnet %q", stub, wantURL, source.Length, magnet)
		}
	})

	t.Run("E8_commit_observes_all_created_finals_no_staging_and_exact_created_rows", func(t *testing.T) {
		if commitObservationErr != nil {
			t.Fatal(commitObservationErr)
		}
	})

	t.Run("E18_committed_response_path_is_a_readable_regular_file", func(t *testing.T) {
		assertAtomicPublishedRows(t, f, response)
	})

	t.Run("E19_newly_published_path_is_invalidated", func(t *testing.T) {
		if got, want := f.invalidations(), []string{finalPath}; !reflect.DeepEqual(got, want) {
			t.Errorf("InvalidatePath calls = %q, want %q", got, want)
		}
	})

	t.Run("E20_no_file_is_written_at_audio_root_or_other_section", func(t *testing.T) {
		assertAtomicFilesConfined(t, f.root, SectionMusic, []string{finalPath})
	})
}

func TestAddAudio_ResponseOrderAndTwelveFileBatch_E2_E4_E18(t *testing.T) {
	files := atomicAlbumFiles(12)
	f := newAtomicFixture(t, files)
	order := []int{11, 0, 7, 2, 9, 1, 5, 3, 10, 4, 8, 6}
	requests := make([]AudioFileRequest, 0, len(order))
	for _, i := range order {
		requests = append(requests, AudioFileRequest{
			SourcePath: files[i].Path,
			Path:       atomicVirtualPath(i + 1),
		})
	}
	var batchCommitObservationErr error
	f.registry.onCommit = func(_ string, rows []metadb.AudioProjection) {
		want := make(map[string]FileStat, len(requests))
		for i, request := range requests {
			want[request.Path] = files[order[i]]
		}
		if len(rows) != len(want) {
			batchCommitObservationErr = fmt.Errorf("E8 rows at commit = %d, want all %d created projections", len(rows), len(want))
			return
		}
		for _, row := range rows {
			source, ok := want[row.VirtualPath]
			if !ok || row.Section != string(SectionMusic) || row.Hash != atomicAddHash ||
				row.SourcePath != source.Path || row.FileIndex != source.ID || row.Size != source.Length {
				batchCommitObservationErr = fmt.Errorf("E8 unexpected row at commit: %+v", row)
				return
			}
			delete(want, row.VirtualPath)
			finalPath := filepath.Join(f.musicRoot, filepath.FromSlash(row.VirtualPath))
			if info, err := os.Stat(finalPath); err != nil || !info.Mode().IsRegular() {
				batchCommitObservationErr = fmt.Errorf("E8 final %q before commit = (%v, %v), want regular file", finalPath, info, err)
				return
			}
		}
		if len(want) != 0 {
			batchCommitObservationErr = fmt.Errorf("E8 created projections absent from commit rows: %+v", want)
			return
		}
		staging, err := atomicDotLeadingPaths(f.musicRoot)
		if err != nil {
			batchCommitObservationErr = fmt.Errorf("E8 inspect batch staging leftovers: %w", err)
			return
		}
		if len(staging) != 0 {
			batchCommitObservationErr = fmt.Errorf("E8 batch staging leftovers before commit = %v, want none", staging)
		}
	}

	response, err := f.manager.AddAudio(context.Background(), AddRequest{
		Type:  "music",
		Hash:  atomicAddHash,
		Title: " \t Request Ordered Album \r\n",
		Files: requests,
	})
	if err != nil {
		t.Fatalf("AddAudio() error = %v, want nil", err)
	}
	if batchCommitObservationErr != nil {
		t.Fatal(batchCommitObservationErr)
	}
	if response == nil {
		t.Fatal("AddAudio() response = nil, want non-nil")
	}

	t.Run("E2_response_echoes_identity_and_file_fields_in_request_order", func(t *testing.T) {
		if response.Hash != atomicAddHash || response.Title != "Request Ordered Album" || response.Type != "music" {
			t.Errorf("response identity = hash %q, title %q, type %q", response.Hash, response.Title, response.Type)
		}
		if len(response.Files) != len(requests) {
			t.Fatalf("len(response.Files) = %d, want %d", len(response.Files), len(requests))
		}
		for i, request := range requests {
			source := files[order[i]]
			got := response.Files[i]
			if got.Path != request.Path || got.SourcePath != request.SourcePath || got.FileIndex != source.ID ||
				got.Size != source.Length || got.State != AudioProjectionCreated {
				t.Errorf("response.Files[%d] = %+v, want path/source %q/%q index %d size %d created", i, got, request.Path, request.SourcePath, source.ID, source.Length)
			}
		}
	})

	t.Run("E4_twelve_files_publish_and_commit_as_one_twelve_row_batch", func(t *testing.T) {
		stages := f.registry.callsFor("stage")
		commits := f.registry.callsFor("commit")
		if len(stages) != 1 || len(stages[0].rows) != 12 {
			t.Fatalf("stage calls = %#v, want one twelve-row batch", stages)
		}
		if len(commits) != 1 || len(commits[0].rows) != 12 {
			t.Fatalf("commit calls = %#v, want one twelve-row batch", commits)
		}
		for _, request := range requests {
			path := filepath.Join(f.musicRoot, filepath.FromSlash(request.Path))
			if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
				t.Errorf("published %q: info = %v, error = %v; want regular file", path, info, err)
			}
		}
	})

	t.Run("E8_commit_observes_every_batch_final_no_staging_and_exact_created_rows", func(t *testing.T) {
		if batchCommitObservationErr != nil {
			t.Fatal(batchCommitObservationErr)
		}
	})

	t.Run("E18_every_committed_row_and_response_entry_is_readable", func(t *testing.T) {
		assertAtomicPublishedRows(t, f, response)
	})
}

func TestAddAudio_PartlyPresentAlbum_E5_E19(t *testing.T) {
	files := atomicAlbumFiles(12)
	requests := make([]AudioFileRequest, len(files))
	var initial []metadb.AudioProjection
	for i, file := range files {
		requests[i] = AudioFileRequest{SourcePath: file.Path, Path: atomicVirtualPath(i + 1)}
		if i < 3 {
			initial = append(initial, atomicCommittedProjection(requests[i], file))
		}
	}
	f := newAtomicFixture(t, files, initial...)

	type identity struct {
		content []byte
		mode    os.FileMode
		mtimeNS int64
	}
	before := make(map[string]identity)
	for i := 0; i < 3; i++ {
		path := filepath.Join(f.musicRoot, filepath.FromSlash(requests[i].Path))
		if err := WriteAudioStub(path, "http://old.invalid/stream", files[i].Length, "magnet:?old", "", ""); err != nil {
			t.Fatalf("create existing stub %q: %v", path, err)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = identity{content: content, mode: info.Mode(), mtimeNS: info.ModTime().UnixNano()}
	}

	response, err := f.manager.AddAudio(context.Background(), AddRequest{
		Type: "music", Hash: atomicAddHash, Title: "Partial Album", Files: requests,
	})
	if err != nil {
		t.Fatalf("AddAudio() error = %v, want nil", err)
	}
	if response == nil {
		t.Fatal("AddAudio() response = nil, want non-nil")
	}

	t.Run("E5_three_present_nine_created_and_present_files_are_not_rewritten", func(t *testing.T) {
		if len(response.Files) != 12 {
			t.Fatalf("len(response.Files) = %d, want 12", len(response.Files))
		}
		for i, got := range response.Files {
			want := AudioProjectionCreated
			if i < 3 {
				want = AudioProjectionPresent
			}
			if got.State != want {
				t.Errorf("response.Files[%d].State = %q, want %q", i, got.State, want)
			}
		}
		stages := f.registry.callsFor("stage")
		if len(stages) != 1 || len(stages[0].rows) != 9 {
			t.Fatalf("stage calls = %#v, want exactly nine new rows", stages)
		}
		for path, old := range before {
			content, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("read present path %q: %v", path, err)
				continue
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Errorf("stat present path %q: %v", path, err)
				continue
			}
			if !reflect.DeepEqual(content, old.content) || info.Mode() != old.mode || info.ModTime().UnixNano() != old.mtimeNS {
				t.Errorf("present path %q identity changed: content/mode/mtime must be untouched", path)
			}
		}
	})

	t.Run("E19_only_the_nine_new_paths_are_invalidated", func(t *testing.T) {
		want := make([]string, 0, 9)
		for i := 3; i < 12; i++ {
			want = append(want, filepath.Join(f.musicRoot, filepath.FromSlash(requests[i].Path)))
		}
		got := f.invalidations()
		sort.Strings(got)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("InvalidatePath calls = %q, want new paths only %q", got, want)
		}
	})
}

func TestAddAudio_UnwritableDestinationRollsBack_E6_E9(t *testing.T) {
	files := atomicAlbumFiles(2)
	f := newAtomicFixture(t, files)
	blocked := filepath.Join(f.musicRoot, "Blocked")
	if err := os.Mkdir(blocked, 0500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(blocked, 0755) }()
	requests := []AudioFileRequest{
		{SourcePath: files[0].Path, Path: "Good/01_01234567.flac"},
		{SourcePath: files[1].Path, Path: "Blocked/02_01234567.flac"},
	}

	response, err := f.manager.AddAudio(context.Background(), AddRequest{
		Type: "music", Hash: atomicAddHash, Title: "Must Roll Back", Files: requests,
	})
	assertAtomicStatus(t, response, err, http.StatusInternalServerError)

	t.Run("E6_real_partial_write_failure_rolls_back_registry_and_all_new_finals", func(t *testing.T) {
		if len(f.registry.callsFor("stage")) != 1 || len(f.registry.callsFor("commit")) != 0 || len(f.registry.callsFor("rollback")) != 1 {
			t.Errorf("registry calls = %#v, want stage then rollback without commit", f.registry.allCalls())
		}
		for _, request := range requests {
			path := filepath.Join(f.musicRoot, filepath.FromSlash(request.Path))
			if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("final path %q survived failed request: stat error = %v", path, statErr)
			}
		}
		removes := f.engine.callsFor("RemoveTorrent")
		if len(removes) != 1 || removes[0].hash != atomicAddHash {
			t.Errorf("RemoveTorrent calls = %#v, want newly hydrated torrent %q dropped", removes, atomicAddHash)
		}
	})

	t.Run("E9_partial_failure_leaves_no_dot_leading_staging_file", func(t *testing.T) {
		assertNoAtomicStagingFiles(t, f.musicRoot)
	})
}

func TestAddAudio_FailedRequestPreservesPresentProjection_E7_E9(t *testing.T) {
	files := atomicAlbumFiles(3)
	presentRequest := AudioFileRequest{SourcePath: files[0].Path, Path: "Existing/01_01234567.flac"}
	present := atomicCommittedProjection(presentRequest, files[0])
	f := newAtomicFixture(t, files, present)
	f.engine.torrents = []TorrentStats{{Hash: atomicAddHash}}
	presentPath := filepath.Join(f.musicRoot, filepath.FromSlash(presentRequest.Path))
	if err := WriteAudioStub(presentPath, "http://old.invalid/existing", files[0].Length, "magnet:?existing", "", ""); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(presentPath)
	if err != nil {
		t.Fatal(err)
	}

	blocked := filepath.Join(f.musicRoot, "Blocked")
	if err := os.Mkdir(blocked, 0500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(blocked, 0755) }()
	requests := []AudioFileRequest{
		presentRequest,
		{SourcePath: files[1].Path, Path: "Good/02_01234567.flac"},
		{SourcePath: files[2].Path, Path: "Blocked/03_01234567.flac"},
	}

	response, addErr := f.manager.AddAudio(context.Background(), AddRequest{
		Type: "music", Hash: atomicAddHash, Title: "Preserve Existing", Files: requests,
	})
	assertAtomicStatus(t, response, addErr, http.StatusInternalServerError)

	t.Run("E7_rollback_does_not_damage_existing_file_or_registry_row", func(t *testing.T) {
		after, readErr := os.ReadFile(presentPath)
		if readErr != nil {
			t.Fatalf("read existing path after rollback: %v", readErr)
		}
		if !reflect.DeepEqual(after, before) {
			t.Errorf("existing path changed after failed request: got %q, want original %q", after, before)
		}
		row, found, lookupErr := f.registry.GetAudioProjection(string(SectionMusic), presentRequest.Path)
		if lookupErr != nil || !found || row.State != metadb.AudioCommitted || row.Hash != atomicAddHash {
			t.Errorf("existing registry row after rollback = (%+v, %v, %v), want original committed row", row, found, lookupErr)
		}
		stages := f.registry.callsFor("stage")
		if len(stages) != 1 || len(stages[0].rows) != 2 {
			t.Errorf("staged rows = %#v, want only two newly created projections", stages)
		}
		if removes := f.engine.callsFor("RemoveTorrent"); len(removes) != 0 {
			t.Errorf("RemoveTorrent calls = %#v, want none for already-running torrent", removes)
		}
	})

	t.Run("E9_failed_mixed_request_leaves_no_staged_files", func(t *testing.T) {
		assertNoAtomicStagingFiles(t, f.musicRoot)
	})
}

func TestAddAudio_NoReplacePublish_E21(t *testing.T) {
	t.Run("E21_unregistered_occupied_final_is_not_replaced_or_committed", func(t *testing.T) {
		file := FileStat{ID: 1, Path: "Release/Occupied.flac", Length: 12345}
		request := AudioFileRequest{SourcePath: file.Path, Path: "Artist/Album/Occupied_01234567.flac"}
		f := newAtomicFixture(t, []FileStat{file})
		finalPath := filepath.Join(f.musicRoot, filepath.FromSlash(request.Path))
		if err := os.MkdirAll(filepath.Dir(finalPath), 0755); err != nil {
			t.Fatal(err)
		}
		original := []byte("unregistered file owned by someone else\n")
		if err := os.WriteFile(finalPath, original, 0644); err != nil {
			t.Fatal(err)
		}

		response, err := f.manager.AddAudio(context.Background(), AddRequest{
			Type: "music", Hash: atomicAddHash, Title: "No Replace", Files: []AudioFileRequest{request},
		})
		assertAtomicFailure(t, response, err)

		after, readErr := os.ReadFile(finalPath)
		if readErr != nil {
			t.Fatalf("read occupied final after failed add: %v", readErr)
		}
		if !reflect.DeepEqual(after, original) {
			t.Errorf("occupied final content = %q, want original bytes %q", after, original)
		}
		if commits := f.registry.callsFor("commit"); len(commits) != 0 {
			t.Errorf("commit calls = %#v, want none when final name is occupied", commits)
		}
		stages := f.registry.callsFor("stage")
		rollbacks := f.registry.callsFor("rollback")
		if len(stages) > 0 && (len(rollbacks) != 1 || rollbacks[0].txnID != stages[0].txnID) {
			t.Errorf("rollback calls = %#v after stages %#v, want any staged transaction rolled back", rollbacks, stages)
		}
		if rows := f.registry.stagedRows(); len(rows) != 0 {
			t.Errorf("staged rows after failure = %+v, want none", rows)
		}
		if rows := f.registry.committedRows(); len(rows) != 0 {
			t.Errorf("committed rows = %+v, want none", rows)
		}
		assertNoAtomicStagingFiles(t, f.musicRoot)
	})
}

func TestAddAudio_RenameFailureRollsBackCreatedAndPreservesPresent_E22_E23(t *testing.T) {
	files := atomicAlbumFiles(3)
	presentRequest := AudioFileRequest{SourcePath: files[0].Path, Path: "Existing/01_01234567.flac"}
	present := atomicCommittedProjection(presentRequest, files[0])
	f := newAtomicFixture(t, files, present)
	f.engine.torrents = []TorrentStats{{Hash: atomicAddHash}}

	presentPath := filepath.Join(f.musicRoot, filepath.FromSlash(presentRequest.Path))
	if err := WriteAudioStub(presentPath, "http://old.invalid/existing", files[0].Length, "magnet:?existing", "", ""); err != nil {
		t.Fatal(err)
	}
	presentBefore, err := os.ReadFile(presentPath)
	if err != nil {
		t.Fatal(err)
	}

	goodRequest := AudioFileRequest{SourcePath: files[1].Path, Path: "Publish/02_01234567.flac"}
	failingRequest := AudioFileRequest{SourcePath: files[2].Path, Path: "Publish/03_01234567.flac"}
	failingFinal := filepath.Join(f.musicRoot, filepath.FromSlash(failingRequest.Path))
	if err := os.MkdirAll(filepath.Dir(failingFinal), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(failingFinal, 0500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(failingFinal, 0755) }()

	response, addErr := f.manager.AddAudio(context.Background(), AddRequest{
		Type:  "music",
		Hash:  atomicAddHash,
		Title: "Rename Rollback",
		Files: []AudioFileRequest{presentRequest, goodRequest, failingRequest},
	})
	assertAtomicFailure(t, response, addErr)

	t.Run("E22_second_rename_failure_removes_renamed_finals_and_staging_then_rolls_back_rows", func(t *testing.T) {
		stages := f.registry.callsFor("stage")
		if len(stages) != 1 || len(stages[0].rows) != 2 {
			t.Fatalf("stage calls = %#v, want one two-row created batch", stages)
		}
		if commits := f.registry.callsFor("commit"); len(commits) != 0 {
			t.Errorf("commit calls = %#v, want none after pre-commit rename failure", commits)
		}
		rollbacks := f.registry.callsFor("rollback")
		if len(rollbacks) != 1 || rollbacks[0].txnID != stages[0].txnID {
			t.Errorf("rollback calls = %#v, want staged transaction %q rolled back once", rollbacks, stages[0].txnID)
		}

		goodFinal := filepath.Join(f.musicRoot, filepath.FromSlash(goodRequest.Path))
		if _, statErr := os.Stat(goodFinal); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("first renamed final %q survived rollback: stat error = %v", goodFinal, statErr)
		}
		if info, statErr := os.Stat(failingFinal); statErr != nil || !info.IsDir() {
			t.Errorf("rename-failure fixture = (%v, %v), want original directory preserved", info, statErr)
		}
		assertNoAtomicStagingFiles(t, f.musicRoot)
	})

	t.Run("E23_rename_rollback_preserves_already_present_file_and_registry_row", func(t *testing.T) {
		presentAfter, readErr := os.ReadFile(presentPath)
		if readErr != nil {
			t.Fatalf("read already-present file after rollback: %v", readErr)
		}
		if !reflect.DeepEqual(presentAfter, presentBefore) {
			t.Errorf("already-present content changed: got %q, want %q", presentAfter, presentBefore)
		}
		rows := f.registry.committedRows()
		if len(rows) != 1 || rows[0].VirtualPath != presentRequest.Path || rows[0].Hash != atomicAddHash || rows[0].State != metadb.AudioCommitted {
			t.Errorf("committed rows after rename rollback = %+v, want only original present row", rows)
		}
	})
}

func TestAddAudio_RegistryFailures_E10_E11(t *testing.T) {
	files := atomicAlbumFiles(2)
	request := AddRequest{
		Type: "music", Hash: atomicAddHash, Title: "Registry Failure",
		Files: []AudioFileRequest{
			{SourcePath: files[0].Path, Path: atomicVirtualPath(1)},
			{SourcePath: files[1].Path, Path: atomicVirtualPath(2)},
		},
	}

	t.Run("E10_staging_conflict_is_409_before_any_filesystem_write", func(t *testing.T) {
		f := newAtomicFixture(t, files)
		f.registry.stageErr = fmt.Errorf("late uniqueness race: %w", metadb.ErrAudioPathConflict)

		response, err := f.manager.AddAudio(context.Background(), request)
		assertAtomicStatus(t, response, err, http.StatusConflict)
		if len(f.registry.callsFor("stage")) != 1 || len(f.registry.callsFor("commit")) != 0 {
			t.Errorf("registry calls = %#v, want one failed stage and no commit", f.registry.allCalls())
		}
		assertNoAtomicRegularFiles(t, f.root)
	})

	t.Run("E11_commit_failure_rolls_back_rows_and_publishes_nothing", func(t *testing.T) {
		f := newAtomicFixture(t, files)
		f.registry.commitErr = errors.New("registry storage unavailable")

		response, err := f.manager.AddAudio(context.Background(), request)
		assertAtomicStatus(t, response, err, http.StatusInternalServerError)
		if len(f.registry.callsFor("stage")) != 1 || len(f.registry.callsFor("commit")) != 1 || len(f.registry.callsFor("rollback")) != 1 {
			t.Errorf("registry calls = %#v, want stage, failed commit, rollback", f.registry.allCalls())
		}
		assertNoAtomicRegularFiles(t, f.root)
		assertNoAtomicStagingFiles(t, f.musicRoot)
		if got := f.registry.committedRows(); len(got) != 0 {
			t.Errorf("committed rows after failed commit = %+v, want none", got)
		}
	})
}

func TestAddAudio_RejectsInvalidProjectionPaths_E12(t *testing.T) {
	file := FileStat{ID: 1, Path: "Release/Track.flac", Length: 1000}
	tests := []struct {
		name string
		path string
	}{
		{name: "dot_dot", path: "../Track_01234567.flac"},
		{name: "absolute", path: "/Artist/Track_01234567.flac"},
		{name: "bad_extension", path: "Artist/Track_01234567.mp3"},
		{name: "missing_hash8", path: "Artist/Track.flac"},
	}
	for _, tt := range tests {
		t.Run("E12_"+tt.name+"_is_400_before_stage_or_write", func(t *testing.T) {
			f := newAtomicFixture(t, []FileStat{file})
			response, err := f.manager.AddAudio(context.Background(), AddRequest{
				Type: "music", Hash: atomicAddHash, Title: "Invalid Path",
				Files: []AudioFileRequest{{SourcePath: file.Path, Path: tt.path}},
			})
			assertAtomicStatus(t, response, err, http.StatusBadRequest)
			if len(f.registry.callsFor("stage")) != 0 {
				t.Errorf("stage called for invalid path: %#v", f.registry.allCalls())
			}
			assertNoAtomicRegularFiles(t, f.root)
		})
	}
}

func TestAddAudio_UsesEngineReportedHashForSuffix_E13(t *testing.T) {
	t.Run("E13_base32_magnet_accepts_engine_hex_hash8_suffix", func(t *testing.T) {
		file := FileStat{ID: 5, Path: "Base32 Release/Track.flac", Length: 5000}
		f := newAtomicFixture(t, []FileStat{file})
		magnet := "magnet:?xt=urn:btih:" + atomicAddBase32Hash + "&dn=Base32"
		path := "Artist/Base32/Track_01234567.flac"

		response, err := f.manager.AddAudio(context.Background(), AddRequest{
			Type: "music", Magnet: magnet, Title: "Base32", Files: []AudioFileRequest{{SourcePath: file.Path, Path: path}},
		})
		if err != nil {
			t.Fatalf("E13 AddAudio() error = %v, want suffix validated against engine hex hash", err)
		}
		if response == nil {
			t.Fatal("E13 AddAudio() response = nil, want non-nil")
		}
		if response.Hash != atomicAddHash {
			t.Errorf("E13 response Hash = %q, want engine-reported %q", response.Hash, atomicAddHash)
		}
		stages := f.registry.callsFor("stage")
		if len(stages) != 1 || len(stages[0].rows) != 1 || stages[0].rows[0].Hash != atomicAddHash {
			t.Errorf("E13 staged rows = %#v, want engine-reported hex hash", stages)
		}
		if _, err := os.Stat(filepath.Join(f.musicRoot, filepath.FromSlash(path))); err != nil {
			t.Errorf("E13 final path missing: %v", err)
		}
	})
}

func TestAddAudio_UnresolvableSource_E14(t *testing.T) {
	t.Run("E14_unresolvable_source_is_422_without_stage_or_write", func(t *testing.T) {
		file := FileStat{ID: 1, Path: "Release/Present.flac", Length: 1000}
		f := newAtomicFixture(t, []FileStat{file})
		response, err := f.manager.AddAudio(context.Background(), AddRequest{
			Type: "music", Hash: atomicAddHash, Title: "Missing Source",
			Files: []AudioFileRequest{{SourcePath: "Release/Missing.flac", Path: atomicVirtualPath(1)}},
		})
		assertAtomicStatus(t, response, err, http.StatusUnprocessableEntity)
		if len(f.registry.callsFor("stage")) != 0 {
			t.Errorf("E14 stage calls = %#v, want none", f.registry.allCalls())
		}
		assertNoAtomicRegularFiles(t, f.root)
	})
}

func TestAddAudio_ClassificationConflict_E15(t *testing.T) {
	t.Run("E15_classification_path_conflict_is_409_without_stage_or_write", func(t *testing.T) {
		file := FileStat{ID: 4, Path: "Release/Track.flac", Length: 4000}
		request := AudioFileRequest{SourcePath: file.Path, Path: atomicVirtualPath(1)}
		conflict := atomicCommittedProjection(request, FileStat{ID: 99, Path: "Other/Track.flac", Length: 9999})
		conflict.Hash = strings.Repeat("f", 40)
		f := newAtomicFixture(t, []FileStat{file}, conflict)

		response, err := f.manager.AddAudio(context.Background(), AddRequest{
			Type: "music", Hash: atomicAddHash, Title: "Conflict", Files: []AudioFileRequest{request},
		})
		assertAtomicStatus(t, response, err, http.StatusConflict)
		if len(f.registry.callsFor("stage")) != 0 {
			t.Errorf("E15 stage calls = %#v, want none after classification conflict", f.registry.allCalls())
		}
		assertNoAtomicRegularFiles(t, f.root)
	})
}

func TestAddAudio_VideoIsNotMisreportedAsAudioClientError_E16(t *testing.T) {
	t.Run("E16_valid_movie_is_distinguishable_from_a_400_audio_failure", func(t *testing.T) {
		f := newAtomicFixture(t, nil)
		response, err := f.manager.AddAudio(context.Background(), AddRequest{
			Type: "movie", Hash: atomicAddHash, Title: "A Valid Movie",
		})
		if response != nil {
			t.Errorf("E16 AddAudio(movie) response = %#v, want no audio response", response)
		}
		if err == nil {
			t.Fatal("E16 AddAudio(movie) error = nil; video is not handled by AddAudio")
		}
		var classified *Error
		if !errors.As(err, &classified) {
			t.Fatalf("E16 AddAudio(movie) error type = %T, want distinguishable *library.Error status", err)
		}
		if classified.Status == http.StatusBadRequest {
			t.Errorf("E16 AddAudio(movie) status = 400, misreporting a valid video request as an invalid audio request: %v", err)
		}
		if calls := f.engine.callsFor("AddTorrent"); len(calls) != 0 {
			t.Errorf("E16 audio path touched GoStorm for video request: %#v", calls)
		}
		if calls := f.registry.allCalls(); len(calls) != 0 {
			t.Errorf("E16 audio path touched projection registry for video request: %#v", calls)
		}
	})
}

func TestAddAudio_MetadataTimeoutAndTorrentOwnership_E17(t *testing.T) {
	tests := []struct {
		name         string
		preexisting  bool
		wantRemovals int
	}{
		{name: "newly_hydrated_torrent_is_dropped", wantRemovals: 1},
		{name: "already_running_torrent_is_not_dropped", preexisting: true, wantRemovals: 0},
	}
	for _, tt := range tests {
		t.Run("E17_metadata_never_arrives_504_"+tt.name, func(t *testing.T) {
			f := newAtomicFixture(t, nil)
			f.engine.info = nil
			f.engine.infoErr = context.DeadlineExceeded
			if tt.preexisting {
				f.engine.torrents = []TorrentStats{{Hash: atomicAddHash}}
			}

			response, err := f.manager.AddAudio(context.Background(), AddRequest{
				Type: "music", Hash: atomicAddHash, Title: "No Metadata",
				Files: []AudioFileRequest{{SourcePath: "Release/Track.flac", Path: atomicVirtualPath(1)}},
			})
			assertAtomicStatus(t, response, err, http.StatusGatewayTimeout)
			if len(f.registry.callsFor("stage")) != 0 {
				t.Errorf("stage calls = %#v, want none without metadata", f.registry.allCalls())
			}
			assertNoAtomicRegularFiles(t, f.root)
			if got := len(f.engine.callsFor("RemoveTorrent")); got != tt.wantRemovals {
				t.Errorf("RemoveTorrent call count = %d, want %d", got, tt.wantRemovals)
			}
		})
	}
}

type atomicStub struct {
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	Magnet string `json:"magnet"`
}

func readAtomicStub(t *testing.T, path string) atomicStub {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	var stub atomicStub
	if err := json.Unmarshal(data, &stub); err != nil {
		t.Fatalf("Unmarshal stub %q: %v; content = %q", path, err, data)
	}
	return stub
}

func atomicAlbumFiles(count int) []FileStat {
	files := make([]FileStat, count)
	for i := range files {
		files[i] = FileStat{
			ID:     101 + i,
			Path:   fmt.Sprintf("Release/Disc 1/%02d - Source.flac", i+1),
			Length: int64(10_000 + i),
		}
	}
	return files
}

func atomicVirtualPath(track int) string {
	return fmt.Sprintf("Artist/Album/%02d - Track_01234567.flac", track)
}

func atomicCommittedProjection(request AudioFileRequest, file FileStat) metadb.AudioProjection {
	return metadb.AudioProjection{
		Section:         string(SectionMusic),
		VirtualPath:     request.Path,
		PortablePathKey: PortablePathKey(request.Path),
		Hash:            atomicAddHash,
		FileIndex:       file.ID,
		SourcePath:      request.SourcePath,
		Size:            file.Length,
		Title:           "Existing",
		Magnet:          "magnet:?existing",
		State:           metadb.AudioCommitted,
	}
}

func assertAtomicStatus(t *testing.T, response *AudioAddResponse, err error, want int) {
	t.Helper()
	if response != nil {
		t.Errorf("AddAudio() response = %#v on failure, want nil", response)
	}
	if err == nil {
		t.Fatalf("AddAudio() error = nil, want status %d", want)
	}
	var classified *Error
	if !errors.As(err, &classified) {
		t.Fatalf("AddAudio() error type = %T (%v), want *library.Error", err, err)
	}
	if classified.Status != want {
		t.Errorf("AddAudio() status = %d, want %d; error = %v", classified.Status, want, err)
	}
}

func assertAtomicFailure(t *testing.T, response *AudioAddResponse, err error) {
	t.Helper()
	if response != nil {
		t.Errorf("AddAudio() response = %#v on failure, want nil", response)
	}
	if err == nil {
		t.Fatal("AddAudio() error = nil, want failure")
	}
	var classified *Error
	if !errors.As(err, &classified) {
		t.Fatalf("AddAudio() error type = %T (%v), want *library.Error", err, err)
	}
}

func assertAtomicPublishedRows(t *testing.T, f *atomicFixture, response *AudioAddResponse) {
	t.Helper()
	rows := f.registry.committedRows()
	if len(rows) != len(response.Files) {
		t.Fatalf("committed rows = %d, response files = %d", len(rows), len(response.Files))
	}
	responsePaths := make(map[string]bool, len(response.Files))
	for _, file := range response.Files {
		responsePaths[file.Path] = true
		path := filepath.Join(f.musicRoot, filepath.FromSlash(file.Path))
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("response path %q is absent: %v", path, err)
			continue
		}
		if !info.Mode().IsRegular() {
			t.Errorf("response path %q mode = %v, want regular file", path, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("response path %q is not readable: %v", path, err)
		}
		if len(data) == 0 {
			t.Errorf("response path %q is empty", path)
		}
	}
	for _, row := range rows {
		if !responsePaths[row.VirtualPath] {
			t.Errorf("committed row %q absent from response", row.VirtualPath)
		}
		path := filepath.Join(f.root, row.Section, filepath.FromSlash(row.VirtualPath))
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			t.Errorf("committed row %q has unusable final path: info = %v, error = %v", row.VirtualPath, info, err)
		}
	}
}

func assertAtomicFilesConfined(t *testing.T, root string, section Section, want []string) {
	t.Helper()
	got, err := atomicRegularFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want = append([]string(nil), want...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("regular files below AudioRoot = %q, want only %q", got, want)
	}
	for _, path := range got {
		rel, err := filepath.Rel(filepath.Join(root, string(section)), path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("file %q escapes AudioRoot/%s", path, section)
		}
	}
}

func assertNoAtomicRegularFiles(t *testing.T, root string) {
	t.Helper()
	files, err := atomicRegularFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("regular files below %q = %q, want none", root, files)
	}
}

func assertNoAtomicStagingFiles(t *testing.T, root string) {
	t.Helper()
	paths, err := atomicDotLeadingPaths(root)
	if err != nil {
		t.Fatalf("walk %q: %v", root, err)
	}
	for _, path := range paths {
		t.Errorf("staging leftover after rollback: %q", path)
	}
}

func atomicDotLeadingPaths(root string) ([]string, error) {
	var paths []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path != root && strings.HasPrefix(filepath.Base(path), ".") {
			paths = append(paths, path)
		}
		return nil
	})
	return paths, err
}

func atomicRegularFiles(root string) ([]string, error) {
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}
