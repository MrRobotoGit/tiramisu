package library

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"tiramisu/internal/metadb"
)

type idempotencyPathKey struct {
	section     string
	virtualPath string
}

type idempotencySourceKey struct {
	hash      string
	fileIndex int
}

type idempotencyPortableKey struct {
	section     string
	portableKey string
}

type idempotencyLookupCall struct {
	method      string
	section     string
	virtualPath string
	portableKey string
	hash        string
	fileIndex   int
}

type idempotencyLookupFake struct {
	byPath        map[idempotencyPathKey]*metadb.AudioProjection
	bySource      map[idempotencySourceKey]*metadb.AudioProjection
	byPortableKey map[idempotencyPortableKey]*metadb.AudioProjection
	pathErrors    map[idempotencyPathKey]error
	sourceErrors  map[idempotencySourceKey]error
	calls         []idempotencyLookupCall
}

type idempotencyPortableLookupContract interface {
	AudioProjectionByPortableKey(section, portableKey string) (*metadb.AudioProjection, bool, error)
}

var (
	_ AudioProjectionLookup             = (*idempotencyLookupFake)(nil)
	_ idempotencyPortableLookupContract = AudioProjectionLookup(nil)
)

func (f *idempotencyLookupFake) GetAudioProjection(section, virtualPath string) (*metadb.AudioProjection, bool, error) {
	key := idempotencyPathKey{section: section, virtualPath: virtualPath}
	f.calls = append(f.calls, idempotencyLookupCall{
		method:      "GetAudioProjection",
		section:     section,
		virtualPath: virtualPath,
	})
	if err := f.pathErrors[key]; err != nil {
		return nil, false, err
	}
	projection, ok := f.byPath[key]
	return projection, ok, nil
}

func (f *idempotencyLookupFake) AudioProjectionBySource(hash string, fileIndex int) (*metadb.AudioProjection, bool, error) {
	key := idempotencySourceKey{hash: hash, fileIndex: fileIndex}
	f.calls = append(f.calls, idempotencyLookupCall{
		method:    "AudioProjectionBySource",
		hash:      hash,
		fileIndex: fileIndex,
	})
	if err := f.sourceErrors[key]; err != nil {
		return nil, false, err
	}
	projection, ok := f.bySource[key]
	return projection, ok, nil
}

func (f *idempotencyLookupFake) AudioProjectionByPortableKey(section, portableKey string) (*metadb.AudioProjection, bool, error) {
	key := idempotencyPortableKey{section: section, portableKey: portableKey}
	f.calls = append(f.calls, idempotencyLookupCall{
		method:      "AudioProjectionByPortableKey",
		section:     section,
		portableKey: portableKey,
	})
	projection, ok := f.byPortableKey[key]
	return projection, ok, nil
}

func newIdempotencyLookupFake(projections ...*metadb.AudioProjection) *idempotencyLookupFake {
	fake := &idempotencyLookupFake{
		byPath:        make(map[idempotencyPathKey]*metadb.AudioProjection),
		bySource:      make(map[idempotencySourceKey]*metadb.AudioProjection),
		byPortableKey: make(map[idempotencyPortableKey]*metadb.AudioProjection),
		pathErrors:    make(map[idempotencyPathKey]error),
		sourceErrors:  make(map[idempotencySourceKey]error),
	}
	for _, projection := range projections {
		fake.byPath[idempotencyPathKey{
			section:     projection.Section,
			virtualPath: projection.VirtualPath,
		}] = projection
		fake.bySource[idempotencySourceKey{
			hash:      projection.Hash,
			fileIndex: projection.FileIndex,
		}] = projection
		if projection.PortablePathKey != "" {
			fake.byPortableKey[idempotencyPortableKey{
				section:     projection.Section,
				portableKey: projection.PortablePathKey,
			}] = projection
		}
	}
	return fake
}

func TestClassifyAudioProjection_CreatedAndPresent(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	virtualPath := "Artist/Album/01 - Track_01234567.flac"
	source := ResolvedSource{SourcePath: "release/disc/01.flac", FileIndex: 7, Size: 987654321}

	t.Run("D1_new_path_and_unused_source_are_created", func(t *testing.T) {
		lookup := newIdempotencyLookupFake()

		got, err := ClassifyAudioProjection(lookup, SectionMusic, hash, virtualPath, source)
		if err != nil {
			t.Fatalf("ClassifyAudioProjection() error = %v, want nil", err)
		}
		want := AudioProjectionPlan{
			VirtualPath: virtualPath,
			Source:      source,
			Status:      AudioProjectionCreated,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ClassifyAudioProjection() = %#v, want %#v", got, want)
		}
	})

	t.Run("D2_every_identity_field_matches_is_present_with_existing_row", func(t *testing.T) {
		existing := &metadb.AudioProjection{
			ID:          41,
			Section:     string(SectionMusic),
			VirtualPath: virtualPath,
			Hash:        hash,
			FileIndex:   source.FileIndex,
			SourcePath:  source.SourcePath,
			Size:        source.Size,
			State:       metadb.AudioCommitted,
		}
		lookup := newIdempotencyLookupFake(existing)

		got, err := ClassifyAudioProjection(lookup, SectionMusic, hash, virtualPath, source)
		if err != nil {
			t.Fatalf("ClassifyAudioProjection() error = %v, want nil", err)
		}
		want := AudioProjectionPlan{
			VirtualPath: virtualPath,
			Source:      source,
			Status:      AudioProjectionPresent,
			Existing:    existing,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ClassifyAudioProjection() = %#v, want %#v", got, want)
		}
	})
}

func TestClassifyAudioProjection_PathIdentityConflicts_D3_D4_D5_D6_D8(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	virtualPath := "Artist/Album/01 - Track_01234567.flac"
	source := ResolvedSource{SourcePath: "release/disc/01.flac", FileIndex: 7, Size: 987654321}

	tests := []struct {
		name   string
		mutate func(*metadb.AudioProjection)
	}{
		{
			name: "D3_different_hash_is_a_path_conflict",
			mutate: func(projection *metadb.AudioProjection) {
				projection.Hash = "fedcba9876543210fedcba9876543210fedcba98"
			},
		},
		{
			name: "D4_different_file_index_is_a_path_conflict",
			mutate: func(projection *metadb.AudioProjection) {
				projection.FileIndex++
			},
		},
		{
			name: "D5_different_size_is_a_path_conflict",
			mutate: func(projection *metadb.AudioProjection) {
				projection.Size++
			},
		},
		{
			name: "D6_different_source_path_is_a_path_conflict",
			mutate: func(projection *metadb.AudioProjection) {
				projection.SourcePath = "release/re-sorted/other.flac"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			existing := &metadb.AudioProjection{
				Section:     string(SectionMusic),
				VirtualPath: virtualPath,
				Hash:        hash,
				FileIndex:   source.FileIndex,
				SourcePath:  source.SourcePath,
				Size:        source.Size,
				State:       metadb.AudioCommitted,
			}
			tt.mutate(existing)
			lookup := newIdempotencyLookupFake(existing)

			_, err := ClassifyAudioProjection(lookup, SectionMusic, hash, virtualPath, source)
			assertIdempotencyConflict(t, err, metadb.ErrAudioPathConflict)
		})
	}
}

func TestClassifyAudioProjection_SourceConflict_D7_D8(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	source := ResolvedSource{SourcePath: "release/disc/01.flac", FileIndex: 7, Size: 987654321}
	existing := &metadb.AudioProjection{
		Section:     string(SectionMusic),
		VirtualPath: "Artist/Album/Already Here_01234567.flac",
		Hash:        hash,
		FileIndex:   source.FileIndex,
		SourcePath:  source.SourcePath,
		Size:        source.Size,
		State:       metadb.AudioCommitted,
	}
	lookup := newIdempotencyLookupFake(existing)

	_, err := ClassifyAudioProjection(
		lookup,
		SectionMusic,
		hash,
		"Artist/Album/New Destination_01234567.flac",
		source,
	)
	assertIdempotencyConflict(t, err, metadb.ErrAudioSourceConflict)
}

func TestClassifyAudioProjection_SectionScope_D9(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	virtualPath := "Shared/Relative/Name_01234567.flac"
	source := ResolvedSource{SourcePath: "release/track.flac", FileIndex: 11, Size: 1100}
	otherSection := &metadb.AudioProjection{
		Section:     string(SectionAudiobooks),
		VirtualPath: virtualPath,
		Hash:        "fedcba9876543210fedcba9876543210fedcba98",
		FileIndex:   99,
		SourcePath:  "book/chapter.m4b",
		Size:        9900,
		State:       metadb.AudioCommitted,
	}
	lookup := newIdempotencyLookupFake(otherSection)

	got, err := ClassifyAudioProjection(lookup, SectionMusic, hash, virtualPath, source)
	if err != nil {
		t.Fatalf("ClassifyAudioProjection() error = %v, want nil", err)
	}
	if got.Status != AudioProjectionCreated {
		t.Errorf("Status = %q, want %q", got.Status, AudioProjectionCreated)
	}
	wantPathCall := idempotencyLookupCall{
		method:      "GetAudioProjection",
		section:     string(SectionMusic),
		virtualPath: virtualPath,
	}
	if !containsIdempotencyCall(lookup.calls, wantPathCall) {
		t.Errorf("registry calls = %+v, want section-scoped path lookup %+v", lookup.calls, wantPathCall)
	}
}

func TestClassifyAudioProjection_RegistryErrors_D10(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	virtualPath := "Artist/Album/01 - Track_01234567.flac"
	source := ResolvedSource{SourcePath: "release/01.flac", FileIndex: 7, Size: 700}

	t.Run("D10_path_lookup_error_is_propagated_not_treated_as_absent", func(t *testing.T) {
		// The requested path really is occupied, but the registry cannot return
		// that committed row. Treating the error as absence would classify a new,
		// unused source as created over an existing destination.
		committed := &metadb.AudioProjection{
			Section:     string(SectionMusic),
			VirtualPath: virtualPath,
			Hash:        "fedcba9876543210fedcba9876543210fedcba98",
			FileIndex:   70,
			SourcePath:  "older/release.flac",
			Size:        7000,
			State:       metadb.AudioCommitted,
		}
		lookup := newIdempotencyLookupFake(committed)
		registryErr := errors.New("registry path read failed")
		lookup.pathErrors[idempotencyPathKey{section: string(SectionMusic), virtualPath: virtualPath}] = registryErr

		got, err := ClassifyAudioProjection(lookup, SectionMusic, hash, virtualPath, source)
		if !errors.Is(err, registryErr) {
			t.Fatalf("error = %v, want errors.Is(_, %v)", err, registryErr)
		}
		if got.Status == AudioProjectionCreated {
			t.Errorf("Status = %q after failed registry read, must not treat failure as absent", got.Status)
		}
		wantCall := idempotencyLookupCall{
			method:      "GetAudioProjection",
			section:     string(SectionMusic),
			virtualPath: virtualPath,
		}
		if !containsIdempotencyCall(lookup.calls, wantCall) {
			t.Errorf("registry calls = %+v, want failed path lookup %+v", lookup.calls, wantCall)
		}
	})

	t.Run("D10_source_lookup_error_is_propagated_not_treated_as_unused", func(t *testing.T) {
		// The source is already committed at another path, but the failed lookup
		// cannot safely be interpreted as an unused source.
		committed := &metadb.AudioProjection{
			Section:     string(SectionMusic),
			VirtualPath: "Artist/Older Destination_01234567.flac",
			Hash:        hash,
			FileIndex:   source.FileIndex,
			SourcePath:  source.SourcePath,
			Size:        source.Size,
			State:       metadb.AudioCommitted,
		}
		lookup := newIdempotencyLookupFake(committed)
		registryErr := errors.New("registry source read failed")
		lookup.sourceErrors[idempotencySourceKey{hash: hash, fileIndex: source.FileIndex}] = registryErr

		got, err := ClassifyAudioProjection(lookup, SectionMusic, hash, virtualPath, source)
		if !errors.Is(err, registryErr) {
			t.Fatalf("error = %v, want errors.Is(_, %v)", err, registryErr)
		}
		if got.Status == AudioProjectionCreated {
			t.Errorf("Status = %q after failed registry read, must not treat failure as absent", got.Status)
		}
		wantCall := idempotencyLookupCall{
			method:    "AudioProjectionBySource",
			hash:      hash,
			fileIndex: source.FileIndex,
		}
		if !containsIdempotencyCall(lookup.calls, wantCall) {
			t.Errorf("registry calls = %+v, want failed source lookup %+v", lookup.calls, wantCall)
		}
	})
}

func TestClassifyAudioProjection_PathConflictPrecedesSourceConflict_D11(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	virtualPath := "Artist/Album/Requested_01234567.flac"
	source := ResolvedSource{SourcePath: "release/requested.flac", FileIndex: 7, Size: 700}
	pathOwner := &metadb.AudioProjection{
		Section:     string(SectionMusic),
		VirtualPath: virtualPath,
		Hash:        "fedcba9876543210fedcba9876543210fedcba98",
		FileIndex:   18,
		SourcePath:  "release/path-owner.flac",
		Size:        1800,
		State:       metadb.AudioCommitted,
	}
	sourceOwner := &metadb.AudioProjection{
		Section:     string(SectionMusic),
		VirtualPath: "Artist/Album/Source Owner_01234567.flac",
		Hash:        hash,
		FileIndex:   source.FileIndex,
		SourcePath:  source.SourcePath,
		Size:        source.Size,
		State:       metadb.AudioCommitted,
	}
	lookup := newIdempotencyLookupFake(pathOwner, sourceOwner)

	_, err := ClassifyAudioProjection(lookup, SectionMusic, hash, virtualPath, source)
	assertIdempotencyConflict(t, err, metadb.ErrAudioPathConflict)
}

func TestPlanAudioProjections_MixedRequestOrder_D12(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	requests, sources := idempotencyAlbumFixture(12)
	presentIndexes := map[int]bool{1: true, 6: true, 9: true}
	projections := make([]*metadb.AudioProjection, 0, len(presentIndexes))
	for i := range requests {
		if !presentIndexes[i] {
			continue
		}
		projections = append(projections, &metadb.AudioProjection{
			ID:          int64(100 + i),
			Section:     string(SectionMusic),
			VirtualPath: requests[i].Path,
			Hash:        hash,
			FileIndex:   sources[i].FileIndex,
			SourcePath:  sources[i].SourcePath,
			Size:        sources[i].Size,
			State:       metadb.AudioCommitted,
		})
	}
	lookup := newIdempotencyLookupFake(projections...)

	got, err := PlanAudioProjections(lookup, SectionMusic, hash, requests, sources)
	if err != nil {
		t.Fatalf("PlanAudioProjections() error = %v, want nil", err)
	}
	wantStatuses := []AudioProjectionStatus{
		AudioProjectionCreated,
		AudioProjectionPresent,
		AudioProjectionCreated,
		AudioProjectionCreated,
		AudioProjectionCreated,
		AudioProjectionCreated,
		AudioProjectionPresent,
		AudioProjectionCreated,
		AudioProjectionCreated,
		AudioProjectionPresent,
		AudioProjectionCreated,
		AudioProjectionCreated,
	}
	if len(got) != len(wantStatuses) {
		t.Fatalf("len(plan) = %d, want %d", len(got), len(wantStatuses))
	}
	gotStatuses := make([]AudioProjectionStatus, len(got))
	for i := range got {
		gotStatuses[i] = got[i].Status
		if got[i].VirtualPath != requests[i].Path || got[i].Source != sources[i] {
			t.Errorf("plan[%d] = %#v, want request path %q and source %#v", i, got[i], requests[i].Path, sources[i])
		}
		if presentIndexes[i] && got[i].Existing == nil {
			t.Errorf("plan[%d] is present but Existing is nil", i)
		}
		if !presentIndexes[i] && got[i].Existing != nil {
			t.Errorf("plan[%d] is created but Existing = %#v, want nil", i, got[i].Existing)
		}
	}
	if !reflect.DeepEqual(gotStatuses, wantStatuses) {
		t.Errorf("status sequence = %v, want exact request-ordered sequence %v", gotStatuses, wantStatuses)
	}
}

func TestPlanAudioProjections_FailClosed_D13(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	requests, sources := idempotencyAlbumFixture(3)
	conflict := &metadb.AudioProjection{
		Section:     string(SectionMusic),
		VirtualPath: requests[2].Path,
		Hash:        "fedcba9876543210fedcba9876543210fedcba98",
		FileIndex:   50,
		SourcePath:  "different/release.flac",
		Size:        5000,
		State:       metadb.AudioCommitted,
	}
	lookup := newIdempotencyLookupFake(conflict)

	got, err := PlanAudioProjections(lookup, SectionMusic, hash, requests, sources)
	assertIdempotencyConflict(t, err, metadb.ErrAudioPathConflict)
	if len(got) != 0 {
		t.Errorf("PlanAudioProjections() returned partial plan of %d entries on last-entry conflict: %#v", len(got), got)
	}
	for i := range requests {
		wantCall := idempotencyLookupCall{
			method:      "GetAudioProjection",
			section:     string(SectionMusic),
			virtualPath: requests[i].Path,
		}
		if !containsIdempotencyCall(lookup.calls, wantCall) {
			t.Errorf("registry calls = %+v, want path lookup for request[%d] %+v", lookup.calls, i, wantCall)
		}
	}
}

func TestPlanAudioProjections_EmptyRequest_D14(t *testing.T) {
	for _, tt := range []struct {
		name     string
		requests []AudioFileRequest
		sources  []ResolvedSource
	}{
		{name: "D14_nil_request_returns_empty_plan", requests: nil, sources: nil},
		{name: "D14_empty_request_returns_empty_plan", requests: []AudioFileRequest{}, sources: []ResolvedSource{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lookup := newIdempotencyLookupFake()

			got, err := PlanAudioProjections(lookup, SectionMusic, "hash", tt.requests, tt.sources)
			if err != nil {
				t.Fatalf("PlanAudioProjections() error = %v, want nil", err)
			}
			if len(got) != 0 {
				t.Errorf("len(plan) = %d, want 0", len(got))
			}
			if len(lookup.calls) != 0 {
				t.Errorf("registry calls = %+v for empty request, want none", lookup.calls)
			}
		})
	}
}

func TestPlanAudioProjections_AllPresent_D15(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	requests, sources := idempotencyAlbumFixture(4)
	projections := make([]*metadb.AudioProjection, len(requests))
	for i := range requests {
		projections[i] = &metadb.AudioProjection{
			ID:          int64(200 + i),
			Section:     string(SectionMusic),
			VirtualPath: requests[i].Path,
			Hash:        hash,
			FileIndex:   sources[i].FileIndex,
			SourcePath:  sources[i].SourcePath,
			Size:        sources[i].Size,
			State:       metadb.AudioCommitted,
		}
	}
	lookup := newIdempotencyLookupFake(projections...)

	got, err := PlanAudioProjections(lookup, SectionMusic, hash, requests, sources)
	if err != nil {
		t.Fatalf("PlanAudioProjections() error = %v, want nil", err)
	}
	if len(got) != len(requests) {
		t.Fatalf("len(plan) = %d, want %d", len(got), len(requests))
	}
	for i := range got {
		if got[i].Status != AudioProjectionPresent {
			t.Errorf("plan[%d].Status = %q, want %q", i, got[i].Status, AudioProjectionPresent)
		}
		if !reflect.DeepEqual(got[i].Existing, projections[i]) {
			t.Errorf("plan[%d].Existing = %#v, want registry row %#v", i, got[i].Existing, projections[i])
		}
	}
}

func TestPlanAudioProjections_InRequestSourceConflict_D16(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	requests := []AudioFileRequest{
		{SourcePath: "malformed/first.flac", Path: "Artist/Album/First_01234567.flac"},
		{SourcePath: "malformed/second.flac", Path: "Artist/Album/Second_01234567.flac"},
	}
	sources := []ResolvedSource{
		{SourcePath: requests[0].SourcePath, FileIndex: 7, Size: 700},
		{SourcePath: requests[1].SourcePath, FileIndex: 7, Size: 700},
	}
	lookup := newIdempotencyLookupFake()

	got, err := PlanAudioProjections(lookup, SectionMusic, hash, requests, sources)
	assertIdempotencyConflict(t, err, metadb.ErrAudioSourceConflict)
	if len(got) != 0 {
		t.Errorf("PlanAudioProjections() returned partial plan %#v for repeated in-request source identity, want none", got)
	}
}

func TestPlanAudioProjections_LengthMismatch_D17(t *testing.T) {
	request := AudioFileRequest{SourcePath: "release/01.flac", Path: "Artist/01_hash.flac"}
	secondRequest := AudioFileRequest{SourcePath: "release/02.flac", Path: "Artist/02_hash.flac"}
	source := ResolvedSource{SourcePath: request.SourcePath, FileIndex: 1, Size: 100}

	for _, tt := range []struct {
		name     string
		requests []AudioFileRequest
		sources  []ResolvedSource
	}{
		{
			name:     "D17_request_without_source_is_an_error",
			requests: []AudioFileRequest{request},
			sources:  nil,
		},
		{
			name:     "D17_source_without_request_is_an_error",
			requests: nil,
			sources:  []ResolvedSource{source},
		},
		{
			name:     "D17_unequal_nonempty_lengths_are_an_error",
			requests: []AudioFileRequest{request, secondRequest},
			sources:  []ResolvedSource{source},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lookup := newIdempotencyLookupFake()

			got, err := PlanAudioProjections(lookup, SectionMusic, "hash", tt.requests, tt.sources)
			if err == nil {
				t.Fatal("PlanAudioProjections() error = nil, want length-mismatch error")
			}
			if len(got) != 0 {
				t.Errorf("PlanAudioProjections() returned %d entries for mismatched inputs, want none", len(got))
			}
		})
	}
}

func TestClassifyAudioProjection_NonCommittedRowsConflict_D18(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	virtualPath := "Artist/Album/01 - Track_01234567.flac"
	source := ResolvedSource{SourcePath: "release/disc/01.flac", FileIndex: 7, Size: 987654321}

	for _, state := range []metadb.AudioProjectionState{metadb.AudioStaged, metadb.AudioRemoving} {
		t.Run("D18_matching_"+string(state)+"_row_is_a_path_conflict_not_present", func(t *testing.T) {
			existing := &metadb.AudioProjection{
				Section:         string(SectionMusic),
				VirtualPath:     virtualPath,
				PortablePathKey: PortablePathKey(virtualPath),
				Hash:            hash,
				FileIndex:       source.FileIndex,
				SourcePath:      source.SourcePath,
				Size:            source.Size,
				State:           state,
			}
			lookup := newIdempotencyLookupFake(existing)

			got, err := ClassifyAudioProjection(lookup, SectionMusic, hash, virtualPath, source)
			assertIdempotencyConflict(t, err, metadb.ErrAudioPathConflict)
			if got.Status == AudioProjectionPresent {
				t.Errorf("Status = %q for %q row, want conflict rather than present", got.Status, state)
			}
		})
	}
}

func TestClassifyAudioProjection_PortableKeyConflicts_D19(t *testing.T) {
	const requestedHash = "0123456789abcdef0123456789abcdef01234567"
	requestedSource := ResolvedSource{SourcePath: "new-release/track.flac", FileIndex: 27, Size: 2700}

	for _, tt := range []struct {
		name          string
		existingPath  string
		requestedPath string
	}{
		{
			name:          "D19_case_variant_collides_on_portable_key",
			existingPath:  "Artist/Album/Track_01234567.flac",
			requestedPath: "artist/album/track_01234567.flac",
		},
		{
			name:          "D19_NFC_request_collides_with_NFD_registry_spelling",
			existingPath:  "Artist/Album/Cafe\u0301_01234567.flac",
			requestedPath: "Artist/Album/Café_01234567.flac",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			expectedKey := PortablePathKey(tt.requestedPath)
			if existingKey := PortablePathKey(tt.existingPath); existingKey != expectedKey {
				t.Fatalf("fixture portable keys differ: existing = %q, requested = %q", existingKey, expectedKey)
			}
			existing := &metadb.AudioProjection{
				Section:         string(SectionMusic),
				VirtualPath:     tt.existingPath,
				PortablePathKey: expectedKey,
				Hash:            "fedcba9876543210fedcba9876543210fedcba98",
				FileIndex:       91,
				SourcePath:      "older-release/track.flac",
				Size:            9100,
				State:           metadb.AudioCommitted,
			}
			lookup := newIdempotencyLookupFake(existing)

			_, err := ClassifyAudioProjection(lookup, SectionMusic, requestedHash, tt.requestedPath, requestedSource)
			assertIdempotencyConflict(t, err, metadb.ErrAudioPathConflict)
			wantCall := idempotencyLookupCall{
				method:      "AudioProjectionByPortableKey",
				section:     string(SectionMusic),
				portableKey: expectedKey,
			}
			if !containsIdempotencyCall(lookup.calls, wantCall) {
				t.Errorf("registry calls = %+v, want portable-key lookup %+v", lookup.calls, wantCall)
			}
		})
	}
}

func TestPlanAudioProjections_InRequestPortableKeyConflict_D20(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	requests := []AudioFileRequest{
		{SourcePath: "release/first.flac", Path: "Artist/Album/Track_01234567.flac"},
		{SourcePath: "release/second.flac", Path: "artist/album/track_01234567.flac"},
	}
	sources := []ResolvedSource{
		{SourcePath: requests[0].SourcePath, FileIndex: 1, Size: 100},
		{SourcePath: requests[1].SourcePath, FileIndex: 2, Size: 200},
	}
	if first, second := PortablePathKey(requests[0].Path), PortablePathKey(requests[1].Path); first != second {
		t.Fatalf("fixture portable keys differ: first = %q, second = %q", first, second)
	}
	lookup := newIdempotencyLookupFake()

	got, err := PlanAudioProjections(lookup, SectionMusic, hash, requests, sources)
	assertIdempotencyConflict(t, err, metadb.ErrAudioPathConflict)
	if len(got) != 0 {
		t.Errorf("PlanAudioProjections() returned partial plan %#v for repeated portable key, want none", got)
	}
}

func TestClassifyAudioProjection_PortablePathConflictPrecedesSourceConflict_D21(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	requestedPath := "artist/album/track_01234567.flac"
	requestedSource := ResolvedSource{SourcePath: "requested/track.flac", FileIndex: 7, Size: 700}
	portableOwnerPath := "Artist/Album/Track_01234567.flac"
	portableOwner := &metadb.AudioProjection{
		Section:         string(SectionMusic),
		VirtualPath:     portableOwnerPath,
		PortablePathKey: PortablePathKey(portableOwnerPath),
		Hash:            "fedcba9876543210fedcba9876543210fedcba98",
		FileIndex:       18,
		SourcePath:      "portable-owner/track.flac",
		Size:            1800,
		State:           metadb.AudioCommitted,
	}
	sourceOwnerPath := "Different/Source Owner_01234567.flac"
	sourceOwner := &metadb.AudioProjection{
		Section:         string(SectionMusic),
		VirtualPath:     sourceOwnerPath,
		PortablePathKey: PortablePathKey(sourceOwnerPath),
		Hash:            hash,
		FileIndex:       requestedSource.FileIndex,
		SourcePath:      requestedSource.SourcePath,
		Size:            requestedSource.Size,
		State:           metadb.AudioCommitted,
	}
	lookup := newIdempotencyLookupFake(portableOwner, sourceOwner)

	_, err := ClassifyAudioProjection(lookup, SectionMusic, hash, requestedPath, requestedSource)
	assertIdempotencyConflict(t, err, metadb.ErrAudioPathConflict)
}

func idempotencyAlbumFixture(count int) ([]AudioFileRequest, []ResolvedSource) {
	requests := make([]AudioFileRequest, count)
	sources := make([]ResolvedSource, count)
	for i := 0; i < count; i++ {
		sourcePath := fmt.Sprintf("release/disc-1/track-%02d.flac", i+1)
		requests[i] = AudioFileRequest{
			SourcePath: sourcePath,
			Path:       fmt.Sprintf("Artist/Album/%02d - Track_01234567.flac", i+1),
		}
		sources[i] = ResolvedSource{
			SourcePath: sourcePath,
			FileIndex:  i + 1,
			Size:       int64(1000 + i),
		}
	}
	return requests, sources
}

func assertIdempotencyConflict(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want errors.Is(_, %v)", err, want)
	}
	other := metadb.ErrAudioPathConflict
	if want == metadb.ErrAudioPathConflict {
		other = metadb.ErrAudioSourceConflict
	}
	if errors.Is(err, other) {
		t.Errorf("error = %v also matches distinct conflict sentinel %v", err, other)
	}
}

func containsIdempotencyCall(calls []idempotencyLookupCall, want idempotencyLookupCall) bool {
	for _, call := range calls {
		if call == want {
			return true
		}
	}
	return false
}
