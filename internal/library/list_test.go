package library

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"tiramisu/internal/metadb"
)

type audioListPageCall struct {
	section string
	prefix  string
	after   string
	limit   int
}

type audioListRegistryFake struct {
	rows  []metadb.AudioProjection
	err   error
	calls []audioListPageCall
}

var _ AudioProjectionRegistry = (*audioListRegistryFake)(nil)

func (f *audioListRegistryFake) AudioProjectionPage(section, prefix, after string, limit int) ([]metadb.AudioProjection, error) {
	f.calls = append(f.calls, audioListPageCall{section: section, prefix: prefix, after: after, limit: limit})
	if f.err != nil {
		return nil, f.err
	}
	rows := append([]metadb.AudioProjection(nil), f.rows...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].VirtualPath < rows[j].VirtualPath })
	page := make([]metadb.AudioProjection, 0, len(rows))
	for _, row := range rows {
		if row.Section != section || !strings.HasPrefix(row.VirtualPath, prefix) || row.VirtualPath <= after {
			continue
		}
		page = append(page, row)
		if limit > 0 && len(page) == limit {
			break
		}
	}
	return page, nil
}

func (f *audioListRegistryFake) GetAudioProjection(string, string) (*metadb.AudioProjection, bool, error) {
	panic("ListAudio must not perform point lookups")
}

func (f *audioListRegistryFake) AudioProjectionBySource(string, int) (*metadb.AudioProjection, bool, error) {
	panic("ListAudio must not perform source lookups")
}

func (f *audioListRegistryFake) AudioProjectionByPortableKey(string, string) (*metadb.AudioProjection, bool, error) {
	panic("ListAudio must not perform portable-key lookups")
}

func (f *audioListRegistryFake) StageAudioProjections(string, []metadb.AudioProjection) error {
	panic("ListAudio must not stage projections")
}

func (f *audioListRegistryFake) CommitAudioProjections(string, int64) (int, error) {
	panic("ListAudio must not commit projections")
}

func (f *audioListRegistryFake) RollbackAudioProjections(string) (int, error) {
	panic("ListAudio must not roll back projections")
}

func newAudioListManager(registry AudioProjectionRegistry) *Manager {
	return New(Config{
		AudioProjections: registry,
		Logger:           log.New(io.Discard, "", 0),
	})
}

func listProjection(section, path string, index int) metadb.AudioProjection {
	return metadb.AudioProjection{
		Section:     section,
		VirtualPath: path,
		Hash:        fmt.Sprintf("hash-%d", index),
		SourcePath:  fmt.Sprintf("release/disc/track-%d.flac", index),
		FileIndex:   index,
		Size:        int64(10_000 + index),
		MtimeNS:     int64(1_700_000_000_000_000_000 + index),
		State:       metadb.AudioCommitted,
	}
}

func listItemPaths(items []AudioListItem) []string {
	paths := make([]string, len(items))
	for i := range items {
		paths[i] = items[i].Path
	}
	return paths
}

func TestListAudioTypeResolution(t *testing.T) {
	valid := []struct {
		name        string
		requestType string
		wantSection string
	}{
		{name: "music_resolves_to_music", requestType: "music", wantSection: "music"},
		{name: "audiobook_resolves_to_audiobooks", requestType: "audiobook", wantSection: "audiobooks"},
	}
	for _, tc := range valid {
		t.Run("L12_"+tc.name, func(t *testing.T) {
			registry := &audioListRegistryFake{}
			response, err := newAudioListManager(registry).ListAudio(AudioListRequest{Type: tc.requestType, Limit: 2})
			if err != nil {
				t.Fatalf("ListAudio: %v", err)
			}
			if response == nil {
				t.Fatal("ListAudio response is nil")
			}
			if len(registry.calls) == 0 {
				t.Fatalf("registry was not queried for section %q", tc.wantSection)
			}
			for _, call := range registry.calls {
				if call.section != tc.wantSection {
					t.Fatalf("registry calls = %#v, want only section %q", registry.calls, tc.wantSection)
				}
			}
		})
	}

	invalid := []string{"", "Music", "music ", "audiobooks", "movie", "tv", "unknown"}
	for _, requestType := range invalid {
		t.Run("L12_exact_matching_rejects_"+fmt.Sprintf("%q", requestType), func(t *testing.T) {
			registry := &audioListRegistryFake{}
			response, err := newAudioListManager(registry).ListAudio(AudioListRequest{Type: requestType, Limit: 2})
			if err == nil {
				t.Fatalf("ListAudio type %q succeeded with %#v, want 400 error", requestType, response)
			}
			if got := StatusForError(err); got != http.StatusBadRequest {
				t.Errorf("status = %d, want %d; error: %v", got, http.StatusBadRequest, err)
			}
			if len(registry.calls) != 0 {
				t.Fatalf("invalid type queried registry: %#v", registry.calls)
			}
		})
	}
}

func TestListAudioResponseShaping(t *testing.T) {
	t.Run("L13_item_preserves_every_reconciliation_field_verbatim", func(t *testing.T) {
		row := listProjection("audiobooks", "Äuthor/% Live/01 - Ch_apter_a1b2c3d4.m4b", 7)
		row.Hash = "ABCDEF0123456789"
		row.SourcePath = "Disc 01/% source_track.M4B"
		row.Size = 9_876_543_210
		row.MtimeNS = 1_800_000_000_123_456_789
		decoy := listProjection("audiobooks", "Other/Book/01 - Decoy_11111111.m4b", 8)
		registry := &audioListRegistryFake{rows: []metadb.AudioProjection{decoy, row}}

		const prefix = "Äuthor/% Live/"
		response, err := newAudioListManager(registry).ListAudio(AudioListRequest{
			Type: "audiobook", Prefix: prefix, Limit: 10,
		})
		if err != nil {
			t.Fatalf("ListAudio: %v", err)
		}
		if len(registry.calls) == 0 {
			t.Fatal("registry was not queried")
		}
		for _, call := range registry.calls {
			if call.prefix != prefix {
				t.Fatalf("registry calls = %#v, want byte-identical prefix %q", registry.calls, prefix)
			}
		}
		want := []AudioListItem{{
			Type:       "audiobook",
			Path:       row.VirtualPath,
			Hash:       row.Hash,
			SourcePath: row.SourcePath,
			FileIndex:  row.FileIndex,
			Size:       row.Size,
			MtimeNS:    row.MtimeNS,
			State:      string(row.State),
		}}
		if !reflect.DeepEqual(response.Items, want) {
			t.Fatalf("items = %#v, want %#v", response.Items, want)
		}
	})

	t.Run("L14_exactly_full_last_page_has_an_empty_cursor", func(t *testing.T) {
		rows := []metadb.AudioProjection{
			listProjection("music", "Artist/Album/01 - One_11111111.flac", 1),
			listProjection("music", "Artist/Album/02 - Two_22222222.flac", 2),
		}
		registry := &audioListRegistryFake{rows: rows}
		response, err := newAudioListManager(registry).ListAudio(AudioListRequest{Type: "music", Limit: 2})
		if err != nil {
			t.Fatalf("ListAudio: %v", err)
		}
		if paths := listItemPaths(response.Items); !reflect.DeepEqual(paths, []string{rows[0].VirtualPath, rows[1].VirtualPath}) {
			t.Fatalf("last-page paths = %v, want both rows", paths)
		}
		if response.NextCursor != "" {
			t.Fatalf("last-page cursor = %q, want empty", response.NextCursor)
		}
	})

	t.Run("L16_empty_result_has_non_null_items_and_empty_cursor", func(t *testing.T) {
		registry := &audioListRegistryFake{}
		response, err := newAudioListManager(registry).ListAudio(AudioListRequest{Type: "music", Limit: 2})
		if err != nil {
			t.Fatalf("ListAudio: %v", err)
		}
		if response.Items == nil || len(response.Items) != 0 {
			t.Fatalf("empty items = %#v, want allocated empty slice", response.Items)
		}
		if response.NextCursor != "" {
			t.Fatalf("empty-result cursor = %q, want empty", response.NextCursor)
		}
		encoded, err := json.Marshal(response)
		if err != nil {
			t.Fatalf("marshal response: %v", err)
		}
		if !strings.Contains(string(encoded), `"items":[]`) {
			t.Fatalf("JSON response = %s, want non-null empty items array", encoded)
		}
	})
}

func TestListAudioCursorRoundTrip(t *testing.T) {
	t.Run("L15_returned_cursor_fetches_the_following_page_without_overlap", func(t *testing.T) {
		rows := []metadb.AudioProjection{
			listProjection("music", "Artist/Album/01 - One_11111111.flac", 1),
			listProjection("music", "Artist/Album/02 - Two_22222222.flac", 2),
			listProjection("music", "Artist/Album/03 - Three_33333333.flac", 3),
		}
		registry := &audioListRegistryFake{rows: rows}
		manager := newAudioListManager(registry)

		first, err := manager.ListAudio(AudioListRequest{Type: "music", Limit: 2})
		if err != nil {
			t.Fatalf("first ListAudio: %v", err)
		}
		if first.NextCursor == "" {
			t.Fatal("first page cursor is empty, want a continuation")
		}
		second, err := manager.ListAudio(AudioListRequest{Type: "music", Limit: 2, Cursor: first.NextCursor})
		if err != nil {
			t.Fatalf("second ListAudio: %v", err)
		}
		if second.NextCursor != "" {
			t.Fatalf("final cursor = %q, want empty", second.NextCursor)
		}

		firstPaths := listItemPaths(first.Items)
		secondPaths := listItemPaths(second.Items)
		if !reflect.DeepEqual(firstPaths, []string{rows[0].VirtualPath, rows[1].VirtualPath}) {
			t.Fatalf("first page = %v, want first two rows", firstPaths)
		}
		if !reflect.DeepEqual(secondPaths, []string{rows[2].VirtualPath}) {
			t.Fatalf("second page = %v, want final row", secondPaths)
		}
		all := append(append([]string(nil), firstPaths...), secondPaths...)
		if !reflect.DeepEqual(all, []string{rows[0].VirtualPath, rows[1].VirtualPath, rows[2].VirtualPath}) {
			t.Fatalf("combined pages = %v, want the full ordered set", all)
		}
	})
}

func TestListAudioRegistryBoundary(t *testing.T) {
	t.Run("L17_registry_request_is_bounded_to_limit_plus_one", func(t *testing.T) {
		rows := make([]metadb.AudioProjection, 0, 50)
		for i := 0; i < 50; i++ {
			rows = append(rows, listProjection(
				"music",
				fmt.Sprintf("Artist/Album/%02d - Track_%08d.flac", i, i),
				i+1,
			))
		}
		registry := &audioListRegistryFake{rows: rows}
		const requestedLimit = 4
		response, err := newAudioListManager(registry).ListAudio(AudioListRequest{Type: "music", Limit: requestedLimit})
		if err != nil {
			t.Fatalf("ListAudio: %v", err)
		}
		if len(response.Items) != requestedLimit {
			t.Fatalf("response items = %d, want %d", len(response.Items), requestedLimit)
		}
		if response.NextCursor == "" {
			t.Fatal("cursor is empty despite more rows")
		}
		if len(registry.calls) == 0 {
			t.Fatal("registry was not queried")
		}
		totalRequested := 0
		for _, call := range registry.calls {
			if call.limit <= 0 {
				t.Fatalf("registry page limit = %d, want a positive bound", call.limit)
			}
			totalRequested += call.limit
		}
		if totalRequested > requestedLimit+1 {
			t.Fatalf("registry was asked for %d rows across calls %#v, want at most %d", totalRequested, registry.calls, requestedLimit+1)
		}
	})

	t.Run("L18_registry_error_is_propagated_not_turned_into_an_empty_page", func(t *testing.T) {
		sentinel := errors.New("registry storage unavailable")
		registry := &audioListRegistryFake{err: sentinel}
		_, err := newAudioListManager(registry).ListAudio(AudioListRequest{Type: "music", Limit: 2})
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want errors.Is(_, %v)", err, sentinel)
		}
		if len(registry.calls) == 0 {
			t.Fatal("registry error was returned without querying the registry")
		}
	})
}
