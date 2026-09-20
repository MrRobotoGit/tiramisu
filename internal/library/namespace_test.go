package library

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type recordingOwnership struct {
	mu      sync.Mutex
	owned   map[AudioPath]bool
	queries []AudioPath
}

func (o *recordingOwnership) HasProjection(section Section, virtualPath string) bool {
	p := AudioPath{Section: section, VirtualPath: virtualPath}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.queries = append(o.queries, p)
	return o.owned[p]
}

func (o *recordingOwnership) queried() []AudioPath {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]AudioPath(nil), o.queries...)
}

type allowAllOwnership struct{}

func (allowAllOwnership) HasProjection(Section, string) bool { return true }

type forbiddenOwnership struct {
	t *testing.T
}

func (o forbiddenOwnership) HasProjection(section Section, virtualPath string) bool {
	o.t.Helper()
	o.t.Fatalf("HasProjection(%q, %q) called for a video path", section, virtualPath)
	return false
}

func TestClassifyProjection_AudioRequiresExactCommittedProjection(t *testing.T) {
	source := filepath.Join(string(filepath.Separator), "srv", "library")
	tests := []struct {
		name        string
		section     Section
		relative    string
		committed   []AudioPath
		want        VFSClass
		wantQueries []AudioPath
	}{
		{
			name:      "V1 committed music projection is exposed",
			section:   SectionMusic,
			relative:  "Artist/Album/Track_a1b2c3d4.flac",
			committed: []AudioPath{{Section: SectionMusic, VirtualPath: "Artist/Album/Track_a1b2c3d4.flac"}},
			want:      VFSClass{Section: SectionMusic, Stub: true, Audio: true},
			wantQueries: []AudioPath{{
				Section: SectionMusic, VirtualPath: "Artist/Album/Track_a1b2c3d4.flac",
			}},
		},
		{
			name:      "V1 committed audiobook projection is exposed",
			section:   SectionAudiobooks,
			relative:  "Author/Series/Book_deadbeef.m4b",
			committed: []AudioPath{{Section: SectionAudiobooks, VirtualPath: "Author/Series/Book_deadbeef.m4b"}},
			want:      VFSClass{Section: SectionAudiobooks, Stub: true, Audio: true},
			wantQueries: []AudioPath{{
				Section: SectionAudiobooks, VirtualPath: "Author/Series/Book_deadbeef.m4b",
			}},
		},
		{
			name:        "V2 valid music extension without committed row is invisible",
			section:     SectionMusic,
			relative:    "Artist/Album/Rejected_a1b2c3d4.flac",
			committed:   nil,
			want:        VFSClass{Section: SectionMusic},
			wantQueries: []AudioPath{{Section: SectionMusic, VirtualPath: "Artist/Album/Rejected_a1b2c3d4.flac"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owned := &recordingOwnership{owned: make(map[AudioPath]bool)}
			for _, p := range tt.committed {
				owned.owned[p] = true
			}
			fullPath := filepath.Join(source, string(tt.section), filepath.FromSlash(tt.relative))
			if got := ClassifyProjection(source, fullPath, owned); got != tt.want {
				t.Errorf("ClassifyProjection(%q, %q) = %#v, want %#v", source, fullPath, got, tt.want)
			}
			gotQueries := owned.queried()
			if len(gotQueries) != len(tt.wantQueries) {
				t.Fatalf("ownership queries = %#v, want %#v", gotQueries, tt.wantQueries)
			}
			for i := range tt.wantQueries {
				if gotQueries[i] != tt.wantQueries[i] {
					t.Errorf("ownership query %d = %#v, want %#v", i, gotQueries[i], tt.wantQueries[i])
				}
			}
		})
	}
}

func TestClassifyProjection_NilOwnershipRejectsEveryAudioProjection(t *testing.T) {
	source := filepath.Join(string(filepath.Separator), "srv", "library")
	tests := []struct {
		name string
		path string
		want VFSClass
	}{
		{"V3 nil ownership rejects music", filepath.Join(source, "music", "Artist", "Track.flac"), VFSClass{Section: SectionMusic}},
		{"V3 nil ownership rejects audiobooks", filepath.Join(source, "audiobooks", "Author", "Book.mp3"), VFSClass{Section: SectionAudiobooks}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyProjection(source, tt.path, nil); got != tt.want {
				t.Errorf("ClassifyProjection(%q, %q, nil) = %#v, want %#v", source, tt.path, got, tt.want)
			}
		})
	}
}

func TestClassifyProjection_DoesNotRedefineClassifyPath(t *testing.T) {
	source := filepath.Join(string(filepath.Separator), "srv", "library")
	path := filepath.Join(source, "music", "Artist", "Uncommitted.flac")

	wantLegacy := VFSClass{Section: SectionMusic, Stub: true, Audio: true}
	if got := ClassifyPath(source, path); got != wantLegacy {
		t.Fatalf("W3 ClassifyPath = %#v, want unchanged extension-only result %#v", got, wantLegacy)
	}
	wantProjection := VFSClass{Section: SectionMusic}
	if got := ClassifyProjection(source, path, NewAudioNamespace()); got != wantProjection {
		t.Errorf("ClassifyProjection = %#v, want registry-aware result %#v", got, wantProjection)
	}
}

func TestClassifyProjection_VideoMatchesClassifyPathForEveryOwnershipState(t *testing.T) {
	source := filepath.Join(string(filepath.Separator), "srv", "library")
	full := NewAudioNamespace()
	full.Add(AudioPath{Section: SectionMusic, VirtualPath: "Artist/Track.flac"})
	ownershipStates := []struct {
		name  string
		owned AudioOwnership
	}{
		{"nil", nil},
		{"empty", NewAudioNamespace()},
		{"populated", full},
	}
	paths := []struct {
		name string
		path string
	}{
		{"V4 movie section", filepath.Join(source, "movies", "Film.mkv")},
		{"V4 TV section", filepath.Join(source, "tv", "Show", "Episode.mkv")},
		{"V4 E1 sibling directory", filepath.Join(source, "extras", "Film.mkv")},
		{"V4 E1 source root", filepath.Join(source, "Film.mkv")},
		{"V4 E1 unrelated absolute path", filepath.Join(string(filepath.Separator), "archive", "Film.mkv")},
		{"V4 E1 deep unrelated path", filepath.Join(string(filepath.Separator), "other", "deep", "layout", "Film.mkv")},
	}

	for _, tt := range paths {
		t.Run(tt.name, func(t *testing.T) {
			want := ClassifyPath(source, tt.path)
			if !want.Stub || want.Audio {
				t.Fatalf("fixture ClassifyPath(%q, %q) = %#v, want a video stub", source, tt.path, want)
			}
			for _, state := range ownershipStates {
				t.Run(state.name, func(t *testing.T) {
					if got := ClassifyProjection(source, tt.path, state.owned); got != want {
						t.Errorf("ClassifyProjection = %#v, ClassifyPath = %#v", got, want)
					}
				})
			}
		})
	}
}

func TestClassifyProjection_NeverConsultsOwnershipForVideo(t *testing.T) {
	source := filepath.Join(string(filepath.Separator), "srv", "library")
	tests := []struct {
		name string
		path string
	}{
		{"W5 movie section", filepath.Join(source, "movies", "Film.mkv")},
		{"W5 TV section", filepath.Join(source, "tv", "Show", "Episode.mkv")},
		{"W5 global MKV compatibility", filepath.Join(source, "extras", "Film.mkv")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := ClassifyPath(source, tt.path)
			if got := ClassifyProjection(source, tt.path, forbiddenOwnership{t: t}); got != want {
				t.Errorf("ClassifyProjection = %#v, ClassifyPath = %#v", got, want)
			}
		})
	}
}

func TestClassifyProjection_BoundariesRequireBothExtensionAndOwnership(t *testing.T) {
	source := filepath.Join(string(filepath.Separator), "path", "that", "does", "not", "exist")
	tests := []struct {
		name       string
		sourcePath string
		fullPath   string
		want       VFSClass
	}{
		{"X1 both paths empty", "", "", VFSClass{}},
		{"X1 empty source with audio-looking path", "", filepath.Join("music", "Track.flac"), VFSClass{}},
		{"X1 empty full path", source, "", VFSClass{}},
		{"X2 dot-leading music basename remains hidden", source, filepath.Join(source, "music", "Artist", ".Track.flac"), VFSClass{Section: SectionMusic}},
		{"X2 dot-leading audiobook basename remains hidden", source, filepath.Join(source, "audiobooks", ".Book.m4b"), VFSClass{Section: SectionAudiobooks}},
		{"X3 ownership cannot admit MKV in music", source, filepath.Join(source, "music", "Film.mkv"), VFSClass{Section: SectionMusic}},
		{"X3 ownership cannot admit FLAC in audiobooks", source, filepath.Join(source, "audiobooks", "Book.flac"), VFSClass{Section: SectionAudiobooks}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyProjection(tt.sourcePath, tt.fullPath, allowAllOwnership{}); got != tt.want {
				t.Errorf("ClassifyProjection(%q, %q) = %#v, want %#v", tt.sourcePath, tt.fullPath, got, tt.want)
			}
		})
	}
}

func TestClassifyProjection_UsesInMemoryOwnershipForNonexistentPaths(t *testing.T) {
	source := filepath.Join(string(filepath.Separator), "definitely", "absent", "library")
	relative := "Artist/Album/Track_a1b2c3d4.flac"
	path := filepath.Join(source, "music", filepath.FromSlash(relative))
	n := NewAudioNamespace()
	n.Add(AudioPath{Section: SectionMusic, VirtualPath: relative})

	want := VFSClass{Section: SectionMusic, Stub: true, Audio: true}
	if got := ClassifyProjection(source, path, n); got != want {
		t.Errorf("W1 ClassifyProjection for nonexistent path = %#v, want %#v", got, want)
	}
	if !n.HasProjection(SectionMusic, relative) {
		t.Error("W1 HasProjection lost an in-memory committed path")
	}
}

func TestAudioNamespace_ExactMembership(t *testing.T) {
	committed := AudioPath{Section: SectionMusic, VirtualPath: "Artist/Album/Track.flac"}
	n := NewAudioNamespace()
	n.Add(committed)
	tests := []struct {
		name    string
		section Section
		path    string
		want    bool
	}{
		{"V6 exact entry matches", SectionMusic, "Artist/Album/Track.flac", true},
		{"V6 different section does not match", SectionAudiobooks, "Artist/Album/Track.flac", false},
		{"V6 different path does not match", SectionMusic, "Artist/Album/Other.flac", false},
		{"V6 parent directory does not match", SectionMusic, "Artist/Album", false},
		{"V6 child path does not match", SectionMusic, "Artist/Album/Track.flac/child", false},
		{"W4 different basename case does not match", SectionMusic, "Artist/Album/track.flac", false},
		{"W4 different directory case does not match", SectionMusic, "artist/Album/Track.flac", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := n.HasProjection(tt.section, tt.path); got != tt.want {
				t.Errorf("HasProjection(%q, %q) = %v, want %v", tt.section, tt.path, got, tt.want)
			}
		})
	}
}

func TestAudioNamespace_ReplaceAndLifecycle(t *testing.T) {
	a := AudioPath{Section: SectionMusic, VirtualPath: "Artist/A.flac"}
	b := AudioPath{Section: SectionAudiobooks, VirtualPath: "Author/B.m4b"}
	c := AudioPath{Section: SectionMusic, VirtualPath: "Artist/C.flac"}

	t.Run("V7 Replace swaps the complete committed set", func(t *testing.T) {
		n := NewAudioNamespace()
		n.Replace([]AudioPath{a, b})
		n.Replace([]AudioPath{c})
		if n.HasProjection(a.Section, a.VirtualPath) || n.HasProjection(b.Section, b.VirtualPath) {
			t.Error("Replace retained an entry absent from the replacement set")
		}
		if !n.HasProjection(c.Section, c.VirtualPath) {
			t.Error("Replace omitted an entry in the replacement set")
		}
		if got := n.Len(); got != 1 {
			t.Errorf("Len after Replace = %d, want 1", got)
		}
	})

	t.Run("V8 Add Remove duplicate and absent operations are set operations", func(t *testing.T) {
		n := NewAudioNamespace()
		n.Remove(a)
		if got := n.Len(); got != 0 {
			t.Fatalf("Len after removing absent entry = %d, want 0", got)
		}
		n.Add(a)
		n.Add(a)
		if got := n.Len(); got != 1 {
			t.Errorf("Len after duplicate Add = %d, want 1", got)
		}
		n.Remove(a)
		if n.HasProjection(a.Section, a.VirtualPath) {
			t.Error("Add followed by Remove left the entry present")
		}
		if got := n.Len(); got != 0 {
			t.Errorf("Len after Add and Remove = %d, want 0", got)
		}
	})

	for _, replacement := range []struct {
		name  string
		paths []AudioPath
	}{
		{"X4 Replace nil empties", nil},
		{"X4 Replace empty slice empties", []AudioPath{}},
	} {
		t.Run(replacement.name, func(t *testing.T) {
			n := NewAudioNamespace()
			n.Replace([]AudioPath{a, b})
			n.Replace(replacement.paths)
			if got := n.Len(); got != 0 {
				t.Errorf("Len after empty Replace = %d, want 0", got)
			}
			if n.HasProjection(a.Section, a.VirtualPath) || n.HasProjection(b.Section, b.VirtualPath) {
				t.Error("empty Replace retained a committed entry")
			}
		})
	}
}

func TestAudioNamespace_ConstructorsAreEmptyAndUsable(t *testing.T) {
	tests := []struct {
		name string
		n    *AudioNamespace
	}{
		{"V9 constructor", NewAudioNamespace()},
		{"V9 zero value", &AudioNamespace{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.n.HasProjection(SectionMusic, "Artist/Track.flac") {
				t.Error("empty namespace reported an owned projection")
			}
			if got := tt.n.Len(); got != 0 {
				t.Errorf("empty namespace Len = %d, want 0", got)
			}
			p := AudioPath{Section: SectionMusic, VirtualPath: "Artist/Track.flac"}
			tt.n.Add(p)
			if !tt.n.HasProjection(p.Section, p.VirtualPath) {
				t.Error("empty namespace was not usable by Add")
			}
			tt.n.Remove(p)
			if tt.n.HasProjection(p.Section, p.VirtualPath) {
				t.Error("empty namespace was not usable by Remove")
			}
		})
	}
}

func TestAudioNamespace_ConcurrentReadsAndMutations(t *testing.T) {
	makeSet := func(prefix string, count int) []AudioPath {
		paths := make([]AudioPath, count)
		for i := range paths {
			paths[i] = AudioPath{
				Section:     SectionMusic,
				VirtualPath: fmt.Sprintf("%s/Track-%04d.flac", prefix, i),
			}
		}
		return paths
	}

	t.Run("W2 Replace publishes only complete old or new sets", func(t *testing.T) {
		oldSet := makeSet("old", 127)
		newSet := makeSet("new", 251)
		n := NewAudioNamespace()
		n.Replace(oldSet)

		const readers = 12
		start := make(chan struct{})
		unexpectedLen := make(chan int, readers)
		var wg sync.WaitGroup
		for i := 0; i < readers; i++ {
			wg.Add(1)
			go func(seed int) {
				defer wg.Done()
				<-start
				for j := 0; j < 8000; j++ {
					_ = n.HasProjection(SectionMusic, fmt.Sprintf("old/Track-%04d.flac", (j+seed)%len(oldSet)))
					if got := n.Len(); got != len(oldSet) && got != len(newSet) {
						unexpectedLen <- got
						return
					}
				}
			}(i)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 500; i++ {
				if i%2 == 0 {
					n.Replace(newSet)
				} else {
					n.Replace(oldSet)
				}
			}
		}()
		close(start)
		wg.Wait()
		close(unexpectedLen)
		for got := range unexpectedLen {
			t.Errorf("Len observed during Replace = %d, want complete old size %d or new size %d", got, len(oldSet), len(newSet))
		}
	})

	t.Run("W2 HasProjection overlaps Add Remove and Replace", func(t *testing.T) {
		base := makeSet("base", 64)
		n := NewAudioNamespace()
		n.Replace(base)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func(seed int) {
				defer wg.Done()
				<-start
				for j := 0; j < 4000; j++ {
					p := base[(j+seed)%len(base)]
					_ = n.HasProjection(p.Section, p.VirtualPath)
				}
			}(i)
		}
		wg.Add(3)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 1000; i++ {
				n.Add(AudioPath{Section: SectionAudiobooks, VirtualPath: fmt.Sprintf("add/Book-%04d.m4b", i%17)})
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 1000; i++ {
				n.Remove(AudioPath{Section: SectionAudiobooks, VirtualPath: fmt.Sprintf("add/Book-%04d.m4b", i%17)})
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 250; i++ {
				n.Replace(base)
			}
		}()
		close(start)
		wg.Wait()
	})
}

func TestAudioNamespace_ReadinessDistinguishesUnpublishedFromPublishedEmpty(t *testing.T) {
	tests := []struct {
		name string
		n    *AudioNamespace
	}{
		{"Z1 fresh constructor is unready", NewAudioNamespace()},
		{"Z1 zero value is unready", &AudioNamespace{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.n.Ready() {
				t.Fatal("fresh namespace Ready = true, want false")
			}
			if got := tt.n.Len(); got != 0 {
				t.Fatalf("fresh namespace Len = %d, want 0", got)
			}

			tt.n.Publish(nil)
			if !tt.n.Ready() {
				t.Fatal("Z2 Publish(nil) Ready = false, want true for a coherent empty namespace")
			}
			if got := tt.n.Len(); got != 0 {
				t.Errorf("Publish(nil) Len = %d, want 0", got)
			}
		})
	}

	t.Run("Z2 explicit empty publish is ready", func(t *testing.T) {
		n := NewAudioNamespace()
		n.Publish([]AudioProjection{})
		if !n.Ready() {
			t.Fatal("Publish(empty) Ready = false, want true")
		}
		if got := n.Len(); got != 0 {
			t.Errorf("Publish(empty) Len = %d, want 0", got)
		}
	})
}

func TestAudioNamespace_PublishAndLookupPreserveCommittedIdentity(t *testing.T) {
	first := AudioProjection{
		Section:     SectionMusic,
		VirtualPath: "Artist/First.flac",
		Hash:        "1111111111111111111111111111111111111111",
		FileIndex:   3,
		Size:        123456,
		MtimeNS:     1700000000000000001,
	}
	second := AudioProjection{
		Section:     SectionMusic,
		VirtualPath: "Artist/Second.flac",
		Hash:        "2222222222222222222222222222222222222222",
		FileIndex:   9,
		Size:        987654,
		MtimeNS:     1700000000000000002,
	}
	n := NewAudioNamespace()
	n.Publish([]AudioProjection{first, second})

	for _, want := range []AudioProjection{first, second} {
		t.Run("Z3_Y2 identity for "+want.VirtualPath, func(t *testing.T) {
			if gotPath := want.Path(); gotPath != (AudioPath{Section: want.Section, VirtualPath: want.VirtualPath}) {
				t.Errorf("AudioProjection.Path() = %#v, want its exact section and virtual path", gotPath)
			}
			got, ok := n.Lookup(want.Section, want.VirtualPath)
			if !ok {
				t.Fatalf("Lookup(%q, %q) did not find published projection", want.Section, want.VirtualPath)
			}
			if got != want {
				t.Errorf("Lookup(%q, %q) = %#v, want every identity field intact: %#v", want.Section, want.VirtualPath, got, want)
			}
		})
	}

	if got, ok := n.Lookup(SectionMusic, "Artist/Absent.flac"); ok {
		t.Errorf("Z3 Lookup of unpublished path = (%#v, true), want false", got)
	}
}

func TestAudioNamespace_LookupPreservesBoundaryValuesAndNestedPath(t *testing.T) {
	tests := []AudioProjection{
		{
			Section: SectionMusic, VirtualPath: "Zero/FileIndex.flac",
			Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FileIndex: 0, Size: 1, MtimeNS: 2,
		},
		{
			Section: SectionMusic, VirtualPath: "Zero/Size.flac",
			Hash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", FileIndex: 4, Size: 0, MtimeNS: 3,
		},
		{
			Section: SectionAudiobooks, VirtualPath: "Empty/Hash.m4b",
			Hash: "", FileIndex: 5, Size: 6, MtimeNS: 7,
		},
		{
			Section: SectionAudiobooks, VirtualPath: "Author/Series/Book/Disc 01/Chapter_01.m4b",
			Hash: "cccccccccccccccccccccccccccccccccccccccc", FileIndex: 8, Size: 9, MtimeNS: 10,
		},
	}
	n := NewAudioNamespace()
	n.Publish(tests)
	for _, want := range tests {
		want := want
		t.Run("X2_X4 "+want.VirtualPath, func(t *testing.T) {
			got, ok := n.Lookup(want.Section, want.VirtualPath)
			if !ok {
				t.Fatalf("Lookup(%q, %q) returned false", want.Section, want.VirtualPath)
			}
			if got != want {
				t.Errorf("Lookup = %#v, want exact boundary projection %#v", got, want)
			}
		})
	}
}

func TestAudioNamespace_MarkUnreadyDropsPublishedAuthority(t *testing.T) {
	p := AudioProjection{
		Section: SectionMusic, VirtualPath: "Artist/Track.flac",
		Hash: "dddddddddddddddddddddddddddddddddddddddd", FileIndex: 2, Size: 100, MtimeNS: 200,
	}
	n := NewAudioNamespace()

	if got, ok := n.Lookup(p.Section, p.VirtualPath); ok {
		t.Fatalf("X1 Lookup on unready namespace = (%#v, true), want false", got)
	}
	n.Publish([]AudioProjection{p})
	if _, ok := n.Lookup(p.Section, p.VirtualPath); !ok {
		t.Fatal("fixture projection was not published")
	}
	n.MarkUnready()
	if n.Ready() {
		t.Error("Z4 Ready after MarkUnready = true, want false")
	}
	if n.HasProjection(p.Section, p.VirtualPath) {
		t.Error("Z4 HasProjection exposed stale projection after MarkUnready")
	}
	if got, ok := n.Lookup(p.Section, p.VirtualPath); ok {
		t.Errorf("Z4 Lookup after MarkUnready = (%#v, true), want false", got)
	}
}

func TestAudioNamespace_HasProjectionAgreesWithLookup(t *testing.T) {
	p := AudioProjection{
		Section: SectionAudiobooks, VirtualPath: "Author/Book.m4b",
		Hash: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", FileIndex: 7, Size: 800, MtimeNS: 900,
	}
	n := NewAudioNamespace()
	tests := []struct {
		name    string
		prepare func()
		section Section
		path    string
	}{
		{"Z5 unready path", func() {}, p.Section, p.VirtualPath},
		{"Z5 published path", func() { n.Publish([]AudioProjection{p}) }, p.Section, p.VirtualPath},
		{"Z5 unpublished path", func() { n.Publish([]AudioProjection{p}) }, p.Section, "Author/Other.m4b"},
		{"Z5 path after MarkUnready", func() { n.Publish([]AudioProjection{p}); n.MarkUnready() }, p.Section, p.VirtualPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n.MarkUnready()
			tt.prepare()
			_, found := n.Lookup(tt.section, tt.path)
			if got := n.HasProjection(tt.section, tt.path); got != found {
				t.Errorf("HasProjection = %v, Lookup found = %v", got, found)
			}
		})
	}
}

func TestAudioNamespace_PublishReplacesWholeSetAndReplaceMarksReady(t *testing.T) {
	first := AudioProjection{
		Section: SectionMusic, VirtualPath: "Artist/First.flac",
		Hash: "1111111111111111111111111111111111111111", FileIndex: 1, Size: 11, MtimeNS: 111,
	}
	second := AudioProjection{
		Section: SectionMusic, VirtualPath: "Artist/Second.flac",
		Hash: "2222222222222222222222222222222222222222", FileIndex: 2, Size: 22, MtimeNS: 222,
	}
	n := NewAudioNamespace()
	n.Publish([]AudioProjection{first})
	n.Publish([]AudioProjection{second})
	if _, ok := n.Lookup(first.Section, first.VirtualPath); ok {
		t.Error("X3 second Publish retained a projection from the first set")
	}
	if got, ok := n.Lookup(second.Section, second.VirtualPath); !ok || got != second {
		t.Errorf("X3 second Publish Lookup = (%#v, %v), want (%#v, true)", got, ok, second)
	}
	if got := n.Len(); got != 1 {
		t.Errorf("X3 Len after second Publish = %d, want 1", got)
	}

	t.Run("Z6 Replace remains compatible and marks ready", func(t *testing.T) {
		legacy := NewAudioNamespace()
		path := AudioPath{Section: SectionAudiobooks, VirtualPath: "Author/Legacy.m4b"}
		legacy.Replace([]AudioPath{path})
		if !legacy.Ready() {
			t.Error("Replace did not mark the complete legacy set ready")
		}
		if !legacy.HasProjection(path.Section, path.VirtualPath) || legacy.Len() != 1 {
			t.Errorf("Replace legacy behavior changed: HasProjection = %v, Len = %d", legacy.HasProjection(path.Section, path.VirtualPath), legacy.Len())
		}
	})
}

func TestAudioNamespace_AddProjectionPreservesIdentity(t *testing.T) {
	p := AudioProjection{
		Section: SectionMusic, VirtualPath: "Artist/Added.flac",
		Hash: "ffffffffffffffffffffffffffffffffffffffff", FileIndex: 13, Size: 1400, MtimeNS: 1500,
	}
	n := NewAudioNamespace()
	n.Publish(nil)
	n.AddProjection(p)
	got, ok := n.Lookup(p.Section, p.VirtualPath)
	if !ok {
		t.Fatal("Z7 Lookup did not find projection added with AudioProjection")
	}
	if got != p {
		t.Errorf("Z7 Lookup after Add = %#v, want identity intact %#v", got, p)
	}
	n.Remove(p.Path())
	if _, ok := n.Lookup(p.Section, p.VirtualPath); ok {
		t.Error("Z7 Remove(AudioPath) left added projection present")
	}
}

func TestAudioNamespace_AccessorsUseNoFilesystemState(t *testing.T) {
	p := AudioProjection{
		Section: SectionMusic, VirtualPath: "definitely/does/not/exist/Track.flac",
		Hash: "0123456789abcdef0123456789abcdef01234567", FileIndex: 3, Size: 4, MtimeNS: 5,
	}
	n := NewAudioNamespace()
	n.Publish([]AudioProjection{p})
	if !n.Ready() {
		t.Fatal("Y3 in-memory namespace is not ready after Publish")
	}
	got, ok := n.Lookup(p.Section, p.VirtualPath)
	if !ok || got != p {
		t.Errorf("Y3 Lookup for nonexistent physical path = (%#v, %v), want (%#v, true)", got, ok, p)
	}
	if !n.HasProjection(p.Section, p.VirtualPath) {
		t.Error("Y3 HasProjection for nonexistent physical path = false, want true")
	}
}

func TestClassifyProjection_ReadinessDoesNotChangeClassification(t *testing.T) {
	source := filepath.Join(string(filepath.Separator), "srv", "library")
	path := filepath.Join(source, "music", "Artist", "Track.flac")
	unready := NewAudioNamespace()
	readyEmpty := NewAudioNamespace()
	readyEmpty.Publish(nil)

	if unready.Ready() || !readyEmpty.Ready() {
		t.Fatalf("Y5 fixtures do not distinguish readiness: unready=%v readyEmpty=%v", unready.Ready(), readyEmpty.Ready())
	}
	gotUnready := ClassifyProjection(source, path, unready)
	gotReadyEmpty := ClassifyProjection(source, path, readyEmpty)
	if gotUnready != gotReadyEmpty {
		t.Errorf("Y5 ClassifyProjection differs by readiness: unready=%#v ready-empty=%#v", gotUnready, gotReadyEmpty)
	}
	want := VFSClass{Section: SectionMusic}
	if gotUnready != want {
		t.Errorf("ClassifyProjection for absent ownership = %#v, want %#v", gotUnready, want)
	}
}

func TestAudioNamespace_ConcurrentPublishIsAtomicWithReadinessAndIdentity(t *testing.T) {
	makeProjections := func(prefix string, count int) []AudioProjection {
		projections := make([]AudioProjection, count)
		for i := range projections {
			projections[i] = AudioProjection{
				Section:     SectionMusic,
				VirtualPath: fmt.Sprintf("%s/Track-%04d.flac", prefix, i),
				Hash:        fmt.Sprintf("%040x", i+1),
				FileIndex:   i,
				Size:        int64(i + 100),
				MtimeNS:     int64(i + 1000),
			}
		}
		return projections
	}

	t.Run("Y1 unready to ready publishes content and readiness together", func(t *testing.T) {
		projections := makeProjections("initial", 4096)
		n := NewAudioNamespace()
		const readers = 12
		start := make(chan struct{})
		errors := make(chan string, readers)
		var primed sync.WaitGroup
		var readersDone sync.WaitGroup
		primed.Add(readers)
		readersDone.Add(readers)
		for i := 0; i < readers; i++ {
			go func(seed int) {
				defer readersDone.Done()
				target := projections[(seed*313)%len(projections)]
				if n.Ready() {
					errors <- "namespace was ready before Publish"
					primed.Done()
					return
				}
				if _, ok := n.Lookup(target.Section, target.VirtualPath); ok {
					errors <- "Lookup exposed content before Publish"
					primed.Done()
					return
				}
				primed.Done()
				<-start
				for j := 0; j < 3000; j++ {
					if n.Ready() {
						if got := n.Len(); got != len(projections) {
							errors <- fmt.Sprintf("Ready namespace Len = %d, want complete set %d", got, len(projections))
							return
						}
						got, ok := n.Lookup(target.Section, target.VirtualPath)
						if !ok || got != target {
							errors <- fmt.Sprintf("Ready namespace Lookup = (%#v, %v), want (%#v, true)", got, ok, target)
							return
						}
					}
					if got, ok := n.Lookup(target.Section, target.VirtualPath); ok {
						if got != target {
							errors <- fmt.Sprintf("Lookup identity = %#v, want %#v", got, target)
							return
						}
						if !n.Ready() {
							errors <- "Lookup exposed the new set while Ready was still false"
							return
						}
					}
				}
			}(i)
		}
		primed.Wait()
		close(start)
		n.Publish(projections)
		readersDone.Wait()
		close(errors)
		for err := range errors {
			t.Error(err)
		}
	})

	t.Run("Y4 repeated Publish exposes only complete old or new sets", func(t *testing.T) {
		oldSet := makeProjections("old", 127)
		newSet := makeProjections("new", 251)
		n := NewAudioNamespace()
		n.Publish(oldSet)
		const readers = 12
		start := make(chan struct{})
		errors := make(chan string, readers)
		var wg sync.WaitGroup
		for i := 0; i < readers; i++ {
			wg.Add(1)
			go func(seed int) {
				defer wg.Done()
				<-start
				for j := 0; j < 8000; j++ {
					if !n.Ready() {
						errors <- "Ready became false during ready-to-ready Publish"
						return
					}
					if got := n.Len(); got != len(oldSet) && got != len(newSet) {
						errors <- fmt.Sprintf("Len during Publish = %d, want %d or %d", got, len(oldSet), len(newSet))
						return
					}
					target := oldSet[(j+seed)%len(oldSet)]
					if got, ok := n.Lookup(target.Section, target.VirtualPath); ok && got != target {
						errors <- fmt.Sprintf("Lookup returned torn identity %#v, want %#v", got, target)
						return
					}
				}
			}(i)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 500; i++ {
				if i%2 == 0 {
					n.Publish(newSet)
				} else {
					n.Publish(oldSet)
				}
			}
		}()
		close(start)
		wg.Wait()
		close(errors)
		for err := range errors {
			t.Error(err)
		}
	})

	t.Run("Y4 accessors race safely with Publish and MarkUnready", func(t *testing.T) {
		set := makeProjections("cycle", 64)
		n := NewAudioNamespace()
		n.Publish(set)
		start := make(chan struct{})
		errors := make(chan string, 8)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(seed int) {
				defer wg.Done()
				<-start
				target := set[seed%len(set)]
				for j := 0; j < 4000; j++ {
					_ = n.Ready()
					_ = n.Len()
					_ = n.HasProjection(target.Section, target.VirtualPath)
					if got, ok := n.Lookup(target.Section, target.VirtualPath); ok && got != target {
						errors <- fmt.Sprintf("Lookup returned corrupt identity %#v, want %#v", got, target)
						return
					}
				}
			}(i)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 1000; i++ {
				n.MarkUnready()
				n.Publish(set)
			}
		}()
		close(start)
		wg.Wait()
		close(errors)
		for err := range errors {
			t.Error(err)
		}
	})
}

const namespaceWaitTestTimeout = time.Second

func startNamespaceWait(n *AudioNamespace, ctx context.Context) (<-chan struct{}, <-chan error) {
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		result <- n.WaitServable(ctx)
	}()
	return started, result
}

func awaitNamespaceWait(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(namespaceWaitTestTimeout):
		t.Fatal("WaitServable did not return before timeout")
		return nil
	}
}

func awaitNamespaceWaitStarted(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(namespaceWaitTestTimeout):
		t.Fatal("WaitServable goroutine did not start before timeout")
	}
}

func assertNamespaceWaitBlocked(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		t.Fatalf("WaitServable returned while namespace should block: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
}

func TestAudioNamespace_UnreconciledBlocksUntilPublish(t *testing.T) {
	tests := []struct {
		name string
		n    *AudioNamespace
	}{
		{"S1 constructor starts unreconciled", NewAudioNamespace()},
		{"S1 zero value starts unreconciled", &AudioNamespace{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.n.State(); got != Unreconciled {
				t.Fatalf("fresh namespace State = %v, want Unreconciled", got)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started, result := startNamespaceWait(tt.n, ctx)
			awaitNamespaceWaitStarted(t, started)
			assertNamespaceWaitBlocked(t, result)

			tt.n.Publish(nil)
			if err := awaitNamespaceWait(t, result); err != nil {
				t.Errorf("S2 blocked WaitServable after Publish = %v, want nil", err)
			}
			if got := tt.n.State(); got != Ready {
				t.Errorf("State after Publish = %v, want Ready", got)
			}
			if !tt.n.Ready() {
				t.Error("Ready after Publish = false, want true")
			}
		})
	}
}

func TestAudioNamespace_UnavailableIsServableWithoutBeingReady(t *testing.T) {
	p := AudioProjection{
		Section: SectionMusic, VirtualPath: "Artist/Stale.flac",
		Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", FileIndex: 2, Size: 3, MtimeNS: 4,
	}
	n := NewAudioNamespace()
	n.Publish([]AudioProjection{p})
	n.MarkUnavailable()

	if got := n.State(); got != Unavailable {
		t.Fatalf("S3 State after MarkUnavailable = %v, want Unavailable", got)
	}
	if n.Ready() {
		t.Error("S3 Ready after MarkUnavailable = true, want false")
	}
	if got, ok := n.Lookup(p.Section, p.VirtualPath); ok {
		t.Errorf("S3 Lookup after MarkUnavailable = (%#v, true), want no stale authority", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, result := startNamespaceWait(n, ctx)
	if err := awaitNamespaceWait(t, result); err != nil {
		t.Errorf("S3 WaitServable for Unavailable = %v, want nil", err)
	}
}

func TestAudioNamespace_FailedIsTerminalAndDistinctFromContext(t *testing.T) {
	sentinel := errors.New("audio reconciliation failed")
	n := NewAudioNamespace()
	n.MarkFailed(sentinel)
	if got := n.State(); got != Failed {
		t.Fatalf("S4 State after MarkFailed = %v, want Failed", got)
	}
	if n.Ready() {
		t.Error("S4 Ready after MarkFailed = true, want false")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, result := startNamespaceWait(n, ctx)
	err := awaitNamespaceWait(t, result)
	if err == nil {
		t.Fatal("S4 WaitServable for Failed = nil, want terminal error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("WaitServable error = %v, want it to preserve %v", err, sentinel)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("terminal reconciliation error is indistinguishable from ctx.Err(): %v", err)
	}
}

func TestAudioNamespace_WaitServableHonorsContext(t *testing.T) {
	t.Run("S5 cancellation wakes blocked waiter", func(t *testing.T) {
		n := NewAudioNamespace()
		ctx, cancel := context.WithCancel(context.Background())
		started, result := startNamespaceWait(n, ctx)
		awaitNamespaceWaitStarted(t, started)
		assertNamespaceWaitBlocked(t, result)
		cancel()
		if err := awaitNamespaceWait(t, result); err != context.Canceled {
			t.Errorf("WaitServable after cancellation = %v, want %v", err, context.Canceled)
		}
	})

	t.Run("S5 deadline wakes blocked waiter", func(t *testing.T) {
		n := NewAudioNamespace()
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, result := startNamespaceWait(n, ctx)
		if err := awaitNamespaceWait(t, result); err != context.DeadlineExceeded {
			t.Errorf("WaitServable after deadline = %v, want %v", err, context.DeadlineExceeded)
		}
	})
}

func TestAudioNamespace_EmptyReadyDiffersFromUnreconciled(t *testing.T) {
	n := NewAudioNamespace()
	if got := n.State(); got != Unreconciled {
		t.Fatalf("S6 empty fresh namespace State = %v, want Unreconciled", got)
	}
	if n.Ready() {
		t.Fatal("S6 empty fresh namespace Ready = true, want false")
	}

	n.Publish([]AudioProjection{})
	if got := n.State(); got != Ready {
		t.Errorf("S6 empty published namespace State = %v, want Ready", got)
	}
	if !n.Ready() {
		t.Error("S6 empty published namespace Ready = false, want true")
	}
	if got := n.Len(); got != 0 {
		t.Errorf("empty published namespace Len = %d, want 0", got)
	}
	_, result := startNamespaceWait(n, context.Background())
	if err := awaitNamespaceWait(t, result); err != nil {
		t.Errorf("WaitServable for genuinely empty Ready namespace = %v, want nil", err)
	}
}

func TestAudioNamespace_MarkUnreadyBlocksAgain(t *testing.T) {
	p := AudioProjection{
		Section: SectionAudiobooks, VirtualPath: "Author/Book.m4b",
		Hash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", FileIndex: 5, Size: 6, MtimeNS: 7,
	}
	n := NewAudioNamespace()
	n.Publish([]AudioProjection{p})
	n.MarkUnready()
	if got := n.State(); got != Unreconciled {
		t.Fatalf("S7 State after MarkUnready = %v, want Unreconciled", got)
	}
	if _, ok := n.Lookup(p.Section, p.VirtualPath); ok {
		t.Error("S7 MarkUnready retained stale published projection")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, result := startNamespaceWait(n, ctx)
	awaitNamespaceWaitStarted(t, started)
	assertNamespaceWaitBlocked(t, result)
	n.Publish(nil)
	if err := awaitNamespaceWait(t, result); err != nil {
		t.Errorf("blocked waiter after re-Publish = %v, want nil", err)
	}
}

func TestAudioNamespace_PublishWakesAllConcurrentWaiters(t *testing.T) {
	const waiters = 64
	n := NewAudioNamespace()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan error, waiters)
	var entered sync.WaitGroup
	entered.Add(waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			entered.Done()
			results <- n.WaitServable(ctx)
		}()
	}
	allEntered := make(chan struct{})
	go func() {
		entered.Wait()
		close(allEntered)
	}()
	select {
	case <-allEntered:
	case <-time.After(namespaceWaitTestTimeout):
		t.Fatal("S8 waiters did not all enter before timeout")
	}
	select {
	case err := <-results:
		t.Fatalf("S8 waiter returned before Publish: %v", err)
	case <-time.After(75 * time.Millisecond):
	}

	n.Publish(nil)
	deadline := time.After(2 * namespaceWaitTestTimeout)
	for i := 0; i < waiters; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Errorf("S8 waiter %d after Publish = %v, want nil", i, err)
			}
		case <-deadline:
			t.Fatalf("S8 only %d of %d waiters woke after one Publish", i, waiters)
		}
	}
}

func TestAudioNamespace_ReadyWinsOverCanceledContext(t *testing.T) {
	n := NewAudioNamespace()
	n.Publish(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, result := startNamespaceWait(n, ctx)
	if err := awaitNamespaceWait(t, result); err != nil {
		t.Errorf("S9 WaitServable for Ready with canceled context = %v, want nil", err)
	}
}

func TestAudioNamespace_RepeatedTransitionsWakeOnceWithoutPanic(t *testing.T) {
	n := NewAudioNamespace()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started, first := startNamespaceWait(n, ctx)
	awaitNamespaceWaitStarted(t, started)
	assertNamespaceWaitBlocked(t, first)
	n.Publish(nil)
	if err := awaitNamespaceWait(t, first); err != nil {
		t.Fatalf("S10 first waiter after Publish = %v, want nil", err)
	}
	// Publishing an already-ready namespace must not close the same wake channel
	// again or manufacture a second completion for the waiter that already left.
	n.Publish(nil)
	n.Publish(nil)
	select {
	case err := <-first:
		t.Fatalf("S10 first waiter completed more than once: %v", err)
	default:
	}

	n.MarkUnready()
	started, second := startNamespaceWait(n, ctx)
	awaitNamespaceWaitStarted(t, started)
	assertNamespaceWaitBlocked(t, second)
	n.MarkUnavailable()
	if err := awaitNamespaceWait(t, second); err != nil {
		t.Errorf("S10 second waiter after MarkUnavailable = %v, want nil", err)
	}
	// Repeating a terminal transition must also be safe for an already-closed
	// generation, while a later MarkUnready creates a fresh blocking generation.
	n.MarkUnavailable()
	n.MarkUnready()
	started, third := startNamespaceWait(n, ctx)
	awaitNamespaceWaitStarted(t, started)
	assertNamespaceWaitBlocked(t, third)
	sentinel := errors.New("third generation failed")
	n.MarkFailed(sentinel)
	if err := awaitNamespaceWait(t, third); !errors.Is(err, sentinel) {
		t.Errorf("S10 third waiter after MarkFailed = %v, want %v", err, sentinel)
	}
}
