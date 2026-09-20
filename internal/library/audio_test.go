package library

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Synthetic infohashes only -- never a real one. hashA's tail token is "a1b2c3d4";
// hashB's tail token is "fedcba98"; both satisfy reInfoHash's 40-hex form.
const (
	hashA        = "0123456789abcdef0123456789abcdefa1b2c3d4"
	hashB        = "0123456789abcdef0123456789abcdeffedcba98"
	hashATokenLC = "a1b2c3d4"
	hashBase32   = "abcdefghijklmnopqrstuvwxyz234567" // 32 chars, matches reInfoHash's base32 arm
)

func TestSectionForType(t *testing.T) {
	t.Run("A1", func(t *testing.T) {
		tests := []struct {
			name    string
			apiType string
			want    Section
		}{
			{"A1/movie_maps_to_SectionMovies", "movie", SectionMovies},
			{"A1/tv_maps_to_SectionTV", "tv", SectionTV},
			{"A1/music_maps_to_SectionMusic", "music", SectionMusic},
			{"A1/audiobook_maps_to_SectionAudiobooks", "audiobook", SectionAudiobooks},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got, ok := SectionForType(tt.apiType)
				if !ok {
					t.Fatalf("SectionForType(%q) ok = false, want true", tt.apiType)
				}
				if got != tt.want {
					t.Errorf("SectionForType(%q) = %q, want %q", tt.apiType, got, tt.want)
				}
			})
		}
	})

	t.Run("A2", func(t *testing.T) {
		// Rejected: spec-forbidden aliases, the legacy movie/TV aliases manager.go
		// accepts today, the empty string, and un-normalized spellings. SectionForType
		// must not trim or case-fold -- that stays the caller's job.
		rejected := []string{
			"audio", "album", "track", "book", "audiobooks",
			"movies", "film", "show", "series", "episode",
			"",
			" music", "Music", "MUSIC",
		}
		for _, apiType := range rejected {
			t.Run("A2/rejects_"+apiType, func(t *testing.T) {
				if _, ok := SectionForType(apiType); ok {
					t.Errorf("SectionForType(%q) ok = true, want false", apiType)
				}
			})
		}
	})
}

func TestIsAudioSection(t *testing.T) {
	tests := []struct {
		name string
		s    Section
		want bool
	}{
		{"A3/music_is_audio", SectionMusic, true},
		{"A3/audiobooks_is_audio", SectionAudiobooks, true},
		{"A3/movies_is_not_audio", SectionMovies, false},
		{"A3/tv_is_not_audio", SectionTV, false},
		{"A3_E6/unknown_section_is_not_audio", Section("audio"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsAudioSection(tt.s); got != tt.want {
				t.Errorf("IsAudioSection(%q) = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}

func TestSectionExtensions(t *testing.T) {
	tests := []struct {
		name string
		s    Section
		want []string
	}{
		{"A4/music", SectionMusic, []string{".flac"}},
		{"A4/audiobooks", SectionAudiobooks, []string{".m4b", ".m4a", ".mp3"}},
		{"A4/movies", SectionMovies, []string{".mkv"}},
		{"A4/tv", SectionTV, []string{".mkv"}},
		{"A4_E6/unknown_section_is_empty", Section("audio"), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SectionExtensions(tt.s)
			if len(got) != len(tt.want) {
				t.Fatalf("SectionExtensions(%q) = %v, want %v", tt.s, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("SectionExtensions(%q)[%d] = %q, want %q", tt.s, i, got[i], tt.want[i])
				}
			}
		})
	}

	t.Run("A4/does_not_alias_shared_state", func(t *testing.T) {
		first := SectionExtensions(SectionAudiobooks)
		if len(first) == 0 {
			t.Fatal("SectionExtensions(SectionAudiobooks) returned no extensions")
		}
		first[0] = "MUTATED"

		second := SectionExtensions(SectionAudiobooks)
		if second[0] != ".m4b" {
			t.Errorf("mutating a previous SectionExtensions result affected a later call: got %q, want %q", second[0], ".m4b")
		}
	})
}

func TestExtensionAllowed(t *testing.T) {
	tests := []struct {
		name string
		s    Section
		file string
		want bool
	}{
		{"A5/music_accepts_uppercase_FLAC", SectionMusic, "x.FLAC", true},
		{"A5/music_accepts_mixed_case_Flac", SectionMusic, "x.Flac", true},
		{"A5/audiobooks_accepts_uppercase_M4B", SectionAudiobooks, "x.M4B", true},
		{"A5/audiobooks_accepts_lowercase_m4a", SectionAudiobooks, "x.m4a", true},
		{"A5/audiobooks_accepts_mixed_case_Mp3", SectionAudiobooks, "x.Mp3", true},

		{"A5/music_rejects_mp3", SectionMusic, "x.mp3", false},
		{"A5/audiobooks_rejects_flac", SectionAudiobooks, "x.flac", false},
		{"A5/music_rejects_mkv", SectionMusic, "x.mkv", false},
		{"A5/audiobooks_rejects_mkv", SectionAudiobooks, "x.mkv", false},

		{"E5/empty_name", SectionMusic, "", false},
		{"E5/single_dot", SectionMusic, ".", false},
		{"E5/double_dot", SectionMusic, "..", false},
		{"E5/no_dot", SectionMusic, "track", false},
		{"E5/trailing_dot", SectionMusic, "track.", false},
		{"E5/only_extension", SectionMusic, ".flac", false},

		{"E6/unknown_section", Section("audio"), "x.flac", false},
		{"E6/empty_section", Section(""), "x.mkv", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtensionAllowed(tt.s, tt.file); got != tt.want {
				t.Errorf("ExtensionAllowed(%q, %q) = %v, want %v", tt.s, tt.file, got, tt.want)
			}
		})
	}
}

func TestExtensionsAgree(t *testing.T) {
	tests := []struct {
		name        string
		source, dst string
		want        bool
	}{
		{
			"A6/case_insensitive_match_with_differing_paths",
			"Release/01 - Track.FLAC", "Artist/Album/01 - Track_a1b2c3d4.flac",
			true,
		},
		{"A6/differing_extensions_m4a_mp3", "x.m4a", "x.mp3", false},
		{"A6/differing_extensions_flac_mkv", "x.flac", "x.mkv", false},

		{"E5/both_empty", "", "", false},
		{"E5/one_empty", "x.flac", "", false},
		{"E5/dot_and_dotdot", ".", "..", false},
		{"E5/no_dot_in_source", "track", "track.flac", false},
		{"E5/trailing_dot_in_source", "track.", "track.flac", false},
		{"E5/source_is_only_extension", ".flac", "track.flac", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtensionsAgree(tt.source, tt.dst); got != tt.want {
				t.Errorf("ExtensionsAgree(%q, %q) = %v, want %v", tt.source, tt.dst, got, tt.want)
			}
		})
	}
}

func TestHashSuffixToken(t *testing.T) {
	t.Run("A7", func(t *testing.T) {
		tests := []struct {
			name string
			hash string
		}{
			{"A7/lowercase_hash", hashA},
			{"A7/uppercase_hash", strings.ToUpper(hashA)},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				token, ok := HashSuffixToken(tt.hash)
				if !ok {
					t.Fatalf("HashSuffixToken(%q) ok = false, want true", tt.hash)
				}
				if token != hashATokenLC {
					t.Errorf("HashSuffixToken(%q) = %q, want %q", tt.hash, token, hashATokenLC)
				}
			})
		}
	})

	t.Run("E1", func(t *testing.T) {
		tests := []struct {
			name string
			hash string
		}{
			{"E1/empty_string", ""},
			{"E1/39_hex_chars", hashA[:39]},
			{"E1/41_hex_chars", hashA + "0"},
			{"E1/32_char_base32_form", hashBase32},
			{"E1/non_hex_character", "g123456789abcdef0123456789abcdefa1b2c3d4"},
			{"E1/trailing_whitespace", hashA + " "},
			{"E1/leading_whitespace", " " + hashA},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				token, ok := HashSuffixToken(tt.hash)
				if ok {
					t.Errorf("HashSuffixToken(%q) ok = true, want false", tt.hash)
				}
				if token != "" {
					t.Errorf("HashSuffixToken(%q) token = %q, want empty", tt.hash, token)
				}
			})
		}
	})
}

func TestHasHashSuffix(t *testing.T) {
	tests := []struct {
		name string
		file string
		hash string
		want bool
	}{
		{"A8/exact_lowercase_match", "01 - Track_a1b2c3d4.flac", hashA, true},
		{"A8/token_case_insensitive", "01 - Track_A1B2C3D4.flac", hashA, true},
		{"A8/extension_case_independent", "01 - Track_a1b2c3d4.FLAC", hashA, true},
		{"A8/token_and_extension_both_uppercase", "01 - Track_A1B2C3D4.Flac", hashA, true},

		{"E2/hash_not_valid_40_hex", "01 - Track_deadbeef.flac", "not-a-hash", false},
		{"E2/hash_is_32_char_base32", "01 - Track_" + hashBase32[len(hashBase32)-8:] + ".flac", hashBase32, false},

		{"E3/token_not_immediately_before_extension", "01 - Track_a1b2c3d4.extra.flac", hashA, false},
		{"E3/missing_separating_underscore", "01 - Tracka1b2c3d4.flac", hashA, false},
		{"E3/no_extension_at_all", "01 - Track_a1b2c3d4", hashA, false},
		{"E3/truncated_token", "Track_a1b2c3d.flac", hashA, false},
		{"E3/lengthened_token", "Track_fa1b2c3d4.flac", hashA, false},
		{"E3/another_torrents_tail", "Track_fedcba98.flac", hashA, false},

		{"E4/token_in_directory_component_only", "a1b2c3d4/01 - Track.flac", hashA, false},
		{"E4/correct_final_component_deep_path", "some/deep/path/01 - Track_a1b2c3d4.flac", hashA, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasHashSuffix(tt.file, tt.hash); got != tt.want {
				t.Errorf("HasHashSuffix(%q, %q) = %v, want %v", tt.file, tt.hash, got, tt.want)
			}
		})
	}
}

func TestIsVirtualStub(t *testing.T) {
	tests := []struct {
		name string
		s    Section
		file string
		want bool
	}{
		// I1: movie/TV recognition is case-sensitive and unchanged.
		{"I1/movies_accepts_lowercase_mkv", SectionMovies, "Movie_1080p_a1b2c3d4.mkv", true},
		{"I1/tv_accepts_lowercase_mkv", SectionTV, "Movie_1080p_a1b2c3d4.mkv", true},
		{"I1/movies_rejects_uppercase_MKV", SectionMovies, "Movie_1080p_a1b2c3d4.MKV", false},
		{"I1/tv_rejects_uppercase_MKV", SectionTV, "Movie_1080p_a1b2c3d4.MKV", false},
		{"I1/tv_rejects_mp4", SectionTV, "Movie_1080p_a1b2c3d4.mp4", false},
		{"I1/movies_rejects_mixed_case_Mkv", SectionMovies, "Movie_1080p_a1b2c3d4.Mkv", false},
		{"I1/movies_rejects_mp4", SectionMovies, "Movie_1080p_a1b2c3d4.mp4", false},
		{"I1/movies_rejects_avi", SectionMovies, "Movie_1080p_a1b2c3d4.avi", false},
		{"I1/movies_rejects_mov", SectionMovies, "Movie_1080p_a1b2c3d4.mov", false},
		{"I1/movies_rejects_m4v", SectionMovies, "Movie_1080p_a1b2c3d4.m4v", false},
		{"I1/extension_must_be_whole_final_segment", SectionMovies, "Movie.mkv.part", false},
		{"I1/dot_leading_name_still_a_stub_in_video_section", SectionMovies, ".hidden_a1b2c3d4.mkv", true},

		// I2: audio recognition is case-insensitive.
		{"I2/music_accepts_lowercase_flac", SectionMusic, "Track_a1b2c3d4.flac", true},
		{"I2/music_accepts_uppercase_FLAC", SectionMusic, "Track_a1b2c3d4.FLAC", true},
		{"I2/music_accepts_mixed_case_Flac", SectionMusic, "Track_a1b2c3d4.Flac", true},
		{"I2/music_rejects_mp3", SectionMusic, "Track_a1b2c3d4.mp3", false},
		{"I2/audiobooks_accepts_m4b", SectionAudiobooks, "Book_a1b2c3d4.m4b", true},
		{"I2/audiobooks_accepts_uppercase_M4A", SectionAudiobooks, "Book_a1b2c3d4.M4A", true},
		{"I2/audiobooks_accepts_mixed_case_Mp3", SectionAudiobooks, "Book_a1b2c3d4.Mp3", true},
		{"I2/audiobooks_rejects_flac", SectionAudiobooks, "Book_a1b2c3d4.flac", false},

		// I3: staging namespace is never projected, audio sections only.
		{"I3/music_rejects_dot_leading_name", SectionMusic, ".Track_a1b2c3d4.flac", false},
		{"I3/audiobooks_rejects_dot_leading_name", SectionAudiobooks, ".Book_a1b2c3d4.m4b", false},
		// I3/A9: the exclusion looks at the final component only. A dot-leading
		// parent does not exclude an ordinary final component (lead's resolution:
		// hiding a staging directory from a listing is the enumeration layer's job,
		// not this predicate's), and an ordinary parent does not save a dot-leading
		// final component.
		{"I3_A9/dot_leading_parent_does_not_exclude_ordinary_final_component", SectionMusic, ".staging/Track_a1b2c3d4.flac", true},
		{"I3_A9/dot_leading_final_component_excluded_under_ordinary_parent", SectionMusic, "Artist/Album/.Track_a1b2c3d4.flac", false},

		// E5: degenerate names.
		{"E5/empty_name", SectionMusic, "", false},
		{"E5/single_dot", SectionMusic, ".", false},
		{"E5/double_dot", SectionMusic, "..", false},
		{"E5/no_dot", SectionMusic, "track", false},
		{"E5/trailing_dot", SectionMusic, "track.", false},
		{"E5/only_extension", SectionMusic, ".flac", false},

		// E6: unknown section never treated as movies.
		{"E6/unknown_section_rejects_mkv", Section("audio"), "Movie_1080p_a1b2c3d4.mkv", false},
		{"E6/unknown_section_rejects_flac", Section("audio"), "Track_a1b2c3d4.flac", false},
		{"E6/empty_section", Section(""), "Track_a1b2c3d4.flac", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsVirtualStub(tt.s, tt.file); got != tt.want {
				t.Errorf("IsVirtualStub(%q, %q) = %v, want %v", tt.s, tt.file, got, tt.want)
			}
		})
	}

	t.Run("A9/decided_from_final_path_component_only", func(t *testing.T) {
		pairs := []struct {
			name     string
			s        Section
			bareName string
			deepPath string
		}{
			{"A9/music_stub_name_agrees_under_deep_path", SectionMusic, "Track_a1b2c3d4.flac", "Artist/Album/Track_a1b2c3d4.flac"},
			{"A9/music_non_stub_name_agrees_under_deep_path", SectionMusic, "bad", "Artist/Album/bad"},
			// I1/A9: the same agreement must hold for a video section, including an
			// absolute path such as startup.go:134 passes this predicate.
			{"A9/movies_stub_name_agrees_under_absolute_path", SectionMovies, "Movie_1080p_a1b2c3d4.mkv", "/mnt/media/movies/Movie_1080p_a1b2c3d4.mkv"},
			{"A9/movies_non_stub_name_agrees_under_absolute_path", SectionMovies, "Movie_1080p_a1b2c3d4.mp4", "/mnt/media/movies/Movie_1080p_a1b2c3d4.mp4"},
		}
		for _, p := range pairs {
			t.Run(p.name, func(t *testing.T) {
				bare := IsVirtualStub(p.s, p.bareName)
				deep := IsVirtualStub(p.s, p.deepPath)
				if bare != deep {
					t.Errorf("IsVirtualStub disagreed between bare name (%v) and deep path (%v) for the same final component", bare, deep)
				}
			})
		}
	})
}

func TestPortablePathKey_Collisions(t *testing.T) {
	// The long-s-with-dot-above-plus-dot-below sequence (U+1E9B U+0323) is a classic
	// Unicode normalization/case-folding pitfall: it is composition-excluded under
	// NFC by itself, but Unicode default case folding maps U+1E9B to U+1E61 (small s
	// with dot above), which *does* recompose with a trailing combining dot below
	// into the precomposed U+1E69 (small s with dot below and dot above). A pipeline
	// that folds case before normalizing therefore collides the two spellings.
	longSDotAboveBelow := "ẛ̣"
	precomposedSDotBelowAbove := "ṩ"

	cafeNFC := "Café"
	cafeNFD := "Café"

	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"A10/ascii_case_collides", "Artist/Album/01 - Track_a1b2c3d4.flac", "ARTIST/ALBUM/01 - TRACK_A1B2C3D4.FLAC", true},
		{"A10/nfc_nfd_cafe_collides", cafeNFC, cafeNFD, true},
		{"A10/long_s_dot_above_below_collides_with_its_nfc_form", longSDotAboveBelow, precomposedSDotBelowAbove, true},
		{"A10/strasse_eszett_folds_to_ss", "Straße", "STRASSE", true},
		{"A10/kelvin_sign_folds_with_k", "K", "k", true},
		{"A10/final_sigma_folds_with_sigma", "ς", "σ", true},

		{"A10/different_components_do_not_collide", "A/b.flac", "A_b.flac", false},
		{"A10/different_depth_does_not_collide", "A/B/c.flac", "A/B c.flac", false},
		{"A10/different_hash_token_does_not_collide", "t_a1b2c3d4.flac", "t_a1b2c3d5.flac", false},
		{"A10/cyrillic_a_does_not_fold_with_latin_a", "а.flac", "a.flac", false},

		{"A6_multibyte_extension/ascii_case_folds_extension_untouched", "Artist/Track.日本語", "ARTIST/TRACK.日本語", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ka, kb := PortablePathKey(tt.a), PortablePathKey(tt.b)
			got := ka == kb
			if got != tt.want {
				t.Errorf("PortablePathKey(%q) == PortablePathKey(%q): got %v (keys %q vs %q), want %v", tt.a, tt.b, got, ka, kb, tt.want)
			}
		})
	}
}

func TestPortablePathKey_Transitivity(t *testing.T) {
	// I5: equality is transitive across three different representations of the
	// same collision class -- uppercase ASCII, lowercase precomposed Unicode, and
	// lowercase decomposed (NFD) Unicode.
	a := "ARTIST/ALBUM/CAFÉ.FLAC"
	b := "artist/album/café.flac"
	c := "artist/album/café.flac"

	ka, kb, kc := PortablePathKey(a), PortablePathKey(b), PortablePathKey(c)

	if ka != kb {
		t.Fatalf("I5: PortablePathKey(%q) != PortablePathKey(%q): %q vs %q", a, b, ka, kb)
	}
	if kb != kc {
		t.Fatalf("I5: PortablePathKey(%q) != PortablePathKey(%q): %q vs %q", b, c, kb, kc)
	}
	if ka != kc {
		t.Errorf("I5: equality was not transitive: PortablePathKey(%q) = %q, PortablePathKey(%q) = %q", a, ka, c, kc)
	}
}

func TestPortablePathKey_ComponentStructurePreserved(t *testing.T) {
	// I5: the number of '/'-separated components in the key equals the number in
	// the input.
	tests := []struct {
		name string
		path string
	}{
		{"I5/single_component", "Track.flac"},
		{"I5/three_components", "Artist/Album/Track.flac"},
		{"I5/four_components", "A/B/C/D.flac"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := PortablePathKey(tt.path)
			wantParts := len(strings.Split(tt.path, "/"))
			gotParts := len(strings.Split(key, "/"))
			if gotParts != wantParts {
				t.Errorf("PortablePathKey(%q) = %q has %d components, want %d (matching the input)", tt.path, key, gotParts, wantParts)
			}
		})
	}
}

func TestPortablePathKey_HostileInputsDoNotPanic(t *testing.T) {
	// I6: PortablePathKey is a key, not a validator. It must return a value,
	// never panic, for inputs full path validation will later reject.
	tests := []struct {
		name string
		path string
	}{
		{"I6/empty_string", ""},
		{"I6/doubled_separator", "a//b.flac"},
		{"I6/dot_dot_traversal", "../x.flac"},
		{"I6/trailing_separator", "path/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("PortablePathKey(%q) panicked: %v", tt.path, r)
				}
			}()
			k1 := PortablePathKey(tt.path)
			k2 := PortablePathKey(tt.path)
			if k1 != k2 {
				t.Errorf("PortablePathKey(%q) is not deterministic: %q vs %q", tt.path, k1, k2)
			}
		})
	}
}

func TestPortablePathKey_LongAndMultibyteComponents(t *testing.T) {
	t.Run("component_exactly_255_utf8_bytes_with_differing_rune_count", func(t *testing.T) {
		// Euro sign is 3 UTF-8 bytes; 85 of them is exactly 255 bytes and 85 runes,
		// so byte length and rune count deliberately disagree.
		component := strings.Repeat("€", 85)
		if len(component) != 255 {
			t.Fatalf("test fixture bug: component is %d bytes, want 255", len(component))
		}
		if len([]rune(component)) != 85 {
			t.Fatalf("test fixture bug: component is %d runes, want 85", len([]rune(component)))
		}

		path := component + "/Track.flac"
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("PortablePathKey panicked on a 255-byte multi-byte component: %v", r)
			}
		}()
		key := PortablePathKey(path)
		if got, want := len(strings.Split(key, "/")), 2; got != want {
			t.Errorf("PortablePathKey(%d-byte component) = %q has %d components, want %d", len(component), key, got, want)
		}
	})

	t.Run("A10/extension_with_multibyte_character_collides_case_insensitively", func(t *testing.T) {
		a := "Release/01 - Track.日本語"
		b := "Artist/Album/01 - Track_a1b2c3d4.日本語"
		// These are two distinct virtual paths (different components), so they must
		// NOT collide; this only proves the multi-byte extension does not break
		// PortablePathKey (no panic, still yields a component-preserving key).
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("PortablePathKey panicked on a multi-byte extension: %v", r)
			}
		}()
		ka := PortablePathKey(a)
		kb := PortablePathKey(b)
		if ka == kb {
			t.Errorf("PortablePathKey(%q) and PortablePathKey(%q) collided but their directory components differ: %q", a, b, ka)
		}
	})
}

// TestConcurrentPureFunctions_Race covers I4: IsVirtualStub, ExtensionAllowed,
// ExtensionsAgree, HasHashSuffix, HashSuffixToken, and PortablePathKey must be safe
// to call concurrently from many goroutines and must agree with the single-threaded
// reference result. Run with -race.
func TestConcurrentPureFunctions_Race(t *testing.T) {
	const goroutines = 50
	const iterations = 200

	wantStub := IsVirtualStub(SectionMusic, "Track_a1b2c3d4.flac")
	wantAllowed := ExtensionAllowed(SectionAudiobooks, "x.M4B")
	wantAgree := ExtensionsAgree("Release/Track.FLAC", "Artist/Track_a1b2c3d4.flac")
	wantHashSuffix := HasHashSuffix("01 - Track_a1b2c3d4.flac", hashA)
	wantToken, wantTokenOK := HashSuffixToken(hashA)
	wantKey := PortablePathKey("Artist/Album/Café.flac")

	var (
		stubMismatches       int64
		allowedMismatches    int64
		agreeMismatches      int64
		hashSuffixMismatches int64
		tokenMismatches      int64
		keyMismatches        int64
	)

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if got := IsVirtualStub(SectionMusic, "Track_a1b2c3d4.flac"); got != wantStub {
					atomic.AddInt64(&stubMismatches, 1)
				}
				if got := ExtensionAllowed(SectionAudiobooks, "x.M4B"); got != wantAllowed {
					atomic.AddInt64(&allowedMismatches, 1)
				}
				if got := ExtensionsAgree("Release/Track.FLAC", "Artist/Track_a1b2c3d4.flac"); got != wantAgree {
					atomic.AddInt64(&agreeMismatches, 1)
				}
				if got := HasHashSuffix("01 - Track_a1b2c3d4.flac", hashA); got != wantHashSuffix {
					atomic.AddInt64(&hashSuffixMismatches, 1)
				}
				if tok, ok := HashSuffixToken(hashA); tok != wantToken || ok != wantTokenOK {
					atomic.AddInt64(&tokenMismatches, 1)
				}
				if got := PortablePathKey("Artist/Album/Café.flac"); got != wantKey {
					atomic.AddInt64(&keyMismatches, 1)
				}
			}
		}(g)
	}
	wg.Wait()

	for msg, count := range map[string]int64{
		"IsVirtualStub disagreed under concurrent use":    atomic.LoadInt64(&stubMismatches),
		"ExtensionAllowed disagreed under concurrent use": atomic.LoadInt64(&allowedMismatches),
		"ExtensionsAgree disagreed under concurrent use":  atomic.LoadInt64(&agreeMismatches),
		"HasHashSuffix disagreed under concurrent use":    atomic.LoadInt64(&hashSuffixMismatches),
		"HashSuffixToken disagreed under concurrent use":  atomic.LoadInt64(&tokenMismatches),
		"PortablePathKey disagreed under concurrent use":  atomic.LoadInt64(&keyMismatches),
	} {
		if count > 0 {
			t.Errorf("%s (%d times)", msg, count)
		}
	}
}
