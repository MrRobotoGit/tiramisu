package library

import (
	"path/filepath"
	"testing"
)

func TestUsesSSDWarmupPolicy(t *testing.T) {
	// W1, W2: only the two established video sections use movie-style SSD
	// warmup. Unknown values must fail closed rather than inherit movie policy.
	tests := []struct {
		name string
		in   Section
		want bool
	}{
		{"W1/movies_warm", SectionMovies, true},
		{"W1/tv_warms", SectionTV, true},
		{"W1/music_bypasses_warmup", SectionMusic, false},
		{"W1/audiobooks_bypass_warmup", SectionAudiobooks, false},
		{"W2/empty_section_does_not_warm", Section(""), false},
		{"W2/unknown_section_does_not_warm", Section("video"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UsesSSDWarmup(tt.in); got != tt.want {
				t.Errorf("UsesSSDWarmup(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestSectionForPathMapsKnownSections(t *testing.T) {
	// W3 and W1 composed: this is the call-site contract--resolve a path, then
	// select warmup policy from the returned section. None of these paths exist.
	source := filepath.Join(string(filepath.Separator), "__tiramisu_warmup_contract__", "source")
	tests := []struct {
		name     string
		dir      string
		file     string
		want     Section
		wantWarm bool
	}{
		{"W3/movies", "movies", "x.mkv", SectionMovies, true},
		{"W3/tv", "tv", "x.mkv", SectionTV, true},
		{"W3/music", "music", "a.flac", SectionMusic, false},
		{"W3/audiobooks", "audiobooks", "a.m4b", SectionAudiobooks, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fullPath := filepath.Join(source, tt.dir, tt.file)
			got, ok := SectionForPath(source, fullPath)
			if !ok {
				t.Fatalf("SectionForPath(%q, %q) ok = false, want true", source, fullPath)
			}
			if got != tt.want {
				t.Fatalf("SectionForPath(%q, %q) = %q, want %q", source, fullPath, got, tt.want)
			}
			if warm := UsesSSDWarmup(got); warm != tt.wantWarm {
				t.Errorf("UsesSSDWarmup(SectionForPath(...)) = %v, want %v", warm, tt.wantWarm)
			}
		})
	}
}

func TestSectionForPathResolvesDeepAudioPathsWithoutFilesystemAccess(t *testing.T) {
	// W4, X3: success for never-created, deeply nested paths proves the answer is
	// lexical and does not depend on statting the source, section, or file.
	source := filepath.Join(string(filepath.Separator), "__tiramisu_absent_source__")
	tests := []struct {
		name       string
		components []string
		want       Section
	}{
		{
			"W4_X3/music_space_and_dot_components",
			[]string{"music", "Artist Name", "Album.v1", "Disc 1", "01 - Track_a1b2c3d4.flac"},
			SectionMusic,
		},
		{
			"W4_X3/audiobook_space_and_dot_components",
			[]string{"audiobooks", "Author Name", "Series.v2", "Book 1", "Chapter 01_a1b2c3d4.m4b"},
			SectionAudiobooks,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fullPath := filepath.Join(append([]string{source}, tt.components...)...)
			got, ok := SectionForPath(source, fullPath)
			if !ok || got != tt.want {
				t.Errorf("SectionForPath(%q, %q) = (%q, %v), want (%q, true)", source, fullPath, got, ok, tt.want)
			}
		})
	}
}

func TestSectionForPathRejectsPathsOutsideSections(t *testing.T) {
	// W5: being under the configured source is insufficient; a file must be
	// strictly below one of the four known section components.
	source := filepath.Join(string(filepath.Separator), "srv", "tiramisu-source")
	tests := []struct {
		name string
		path string
	}{
		{"W5/unknown_section_sibling", filepath.Join(source, "other", "x.mkv")},
		{"W5/file_directly_in_source", filepath.Join(source, "x.mkv")},
		{"W5/source_root_itself", source},
		{"W5/path_above_source", filepath.Join(filepath.Dir(source), "x.mkv")},
		{"W5/unrelated_absolute_path", filepath.Join(string(filepath.Separator), "unrelated", "music", "x.flac")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, ok := SectionForPath(source, tt.path); ok {
				t.Errorf("SectionForPath(%q, %q) = (%q, true), want (_, false)", source, tt.path, got)
			}
		})
	}
}

func TestSectionForPathRejectsSectionDirectories(t *testing.T) {
	// W6: a section root names a directory, not a projected path within it.
	source := filepath.Join(string(filepath.Separator), "srv", "tiramisu-source")
	for _, section := range []Section{SectionMovies, SectionTV, SectionMusic, SectionAudiobooks} {
		t.Run("W6/"+string(section), func(t *testing.T) {
			path := filepath.Join(source, string(section))
			if got, ok := SectionForPath(source, path); ok {
				t.Errorf("SectionForPath(%q, %q) = (%q, true), want (_, false)", source, path, got)
			}
		})
	}
}

func TestSectionForPathUsesComponentBoundaries(t *testing.T) {
	// X1: each adversarial sibling shares a textual prefix with a real section;
	// strings.HasPrefix classification would incorrectly accept these paths.
	source := filepath.Join(string(filepath.Separator), "srv", "tiramisu-source")
	for _, sibling := range []string{"musicians", "music-extra", "tvshows", "audiobooks2", "movies-extra"} {
		t.Run("X1/rejects_"+sibling, func(t *testing.T) {
			path := filepath.Join(source, sibling, "x.flac")
			if got, ok := SectionForPath(source, path); ok {
				t.Errorf("SectionForPath(%q, %q) = (%q, true), want (_, false)", source, path, got)
			}
		})
	}
}

func TestSectionForPathClassifiesTheCleanedTraversalDestination(t *testing.T) {
	// X2: this suite chooses the brief's allowed clean-and-classify behavior.
	// Classification must describe the lexical destination, never the component
	// that happened to precede a traversal. Raw native separators are deliberate
	// here because filepath.Join would remove the traversal before the call.
	source := filepath.Join(string(filepath.Separator), "srv", "tiramisu-source")
	separator := string(filepath.Separator)
	tests := []struct {
		name   string
		path   string
		want   Section
		wantOK bool
	}{
		{
			"X2/traversal_out_of_music_is_not_music",
			source + separator + "music" + separator + ".." + separator + "other" + separator + "x.flac",
			"",
			false,
		},
		{
			"X2/traversal_into_music_resolves_to_music",
			source + separator + "other" + separator + ".." + separator + "music" + separator + "x.flac",
			SectionMusic,
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := SectionForPath(source, tt.path)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("SectionForPath(%q, %q) = (%q, %v), want (%q, %v)", source, tt.path, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestExistingLibraryPathHelpersRemainCompatible(t *testing.T) {
	// X4: the new classifier and policy must not repurpose the existing
	// movie/TV-only helpers. These assertions record their current path behavior.
	root := filepath.Join(string(filepath.Separator), "srv", "legacy-library")
	movies := filepath.Join(root, "movies")
	tv := filepath.Join(root, "tv")
	m := &Manager{cfg: Config{
		MoviesDir: movies + string(filepath.Separator) + ".",
		TVDir:     tv + string(filepath.Separator) + ".",
	}}

	t.Run("X4/kindOf_remains_tv_or_movie_only", func(t *testing.T) {
		for _, tt := range []struct {
			name string
			path string
			want string
		}{
			{"tv_descendant", filepath.Join(tv, "Show", "episode.mkv"), "tv"},
			{"movie_descendant", filepath.Join(movies, "film.mkv"), "movie"},
			{"audio_path_keeps_legacy_movie_fallback", filepath.Join(root, "music", "track.flac"), "movie"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				if got := m.kindOf(tt.path); got != tt.want {
					t.Errorf("kindOf(%q) = %q, want %q", tt.path, got, tt.want)
				}
			})
		}
	})

	t.Run("X4/mediaDirs_remains_clean_ordered_video_roots", func(t *testing.T) {
		got := m.mediaDirs()
		want := []string{movies, tv}
		if len(got) != len(want) {
			t.Fatalf("mediaDirs() = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("mediaDirs()[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("X4/fusePath_remains_video_relative", func(t *testing.T) {
		path := filepath.Join(tv, "Show", "episode.mkv")
		want := filepath.ToSlash(filepath.Join("tv", "Show", "episode.mkv"))
		if got := m.fusePath(path); got != want {
			t.Errorf("fusePath(%q) = %q, want %q", path, got, want)
		}
		outside := filepath.Join(root, "music", "track.flac")
		if got, want := m.fusePath(outside), filepath.ToSlash(outside); got != want {
			t.Errorf("fusePath(%q) = %q, want unchanged %q", outside, got, want)
		}
	})

	t.Run("X4/insideMediaDirs_keeps_component_containment", func(t *testing.T) {
		if !m.insideMediaDirs(filepath.Join(movies, "film.mkv")) {
			t.Error("insideMediaDirs rejected a movie descendant")
		}
		if m.insideMediaDirs(filepath.Join(root, "movies-extra", "film.mkv")) {
			t.Error("insideMediaDirs accepted a prefix-sharing sibling")
		}
	})
}

func TestSectionForPathRejectsEmptyAndUnrelatedRelativeInputs(t *testing.T) {
	// Y1, Y3: malformed or unrelated inputs fail closed and do not panic.
	source := filepath.Join(string(filepath.Separator), "srv", "tiramisu-source")
	tests := []struct {
		name       string
		sourcePath string
		fullPath   string
	}{
		{"Y1/empty_source", "", filepath.Join(source, "music", "x.flac")},
		{"Y1/empty_full_path", source, ""},
		{"Y1/both_empty", "", ""},
		{"Y3/relative_full_path", source, filepath.Join("music", "x.flac")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, ok := SectionForPath(tt.sourcePath, tt.fullPath); ok {
				t.Errorf("SectionForPath(%q, %q) = (%q, true), want (_, false)", tt.sourcePath, tt.fullPath, got)
			}
		})
	}
}

func TestSectionForPathAcceptsTrailingSourceSeparator(t *testing.T) {
	// Y2: appending a raw native separator is deliberate; filepath.Join would
	// clean it away and fail to exercise this boundary.
	source := filepath.Join(string(filepath.Separator), "srv", "tiramisu-source")
	fullPath := filepath.Join(source, "music", "x.flac")
	without, okWithout := SectionForPath(source, fullPath)
	with, okWith := SectionForPath(source+string(filepath.Separator), fullPath)
	if !okWithout || without != SectionMusic {
		t.Fatalf("fixture baseline SectionForPath(%q, %q) = (%q, %v), want (%q, true)", source, fullPath, without, okWithout, SectionMusic)
	}
	if with != without || okWith != okWithout {
		t.Errorf("trailing separator changed result: without = (%q, %v), with = (%q, %v)", without, okWithout, with, okWith)
	}
}

func TestSectionForPathMatchingIsCaseSensitive(t *testing.T) {
	// Y4: differently-cased components are separate namespaces.
	source := filepath.Join(string(filepath.Separator), "srv", "tiramisu-source")
	for _, component := range []string{"Movies", "TV", "Music", "Audiobooks"} {
		t.Run("Y4/rejects_"+component, func(t *testing.T) {
			path := filepath.Join(source, component, "x.flac")
			if got, ok := SectionForPath(source, path); ok {
				t.Errorf("SectionForPath(%q, %q) = (%q, true), want (_, false)", source, path, got)
			}
		})
	}
}
