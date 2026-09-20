package library

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"tiramisu/internal/metadb"
)

const referenceHash = "abcdef0123456789abcdef0123456789abcdef01"

type referenceGoStorm struct {
	mu sync.Mutex

	addHash string
	addErr  error
	info    *TorrentStats
	infoErr error
	removed []string
}

func (f *referenceGoStorm) AddTorrent(context.Context, string, string) (string, error) {
	return f.addHash, f.addErr
}

func (f *referenceGoStorm) GetTorrentInfo(context.Context, string, int) (*TorrentStats, error) {
	return f.info, f.infoErr
}

func (f *referenceGoStorm) RemoveTorrent(_ context.Context, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, hash)
	return nil
}

func (f *referenceGoStorm) ListTorrents(context.Context) ([]TorrentStats, error) {
	return nil, nil
}

func (f *referenceGoStorm) removedHashes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}

type referenceAudioRegistry struct {
	mu sync.Mutex

	referenced map[string]bool
	err        error
	queries    []string
}

func (r *referenceAudioRegistry) AudioHashReferenced(hash string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, hash)
	return r.referenced[hash], r.err
}

func (r *referenceAudioRegistry) setReferenced(hash string, referenced bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.referenced == nil {
		r.referenced = make(map[string]bool)
	}
	r.referenced[hash] = referenced
}

func (r *referenceAudioRegistry) queriedHashes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.queries...)
}

func newReferenceManager(t *testing.T, engine GoStorm, registry AudioRegistry) (*Manager, string, string) {
	t.Helper()
	root := t.TempDir()
	movies := filepath.Join(root, "movies")
	tv := filepath.Join(root, "tv")
	for _, dir := range []string{movies, tv} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create media directory %s: %v", dir, err)
		}
	}
	return New(Config{
		MoviesDir:     movies,
		TVDir:         tv,
		GoStormURL:    "http://gostorm.invalid",
		GoStorm:       engine,
		AudioRegistry: registry,
		Logger:        log.New(io.Discard, "", 0),
	}), movies, tv
}

func writeReferenceStub(t *testing.T, dir, name, hash string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	url := "http://gostorm.invalid/stream?link=" + hash + "&index=1&play"
	if err := WriteStub(path, url, 1234, "magnet:?xt=urn:btih:"+hash, "tt1234567"); err != nil {
		t.Fatalf("WriteStub(%s): %v", path, err)
	}
	return path
}

func movieStubName(label, hash string) string {
	return label + "_" + HashSuffix(hash) + ".mkv"
}

func tvStubName(label, hash string) string {
	return label + "_S01E01_" + HashPrefix(hash) + ".mkv"
}

func requireRemovedHashes(t *testing.T, engine *referenceGoStorm, want ...string) {
	t.Helper()
	got := engine.removedHashes()
	if len(got) != len(want) {
		t.Fatalf("RemoveTorrent calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("RemoveTorrent call %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func requireSuccessfulRemoval(t *testing.T, m *Manager, path string) {
	t.Helper()
	resp, err := m.Remove(context.Background(), RemoveRequest{Path: path})
	if err != nil {
		t.Fatalf("Remove(%s): %v", path, err)
	}
	if len(resp.Removed) != 1 || resp.Removed[0] != path {
		t.Fatalf("Remove(%s).Removed = %v, want [%s]", path, resp.Removed, path)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("removed stub still exists or cannot be checked: %v", err)
	}
}

func TestRemoveSharedHashHonorsAudioReference(t *testing.T) {
	// K1, K2, K3, K4: the same torrent crosses the movie/audio boundary. It is
	// retained while audio owns it, then becomes droppable once that ownership ends.
	engine := &referenceGoStorm{}
	registry := &referenceAudioRegistry{referenced: map[string]bool{referenceHash: true}}
	m, movies, _ := newReferenceManager(t, engine, registry)

	uppercaseHash := strings.ToUpper(referenceHash)
	first := writeReferenceStub(t, movies, movieStubName("shared-first", uppercaseHash), uppercaseHash)
	requireSuccessfulRemoval(t, m, first)
	requireRemovedHashes(t, engine)
	if got := registry.queriedHashes(); len(got) != 1 || got[0] != referenceHash {
		t.Fatalf("AudioHashReferenced queries = %v, want lowercase hash %q", got, referenceHash)
	}

	registry.setReferenced(referenceHash, false)
	second := writeReferenceStub(t, movies, movieStubName("shared-second", referenceHash), referenceHash)
	requireSuccessfulRemoval(t, m, second)
	requireRemovedHashes(t, engine, referenceHash)
}

func TestRemoveKeepsTorrentForEveryAudioProjectionState(t *testing.T) {
	// L1, L5: staged, committed, and removing rows all retain ownership, and a
	// manager drop decision is read-only with respect to the projection registry.
	states := []metadb.AudioProjectionState{
		metadb.AudioStaged,
		metadb.AudioCommitted,
		metadb.AudioRemoving,
	}
	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			db, err := metadb.New(filepath.Join(t.TempDir(), "registry.db"), nil)
			if err != nil {
				t.Fatalf("open metadb: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })

			projection := metadb.AudioProjection{
				Section:         "music",
				VirtualPath:     "Artist/Album/Track.flac",
				PortablePathKey: "artist/album/track.flac",
				Hash:            referenceHash,
				FileIndex:       1,
				SourcePath:      "source/Track.flac",
				Size:            1234,
				MtimeNS:         100,
				Title:           "Track",
				TxnID:           "txn-state",
				StagingName:     ".txn-state",
				CreatedAtNS:     100,
				UpdatedAtNS:     100,
			}
			if err := db.StageAudioProjections(projection.TxnID, []metadb.AudioProjection{projection}); err != nil {
				t.Fatalf("stage audio projection: %v", err)
			}
			if state != metadb.AudioStaged {
				if n, err := db.CommitAudioProjections(projection.TxnID, 101); err != nil || n != 1 {
					t.Fatalf("commit audio projection = (%d, %v), want (1, nil)", n, err)
				}
			}
			if state == metadb.AudioRemoving {
				if ok, err := db.MarkAudioProjectionRemoving(projection.Section, projection.VirtualPath, 102); err != nil || !ok {
					t.Fatalf("mark audio projection removing = (%v, %v), want (true, nil)", ok, err)
				}
			}

			var registry AudioRegistry = db
			engine := &referenceGoStorm{}
			m, movies, _ := newReferenceManager(t, engine, registry)
			stubPath := writeReferenceStub(t, movies, movieStubName("state", referenceHash), referenceHash)
			requireSuccessfulRemoval(t, m, stubPath)
			requireRemovedHashes(t, engine)

			stillReferenced, err := db.AudioHashReferenced(referenceHash)
			if err != nil {
				t.Fatalf("AudioHashReferenced after removal: %v", err)
			}
			if !stillReferenced {
				t.Fatal("manager removal mutated the audio registry row")
			}
			got, found, err := db.GetAudioProjection(projection.Section, projection.VirtualPath)
			if err != nil || !found {
				t.Fatalf("GetAudioProjection after removal = (%#v, %v, %v), want existing row", got, found, err)
			}
			if got.State != state {
				t.Errorf("projection state after removal = %q, want %q", got.State, state)
			}
		})
	}
}

func TestRemovePreservesMovieAndTVReferenceChecks(t *testing.T) {
	// K5, L2: nil audio configuration behaves as before, and a configured registry
	// that reports no audio ownership cannot replace either filesystem check.
	tests := []struct {
		name        string
		siblingKind string
		configured  bool
	}{
		{name: "nil registry movie sibling keeps torrent", siblingKind: "movie"},
		{name: "nil registry TV sibling keeps torrent", siblingKind: "tv"},
		{name: "nil registry no sibling drops torrent", siblingKind: ""},
		{name: "configured registry movie sibling keeps torrent", siblingKind: "movie", configured: true},
		{name: "configured registry TV sibling keeps torrent", siblingKind: "tv", configured: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := &referenceGoStorm{}
			var registry AudioRegistry
			if tt.configured {
				registry = &referenceAudioRegistry{referenced: map[string]bool{referenceHash: false}}
			}
			m, movies, tv := newReferenceManager(t, engine, registry)
			target := writeReferenceStub(t, movies, movieStubName("target", referenceHash), referenceHash)
			if tt.siblingKind == "movie" {
				writeReferenceStub(t, movies, movieStubName("sibling", referenceHash), referenceHash)
			}
			if tt.siblingKind == "tv" {
				writeReferenceStub(t, tv, tvStubName("sibling", referenceHash), referenceHash)
			}

			requireSuccessfulRemoval(t, m, target)
			if tt.siblingKind == "" {
				requireRemovedHashes(t, engine, referenceHash)
			} else {
				requireRemovedHashes(t, engine)
			}
		})
	}
}

func TestRemoveKeepsTorrentWhenAudioRegistryFails(t *testing.T) {
	// L3: an unavailable ownership answer is never interpreted as "unreferenced".
	engine := &referenceGoStorm{}
	registry := &referenceAudioRegistry{err: errors.New("registry unavailable")}
	m, movies, _ := newReferenceManager(t, engine, registry)
	path := writeReferenceStub(t, movies, movieStubName("registry-error", referenceHash), referenceHash)

	requireSuccessfulRemoval(t, m, path)
	requireRemovedHashes(t, engine)
	if got := registry.queriedHashes(); len(got) != 1 || got[0] != referenceHash {
		t.Errorf("AudioHashReferenced queries = %v, want [%s]", got, referenceHash)
	}
}

func TestDropTorrentIfUnusedBailsOutWhileHashLockHeld(t *testing.T) {
	// L4: cleanup neither consults ownership nor drops while an add holds the hash.
	engine := &referenceGoStorm{}
	registry := &referenceAudioRegistry{}
	m, _, _ := newReferenceManager(t, engine, registry)
	unlock := m.hashLocks.Lock(referenceHash)
	m.dropTorrentIfUnused(context.Background(), referenceHash)
	unlock()

	requireRemovedHashes(t, engine)
	if got := registry.queriedHashes(); len(got) != 0 {
		t.Errorf("AudioHashReferenced was called while hash lock held: %v", got)
	}
}

func TestDropTorrentIfUnusedIgnoresEmptyHash(t *testing.T) {
	// M1: an empty identity cannot be looked up or sent to the engine.
	engine := &referenceGoStorm{}
	registry := &referenceAudioRegistry{}
	m, _, _ := newReferenceManager(t, engine, registry)
	m.dropTorrentIfUnused(context.Background(), "")

	requireRemovedHashes(t, engine)
	if got := registry.queriedHashes(); len(got) != 0 {
		t.Errorf("AudioHashReferenced was called with an empty hash: %v", got)
	}
}

func TestRemoveHashlessStubDropsNothing(t *testing.T) {
	// M2: a valid stub without an info hash is removable but cannot identify a torrent.
	engine := &referenceGoStorm{}
	registry := &referenceAudioRegistry{}
	m, movies, _ := newReferenceManager(t, engine, registry)
	path := filepath.Join(movies, "hashless.mkv")
	if err := os.WriteFile(path, []byte(`{"url":"http://gostorm.invalid/stream?index=1&play","size":1234}`), 0o644); err != nil {
		t.Fatalf("write hashless stub: %v", err)
	}

	requireSuccessfulRemoval(t, m, path)
	requireRemovedHashes(t, engine)
	if got := registry.queriedHashes(); len(got) != 0 {
		t.Errorf("AudioHashReferenced was called for a hashless stub: %v", got)
	}
}

func TestDropTorrentIfUnusedKeepsTorrentWhenStubScanFails(t *testing.T) {
	// M3: a filesystem inspection error wins over an audio "not referenced" answer.
	engine := &referenceGoStorm{}
	registry := &referenceAudioRegistry{referenced: map[string]bool{referenceHash: false}}
	m, _, _ := newReferenceManager(t, engine, registry)
	m.cfg.MoviesDir = string([]byte{0}) // filepath.Walk deterministically returns an invalid-path error.

	m.dropTorrentIfUnused(context.Background(), referenceHash)
	requireRemovedHashes(t, engine)
}

func TestRemoveByHashDropsOnceAfterAllSharedStubsAreGone(t *testing.T) {
	// M4: Remove(hash) deletes the full sibling set before making one drop decision.
	engine := &referenceGoStorm{}
	registry := &referenceAudioRegistry{referenced: map[string]bool{referenceHash: false}}
	m, movies, tv := newReferenceManager(t, engine, registry)
	paths := []string{
		writeReferenceStub(t, movies, movieStubName("one", referenceHash), referenceHash),
		writeReferenceStub(t, movies, movieStubName("two", referenceHash), referenceHash),
		writeReferenceStub(t, tv, tvStubName("three", referenceHash), referenceHash),
	}

	resp, err := m.Remove(context.Background(), RemoveRequest{Hash: strings.ToUpper(referenceHash)})
	if err != nil {
		t.Fatalf("Remove(hash): %v", err)
	}
	if len(resp.Removed) != len(paths) {
		t.Fatalf("Remove(hash).Removed = %v, want %d paths", resp.Removed, len(paths))
	}
	sort.Strings(paths)
	sort.Strings(resp.Removed)
	for i := range paths {
		if resp.Removed[i] != paths[i] {
			t.Errorf("Remove(hash).Removed[%d] = %q, want %q", i, resp.Removed[i], paths[i])
		}
		if _, err := os.Stat(paths[i]); !os.IsNotExist(err) {
			t.Errorf("stub %s still exists or cannot be checked: %v", paths[i], err)
		}
	}
	requireRemovedHashes(t, engine, referenceHash)
	if got := registry.queriedHashes(); len(got) != 1 || got[0] != referenceHash {
		t.Errorf("AudioHashReferenced queries = %v, want one query for %q", got, referenceHash)
	}
}

func TestFailedAddCleanupHonorsAudioReference(t *testing.T) {
	// K6: hydration succeeded, metadata failed, and deferred cleanup still respects
	// an unrelated committed projection's ownership.
	engine := &referenceGoStorm{
		addHash: referenceHash,
		infoErr: errors.New("metadata unavailable"),
	}
	registry := &referenceAudioRegistry{referenced: map[string]bool{referenceHash: true}}
	m, _, _ := newReferenceManager(t, engine, registry)

	resp, err := m.Add(context.Background(), AddRequest{
		Type:  "movie",
		Hash:  referenceHash,
		Title: "Hydrated but unavailable",
	})
	if err == nil {
		t.Fatalf("Add() = (%#v, nil), want metadata failure", resp)
	}
	if resp != nil {
		t.Errorf("Add() response = %#v, want nil on failure", resp)
	}
	requireRemovedHashes(t, engine)
	if got := registry.queriedHashes(); len(got) != 1 || got[0] != referenceHash {
		t.Errorf("AudioHashReferenced queries = %v, want [%s]", got, referenceHash)
	}
}
