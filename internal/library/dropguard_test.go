package library

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"tiramisu/internal/metadb"
)

const dropGuardHash = "AbCdEf0123456789AbCdEf0123456789AbCdEf01"

type dropGuardRegistry struct {
	mu         sync.Mutex
	referenced bool
	err        error
	hashes     []string
}

func (r *dropGuardRegistry) AudioHashReferenced(hash string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hashes = append(r.hashes, hash)
	return r.referenced, r.err
}

func (r *dropGuardRegistry) queriedHashes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hashes...)
}

func TestMayDropTorrentAudioOwnershipDecision(t *testing.T) {
	registryFailure := errors.New("registry unavailable")
	tests := []struct {
		name      string
		registry  AudioRegistry
		hash      string
		wantDrop  bool
		wantErr   error
		ignoreErr bool
		wantQuery []string
	}{
		{
			name:      "G1 unreferenced hash may be dropped",
			registry:  &dropGuardRegistry{},
			hash:      dropGuardHash,
			wantDrop:  true,
			wantQuery: []string{dropGuardHash},
		},
		{
			name:      "G2 referenced hash is retained",
			registry:  &dropGuardRegistry{referenced: true},
			hash:      dropGuardHash,
			wantQuery: []string{dropGuardHash},
		},
		{
			name:     "G3 nil registry preserves existing drop behavior",
			registry: nil,
			hash:     dropGuardHash,
			wantDrop: true,
		},
		{
			name:      "G4 empty hash fails closed without registry lookup",
			registry:  &dropGuardRegistry{},
			hash:      "",
			ignoreErr: true,
			wantQuery: nil,
		},
		{
			name:      "G5 registry failure is wrapped and fails closed",
			registry:  &dropGuardRegistry{err: registryFailure},
			hash:      dropGuardHash,
			wantErr:   registryFailure,
			wantQuery: []string{dropGuardHash},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDrop, gotErr := MayDropTorrent(tt.registry, tt.hash)
			if gotDrop != tt.wantDrop {
				t.Errorf("MayDropTorrent drop = %v, want %v", gotDrop, tt.wantDrop)
			}
			if !tt.ignoreErr && tt.wantErr == nil {
				if gotErr != nil {
					t.Errorf("MayDropTorrent error = %v, want nil", gotErr)
				}
			} else if !tt.ignoreErr && !errors.Is(gotErr, tt.wantErr) {
				t.Errorf("MayDropTorrent error = %v, want an error wrapping %v", gotErr, tt.wantErr)
			}
			if registry, ok := tt.registry.(*dropGuardRegistry); ok {
				if got := registry.queriedHashes(); !reflect.DeepEqual(got, tt.wantQuery) {
					t.Errorf("AudioHashReferenced queries = %q, want %q", got, tt.wantQuery)
				}
			}
		})
	}
}

func TestMayDropTorrentPassesHashVerbatim(t *testing.T) {
	registry := &dropGuardRegistry{}
	gotDrop, err := MayDropTorrent(registry, dropGuardHash)
	if err != nil || !gotDrop {
		t.Fatalf("G6 MayDropTorrent = (%v, %v), want (true, nil)", gotDrop, err)
	}
	if got := registry.queriedHashes(); !reflect.DeepEqual(got, []string{dropGuardHash}) {
		t.Errorf("G6 AudioHashReferenced queries = %q, want exact mixed-case hash %q", got, dropGuardHash)
	}
}

func TestMayDropTorrentRetainsEveryProjectionStateWithoutMutation(t *testing.T) {
	states := []metadb.AudioProjectionState{
		metadb.AudioStaged,
		metadb.AudioCommitted,
		metadb.AudioRemoving,
	}
	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			root := t.TempDir()
			db, err := metadb.New(filepath.Join(root, "registry.db"), nil)
			if err != nil {
				t.Fatalf("open metadb: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })

			sourcePath := filepath.Join(root, "source.flac")
			const sourceContents = "audio sentinel"
			if err := os.WriteFile(sourcePath, []byte(sourceContents), 0o600); err != nil {
				t.Fatalf("write source sentinel: %v", err)
			}
			projection := metadb.AudioProjection{
				Section:         "music",
				VirtualPath:     "Artist/Album/Track.flac",
				PortablePathKey: "artist/album/track.flac",
				Hash:            dropGuardHash,
				FileIndex:       1,
				SourcePath:      sourcePath,
				Size:            int64(len(sourceContents)),
				MtimeNS:         100,
				Title:           "Track",
				Magnet:          "magnet:?xt=urn:btih:" + dropGuardHash,
				TxnID:           "txn-state",
				StagingName:     ".txn-state",
				CreatedAtNS:     100,
				UpdatedAtNS:     100,
			}
			if err := db.StageAudioProjections(projection.TxnID, []metadb.AudioProjection{projection}); err != nil {
				t.Fatalf("stage projection: %v", err)
			}
			if state != metadb.AudioStaged {
				if n, err := db.CommitAudioProjections(projection.TxnID, 101); err != nil || n != 1 {
					t.Fatalf("commit projection = (%d, %v), want (1, nil)", n, err)
				}
			}
			if state == metadb.AudioRemoving {
				if ok, err := db.MarkAudioProjectionRemoving(projection.Section, projection.VirtualPath, 102); err != nil || !ok {
					t.Fatalf("mark projection removing = (%v, %v), want (true, nil)", ok, err)
				}
			}

			before, found, err := db.GetAudioProjection(projection.Section, projection.VirtualPath)
			if err != nil || !found {
				t.Fatalf("read projection before decision = (%#v, %v, %v)", before, found, err)
			}
			gotDrop, err := MayDropTorrent(db, dropGuardHash)
			if err != nil || gotDrop {
				t.Fatalf("H1 MayDropTorrent for %q = (%v, %v), want (false, nil)", state, gotDrop, err)
			}
			after, found, err := db.GetAudioProjection(projection.Section, projection.VirtualPath)
			if err != nil || !found {
				t.Fatalf("read projection after decision = (%#v, %v, %v)", after, found, err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Errorf("H2 projection mutated by guard:\nbefore: %#v\nafter:  %#v", before, after)
			}
			contents, err := os.ReadFile(sourcePath)
			if err != nil {
				t.Fatalf("H2 read source after decision: %v", err)
			}
			if string(contents) != sourceContents {
				t.Errorf("H2 source contents = %q, want %q", contents, sourceContents)
			}
		})
	}
}

func TestMayDropTorrentIsAdvisoryAboutAudioOnly(t *testing.T) {
	registry := &dropGuardRegistry{referenced: false}
	gotDrop, err := MayDropTorrent(registry, "hash-still-owned-by-movie-or-tv")
	if err != nil || !gotDrop {
		t.Errorf("H3 audio-only answer = (%v, %v), want (true, nil); other media checks belong to caller", gotDrop, err)
	}
}

func TestMayDropTorrentConcurrentCalls(t *testing.T) {
	registry := &dropGuardRegistry{referenced: true}
	const callers = 32
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			drop, err := MayDropTorrent(registry, fmt.Sprintf("hash-%02d", i))
			if err != nil || drop {
				errs <- fmt.Errorf("H4 caller %d = (%v, %v), want (false, nil)", i, drop, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := len(registry.queriedHashes()); got != callers {
		t.Errorf("H4 registry query count = %d, want %d", got, callers)
	}
}

func TestUnavailableAudioRegistryFailsClosed(t *testing.T) {
	sentinel := errors.New("state database unavailable")
	tests := []struct {
		name    string
		reg     UnavailableAudioRegistry
		hash    string
		wantErr error
	}{
		{
			name:    "S1 configured cause is wrapped",
			reg:     UnavailableAudioRegistry{Err: sentinel},
			hash:    dropGuardHash,
			wantErr: sentinel,
		},
		{
			name: "S1 nil cause still returns an error",
			reg:  UnavailableAudioRegistry{},
			hash: dropGuardHash,
		},
		{
			name:    "U3 empty hash still returns an error",
			reg:     UnavailableAudioRegistry{Err: sentinel},
			hash:    "",
			wantErr: sentinel,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			referenced, err := tt.reg.AudioHashReferenced(tt.hash)
			if referenced {
				t.Error("AudioHashReferenced = true, want false while registry is unavailable")
			}
			if err == nil {
				t.Fatal("AudioHashReferenced error = nil, want unavailable error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("AudioHashReferenced error = %v, want an error wrapping %v", err, tt.wantErr)
			}
		})
	}
}

func TestMayDropTorrentDistinguishesUnavailableFromUnconfigured(t *testing.T) {
	sentinel := errors.New("registry open failed")
	tests := []struct {
		name     string
		registry AudioRegistry
		wantDrop bool
		wantErr  error
	}{
		{
			name:     "S2 expected registry unavailable fails closed",
			registry: UnavailableAudioRegistry{Err: sentinel},
			wantErr:  sentinel,
		},
		{
			name:     "S3 genuinely unconfigured audio remains droppable",
			registry: nil,
			wantDrop: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDrop, gotErr := MayDropTorrent(tt.registry, dropGuardHash)
			if gotDrop != tt.wantDrop {
				t.Errorf("MayDropTorrent drop = %v, want %v", gotDrop, tt.wantDrop)
			}
			if tt.wantErr == nil {
				if gotErr != nil {
					t.Errorf("MayDropTorrent error = %v, want nil", gotErr)
				}
			} else if !errors.Is(gotErr, tt.wantErr) {
				t.Errorf("MayDropTorrent error = %v, want an error wrapping %v", gotErr, tt.wantErr)
			}
		})
	}
}

func TestUnavailableAudioRegistryConcurrentUse(t *testing.T) {
	sentinel := errors.New("registry unavailable")
	registry := UnavailableAudioRegistry{Err: sentinel}
	const callers = 32
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			referenced, err := registry.AudioHashReferenced(fmt.Sprintf("hash-%02d", i))
			if referenced || !errors.Is(err, sentinel) {
				errs <- fmt.Errorf("T3 caller %d = (%v, %v), want (false, wrapped sentinel)", i, referenced, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
