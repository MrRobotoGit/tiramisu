package library

import (
	"errors"
	"reflect"
	"testing"
)

func TestResolveSource_Matching(t *testing.T) {
	t.Run("S1_exact_match_returns_the_bound_file_and_echoes_source_path", func(t *testing.T) {
		sourcePath := "Beyonc\u00e9/Live Album/01 - MiXeD Case.flac"
		files := []FileStat{
			{ID: 4, Path: "Artwork/cover.jpg", Length: 4096},
			{ID: 37, Path: sourcePath, Length: 987654321},
		}

		got, err := ResolveSource(files, sourcePath)
		if err != nil {
			t.Fatalf("ResolveSource() error = %v, want nil", err)
		}
		want := ResolvedSource{SourcePath: sourcePath, FileIndex: 37, Size: 987654321}
		if got != want {
			t.Errorf("ResolveSource() = %+v, want %+v", got, want)
		}
	})

	t.Run("S2_missing_path_does_not_fall_back_to_the_only_file", func(t *testing.T) {
		files := []FileStat{{ID: 9, Path: "Album/only-and-largest.flac", Length: 999999}}

		_, err := ResolveSource(files, "Album/not-present.flac")
		assertSourceErrorClass(t, err, ErrSourceNotFound)
	})

	t.Run("S2_missing_path_does_not_fall_back_to_the_first_or_largest_file", func(t *testing.T) {
		files := []FileStat{
			{ID: 11, Path: "Album/first-and-largest.flac", Length: 999999},
			{ID: 12, Path: "Album/second.flac", Length: 1},
		}

		_, err := ResolveSource(files, "Album/not-present.flac")
		assertSourceErrorClass(t, err, ErrSourceNotFound)
	})

	t.Run("S3_matching_is_case_sensitive", func(t *testing.T) {
		files := []FileStat{{ID: 1, Path: "Album/Track.flac", Length: 100}}

		_, err := ResolveSource(files, "album/track.flac")
		assertSourceErrorClass(t, err, ErrSourceNotFound)
	})

	for _, tt := range []struct {
		name       string
		sourcePath string
	}{
		{"S4_suffix_is_not_an_exact_match", "01 - Track.flac"},
		{"S4_prefix_is_not_an_exact_match", "Album/01"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			files := []FileStat{{ID: 1, Path: "Album/01 - Track.flac", Length: 100}}

			_, err := ResolveSource(files, tt.sourcePath)
			assertSourceErrorClass(t, err, ErrSourceNotFound)
		})
	}

	t.Run("S5_leading_slash_is_not_stripped", func(t *testing.T) {
		files := []FileStat{{ID: 1, Path: "Album/01.flac", Length: 100}}

		_, err := ResolveSource(files, "/Album/01.flac")
		assertSourceErrorClass(t, err, ErrSourceNotFound)
	})

	t.Run("S6_empty_source_path_never_matches", func(t *testing.T) {
		files := []FileStat{{ID: 1, Path: "", Length: 100}}

		_, err := ResolveSource(files, "")
		assertSourceErrorClass(t, err, ErrSourceNotFound)
	})

	for _, tt := range []struct {
		name  string
		files []FileStat
	}{
		{"S7_nil_file_list_is_not_found", nil},
		{"S7_empty_file_list_is_not_found", []FileStat{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ResolveSource(tt.files, "Album/01.flac")
			assertSourceErrorClass(t, err, ErrSourceNotFound)
		})
	}

	for _, tt := range []struct {
		name   string
		length int64
	}{
		{"S8_preserves_a_length_larger_than_int32", int64(1<<31) + 12345},
		{"S8_zero_length_file_resolves_successfully", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			files := []FileStat{{ID: 3, Path: "Album/01.flac", Length: tt.length}}

			got, err := ResolveSource(files, "Album/01.flac")
			if err != nil {
				t.Fatalf("ResolveSource() error = %v, want nil", err)
			}
			if got.Size != tt.length {
				t.Errorf("Size = %d, want exact FileStat.Length %d", got.Size, tt.length)
			}
		})
	}
}

func TestResolveSource_MalformedFileLists(t *testing.T) {
	t.Run("S9_duplicate_matching_paths_are_ambiguous", func(t *testing.T) {
		files := []FileStat{
			{ID: 1, Path: "Album/duplicate.flac", Length: 100},
			{ID: 2, Path: "Album/duplicate.flac", Length: 100},
			{ID: 3, Path: "Album/distinct.flac", Length: 200},
		}

		_, err := ResolveSource(files, "Album/duplicate.flac")
		assertSourceErrorClass(t, err, ErrSourceAmbiguous)
	})

	t.Run("S9_different_path_in_a_list_with_duplicates_still_resolves", func(t *testing.T) {
		files := []FileStat{
			{ID: 1, Path: "Album/duplicate.flac", Length: 100},
			{ID: 2, Path: "Album/duplicate.flac", Length: 100},
			{ID: 3, Path: "Album/distinct.flac", Length: 200},
		}

		got, err := ResolveSource(files, "Album/distinct.flac")
		if err != nil {
			t.Fatalf("ResolveSource() error = %v, want nil", err)
		}
		want := ResolvedSource{SourcePath: "Album/distinct.flac", FileIndex: 3, Size: 200}
		if got != want {
			t.Errorf("ResolveSource() = %+v, want %+v", got, want)
		}
	})

	for _, id := range []int{0, -1} {
		name := "S10_zero_ID_is_ambiguous"
		if id < 0 {
			name = "S10_negative_ID_is_ambiguous"
		}
		t.Run(name, func(t *testing.T) {
			files := []FileStat{{ID: id, Path: "Album/01.flac", Length: 100}}

			_, err := ResolveSource(files, "Album/01.flac")
			assertSourceErrorClass(t, err, ErrSourceAmbiguous)
		})
	}

	t.Run("S11_negative_length_is_ambiguous", func(t *testing.T) {
		files := []FileStat{{ID: 1, Path: "Album/01.flac", Length: -1}}

		_, err := ResolveSource(files, "Album/01.flac")
		assertSourceErrorClass(t, err, ErrSourceAmbiguous)
	})
}

func TestResolveSources_Batch(t *testing.T) {
	t.Run("S12_results_follow_request_order_not_file_list_order", func(t *testing.T) {
		files := []FileStat{
			{ID: 1, Path: "Album/01.flac", Length: 101},
			{ID: 2, Path: "Album/02.flac", Length: 202},
			{ID: 3, Path: "Album/03.flac", Length: 303},
		}
		request := []string{"Album/03.flac", "Album/01.flac", "Album/02.flac"}

		got, err := ResolveSources(files, request)
		if err != nil {
			t.Fatalf("ResolveSources() error = %v, want nil", err)
		}
		want := []ResolvedSource{
			{SourcePath: "Album/03.flac", FileIndex: 3, Size: 303},
			{SourcePath: "Album/01.flac", FileIndex: 1, Size: 101},
			{SourcePath: "Album/02.flac", FileIndex: 2, Size: 202},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ResolveSources() = %+v, want request-ordered %+v", got, want)
		}
	})

	t.Run("S13_repeated_source_path_is_duplicate", func(t *testing.T) {
		files := []FileStat{{ID: 1, Path: "Album/01.flac", Length: 101}}

		got, err := ResolveSources(files, []string{"Album/01.flac", "Album/01.flac"})
		assertSourceErrorClass(t, err, ErrSourceDuplicate)
		if len(got) != 0 {
			t.Errorf("ResolveSources() returned %d results on duplicate request, want none", len(got))
		}
	})

	t.Run("S14_distinct_paths_resolving_to_one_file_index_are_duplicate", func(t *testing.T) {
		files := []FileStat{
			{ID: 7, Path: "Album/01.flac", Length: 101},
			{ID: 7, Path: "Album/02.flac", Length: 202},
		}

		got, err := ResolveSources(files, []string{"Album/01.flac", "Album/02.flac"})
		assertSourceErrorClass(t, err, ErrSourceDuplicate)
		if len(got) != 0 {
			t.Errorf("ResolveSources() returned %d results for one file index selected twice, want none", len(got))
		}
	})

	t.Run("S15_failure_in_last_entry_returns_no_partial_results", func(t *testing.T) {
		files := []FileStat{
			{ID: 1, Path: "Album/01.flac", Length: 101},
			{ID: 2, Path: "Album/02.flac", Length: 202},
		}
		request := []string{"Album/01.flac", "Album/02.flac", "Album/missing.flac"}

		got, err := ResolveSources(files, request)
		assertSourceErrorClass(t, err, ErrSourceNotFound)
		if len(got) != 0 {
			t.Errorf("ResolveSources() returned partial results %+v, want none", got)
		}
	})

	for _, tt := range []struct {
		name    string
		request []string
	}{
		{"S16_nil_request_returns_an_empty_result", nil},
		{"S16_empty_request_returns_an_empty_result", []string{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveSources([]FileStat{{ID: 1, Path: "Album/01.flac", Length: 101}}, tt.request)
			if err != nil {
				t.Fatalf("ResolveSources() error = %v, want nil", err)
			}
			if len(got) != 0 {
				t.Errorf("ResolveSources() returned %d results, want empty", len(got))
			}
		})
	}

	t.Run("S17_duplicate_outranks_an_earlier_unrelated_not_found_path", func(t *testing.T) {
		files := []FileStat{{ID: 1, Path: "Album/01.flac", Length: 101}}
		request := []string{"Album/missing.flac", "Album/01.flac", "Album/01.flac"}

		got, err := ResolveSources(files, request)
		assertSourceErrorClass(t, err, ErrSourceDuplicate)
		if len(got) != 0 {
			t.Errorf("ResolveSources() returned partial results %+v, want none", got)
		}
	})
}

func TestVerifyResolvedSource_Drift(t *testing.T) {
	t.Run("S18_matching_persisted_triple_is_current", func(t *testing.T) {
		files := []FileStat{
			{ID: 4, Path: "Artwork/cover.jpg", Length: 4096},
			{ID: 7, Path: "Album/01.flac", Length: 123456},
		}
		prior := ResolvedSource{SourcePath: "Album/01.flac", FileIndex: 7, Size: 123456}

		if err := VerifyResolvedSource(files, prior); err != nil {
			t.Fatalf("VerifyResolvedSource() error = %v, want nil", err)
		}
	})

	t.Run("S19_same_path_at_a_different_ID_fails_with_drift", func(t *testing.T) {
		files := []FileStat{{ID: 12, Path: "Album/01.flac", Length: 123456}}
		prior := ResolvedSource{SourcePath: "Album/01.flac", FileIndex: 7, Size: 123456}

		err := VerifyResolvedSource(files, prior)
		assertSourceErrorClass(t, err, ErrSourceDrift)
	})

	t.Run("S20_same_path_and_ID_at_a_different_length_fails_with_drift", func(t *testing.T) {
		files := []FileStat{{ID: 7, Path: "Album/01.flac", Length: 654321}}
		prior := ResolvedSource{SourcePath: "Album/01.flac", FileIndex: 7, Size: 123456}

		err := VerifyResolvedSource(files, prior)
		assertSourceErrorClass(t, err, ErrSourceDrift)
	})

	t.Run("S21_absent_persisted_path_is_not_found_not_drift", func(t *testing.T) {
		files := []FileStat{{ID: 7, Path: "Album/02.flac", Length: 123456}}
		prior := ResolvedSource{SourcePath: "Album/01.flac", FileIndex: 7, Size: 123456}

		err := VerifyResolvedSource(files, prior)
		assertSourceErrorClass(t, err, ErrSourceNotFound)
	})

	t.Run("S22_re_sorting_the_same_files_and_reassigning_IDs_is_drift", func(t *testing.T) {
		before := []FileStat{
			{ID: 1, Path: "Zoo/Track.flac", Length: 101},
			{ID: 2, Path: "Album/Track.flac", Length: 202},
		}
		after := []FileStat{
			{ID: 1, Path: "Album/Track.flac", Length: 202},
			{ID: 2, Path: "Zoo/Track.flac", Length: 101},
		}
		prior := ResolvedSource{SourcePath: "Album/Track.flac", FileIndex: 2, Size: 202}

		if err := VerifyResolvedSource(before, prior); err != nil {
			t.Fatalf("VerifyResolvedSource(before re-sort) error = %v, want nil", err)
		}
		err := VerifyResolvedSource(after, prior)
		assertSourceErrorClass(t, err, ErrSourceDrift)
	})
}

func assertSourceErrorClass(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want errors.Is(_, %v)", err, want)
	}
	for _, other := range []error{ErrSourceNotFound, ErrSourceDuplicate, ErrSourceAmbiguous, ErrSourceDrift} {
		if other != want && errors.Is(err, other) {
			t.Errorf("error = %v also matches distinct sentinel %v", err, other)
		}
	}
}
