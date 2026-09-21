package library

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"tiramisu/internal/metadb"
)

const livePubHash = "fedcba9876543210fedcba9876543210fedcba98"

// livePubEvent is one observable side effect of AddAudio, recorded in the order
// it happened.
type livePubEvent struct {
	kind string // "publish" or "invalidate"
	path AudioPath
	file string // the physical path, for invalidate
}

type livePubEngine struct{ info *TorrentStats }

func (e *livePubEngine) AddTorrent(context.Context, string, string) (string, error) {
	return livePubHash, nil
}

func (e *livePubEngine) GetTorrentInfo(context.Context, string, int) (*TorrentStats, error) {
	return e.info, nil
}
func (e *livePubEngine) RemoveTorrent(context.Context, string) error { return nil }
func (e *livePubEngine) ListTorrents(context.Context) ([]TorrentStats, error) {
	return nil, nil
}

// livePubRegistry is a minimal in-memory projection registry. onCommit runs
// inside CommitAudioProjections before the batch is committed.
type livePubRegistry struct {
	mu        sync.Mutex
	rows      map[string]metadb.AudioProjection
	staged    map[string][]metadb.AudioProjection
	stageErr  error
	commitErr error
	onCommit  func()
	commits   int
}

var _ AudioProjectionRegistry = (*livePubRegistry)(nil)

func livePubKey(section, path string) string { return section + "\x00" + path }

func newLivePubRegistry(initial ...metadb.AudioProjection) *livePubRegistry {
	r := &livePubRegistry{
		rows:   map[string]metadb.AudioProjection{},
		staged: map[string][]metadb.AudioProjection{},
	}
	for _, row := range initial {
		r.rows[livePubKey(row.Section, row.VirtualPath)] = row
	}
	return r
}

func (r *livePubRegistry) GetAudioProjection(section, path string) (*metadb.AudioProjection, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[livePubKey(section, path)]
	if !ok {
		return nil, false, nil
	}
	return &row, true, nil
}

func (r *livePubRegistry) AudioProjectionBySource(hash string, idx int) (*metadb.AudioProjection, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range r.rows {
		if row.Hash == hash && row.FileIndex == idx {
			row := row
			return &row, true, nil
		}
	}
	return nil, false, nil
}

func (r *livePubRegistry) AudioProjectionByPortableKey(section, key string) (*metadb.AudioProjection, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range r.rows {
		if row.Section == section && row.PortablePathKey == key {
			row := row
			return &row, true, nil
		}
	}
	return nil, false, nil
}

func (r *livePubRegistry) StageAudioProjections(txn string, rows []metadb.AudioProjection) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stageErr != nil {
		return r.stageErr
	}
	r.staged[txn] = append([]metadb.AudioProjection(nil), rows...)
	return nil
}

func (r *livePubRegistry) CommitAudioProjections(txn string, _ int64) (int, error) {
	r.mu.Lock()
	rows := append([]metadb.AudioProjection(nil), r.staged[txn]...)
	hook, commitErr := r.onCommit, r.commitErr
	r.commits++
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	if commitErr != nil {
		return 0, commitErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range rows {
		row.State = metadb.AudioCommitted
		r.rows[livePubKey(row.Section, row.VirtualPath)] = row
	}
	delete(r.staged, txn)
	return len(rows), nil
}

func (r *livePubRegistry) RollbackAudioProjections(txn string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.staged[txn])
	delete(r.staged, txn)
	return n, nil
}

func (r *livePubRegistry) AudioHashReferenced(hash string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range r.rows {
		if row.Hash == hash {
			return true, nil
		}
	}
	return false, nil
}

func (r *livePubRegistry) commitCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.commits
}

type livePubFixture struct {
	root     string
	registry *livePubRegistry
	manager  *Manager

	mu     sync.Mutex
	events []livePubEvent
}

// livePubOptions selects which hooks the manager under test is given.
type livePubOptions struct {
	noPublisher  bool
	noInvalidate bool
	onPublish    func(AudioPath) // runs inside the publisher, before it records
}

func newLivePubFixture(t *testing.T, files []FileStat, opts livePubOptions, initial ...metadb.AudioProjection) *livePubFixture {
	t.Helper()
	root := t.TempDir()
	for _, sec := range []Section{SectionMusic, SectionAudiobooks, SectionMovies, SectionTV} {
		if err := os.Mkdir(filepath.Join(root, string(sec)), 0755); err != nil {
			t.Fatal(err)
		}
	}
	registry := newLivePubRegistry(initial...)
	f := &livePubFixture{root: root, registry: registry}
	cfg := Config{
		MoviesDir:        filepath.Join(root, string(SectionMovies)),
		TVDir:            filepath.Join(root, string(SectionTV)),
		GoStormURL:       "http://gostorm.invalid",
		GoStorm:          &livePubEngine{info: &TorrentStats{Hash: livePubHash, FileStats: append([]FileStat(nil), files...)}},
		AudioRoot:        root,
		AudioRegistry:    registry,
		AudioProjections: registry,
		Logger:           log.New(io.Discard, "", 0),
	}
	if !opts.noPublisher {
		cfg.PublishAudioPath = func(p AudioPath) {
			if opts.onPublish != nil {
				opts.onPublish(p)
			}
			f.record(livePubEvent{kind: "publish", path: p})
		}
	}
	if !opts.noInvalidate {
		cfg.InvalidatePath = func(path string) {
			f.record(livePubEvent{kind: "invalidate", file: path})
		}
	}
	f.manager = New(cfg)
	return f
}

func (f *livePubFixture) record(e livePubEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
}

func (f *livePubFixture) all() []livePubEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]livePubEvent(nil), f.events...)
}

func (f *livePubFixture) published() []AudioPath {
	var out []AudioPath
	for _, e := range f.all() {
		if e.kind == "publish" {
			out = append(out, e.path)
		}
	}
	return out
}

func (f *livePubFixture) invalidated() []string {
	var out []string
	for _, e := range f.all() {
		if e.kind == "invalidate" {
			out = append(out, e.file)
		}
	}
	return out
}

func (f *livePubFixture) finalPath(section Section, virtualPath string) string {
	return filepath.Join(f.root, string(section), filepath.FromSlash(virtualPath))
}

func livePubFiles(n int) []FileStat {
	files := make([]FileStat, n)
	for i := range files {
		files[i] = FileStat{ID: 11 + i, Path: fmt.Sprintf("Rel/%02d - Src.flac", i+1), Length: int64(5000 + i)}
	}
	return files
}

func livePubVPath(track int) string {
	return fmt.Sprintf("Some Artist/Some Album/%02d - Track_fedcba98.flac", track)
}

func livePubRequest(files []FileStat, tracks ...int) AddRequest {
	req := AddRequest{Type: "music", Hash: livePubHash, Title: "Live Album"}
	for _, n := range tracks {
		req.Files = append(req.Files, AudioFileRequest{SourcePath: files[n-1].Path, Path: livePubVPath(n)})
	}
	return req
}

func livePubCommitted(section Section, path string, file FileStat) metadb.AudioProjection {
	return metadb.AudioProjection{
		Section:         string(section),
		VirtualPath:     path,
		PortablePathKey: PortablePathKey(path),
		Hash:            livePubHash,
		FileIndex:       file.ID,
		SourcePath:      file.Path,
		Size:            file.Length,
		Title:           "Existing",
		Magnet:          "magnet:?existing",
		State:           metadb.AudioCommitted,
	}
}

func TestAddAudioLivePublication_P1_SinglePublishWithSectionAndStoredPath(t *testing.T) {
	cases := []struct {
		name    string
		apiType string
		section Section
		file    FileStat
		path    string
	}{
		{"music_section_and_caller_spelling_preserved", "music", SectionMusic,
			FileStat{ID: 3, Path: "Rel/Disc/01.flac", Length: 1234},
			"Mixed CASE Artist/Album (2020)/01 - Track Name_fedcba98.flac"},
		{"audiobook_section_is_passed_through", "audiobook", SectionAudiobooks,
			FileStat{ID: 4, Path: "Rel/book.m4b", Length: 99999},
			"Author/Title/Title_fedcba98.m4b"},
	}
	for _, tc := range cases {
		t.Run("P1_"+tc.name, func(t *testing.T) {
			f := newLivePubFixture(t, []FileStat{tc.file}, livePubOptions{})
			_, err := f.manager.AddAudio(context.Background(), AddRequest{
				Type: tc.apiType, Hash: livePubHash, Title: "T",
				Files: []AudioFileRequest{{SourcePath: tc.file.Path, Path: tc.path}},
			})
			if err != nil {
				t.Fatalf("AddAudio() error = %v", err)
			}
			want := []AudioPath{{Section: tc.section, VirtualPath: tc.path}}
			if got := f.published(); !reflect.DeepEqual(got, want) {
				t.Errorf("published = %#v, want exactly %#v", got, want)
			}
		})
	}
}

func TestAddAudioLivePublication_P2_MultiFilePublishesEachOnceInRequestOrder(t *testing.T) {
	files := livePubFiles(6)
	order := []int{5, 1, 6, 3, 2}
	f := newLivePubFixture(t, files, livePubOptions{})
	if _, err := f.manager.AddAudio(context.Background(), livePubRequest(files, order...)); err != nil {
		t.Fatalf("AddAudio() error = %v", err)
	}
	var want []AudioPath
	for _, n := range order {
		want = append(want, AudioPath{Section: SectionMusic, VirtualPath: livePubVPath(n)})
	}
	t.Run("P2_every_created_projection_published_once_in_request_order", func(t *testing.T) {
		if got := f.published(); !reflect.DeepEqual(got, want) {
			t.Errorf("published = %#v, want %#v", got, want)
		}
	})
}

func TestAddAudioLivePublication_P3_AlreadyPresentIsNotRepublished(t *testing.T) {
	files := livePubFiles(5)
	// Tracks 1 and 4 already exist; 2, 3 and 5 are new, interleaved with them.
	initial := []metadb.AudioProjection{
		livePubCommitted(SectionMusic, livePubVPath(1), files[0]),
		livePubCommitted(SectionMusic, livePubVPath(4), files[3]),
	}
	t.Run("P3_mixed_request_publishes_only_the_created_ones", func(t *testing.T) {
		f := newLivePubFixture(t, files, livePubOptions{}, initial...)
		resp, err := f.manager.AddAudio(context.Background(), livePubRequest(files, 1, 2, 3, 4, 5))
		if err != nil {
			t.Fatalf("AddAudio() error = %v", err)
		}
		var present int
		for _, file := range resp.Files {
			if file.Status == AudioProjectionPresent {
				present++
			}
		}
		if present != 2 {
			t.Fatalf("fixture invalid: %d present in response, want 2", present)
		}
		want := []AudioPath{
			{Section: SectionMusic, VirtualPath: livePubVPath(2)},
			{Section: SectionMusic, VirtualPath: livePubVPath(3)},
			{Section: SectionMusic, VirtualPath: livePubVPath(5)},
		}
		if got := f.published(); !reflect.DeepEqual(got, want) {
			t.Errorf("published = %#v, want only the created %#v", got, want)
		}
	})

	t.Run("P3_fully_present_request_publishes_nothing", func(t *testing.T) {
		f := newLivePubFixture(t, files, livePubOptions{}, initial...)
		if _, err := f.manager.AddAudio(context.Background(), livePubRequest(files, 1, 4)); err != nil {
			t.Fatalf("AddAudio() error = %v", err)
		}
		if got := f.published(); len(got) != 0 {
			t.Errorf("published = %#v, want none for an all-present request", got)
		}
		if got := f.invalidated(); len(got) != 0 {
			t.Errorf("invalidated = %q, want none for an all-present request", got)
		}
	})
}

func TestAddAudioLivePublication_P4_NothingPublishedBeforeRegistryCommit(t *testing.T) {
	files := livePubFiles(3)
	f := newLivePubFixture(t, files, livePubOptions{})
	var atCommit []AudioPath
	commitSeen := false
	f.registry.onCommit = func() {
		commitSeen = true
		atCommit = f.published()
	}
	if _, err := f.manager.AddAudio(context.Background(), livePubRequest(files, 1, 2, 3)); err != nil {
		t.Fatalf("AddAudio() error = %v", err)
	}
	t.Run("P4_no_path_is_published_while_the_commit_is_in_flight", func(t *testing.T) {
		if !commitSeen {
			t.Fatal("registry commit was never observed")
		}
		if len(atCommit) != 0 {
			t.Errorf("published at commit time = %#v, want none", atCommit)
		}
	})
	t.Run("P4_publication_happens_after_the_commit", func(t *testing.T) {
		if got := len(f.published()); got != 3 {
			t.Errorf("published after add = %d, want 3", got)
		}
	})
}

func TestAddAudioLivePublication_P5_PublishedOnlyAfterFinalFileExists(t *testing.T) {
	files := livePubFiles(3)
	var f *livePubFixture
	var problems []string
	f = newLivePubFixture(t, files, livePubOptions{
		onPublish: func(p AudioPath) {
			final := f.finalPath(p.Section, p.VirtualPath)
			info, err := os.Stat(final)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: final file missing at publish time: %v", p.VirtualPath, err))
				return
			}
			if !info.Mode().IsRegular() {
				problems = append(problems, fmt.Sprintf("%s: final path is not a regular file", p.VirtualPath))
			}
		},
	})
	if _, err := f.manager.AddAudio(context.Background(), livePubRequest(files, 1, 2, 3)); err != nil {
		t.Fatalf("AddAudio() error = %v", err)
	}
	t.Run("P5_final_file_exists_when_each_path_is_published", func(t *testing.T) {
		if got := len(f.published()); got != 3 {
			t.Fatalf("published = %d, want 3 (hook must have fired)", got)
		}
		for _, p := range problems {
			t.Error(p)
		}
	})
}

func TestAddAudioLivePublication_P6_FailedAddPublishesNothing(t *testing.T) {
	files := livePubFiles(3)
	cases := []struct {
		name  string
		setup func(t *testing.T, f *livePubFixture)
		req   func() AddRequest
	}{
		{
			name: "staging_conflict",
			setup: func(t *testing.T, f *livePubFixture) {
				f.registry.stageErr = fmt.Errorf("late race: %w", metadb.ErrAudioPathConflict)
			},
			req: func() AddRequest { return livePubRequest(files, 1, 2) },
		},
		{
			name: "unresolvable_source",
			req: func() AddRequest {
				r := livePubRequest(files, 1)
				r.Files = append(r.Files, AudioFileRequest{SourcePath: "Rel/Missing.flac", Path: livePubVPath(2)})
				return r
			},
		},
		{
			name: "stub_write_failure_on_second_file",
			setup: func(t *testing.T, f *livePubFixture) {
				// A regular file where a directory is needed makes the write fail
				// regardless of the user's privileges.
				blocker := filepath.Join(f.root, string(SectionMusic), "Blocked")
				if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
					t.Fatal(err)
				}
			},
			req: func() AddRequest {
				r := livePubRequest(files, 1)
				r.Files = append(r.Files, AudioFileRequest{SourcePath: files[1].Path, Path: "Blocked/02 - Track_fedcba98.flac"})
				return r
			},
		},
		{
			name: "registry_commit_failure",
			setup: func(t *testing.T, f *livePubFixture) {
				f.registry.commitErr = errors.New("disk full")
			},
			req: func() AddRequest { return livePubRequest(files, 1, 2) },
		},
	}
	for _, tc := range cases {
		t.Run("P6_"+tc.name+"_publishes_nothing", func(t *testing.T) {
			f := newLivePubFixture(t, files, livePubOptions{})
			if tc.setup != nil {
				tc.setup(t, f)
			}
			resp, err := f.manager.AddAudio(context.Background(), tc.req())
			if err == nil {
				t.Fatalf("AddAudio() = %#v, want an error (fixture must actually fail)", resp)
			}
			if got := f.published(); len(got) != 0 {
				t.Errorf("published = %#v after a failed add, want none", got)
			}
		})
	}
}

func TestAddAudioLivePublication_P7_NilPublisherIsTolerated(t *testing.T) {
	files := livePubFiles(2)
	cases := []struct {
		name string
		opts livePubOptions
	}{
		{"no_publisher_with_invalidate", livePubOptions{noPublisher: true}},
		{"no_publisher_and_no_invalidate", livePubOptions{noPublisher: true, noInvalidate: true}},
	}
	for _, tc := range cases {
		t.Run("P7_"+tc.name+"_add_still_succeeds", func(t *testing.T) {
			f := newLivePubFixture(t, files, tc.opts)
			resp, err := f.manager.AddAudio(context.Background(), livePubRequest(files, 1, 2))
			if err != nil {
				t.Fatalf("AddAudio() error = %v, want success without a publisher", err)
			}
			if len(resp.Files) != 2 {
				t.Fatalf("response files = %d, want 2", len(resp.Files))
			}
			for _, n := range []int{1, 2} {
				if _, statErr := os.Stat(f.finalPath(SectionMusic, livePubVPath(n))); statErr != nil {
					t.Errorf("track %d not on disk: %v", n, statErr)
				}
			}
			if !tc.opts.noInvalidate && len(f.invalidated()) != 2 {
				t.Errorf("invalidated = %q, want both paths", f.invalidated())
			}
		})
	}
}

func TestAddAudioLivePublication_P8_PublishAndInvalidateBothFire(t *testing.T) {
	files := livePubFiles(2)
	f := newLivePubFixture(t, files, livePubOptions{})
	if _, err := f.manager.AddAudio(context.Background(), livePubRequest(files, 1, 2)); err != nil {
		t.Fatalf("AddAudio() error = %v", err)
	}
	t.Run("P8_each_created_projection_is_published_and_invalidated", func(t *testing.T) {
		wantPub := []AudioPath{
			{Section: SectionMusic, VirtualPath: livePubVPath(1)},
			{Section: SectionMusic, VirtualPath: livePubVPath(2)},
		}
		wantInv := []string{
			f.finalPath(SectionMusic, livePubVPath(1)),
			f.finalPath(SectionMusic, livePubVPath(2)),
		}
		if got := f.published(); !reflect.DeepEqual(got, wantPub) {
			t.Errorf("published = %#v, want %#v", got, wantPub)
		}
		if got := f.invalidated(); !reflect.DeepEqual(got, wantInv) {
			t.Errorf("invalidated = %q, want %q (publish must not replace invalidate)", got, wantInv)
		}
	})
}

func TestAddAudioLivePublication_P9_PublishPrecedesInvalidateForSamePath(t *testing.T) {
	files := livePubFiles(4)
	f := newLivePubFixture(t, files, livePubOptions{})
	if _, err := f.manager.AddAudio(context.Background(), livePubRequest(files, 1, 2, 3, 4)); err != nil {
		t.Fatalf("AddAudio() error = %v", err)
	}
	t.Run("P9_publish_is_recorded_before_invalidate_for_every_path", func(t *testing.T) {
		events := f.all()
		if len(events) != 8 {
			t.Fatalf("events = %d, want 4 publishes + 4 invalidates: %#v", len(events), events)
		}
		publishedAt := map[string]int{}
		for i, e := range events {
			switch e.kind {
			case "publish":
				publishedAt[f.finalPath(e.path.Section, e.path.VirtualPath)] = i
			case "invalidate":
				pi, ok := publishedAt[e.file]
				if !ok {
					t.Errorf("invalidate of %q at event %d had no earlier publish", e.file, i)
				} else if pi >= i {
					t.Errorf("publish of %q at %d not before invalidate at %d", e.file, pi, i)
				}
			}
		}
	})
}
