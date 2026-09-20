package library

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

const validationHash = "0123456789abcdef0123456789abcdefa1b2c3d4"

func TestValidateProjectionPath_AcceptsAndDerives(t *testing.T) {
	t.Run("P1_preserves_the_callers_spelling_byte_for_byte", func(t *testing.T) {
		virtualPath := "Beyoncé/Live Album/01 - MiXeD Case_A1B2C3D4.FLAC"
		got, err := ValidateProjectionPath(SectionMusic, virtualPath, "release/track.flac", validationHash)
		if err != nil {
			t.Fatalf("ValidateProjectionPath() error = %v, want nil", err)
		}
		if got.VirtualPath != virtualPath {
			t.Errorf("VirtualPath = %q, want caller spelling %q", got.VirtualPath, virtualPath)
		}
		if got.Section != SectionMusic {
			t.Errorf("Section = %q, want %q", got.Section, SectionMusic)
		}
	})

	t.Run("P2_computes_the_portable_collision_key", func(t *testing.T) {
		paths := []string{
			"Café/Album/Track_a1b2c3d4.flac",
			"CAFÉ/ALBUM/TRACK_A1B2C3D4.FLAC",
		}
		var keys []string
		for _, virtualPath := range paths {
			got, err := ValidateProjectionPath(SectionMusic, virtualPath, "source.flac", validationHash)
			if err != nil {
				t.Fatalf("ValidateProjectionPath(%q) error = %v, want nil", virtualPath, err)
			}
			want := PortablePathKey(virtualPath)
			if got.PortablePathKey != want {
				t.Errorf("PortablePathKey = %q, want PortablePathKey(%q) = %q", got.PortablePathKey, virtualPath, want)
			}
			keys = append(keys, got.PortablePathKey)
		}
		if keys[0] != keys[1] {
			t.Errorf("case-only variants produced different keys: %q vs %q", keys[0], keys[1])
		}

		// Validation rejects the NFD spelling (P7), but collision-key semantics
		// still require canonically equivalent spellings to share a key.
		nfc := "Café/Album/Track_a1b2c3d4.flac"
		nfd := "Cafe\u0301/Album/Track_a1b2c3d4.flac"
		if PortablePathKey(nfc) != PortablePathKey(nfd) {
			t.Errorf("NFC-equivalent spellings produced different portable keys: %q vs %q", PortablePathKey(nfc), PortablePathKey(nfd))
		}
	})

	t.Run("P3_accepts_a_single_component_path", func(t *testing.T) {
		assertProjectionAccepted(t, SectionMusic, "Track_a1b2c3d4.flac", "source.flac", validationHash)
	})

	t.Run("P3_P16_accepts_exactly_sixteen_components", func(t *testing.T) {
		parts := make([]string, 15)
		for i := range parts {
			parts[i] = "directory"
		}
		path := strings.Join(append(parts, "Track_a1b2c3d4.flac"), "/")
		if got := len(strings.Split(path, "/")); got != 16 {
			t.Fatalf("test fixture has %d components, want 16", got)
		}
		assertProjectionAccepted(t, SectionMusic, path, "source.flac", validationHash)
	})

	t.Run("P17_accepts_a_component_of_exactly_255_UTF8_bytes", func(t *testing.T) {
		component := strings.Repeat("€", 85)
		if len(component) != 255 || len([]rune(component)) != 85 {
			t.Fatalf("test fixture is %d bytes and %d runes, want 255 bytes and 85 runes", len(component), len([]rune(component)))
		}
		assertProjectionAccepted(t, SectionMusic, component+"/Track_a1b2c3d4.flac", "source.flac", validationHash)
	})

	t.Run("P18_jointly_reachable_maximum_is_4095_bytes", func(t *testing.T) {
		// Sixteen 255-byte components plus fifteen separators is 4095 bytes.
		// This is the longest path that can obey P16 and P17 simultaneously;
		// the brief's requested accepted 4096-byte fixture is mathematically
		// unreachable and is reported in the handoff questions.
		parts := make([]string, 15)
		for i := range parts {
			parts[i] = strings.Repeat("d", 255)
		}
		final := strings.Repeat("f", 241) + "_a1b2c3d4.flac"
		if len(final) != 255 {
			t.Fatalf("final component is %d bytes, want 255", len(final))
		}
		path := strings.Join(append(parts, final), "/")
		if len(path) != 4095 {
			t.Fatalf("test fixture is %d bytes, want 4095", len(path))
		}
		assertProjectionAccepted(t, SectionMusic, path, "source.flac", validationHash)
	})
}

func TestValidateProjectionPath_SyntaxFailures(t *testing.T) {
	invalidUTF8 := "Artist/" + string([]byte{0xff}) + "_a1b2c3d4.flac"
	nfdPath := "Cafe\u0301/Track_a1b2c3d4.flac"
	if nfdPath == "Café/Track_a1b2c3d4.flac" {
		t.Fatal("test fixture bug: NFC and NFD spellings must differ")
	}

	tests := []struct {
		name string
		path string
	}{
		{"P4_rejects_invalid_UTF8", invalidUTF8},
		{"P5_rejects_an_absolute_path", "/Artist/Track_a1b2c3d4.flac"},
		{"P5_rejects_a_path_escaping_the_section_root", "../Track_a1b2c3d4.flac"},
		{"P6_rejects_backslash_as_a_separator", "Artist\\Track_a1b2c3d4.flac"},
		{"P7_rejects_NFD_instead_of_normalising_it", nfdPath},
		{"P8_rejects_an_empty_middle_component", "Artist//Track_a1b2c3d4.flac"},
		{"P8_rejects_a_leading_empty_component", "/Track_a1b2c3d4.flac"},
		{"P8_rejects_a_trailing_empty_component", "Track_a1b2c3d4.flac/"},
		{"P9_rejects_a_dot_component", "Artist/./Track_a1b2c3d4.flac"},
		{"P9_rejects_a_dot_dot_component", "Artist/../Track_a1b2c3d4.flac"},
		{"P10_rejects_a_leading_dot_directory", ".staging/Track_a1b2c3d4.flac"},
		{"P10_rejects_a_leading_dot_final_component", "Artist/.Track_a1b2c3d4.flac"},
		{"P11_rejects_NUL", "Artist/Bad\x00Name/Track_a1b2c3d4.flac"},
		{"P14_rejects_a_component_ending_in_period", "Artist./Track_a1b2c3d4.flac"},
		{"P14_rejects_a_component_ending_in_space", "Artist /Track_a1b2c3d4.flac"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidateProjectionPath(SectionMusic, tt.path, "source.flac", validationHash)
			assertPathErrorClass(t, err, ErrPathInvalid)
		})
	}

	for control := rune(0x01); control <= 0x1f; control++ {
		t.Run(fmt.Sprintf("P12_rejects_C0_U+%04X", control), func(t *testing.T) {
			path := "Artist/Bad" + string(control) + "Name/Track_a1b2c3d4.flac"
			_, err := ValidateProjectionPath(SectionMusic, path, "source.flac", validationHash)
			assertPathErrorClass(t, err, ErrPathInvalid)
		})
	}
	for control := rune(0x80); control <= 0x9f; control++ {
		t.Run(fmt.Sprintf("P12_rejects_C1_U+%04X", control), func(t *testing.T) {
			path := "Artist/Bad" + string(control) + "Name/Track_a1b2c3d4.flac"
			_, err := ValidateProjectionPath(SectionMusic, path, "source.flac", validationHash)
			assertPathErrorClass(t, err, ErrPathInvalid)
		})
	}

	for _, forbidden := range []string{"\\", ":", "*", "?", "\"", "<", ">", "|"} {
		t.Run("P13_rejects_forbidden_character_"+forbidden, func(t *testing.T) {
			path := "Artist/Bad" + forbidden + "Name/Track_a1b2c3d4.flac"
			_, err := ValidateProjectionPath(SectionMusic, path, "source.flac", validationHash)
			assertPathErrorClass(t, err, ErrPathInvalid)
		})
	}
}

func TestValidateProjectionPath_ReservedDeviceBasenames_P15(t *testing.T) {
	reserved := []string{
		"CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9",
	}
	for _, basename := range reserved {
		for _, component := range []string{basename, strings.ToLower(basename) + ".flac"} {
			t.Run("P15_rejects_"+component, func(t *testing.T) {
				path := component + "/Track_a1b2c3d4.flac"
				_, err := ValidateProjectionPath(SectionMusic, path, "source.flac", validationHash)
				assertPathErrorClass(t, err, ErrPathInvalid)
			})
		}
	}
}

func TestValidateProjectionPath_SyntaxLimitRejections(t *testing.T) {
	t.Run("P16_rejects_seventeen_components", func(t *testing.T) {
		parts := make([]string, 16)
		for i := range parts {
			parts[i] = "directory"
		}
		path := strings.Join(append(parts, "Track_a1b2c3d4.flac"), "/")
		if got := len(strings.Split(path, "/")); got != 17 {
			t.Fatalf("test fixture has %d components, want 17", got)
		}
		_, err := ValidateProjectionPath(SectionMusic, path, "source.flac", validationHash)
		assertPathErrorClass(t, err, ErrPathInvalid)
	})

	t.Run("P17_rejects_a_component_of_256_UTF8_bytes", func(t *testing.T) {
		component := strings.Repeat("€", 84) + "aaaa"
		if len(component) != 256 || len([]rune(component)) != 88 {
			t.Fatalf("test fixture is %d bytes and %d runes, want 256 bytes and 88 runes", len(component), len([]rune(component)))
		}
		_, err := ValidateProjectionPath(SectionMusic, component+"/Track_a1b2c3d4.flac", "source.flac", validationHash)
		assertPathErrorClass(t, err, ErrPathInvalid)
	})
}

func TestValidateProjectionPath_ExtensionRules(t *testing.T) {
	tests := []struct {
		name        string
		section     Section
		virtualPath string
		sourcePath  string
		wantErr     error
	}{
		{"P19_music_rejects_m4b", SectionMusic, "Track_a1b2c3d4.m4b", "source.m4b", ErrExtensionUnsupported},
		{"P19_music_rejects_m4a", SectionMusic, "Track_a1b2c3d4.m4a", "source.m4a", ErrExtensionUnsupported},
		{"P19_music_rejects_mp3", SectionMusic, "Track_a1b2c3d4.mp3", "source.mp3", ErrExtensionUnsupported},
		{"P19_audiobooks_reject_flac", SectionAudiobooks, "Book_a1b2c3d4.flac", "source.flac", ErrExtensionUnsupported},
		{"P21_mismatching_extensions_are_distinct_from_unsupported", SectionMusic, "Track_a1b2c3d4.mp3", "source.flac", ErrExtensionMismatch},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidateProjectionPath(tt.section, tt.virtualPath, tt.sourcePath, validationHash)
			assertPathErrorClass(t, err, tt.wantErr)
		})
	}

	t.Run("P20_extension_comparison_is_case_insensitive", func(t *testing.T) {
		assertProjectionAccepted(t, SectionMusic, "Track_a1b2c3d4.FLAC", "source.flac", validationHash)
	})

	for _, extension := range []string{".m4b", ".m4a", ".mp3"} {
		t.Run("P19_audiobooks_admit"+extension, func(t *testing.T) {
			assertProjectionAccepted(t, SectionAudiobooks, "Book_a1b2c3d4"+extension, "source"+extension, validationHash)
		})
	}
}

func TestValidateProjectionPath_HashSuffixRules(t *testing.T) {
	t.Run("P22_missing_hash_suffix_fails_without_repair", func(t *testing.T) {
		got, err := ValidateProjectionPath(SectionMusic, "Artist/Track.flac", "source.flac", validationHash)
		assertPathErrorClass(t, err, ErrHashSuffixInvalid)
		if got.VirtualPath == "Artist/Track_a1b2c3d4.flac" {
			t.Error("ValidateProjectionPath repaired a missing suffix; it must reject instead")
		}
	})

	t.Run("P23_wrong_hash_suffix_is_rejected", func(t *testing.T) {
		_, err := ValidateProjectionPath(SectionMusic, "Track_fedcba98.flac", "source.flac", validationHash)
		assertPathErrorClass(t, err, ErrHashSuffixInvalid)
	})

	tests := []struct {
		name string
		path string
		hash string
	}{
		{"P24_uppercase_token_matches_lowercase_hash", "Track_A1B2C3D4.flac", validationHash},
		{"P24_lowercase_token_matches_uppercase_hash", "Track_a1b2c3d4.flac", strings.ToUpper(validationHash)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertProjectionAccepted(t, SectionMusic, tt.path, "source.flac", tt.hash)
		})
	}

	t.Run("P25_hash8_must_be_the_final_underscore_separated_token", func(t *testing.T) {
		_, err := ValidateProjectionPath(SectionMusic, "01 - a1b2c3d4_Track.flac", "source.flac", validationHash)
		assertPathErrorClass(t, err, ErrHashSuffixInvalid)
	})

	for _, hash := range []string{"not-a-hash", strings.Repeat("g", 40)} {
		t.Run("P26_noncanonical_infohash_is_rejected_"+hash, func(t *testing.T) {
			_, err := ValidateProjectionPath(SectionMusic, "Track_a1b2c3d4.flac", "source.flac", hash)
			assertPathErrorClass(t, err, ErrHashSuffixInvalid)
		})
	}
}

func TestValidateProjectionPath_P27_SyntaxFailureTakesPrecedence(t *testing.T) {
	_, err := ValidateProjectionPath(SectionMusic, "../Track_fedcba98.flac", "source.flac", validationHash)
	assertPathErrorClass(t, err, ErrPathInvalid)
}

func assertProjectionAccepted(t *testing.T, section Section, virtualPath, sourcePath, hash string) ValidatedPath {
	t.Helper()
	got, err := ValidateProjectionPath(section, virtualPath, sourcePath, hash)
	if err != nil {
		t.Fatalf("ValidateProjectionPath(%q, %q, %q, %q) error = %v, want nil", section, virtualPath, sourcePath, hash, err)
	}
	if got.VirtualPath != virtualPath {
		t.Errorf("VirtualPath = %q, want %q", got.VirtualPath, virtualPath)
	}
	if got.PortablePathKey != PortablePathKey(virtualPath) {
		t.Errorf("PortablePathKey = %q, want %q", got.PortablePathKey, PortablePathKey(virtualPath))
	}
	if got.Section != section {
		t.Errorf("Section = %q, want %q", got.Section, section)
	}
	return got
}

func assertPathErrorClass(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want errors.Is(_, %v)", err, want)
	}
	for _, other := range []error{ErrPathInvalid, ErrExtensionUnsupported, ErrExtensionMismatch, ErrHashSuffixInvalid} {
		if other != want && errors.Is(err, other) {
			t.Errorf("error = %v also matches distinct sentinel %v", err, other)
		}
	}
}
