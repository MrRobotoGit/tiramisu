package metadb

import (
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func audioPageProjection(tag, section, virtualPath string) AudioProjection {
	p := audioProjection("page-" + tag)
	p.Section = section
	p.VirtualPath = virtualPath
	p.PortablePathKey = "portable/" + tag
	p.Hash = "page-hash-" + tag
	p.FileIndex = 1
	p.SourcePath = "source/" + tag + ".flac"
	p.Size = int64(10_000 + len(tag))
	p.MtimeNS = int64(1_700_000_000_000_000_000 + len(tag))
	p.Title = "Page title " + tag
	p.Magnet = "magnet:?xt=urn:btih:page-" + tag
	p.ExternalID = "external-" + tag
	p.ExternalIDNamespace = "test-namespace"
	p.StagingName = ".stage-page-" + tag
	p.CreatedAtNS = int64(1_700_000_000_100_000_000 + len(tag))
	p.UpdatedAtNS = int64(1_700_000_000_200_000_000 + len(tag))
	return p
}

func commitAudioPageRows(t *testing.T, db *DB, txnID string, updatedAtNS int64, rows []AudioProjection) []AudioProjection {
	t.Helper()
	if err := db.StageAudioProjections(txnID, rows); err != nil {
		t.Fatalf("stage audio page rows: %v", err)
	}
	changed, err := db.CommitAudioProjections(txnID, updatedAtNS)
	if err != nil {
		t.Fatalf("commit audio page rows: %v", err)
	}
	if changed != len(rows) {
		t.Fatalf("committed rows = %d, want %d", changed, len(rows))
	}
	want := append([]AudioProjection(nil), rows...)
	for i := range want {
		want[i].TxnID = txnID
		want[i].State = AudioCommitted
		want[i].UpdatedAtNS = updatedAtNS
	}
	return want
}

func audioPagePaths(rows []AudioProjection) []string {
	paths := make([]string, len(rows))
	for i := range rows {
		paths[i] = rows[i].VirtualPath
	}
	return paths
}

func TestAudioProjectionPagePublishedRows(t *testing.T) {
	t.Run("L1_committed_rows_are_ordered_and_every_column_is_populated", func(t *testing.T) {
		db := openAudioTestDB(t, filepath.Join(t.TempDir(), "ordered.db"))
		rows := []AudioProjection{
			audioPageProjection("z", "music", "Artist/Album/02 - Zed_22222222.flac"),
			audioPageProjection("a", "music", "Artist/Album/01 - Alpha_11111111.flac"),
		}
		const updatedAtNS = int64(1_800_000_000_000_000_001)
		want := commitAudioPageRows(t, db, "L1-committed", updatedAtNS, rows)
		sort.Slice(want, func(i, j int) bool { return want[i].VirtualPath < want[j].VirtualPath })

		got, err := db.AudioProjectionPage("music", "", "", 10)
		if err != nil {
			t.Fatalf("AudioProjectionPage: %v", err)
		}
		if len(got) != len(want) {
			t.Fatalf("rows = %#v, want %d rows", got, len(want))
		}
		for i := range want {
			requireProjectionFields(t, &got[i], want[i], AudioCommitted)
		}
	})

	t.Run("L2_other_audio_section_is_excluded", func(t *testing.T) {
		db := openAudioTestDB(t, filepath.Join(t.TempDir(), "sections.db"))
		music := audioPageProjection("music", "music", "Artist/Album/Track_11111111.flac")
		book := audioPageProjection("book", "audiobooks", "Author/Book/Chapter_22222222.m4b")
		commitAudioPageRows(t, db, "L2-sections", 2_001, []AudioProjection{book, music})

		got, err := db.AudioProjectionPage("music", "", "", 10)
		if err != nil {
			t.Fatalf("AudioProjectionPage: %v", err)
		}
		if paths := audioPagePaths(got); !reflect.DeepEqual(paths, []string{music.VirtualPath}) {
			t.Fatalf("music page paths = %v, want [%s]", paths, music.VirtualPath)
		}
	})

	t.Run("L3_staged_and_removing_rows_are_not_published", func(t *testing.T) {
		db := openAudioTestDB(t, filepath.Join(t.TempDir(), "states.db"))
		committed := audioPageProjection("committed", "music", "Artist/Album/Committed_11111111.flac")
		staged := audioPageProjection("staged", "music", "Artist/Album/Staged_22222222.flac")
		removing := audioPageProjection("removing", "music", "Artist/Album/Removing_33333333.flac")
		commitAudioPageRows(t, db, "L3-committed", 3_001, []AudioProjection{committed})
		if err := db.StageAudioProjections("L3-staged", []AudioProjection{staged}); err != nil {
			t.Fatalf("stage unpublished row: %v", err)
		}
		commitAudioPageRows(t, db, "L3-removing", 3_002, []AudioProjection{removing})
		ok, err := db.MarkAudioProjectionRemoving(removing.Section, removing.VirtualPath, 3_003)
		if err != nil || !ok {
			t.Fatalf("mark removing row = %v, %v; want true, nil", ok, err)
		}

		got, err := db.AudioProjectionPage("music", "", "", 10)
		if err != nil {
			t.Fatalf("AudioProjectionPage: %v", err)
		}
		if paths := audioPagePaths(got); !reflect.DeepEqual(paths, []string{committed.VirtualPath}) {
			t.Fatalf("published paths = %v, want only %q", paths, committed.VirtualPath)
		}
		if got[0].State != AudioCommitted {
			t.Fatalf("published state = %q, want %q", got[0].State, AudioCommitted)
		}
	})
}

func TestAudioProjectionPagePrefix(t *testing.T) {
	t.Run("L4_prefix_matches_only_at_the_start_of_the_path", func(t *testing.T) {
		db := openAudioTestDB(t, filepath.Join(t.TempDir(), "prefix.db"))
		atStart := audioPageProjection("prefix-start", "music", "Artist/Album/Track_11111111.flac")
		later := audioPageProjection("prefix-later", "music", "Archive/Artist/Album/Track_22222222.flac")
		commitAudioPageRows(t, db, "L4-prefix", 4_001, []AudioProjection{later, atStart})

		got, err := db.AudioProjectionPage("music", "Artist/Album/", "", 10)
		if err != nil {
			t.Fatalf("AudioProjectionPage: %v", err)
		}
		if paths := audioPagePaths(got); !reflect.DeepEqual(paths, []string{atStart.VirtualPath}) {
			t.Fatalf("prefix page paths = %v, want [%s]", paths, atStart.VirtualPath)
		}
	})

	t.Run("L5_prefix_is_byte_exact_and_case_sensitive", func(t *testing.T) {
		db := openAudioTestDB(t, filepath.Join(t.TempDir(), "case.db"))
		exact := audioPageProjection("case-exact", "music", "Artist/Album/Track_11111111.flac")
		variant := audioPageProjection("case-variant", "music", "artist/Album/Track_22222222.flac")
		commitAudioPageRows(t, db, "L5-case", 5_001, []AudioProjection{variant, exact})

		got, err := db.AudioProjectionPage("music", "Artist/", "", 10)
		if err != nil {
			t.Fatalf("AudioProjectionPage: %v", err)
		}
		if paths := audioPagePaths(got); !reflect.DeepEqual(paths, []string{exact.VirtualPath}) {
			t.Fatalf("case-sensitive prefix paths = %v, want [%s]", paths, exact.VirtualPath)
		}
	})

	wildcards := []struct {
		name        string
		prefix      string
		literalPath string
		decoyPath   string
	}{
		{
			name:        "underscore_is_not_a_single_character_wildcard",
			prefix:      "Artist/Album/01 - Track_",
			literalPath: "Artist/Album/01 - Track_Name_a1b2c3d4.flac",
			decoyPath:   "Artist/Album/01 - TrackXName_e5f6a7b8.flac",
		},
		{
			name:        "percent_is_not_a_multi_character_wildcard",
			prefix:      "Artist/Album/02 - Track%",
			literalPath: "Artist/Album/02 - Track%Live_a1b2c3d4.flac",
			decoyPath:   "Artist/Album/02 - TrackAnythingLive_e5f6a7b8.flac",
		},
	}
	for _, tc := range wildcards {
		t.Run("L9_"+tc.name, func(t *testing.T) {
			db := openAudioTestDB(t, filepath.Join(t.TempDir(), "literal-wildcard.db"))
			literal := audioPageProjection(tc.name+"-literal", "music", tc.literalPath)
			decoy := audioPageProjection(tc.name+"-decoy", "music", tc.decoyPath)
			commitAudioPageRows(t, db, "L9-"+tc.name, 9_001, []AudioProjection{decoy, literal})

			got, err := db.AudioProjectionPage("music", tc.prefix, "", 10)
			if err != nil {
				t.Fatalf("AudioProjectionPage(%q): %v", tc.prefix, err)
			}
			if paths := audioPagePaths(got); !reflect.DeepEqual(paths, []string{literal.VirtualPath}) {
				t.Fatalf("literal prefix %q paths = %v, want [%s]", tc.prefix, paths, literal.VirtualPath)
			}
		})
	}
}

func TestAudioProjectionPagePagination(t *testing.T) {
	t.Run("L6_limit_caps_the_page_size", func(t *testing.T) {
		db := openAudioTestDB(t, filepath.Join(t.TempDir(), "limit.db"))
		var rows []AudioProjection
		for i := 0; i < 5; i++ {
			rows = append(rows, audioPageProjection(
				fmt.Sprintf("limit-%02d", i),
				"music",
				fmt.Sprintf("Artist/Album/%02d - Track_%08d.flac", i, i),
			))
		}
		commitAudioPageRows(t, db, "L6-limit", 6_001, rows)

		got, err := db.AudioProjectionPage("music", "", "", 2)
		if err != nil {
			t.Fatalf("AudioProjectionPage: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("page length = %d, want 2", len(got))
		}
		if paths := audioPagePaths(got); !reflect.DeepEqual(paths, audioPagePaths(rows[:2])) {
			t.Fatalf("limited paths = %v, want %v", paths, audioPagePaths(rows[:2]))
		}
	})

	t.Run("L7_after_path_resumes_strictly_after_the_cursor", func(t *testing.T) {
		db := openAudioTestDB(t, filepath.Join(t.TempDir(), "cursor.db"))
		var rows []AudioProjection
		for i, name := range []string{"Alpha", "Bravo", "Charlie", "Delta"} {
			rows = append(rows, audioPageProjection(
				fmt.Sprintf("cursor-%d", i),
				"music",
				fmt.Sprintf("Artist/Album/%s_%08d.flac", name, i),
			))
		}
		commitAudioPageRows(t, db, "L7-cursor", 7_001, rows)

		got, err := db.AudioProjectionPage("music", "", rows[1].VirtualPath, 10)
		if err != nil {
			t.Fatalf("AudioProjectionPage: %v", err)
		}
		if paths := audioPagePaths(got); !reflect.DeepEqual(paths, audioPagePaths(rows[2:])) {
			t.Fatalf("paths after %q = %v, want %v", rows[1].VirtualPath, paths, audioPagePaths(rows[2:]))
		}
	})

	t.Run("L8_pages_of_three_walk_every_row_once_in_order", func(t *testing.T) {
		db := openAudioTestDB(t, filepath.Join(t.TempDir(), "walk.db"))
		const rowCount = 10
		rows := make([]AudioProjection, 0, rowCount)
		wantPaths := make([]string, 0, rowCount)
		for i := 0; i < rowCount; i++ {
			path := fmt.Sprintf("Artist/Album/%02d - Track_%08d.flac", i, i)
			rows = append(rows, audioPageProjection(fmt.Sprintf("walk-%02d", i), "music", path))
			wantPaths = append(wantPaths, path)
		}
		commitAudioPageRows(t, db, "L8-walk", 8_001, rows)

		var gotPaths []string
		afterPath := ""
		nonemptyPages := 0
		for {
			page, err := db.AudioProjectionPage("music", "", afterPath, 3)
			if err != nil {
				t.Fatalf("AudioProjectionPage after %q: %v", afterPath, err)
			}
			if len(page) > 3 {
				t.Fatalf("page %d length = %d, want at most 3", nonemptyPages+1, len(page))
			}
			if len(page) == 0 {
				break
			}
			nonemptyPages++
			for _, row := range page {
				if row.VirtualPath <= afterPath {
					t.Fatalf("page %d returned %q at or before cursor %q", nonemptyPages, row.VirtualPath, afterPath)
				}
				gotPaths = append(gotPaths, row.VirtualPath)
			}
			afterPath = page[len(page)-1].VirtualPath
			if nonemptyPages > rowCount {
				t.Fatal("pagination did not terminate")
			}
		}
		if nonemptyPages < 4 {
			t.Fatalf("nonempty pages = %d, want at least 4", nonemptyPages)
		}
		if !reflect.DeepEqual(gotPaths, wantPaths) {
			t.Fatalf("walked paths = %v, want %v", gotPaths, wantPaths)
		}
		seen := make(map[string]bool, len(gotPaths))
		for _, path := range gotPaths {
			if seen[path] {
				t.Fatalf("path %q appeared more than once", path)
			}
			seen[path] = true
		}
	})

	t.Run("L10_nonpositive_defaults_and_above_maximum_is_capped", func(t *testing.T) {
		if DefaultAudioProjectionPageLimit <= 0 {
			t.Fatalf("DefaultAudioProjectionPageLimit = %d, want positive", DefaultAudioProjectionPageLimit)
		}
		if MaxAudioProjectionPageLimit < DefaultAudioProjectionPageLimit {
			t.Fatalf("MaxAudioProjectionPageLimit = %d, want at least default %d", MaxAudioProjectionPageLimit, DefaultAudioProjectionPageLimit)
		}
		db := openAudioTestDB(t, filepath.Join(t.TempDir(), "normalized-limit.db"))
		rows := make([]AudioProjection, 0, MaxAudioProjectionPageLimit+1)
		for i := 0; i <= MaxAudioProjectionPageLimit; i++ {
			rows = append(rows, audioPageProjection(
				fmt.Sprintf("normalized-%06d", i),
				"music",
				fmt.Sprintf("Artist/Album/%06d - Track_%08d.flac", i, i),
			))
		}
		commitAudioPageRows(t, db, "L10-normalized", 10_001, rows)

		for _, limit := range []int{0, -1} {
			got, err := db.AudioProjectionPage("music", "", "", limit)
			if err != nil {
				t.Fatalf("AudioProjectionPage limit %d: %v", limit, err)
			}
			if len(got) != DefaultAudioProjectionPageLimit {
				t.Errorf("limit %d returned %d rows, want default %d", limit, len(got), DefaultAudioProjectionPageLimit)
			}
		}

		got, err := db.AudioProjectionPage("music", "", "", MaxAudioProjectionPageLimit+1)
		if err != nil {
			t.Fatalf("AudioProjectionPage above maximum: %v", err)
		}
		if len(got) != MaxAudioProjectionPageLimit {
			t.Fatalf("above-maximum limit returned %d rows, want cap %d", len(got), MaxAudioProjectionPageLimit)
		}
	})
}

func TestAudioProjectionPageEmpty(t *testing.T) {
	t.Run("L11_empty_section_returns_no_rows_and_no_error", func(t *testing.T) {
		db := openAudioTestDB(t, filepath.Join(t.TempDir(), "empty.db"))
		got, err := db.AudioProjectionPage("music", "", "", 10)
		if err != nil {
			t.Fatalf("AudioProjectionPage: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("empty page = %#v, want no rows", got)
		}
	})
}
