package library

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
)

func TestValidateAudioAddRequest_TypeResolution(t *testing.T) {
	validFiles := []AudioFileRequest{{SourcePath: "release/track.flac", Path: "Artist/track.flac"}}

	for _, tt := range []struct {
		name        string
		requestType string
		wantSection Section
	}{
		{name: "A1_music_resolves_to_the_music_section", requestType: "music", wantSection: SectionMusic},
		{name: "A1_singular_audiobook_resolves_to_the_plural_audiobooks_section", requestType: "audiobook", wantSection: SectionAudiobooks},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateAudioAddRequest(AddRequest{
				Type:  tt.requestType,
				Title: "Title",
				Files: validFiles,
			})
			if err != nil {
				t.Fatalf("ValidateAudioAddRequest() error = %v, want nil", err)
			}
			if got.Section != tt.wantSection {
				t.Errorf("Section = %q, want %q", got.Section, tt.wantSection)
			}
		})
	}

	for _, requestType := range []string{"Music", " music", "musics", "audiobooks", "book", "flac"} {
		t.Run("A2_rejects_noncanonical_audio_type_"+requestType, func(t *testing.T) {
			got, err := ValidateAudioAddRequest(AddRequest{
				Type:  requestType,
				Title: "Title",
				Files: validFiles,
			})
			assertAudioAddStatus(t, err, http.StatusBadRequest)
			if IsAudioSection(got.Section) {
				t.Errorf("Section = %q for invalid API type %q, want no audio alias resolution", got.Section, requestType)
			}
		})
	}

	t.Run("A3_unknown_type_is_400_and_never_falls_back_to_movies", func(t *testing.T) {
		got, err := ValidateAudioAddRequest(AddRequest{
			Type:  "documentary",
			Title: "Unknown",
			Files: validFiles,
		})
		assertAudioAddStatus(t, err, http.StatusBadRequest)
		if got.Section == SectionMovies {
			t.Error("unknown type resolved to SectionMovies; unknown audio types must never use the movie fallback")
		}
	})

	t.Run("A4_unknown_type_is_distinguishable_from_both_video_sections", func(t *testing.T) {
		got, err := ValidateAudioAddRequest(AddRequest{
			Type:  "documentary",
			Title: "Unknown",
			Files: validFiles,
		})
		assertAudioAddStatus(t, err, http.StatusBadRequest)
		if got.Section == SectionMovies || got.Section == SectionTV {
			t.Errorf("unknown type resolved to video section %q, so it cannot be distinguished from a video request", got.Section)
		}
	})

	for _, tt := range []struct {
		name        string
		requestType string
		wantSection Section
	}{
		{name: "A4_empty_type_is_distinguishable_as_default_movie", requestType: "", wantSection: SectionMovies},
		{name: "A4_movie_is_distinguishable_as_video", requestType: "movie", wantSection: SectionMovies},
		{name: "A4_movies_alias_is_distinguishable_as_video", requestType: "movies", wantSection: SectionMovies},
		{name: "A4_film_alias_is_distinguishable_as_video", requestType: "film", wantSection: SectionMovies},
		{name: "A4_tv_is_distinguishable_as_video", requestType: "tv", wantSection: SectionTV},
		{name: "A4_show_alias_is_distinguishable_as_video", requestType: "show", wantSection: SectionTV},
		{name: "A4_series_alias_is_distinguishable_as_video", requestType: "series", wantSection: SectionTV},
		{name: "A4_episode_alias_is_distinguishable_as_video", requestType: "episode", wantSection: SectionTV},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateAudioAddRequest(AddRequest{
				Type:  tt.requestType,
				Title: "Video",
				Files: validFiles,
			})
			assertAudioAddStatus(t, err, http.StatusBadRequest)
			if got.Section != tt.wantSection {
				t.Errorf("Section = %q, want resolved video section %q so the caller can route to the existing add path", got.Section, tt.wantSection)
			}
		})
	}
}

func TestValidateAudioAddRequest_Title(t *testing.T) {
	validFiles := []AudioFileRequest{{SourcePath: "release/track.flac", Path: "Artist/track.flac"}}

	for _, title := range []string{"", " \t\r\n "} {
		t.Run("A5_empty_or_whitespace_only_title_is_400_"+fmt.Sprintf("%q", title), func(t *testing.T) {
			_, err := ValidateAudioAddRequest(AddRequest{
				Type:  "music",
				Title: title,
				Files: validFiles,
			})
			assertAudioAddStatus(t, err, http.StatusBadRequest)
		})
	}

	t.Run("A6_title_is_trimmed_in_the_validated_request", func(t *testing.T) {
		got, err := ValidateAudioAddRequest(AddRequest{
			Type:  "music",
			Title: " \t  Béla Bartók  \r\n",
			Files: validFiles,
		})
		if err != nil {
			t.Fatalf("ValidateAudioAddRequest() error = %v, want nil", err)
		}
		if got.Title != "Béla Bartók" {
			t.Errorf("Title = %q, want trimmed title %q", got.Title, "Béla Bartók")
		}
	})

	t.Run("A7_title_does_not_shape_or_rename_files", func(t *testing.T) {
		first, err := ValidateAudioAddRequest(AddRequest{
			Type:  "music",
			Title: "First caller label",
			Files: validFiles,
		})
		if err != nil {
			t.Fatalf("first ValidateAudioAddRequest() error = %v, want nil", err)
		}
		second, err := ValidateAudioAddRequest(AddRequest{
			Type:  "music",
			Title: "Entirely different label",
			Files: validFiles,
		})
		if err != nil {
			t.Fatalf("second ValidateAudioAddRequest() error = %v, want nil", err)
		}
		if !reflect.DeepEqual(first.Files, second.Files) {
			t.Errorf("files changed with title: first = %#v, second = %#v", first.Files, second.Files)
		}
	})
}

func TestValidateAudioAddRequest_Files(t *testing.T) {
	for _, tt := range []struct {
		name  string
		files []AudioFileRequest
	}{
		{name: "A8_missing_files_array_is_400", files: nil},
		{name: "A8_empty_files_array_is_400", files: []AudioFileRequest{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidateAudioAddRequest(AddRequest{Type: "music", Title: "No projections", Files: tt.files})
			assertAudioAddStatus(t, err, http.StatusBadRequest)
		})
	}

	for _, tt := range []struct {
		name string
		file AudioFileRequest
	}{
		{name: "A9_empty_source_path_is_400", file: AudioFileRequest{SourcePath: "", Path: "Artist/track.flac"}},
		{name: "A9_whitespace_only_source_path_is_400", file: AudioFileRequest{SourcePath: " \t\r\n ", Path: "Artist/track.flac"}},
		{name: "A9_empty_destination_path_is_400", file: AudioFileRequest{SourcePath: "release/track.flac", Path: ""}},
		{name: "A9_whitespace_only_destination_path_is_400", file: AudioFileRequest{SourcePath: "release/track.flac", Path: " \t\r\n "}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidateAudioAddRequest(AddRequest{
				Type:  "music",
				Title: "Required fields",
				Files: []AudioFileRequest{tt.file},
			})
			assertAudioAddStatus(t, err, http.StatusBadRequest)
		})
	}

	t.Run("A10_paths_are_byte_preserved_while_title_is_trimmed", func(t *testing.T) {
		file := AudioFileRequest{
			SourcePath: "  Béyoncé / 日本語 / track.flac  ",
			Path:       "  Artïst / Album  / 01_été.flac  ",
		}
		got, err := ValidateAudioAddRequest(AddRequest{
			Type:  "music",
			Title: "  Caller label  ",
			Files: []AudioFileRequest{file},
		})
		if err != nil {
			t.Fatalf("ValidateAudioAddRequest() error = %v, want nil", err)
		}
		if got.Title != "Caller label" {
			t.Errorf("Title = %q, want trimmed title", got.Title)
		}
		if len(got.Files) != 1 {
			t.Fatalf("len(Files) = %d, want 1", len(got.Files))
		}
		if got.Files[0] != file {
			t.Errorf("Files[0] = %#v, want byte-preserved %#v", got.Files[0], file)
		}
	})

	t.Run("A11_repeated_destination_path_is_400", func(t *testing.T) {
		_, err := ValidateAudioAddRequest(AddRequest{
			Type:  "music",
			Title: "Duplicate destination",
			Files: []AudioFileRequest{
				{SourcePath: "release/01.flac", Path: "Artist/same.flac"},
				{SourcePath: "release/02.flac", Path: "Artist/same.flac"},
			},
		})
		assertAudioAddStatus(t, err, http.StatusBadRequest)
	})

	t.Run("A12_repeated_source_path_is_400", func(t *testing.T) {
		_, err := ValidateAudioAddRequest(AddRequest{
			Type:  "music",
			Title: "Duplicate source",
			Files: []AudioFileRequest{
				{SourcePath: "release/same.flac", Path: "Artist/01.flac"},
				{SourcePath: "release/same.flac", Path: "Artist/02.flac"},
			},
		})
		assertAudioAddStatus(t, err, http.StatusBadRequest)
	})

	t.Run("A13_entry_order_is_preserved", func(t *testing.T) {
		files := []AudioFileRequest{
			{SourcePath: "release/03.flac", Path: "Artist/03.flac"},
			{SourcePath: "release/01.flac", Path: "Artist/01.flac"},
			{SourcePath: "release/02.flac", Path: "Artist/02.flac"},
		}
		got, err := ValidateAudioAddRequest(AddRequest{Type: "music", Title: "Order", Files: files})
		if err != nil {
			t.Fatalf("ValidateAudioAddRequest() error = %v, want nil", err)
		}
		if !reflect.DeepEqual(got.Files, files) {
			t.Errorf("Files = %#v, want request order %#v", got.Files, files)
		}
	})

	t.Run("A14_exactly_512_entries_is_accepted", func(t *testing.T) {
		files := distinctAudioFiles(512)
		got, err := ValidateAudioAddRequest(AddRequest{Type: "music", Title: "At cap", Files: files})
		if err != nil {
			t.Fatalf("ValidateAudioAddRequest() error = %v, want nil", err)
		}
		if !reflect.DeepEqual(got.Files, files) {
			t.Errorf("Files differ at the accepted cap: got %d ordered entries, want %d", len(got.Files), len(files))
		}
	})

	t.Run("A14_513_entries_is_400", func(t *testing.T) {
		_, err := ValidateAudioAddRequest(AddRequest{Type: "music", Title: "Over cap", Files: distinctAudioFiles(513)})
		assertAudioAddStatus(t, err, http.StatusBadRequest)
	})

	for _, tt := range []struct {
		name        string
		requestType string
		wantSection Section
	}{
		{name: "A15_movie_with_files_is_rejected_as_non_audio", requestType: "movie", wantSection: SectionMovies},
		{name: "A15_tv_with_files_is_rejected_as_non_audio", requestType: "tv", wantSection: SectionTV},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateAudioAddRequest(AddRequest{
				Type:  tt.requestType,
				Title: "Video with reserved files",
				Files: []AudioFileRequest{{SourcePath: "video.mkv", Path: "video.mkv"}},
			})
			assertAudioAddStatus(t, err, http.StatusBadRequest)
			if got.Section != tt.wantSection {
				t.Errorf("Section = %q, want resolved non-audio section %q", got.Section, tt.wantSection)
			}
		})
	}
}

func TestStatusForError(t *testing.T) {
	for _, tt := range []struct {
		name       string
		err        error
		wantStatus int
	}{
		{name: "A16_path_invalid_maps_to_400", err: ErrPathInvalid, wantStatus: http.StatusBadRequest},
		{name: "A17_unsupported_extension_maps_to_400", err: ErrExtensionUnsupported, wantStatus: http.StatusBadRequest},
		{name: "A18_extension_mismatch_maps_to_400", err: ErrExtensionMismatch, wantStatus: http.StatusBadRequest},
		{name: "A19_invalid_hash_suffix_maps_to_400", err: ErrHashSuffixInvalid, wantStatus: http.StatusBadRequest},
		{name: "A20_source_not_found_maps_to_422", err: ErrSourceNotFound, wantStatus: http.StatusUnprocessableEntity},
		{name: "A21_duplicate_source_maps_to_400", err: ErrSourceDuplicate, wantStatus: http.StatusBadRequest},
		{name: "A22_ambiguous_source_maps_to_502", err: ErrSourceAmbiguous, wantStatus: http.StatusBadGateway},
		{name: "A23_source_drift_maps_to_409", err: ErrSourceDrift, wantStatus: http.StatusConflict},
		{name: "A24_wrapped_sentinel_uses_errors_Is", err: fmt.Errorf("validation context: %w", ErrPathInvalid), wantStatus: http.StatusBadRequest},
		{name: "A25_nil_maps_to_200", err: nil, wantStatus: http.StatusOK},
		{name: "A25_unrecognised_error_maps_to_500", err: errors.New("unrecognised failure"), wantStatus: http.StatusInternalServerError},
		{name: "A26_existing_library_Error_keeps_its_status", err: &Error{Status: http.StatusTeapot, Message: "already classified"}, wantStatus: http.StatusTeapot},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := StatusForError(tt.err); got != tt.wantStatus {
				t.Errorf("StatusForError(%v) = %d, want %d", tt.err, got, tt.wantStatus)
			}
		})
	}
}

func distinctAudioFiles(count int) []AudioFileRequest {
	files := make([]AudioFileRequest, count)
	for i := range files {
		files[i] = AudioFileRequest{
			SourcePath: fmt.Sprintf("release/track-%03d.flac", i),
			Path:       fmt.Sprintf("Artist/track-%03d.flac", i),
		}
	}
	return files
}

func assertAudioAddStatus(t *testing.T, err error, wantStatus int) {
	t.Helper()
	var libraryErr *Error
	if !errors.As(err, &libraryErr) {
		t.Fatalf("ValidateAudioAddRequest() error = %v (%T), want *Error with status %d", err, err, wantStatus)
	}
	if libraryErr.Status != wantStatus {
		t.Errorf("ValidateAudioAddRequest() status = %d, want %d", libraryErr.Status, wantStatus)
	}
}
