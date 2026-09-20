package library

import (
	"path/filepath"
	"sync"
	"testing"
)

func TestClassifyPath_SectionDispatch(t *testing.T) {
	source := filepath.Join("srv", "library")
	tests := []struct {
		name string
		path string
		want VFSClass
	}{
		// D1: video stubs retain their section and are never audio.
		{"D1/movie directly under movies", filepath.Join(source, "movies", "Film.mkv"), VFSClass{Section: SectionMovies, Stub: true}},
		{"D1/episode directly under TV", filepath.Join(source, "tv", "Episode.mkv"), VFSClass{Section: SectionTV, Stub: true}},

		// D2-D3: each audio section has its own allowlist, at arbitrary depth.
		{"D2/music FLAC directly in section", filepath.Join(source, "music", "Track.flac"), VFSClass{Section: SectionMusic, Stub: true, Audio: true}},
		{"D2/music FLAC at deep nesting", filepath.Join(source, "music", "Artist", "Album", "Disc", "Track.flac"), VFSClass{Section: SectionMusic, Stub: true, Audio: true}},
		{"D3/audiobook M4B", filepath.Join(source, "audiobooks", "Author", "Book.m4b"), VFSClass{Section: SectionAudiobooks, Stub: true, Audio: true}},
		{"D3/audiobook M4A", filepath.Join(source, "audiobooks", "Author", "Book.m4a"), VFSClass{Section: SectionAudiobooks, Stub: true, Audio: true}},
		{"D3/audiobook MP3", filepath.Join(source, "audiobooks", "Author", "Book.mp3"), VFSClass{Section: SectionAudiobooks, Stub: true, Audio: true}},

		// D4 and E2-E3: being in a section does not widen its allowlist, and a
		// rejected projection must not claim audio treatment.
		{"D4/movies reject FLAC", filepath.Join(source, "movies", "Track.flac"), VFSClass{Section: SectionMovies}},
		{"D4/music rejects MKV", filepath.Join(source, "music", "Film.mkv"), VFSClass{Section: SectionMusic}},
		{"D4/music rejects MP3", filepath.Join(source, "music", "Track.mp3"), VFSClass{Section: SectionMusic}},
		{"D4/audiobooks reject FLAC", filepath.Join(source, "audiobooks", "Book.flac"), VFSClass{Section: SectionAudiobooks}},

		// D5: audio is case-insensitive while the legacy video rule is not.
		{"D5/music accepts uppercase FLAC", filepath.Join(source, "music", "Track.FLAC"), VFSClass{Section: SectionMusic, Stub: true, Audio: true}},
		{"D5/audiobooks accept uppercase M4B", filepath.Join(source, "audiobooks", "Book.M4B"), VFSClass{Section: SectionAudiobooks, Stub: true, Audio: true}},
		{"D5/audiobooks accept mixed-case Mp3", filepath.Join(source, "audiobooks", "Book.Mp3"), VFSClass{Section: SectionAudiobooks, Stub: true, Audio: true}},
		{"D5/movies reject uppercase MKV", filepath.Join(source, "movies", "Film.MKV"), VFSClass{Section: SectionMovies}},

		// D6: only a dot-leading final component is excluded for audio.
		{"D6/music rejects hidden final component", filepath.Join(source, "music", "Artist", ".Track.flac"), VFSClass{Section: SectionMusic}},
		{"D6/audiobooks reject hidden final component", filepath.Join(source, "audiobooks", ".Book.m4b"), VFSClass{Section: SectionAudiobooks}},
		{"D6/video keeps hidden MKV compatibility", filepath.Join(source, "movies", ".Film.mkv"), VFSClass{Section: SectionMovies, Stub: true}},
		{"D6/hidden audio directory does not hide ordinary final component", filepath.Join(source, "music", ".staging", "Track.flac"), VFSClass{Section: SectionMusic, Stub: true, Audio: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyPath(source, tt.path); got != tt.want {
				t.Errorf("ClassifyPath(%q, %q) = %#v, want %#v", source, tt.path, got, tt.want)
			}
		})
	}
}

func TestClassifyPath_PreservesGlobalLowercaseMKVCompatibility(t *testing.T) {
	source := filepath.Join(string(filepath.Separator), "srv", "library")
	tests := []struct {
		name string
		path string
	}{
		{"E1/sibling directory inside source", filepath.Join(source, "extras", "Film.mkv")},
		{"E1/file in source root", filepath.Join(source, "Film.mkv")},
		{"E1/unrelated absolute path", filepath.Join(string(filepath.Separator), "archive", "Film.mkv")},
		{"E1/deep path under unrelated root", filepath.Join(string(filepath.Separator), "other", "deep", "layout", "Film.mkv")},
		{"F3/prefix sibling tvshows is not TV but remains global MKV stub", filepath.Join(source, "tvshows", "Episode.mkv")},
	}

	want := VFSClass{Stub: true}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyPath(source, tt.path); got != want {
				t.Errorf("ClassifyPath(%q, %q) = %#v, want %#v", source, tt.path, got, want)
			}
		})
	}
}

func TestClassifyPath_BoundariesAndCleaning(t *testing.T) {
	source := filepath.Join(string(filepath.Separator), "srv", "library")
	// Deliberately retain a raw separator so ClassifyPath, rather than this
	// fixture's filepath.Join call, receives and cleans the traversal.
	outsideTraversal := source + string(filepath.Separator) + filepath.Join("music", "..", "..", "outside", "Film.mkv")
	tests := []struct {
		name       string
		sourcePath string
		fullPath   string
		want       VFSClass
	}{
		{"D7/nested directory path without extension", source, filepath.Join(source, "music", "Artist", "Album"), VFSClass{Section: SectionMusic}},
		{"D7/path with no extension", source, filepath.Join(source, "movies", "Film"), VFSClass{Section: SectionMovies}},
		{"D7/music section directory itself", source, filepath.Join(source, "music"), VFSClass{}},
		{"D7/movies section directory itself", source, filepath.Join(source, "movies"), VFSClass{}},
		{"F1/empty source and full path", "", "", VFSClass{}},
		{"F1/empty full path", source, "", VFSClass{}},
		{"F1/empty source still preserves global MKV", "", filepath.Join("somewhere", "Film.mkv"), VFSClass{Stub: true}},
		{"F3/musicians prefix sibling is not music", source, filepath.Join(source, "musicians", "Track.flac"), VFSClass{}},
		{"F4/traversal cleaned outside source keeps global MKV rule", source, outsideTraversal, VFSClass{Stub: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyPath(tt.sourcePath, tt.fullPath); got != tt.want {
				t.Errorf("ClassifyPath(%q, %q) = %#v, want %#v", tt.sourcePath, tt.fullPath, got, tt.want)
			}
		})
	}

	t.Run("F2/trailing source separator is equivalent", func(t *testing.T) {
		path := filepath.Join(source, "music", "Artist", "Track.flac")
		without := ClassifyPath(source, path)
		// Deliberately append the platform separator: filepath.Join cleans it away.
		with := ClassifyPath(source+string(filepath.Separator), path)
		if with != without {
			t.Errorf("ClassifyPath with trailing source separator = %#v, without = %#v", with, without)
		}
		want := VFSClass{Section: SectionMusic, Stub: true, Audio: true}
		if with != want {
			t.Errorf("ClassifyPath trailing-separator result = %#v, want %#v", with, want)
		}
	})
}

func TestClassifyPath_ComposesWithExistingHelpersWithoutFilesystemState(t *testing.T) {
	source := filepath.Join(string(filepath.Separator), "path", "that", "need", "not", "exist")
	tests := []struct {
		name string
		path string
		want VFSClass
	}{
		{"E4_E5/missing music path", filepath.Join(source, "music", "Artist", "Track.flac"), VFSClass{Section: SectionMusic, Stub: true, Audio: true}},
		{"E4_E5/missing non-stub in audiobooks", filepath.Join(source, "audiobooks", "Author", "Book.flac"), VFSClass{Section: SectionAudiobooks}},
		{"E4_E5/missing video path", filepath.Join(source, "tv", "Show", "Episode.mkv"), VFSClass{Section: SectionTV, Stub: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyPath(source, tt.path)
			if got != tt.want {
				t.Fatalf("ClassifyPath(%q, %q) = %#v, want %#v", source, tt.path, got, tt.want)
			}

			section, ok := SectionForPath(source, tt.path)
			if !ok {
				t.Fatalf("SectionForPath(%q, %q) did not recognize test fixture section", source, tt.path)
			}
			if got.Section != section {
				t.Errorf("ClassifyPath section = %q, SectionForPath section = %q", got.Section, section)
			}
			if got.Stub != IsVirtualStub(section, tt.path) {
				t.Errorf("ClassifyPath stub = %v, IsVirtualStub(%q, %q) = %v", got.Stub, section, tt.path, IsVirtualStub(section, tt.path))
			}
		})
	}
}

func TestClassifyPath_ConcurrentCallsAreDeterministic(t *testing.T) {
	const (
		goroutines = 32
		iterations = 200
	)
	source := filepath.Join(string(filepath.Separator), "srv", "library")
	tests := []struct {
		name string
		path string
		want VFSClass
	}{
		{"E6/audio stub", filepath.Join(source, "music", "Artist", "Track.FLAC"), VFSClass{Section: SectionMusic, Stub: true, Audio: true}},
		{"E6/video stub outside sections", filepath.Join(string(filepath.Separator), "legacy", "Film.mkv"), VFSClass{Stub: true}},
		{"E6/rejected extension", filepath.Join(source, "audiobooks", "Book.flac"), VFSClass{Section: SectionAudiobooks}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			mismatches := make(chan VFSClass, goroutines)
			var wg sync.WaitGroup
			for i := 0; i < goroutines; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for j := 0; j < iterations; j++ {
						if got := ClassifyPath(source, tt.path); got != tt.want {
							mismatches <- got
							return
						}
					}
				}()
			}
			wg.Wait()
			close(mismatches)
			for got := range mismatches {
				t.Errorf("concurrent ClassifyPath(%q, %q) = %#v, want %#v", source, tt.path, got, tt.want)
			}
		})
	}
}
