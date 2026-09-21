package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"tiramisu/internal/metadb"
)

// The audio external identity is an opaque, caller-owned token pair. These tests
// pin that the engine carries it per projection, byte for byte, and never
// interprets, polices or invents it.

const (
	extidHash = "0123456789abcdef0123456789abcdef01234567"

	// Real-shaped values: a MusicBrainz recording MBID and an Audible ASIN.
	extidMBID = "b9ad642e-b012-41c7-b72a-42cf4911f9ff"
	extidASIN = "B002V0QK4C"
)

type extidEngine struct {
	mu    sync.Mutex
	info  *TorrentStats
	calls []string
}

func (e *extidEngine) record(call string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, call)
}

func (e *extidEngine) AddTorrent(context.Context, string, string) (string, error) {
	e.record("AddTorrent")
	return extidHash, nil
}

func (e *extidEngine) GetTorrentInfo(context.Context, string, int) (*TorrentStats, error) {
	e.record("GetTorrentInfo")
	return e.info, nil
}

func (e *extidEngine) RemoveTorrent(context.Context, string) error {
	e.record("RemoveTorrent")
	return nil
}

func (e *extidEngine) ListTorrents(context.Context) ([]TorrentStats, error) {
	e.record("ListTorrents")
	return nil, nil
}

func (e *extidEngine) callLog() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

type extidRegistry struct {
	mu        sync.Mutex
	committed map[string]metadb.AudioProjection
	staged    map[string][]metadb.AudioProjection
	stageErr  error
	stages    [][]metadb.AudioProjection
}

var _ AudioProjectionRegistry = (*extidRegistry)(nil)

func newExtidRegistry(seed ...metadb.AudioProjection) *extidRegistry {
	r := &extidRegistry{
		committed: make(map[string]metadb.AudioProjection),
		staged:    make(map[string][]metadb.AudioProjection),
	}
	for _, row := range seed {
		r.committed[row.Section+"\x00"+row.VirtualPath] = row
	}
	return r
}

func (r *extidRegistry) GetAudioProjection(section, path string) (*metadb.AudioProjection, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.committed[section+"\x00"+path]
	if !ok {
		return nil, false, nil
	}
	return &row, true, nil
}

func (r *extidRegistry) AudioProjectionBySource(hash string, fileIndex int) (*metadb.AudioProjection, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range r.committed {
		if row.Hash == hash && row.FileIndex == fileIndex {
			row := row
			return &row, true, nil
		}
	}
	return nil, false, nil
}

func (r *extidRegistry) AudioProjectionByPortableKey(section, key string) (*metadb.AudioProjection, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range r.committed {
		if row.Section == section && row.PortablePathKey == key {
			row := row
			return &row, true, nil
		}
	}
	return nil, false, nil
}

func (r *extidRegistry) StageAudioProjections(txnID string, rows []metadb.AudioProjection) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stages = append(r.stages, append([]metadb.AudioProjection(nil), rows...))
	if r.stageErr != nil {
		return r.stageErr
	}
	r.staged[txnID] = append([]metadb.AudioProjection(nil), rows...)
	return nil
}

func (r *extidRegistry) CommitAudioProjections(txnID string, _ int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows := r.staged[txnID]
	for _, row := range rows {
		row.State = metadb.AudioCommitted
		r.committed[row.Section+"\x00"+row.VirtualPath] = row
	}
	delete(r.staged, txnID)
	return len(rows), nil
}

func (r *extidRegistry) RollbackAudioProjections(txnID string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.staged[txnID])
	delete(r.staged, txnID)
	return n, nil
}

func (r *extidRegistry) AudioProjectionPage(section, prefix, after string, limit int) ([]metadb.AudioProjection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var rows []metadb.AudioProjection
	for _, row := range r.committed {
		if row.Section == section && strings.HasPrefix(row.VirtualPath, prefix) && row.VirtualPath > after {
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].VirtualPath < rows[j].VirtualPath })
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (r *extidRegistry) stagedBatches() [][]metadb.AudioProjection {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]metadb.AudioProjection(nil), r.stages...)
}

type extidFixture struct {
	root     string
	engine   *extidEngine
	registry *extidRegistry
	manager  *Manager
}

// extidTrack is the source path of track n and extidDest its caller path.
func extidTrack(n int) string { return fmt.Sprintf("Release/%02d - Track.flac", n) }

func extidDest(n int) string {
	return fmt.Sprintf("Artist/Album/%02d - Track_%s.flac", n, extidHash[:8])
}

func newExtidFixture(t *testing.T, seed ...metadb.AudioProjection) *extidFixture {
	t.Helper()
	root := t.TempDir()
	for _, section := range []Section{SectionMusic, SectionAudiobooks, SectionMovies, SectionTV} {
		if err := os.Mkdir(filepath.Join(root, string(section)), 0o755); err != nil {
			t.Fatalf("Mkdir(%s): %v", section, err)
		}
	}
	stats := make([]FileStat, 0, 4)
	for n := 1; n <= 4; n++ {
		stats = append(stats, FileStat{ID: n, Path: extidTrack(n), Length: int64(1000 * n)})
	}
	engine := &extidEngine{info: &TorrentStats{Hash: extidHash, FileStats: stats}}
	registry := newExtidRegistry(seed...)
	return &extidFixture{
		root:     root,
		engine:   engine,
		registry: registry,
		manager: New(Config{
			MoviesDir:        filepath.Join(root, string(SectionMovies)),
			TVDir:            filepath.Join(root, string(SectionTV)),
			GoStormURL:       "http://gostorm.invalid",
			GoStorm:          engine,
			AudioRoot:        root,
			AudioProjections: registry,
			Logger:           log.New(io.Discard, "", 0),
		}),
	}
}

func extidRequest(files ...AudioFileRequest) AddRequest {
	return AddRequest{Type: "music", Hash: extidHash, Title: "An Album", Files: files}
}

// extidFile builds the request entry for track n.
func extidFile(n int, id, ns string) AudioFileRequest {
	return AudioFileRequest{
		SourcePath:          extidTrack(n),
		Path:                extidDest(n),
		ExternalID:          id,
		ExternalIDNamespace: ns,
	}
}

func extidReadStub(t *testing.T, root string, n int) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, string(SectionMusic), filepath.FromSlash(extidDest(n))))
	if err != nil {
		t.Fatalf("read stub for track %d: %v", n, err)
	}
	var stub map[string]any
	if err := json.Unmarshal(data, &stub); err != nil {
		t.Fatalf("stub for track %d is not JSON: %v\n%s", n, err, data)
	}
	return stub
}

func extidRequireStatus(t *testing.T, err error, want int) {
	t.Helper()
	var libErr *Error
	if !errors.As(err, &libErr) {
		t.Fatalf("error = %v (%T), want *Error with status %d", err, err, want)
	}
	if libErr.Status != want {
		t.Fatalf("status = %d, want %d (error: %v)", libErr.Status, want, err)
	}
}

// Values that a normalising or trimming implementation would rewrite.
var extidOpaqueValues = []struct {
	name, id, ns string
}{
	{"real_mbid_and_asin_shape", extidMBID, "musicbrainz"},
	{"asin_audiobook_shape", extidASIN, "asin"},
	{"mixed_case_id_is_not_folded", "B002v0Qk4c-MiXeD", "MusicBrainz"},
	{"upper_case_namespace_is_not_folded", extidMBID, "ASIN"},
	{"upper_case_mbid_is_not_folded", strings.ToUpper(extidMBID), "musicbrainz"},
	{"surrounding_punctuation_is_kept", `"<{[(` + extidMBID + `)]}>";.`, "mb:v2/"},
	{"padded_whitespace_is_not_trimmed", "  " + extidMBID + "\t", " musicbrainz "},
	{"interior_whitespace_is_kept", "a  b\tc", "two  words"},
	{"non_nfc_text_is_not_normalised", "café", "nfd́"},
	{"json_hostile_characters_round_trip", `a"b\c<&>` + " ", "ns\"\\"},
	{"not_an_mbid_and_not_an_asin", "1", "x"},
}

func TestExternalIdentity_ValidationCarriesBothFields(t *testing.T) {
	t.Run("X1_byte_for_byte_preservation", func(t *testing.T) {
		for _, tc := range extidOpaqueValues {
			t.Run(tc.name, func(t *testing.T) {
				intent, err := ValidateAudioAddRequest(extidRequest(extidFile(1, tc.id, tc.ns)))
				if err != nil {
					t.Fatalf("ValidateAudioAddRequest() error = %v, want nil", err)
				}
				if len(intent.Files) != 1 {
					t.Fatalf("Files = %#v, want one entry", intent.Files)
				}
				got := intent.Files[0]
				if got.ExternalID != tc.id || got.ExternalIDNamespace != tc.ns {
					t.Errorf("identity = (%q, %q), want (%q, %q) unchanged", got.ExternalID, got.ExternalIDNamespace, tc.id, tc.ns)
				}
			})
		}
	})

	t.Run("X2_neither_supplied_is_accepted_and_empty", func(t *testing.T) {
		intent, err := ValidateAudioAddRequest(extidRequest(extidFile(1, "", "")))
		if err != nil {
			t.Fatalf("ValidateAudioAddRequest() error = %v, want nil", err)
		}
		if got := intent.Files[0]; got.ExternalID != "" || got.ExternalIDNamespace != "" {
			t.Errorf("identity = (%q, %q), want both empty", got.ExternalID, got.ExternalIDNamespace)
		}
	})

	t.Run("X7_no_vocabulary_is_enforced_on_the_namespace", func(t *testing.T) {
		for _, ns := range []string{"discogs", "isbn", "spotify", "acoustid", "made-up-by-a-controller", "MUSICBRAINZ", "x"} {
			t.Run(ns, func(t *testing.T) {
				intent, err := ValidateAudioAddRequest(extidRequest(extidFile(1, "some-id", ns)))
				if err != nil {
					t.Fatalf("namespace %q: error = %v, want accepted", ns, err)
				}
				if intent.Files[0].ExternalIDNamespace != ns {
					t.Errorf("namespace = %q, want %q", intent.Files[0].ExternalIDNamespace, ns)
				}
			})
		}
	})

	t.Run("X7_no_format_check_on_the_id", func(t *testing.T) {
		// Not an MBID, not a 10-character ASIN, not even printable-looking.
		for _, id := range []string{"1", "not-a-uuid", extidMBID + "-extra", "\x01\x02", strings.Repeat("z", 300)} {
			if _, err := ValidateAudioAddRequest(extidRequest(extidFile(1, id, "musicbrainz"))); err != nil {
				t.Errorf("id %q: error = %v, want accepted", id, err)
			}
		}
	})

	t.Run("X8_entries_may_carry_different_namespaces", func(t *testing.T) {
		intent, err := ValidateAudioAddRequest(extidRequest(
			extidFile(1, extidMBID, "musicbrainz"),
			extidFile(2, extidASIN, "asin"),
			extidFile(3, "r123", "discogs"),
			extidFile(4, "", ""),
		))
		if err != nil {
			t.Fatalf("ValidateAudioAddRequest() error = %v, want nil", err)
		}
		want := [][2]string{{extidMBID, "musicbrainz"}, {extidASIN, "asin"}, {"r123", "discogs"}, {"", ""}}
		for i, w := range want {
			got := intent.Files[i]
			if got.ExternalID != w[0] || got.ExternalIDNamespace != w[1] {
				t.Errorf("files[%d] identity = (%q, %q), want (%q, %q)", i, got.ExternalID, got.ExternalIDNamespace, w[0], w[1])
			}
		}
	})

	t.Run("X8_identity_repeated_across_entries_is_not_a_duplicate", func(t *testing.T) {
		// An audiobook repeats one ASIN across chapters; the engine imposes no
		// uniqueness on the external identity.
		intent, err := ValidateAudioAddRequest(AddRequest{
			Type: "audiobook", Title: "A Book",
			Files: []AudioFileRequest{extidFile(1, extidASIN, "asin"), extidFile(2, extidASIN, "asin"), extidFile(3, extidASIN, "asin")},
		})
		if err != nil {
			t.Fatalf("ValidateAudioAddRequest() error = %v, want nil", err)
		}
		for i, f := range intent.Files {
			if f.ExternalID != extidASIN || f.ExternalIDNamespace != "asin" {
				t.Errorf("files[%d] identity = (%q, %q), want the repeated pair", i, f.ExternalID, f.ExternalIDNamespace)
			}
		}
	})

	t.Run("X8_source_and_path_uniqueness_still_apply", func(t *testing.T) {
		// (hash, file_index) stays the identity key: a shared external identity
		// must not paper over a repeated source or destination.
		dup := extidFile(1, extidMBID, "musicbrainz")
		_, err := ValidateAudioAddRequest(extidRequest(dup, dup))
		extidRequireStatus(t, err, http.StatusBadRequest)
	})
}

func TestExternalIdentity_PairRule(t *testing.T) {
	halves := []struct {
		name, id, ns string
	}{
		{"X3_id_without_namespace", extidMBID, ""},
		{"X4_namespace_without_id", "", "musicbrainz"},
		{"X5_whitespace_only_id_with_namespace", " ", "musicbrainz"},
		{"X5_tab_newline_id_with_namespace", "\t\n", "musicbrainz"},
		{"X5_whitespace_only_namespace_with_id", extidMBID, "  "},
		{"X5_whitespace_only_id_is_not_a_valid_identity_for_any_namespace", " ", "discogs"},
	}

	for _, tc := range halves {
		t.Run(tc.name+"_validate", func(t *testing.T) {
			_, err := ValidateAudioAddRequest(extidRequest(extidFile(1, tc.id, tc.ns)))
			extidRequireStatus(t, err, http.StatusBadRequest)
		})
		t.Run(tc.name+"_add_rejected_before_engine_registry_or_disk", func(t *testing.T) {
			f := newExtidFixture(t)
			resp, err := f.manager.AddAudio(context.Background(), extidRequest(extidFile(1, tc.id, tc.ns)))
			extidRequireStatus(t, err, http.StatusBadRequest)
			if resp != nil {
				t.Errorf("response = %#v, want nil", resp)
			}
			extidRequireNothingHappened(t, f)
		})
	}

	t.Run("X3_error_names_the_offending_file", func(t *testing.T) {
		_, err := ValidateAudioAddRequest(extidRequest(
			extidFile(1, extidMBID, "musicbrainz"),
			extidFile(2, extidMBID, ""),
		))
		extidRequireStatus(t, err, http.StatusBadRequest)
		if !strings.Contains(err.Error(), "files[1]") {
			t.Errorf("error = %q, want it to name files[1]", err.Error())
		}
	})

	t.Run("X6_one_half_supplied_entry_fails_the_whole_request", func(t *testing.T) {
		cases := []struct {
			name  string
			files []AudioFileRequest
		}{
			{"half_entry_last", []AudioFileRequest{
				extidFile(1, extidMBID, "musicbrainz"), extidFile(2, "", ""), extidFile(3, extidMBID, ""),
			}},
			{"half_entry_first", []AudioFileRequest{
				extidFile(1, "", "musicbrainz"), extidFile(2, extidMBID, "musicbrainz"), extidFile(3, "", ""),
			}},
			{"half_entry_among_complete_ones", []AudioFileRequest{
				extidFile(1, extidMBID, "musicbrainz"), extidFile(2, " ", "musicbrainz"), extidFile(3, extidMBID, "musicbrainz"),
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := ValidateAudioAddRequest(extidRequest(tc.files...))
				extidRequireStatus(t, err, http.StatusBadRequest)

				f := newExtidFixture(t)
				resp, err := f.manager.AddAudio(context.Background(), extidRequest(tc.files...))
				extidRequireStatus(t, err, http.StatusBadRequest)
				if resp != nil {
					t.Errorf("response = %#v, want nil", resp)
				}
				extidRequireNothingHappened(t, f)
			})
		}
	})
}

// extidRequireNothingHappened proves a rejected request touched nothing.
func extidRequireNothingHappened(t *testing.T, f *extidFixture) {
	t.Helper()
	if calls := f.engine.callLog(); len(calls) != 0 {
		t.Errorf("engine calls = %v, want none: a half-supplied identity is rejected before the engine", calls)
	}
	if batches := f.registry.stagedBatches(); len(batches) != 0 {
		t.Errorf("staged batches = %#v, want none", batches)
	}
	var leftovers []string
	err := filepath.WalkDir(f.root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			leftovers = append(leftovers, path)
		}
		return err
	})
	if err != nil {
		t.Fatalf("walk %s: %v", f.root, err)
	}
	if len(leftovers) != 0 {
		t.Errorf("files on disk = %v, want none", leftovers)
	}
}

func TestExternalIdentity_CarriedThroughAdd(t *testing.T) {
	t.Run("X9_staged_rows_carry_the_identity_byte_for_byte", func(t *testing.T) {
		for _, tc := range extidOpaqueValues {
			t.Run(tc.name, func(t *testing.T) {
				f := newExtidFixture(t)
				if _, err := f.manager.AddAudio(context.Background(), extidRequest(extidFile(1, tc.id, tc.ns))); err != nil {
					t.Fatalf("AddAudio() error = %v, want nil", err)
				}
				batches := f.registry.stagedBatches()
				if len(batches) != 1 || len(batches[0]) != 1 {
					t.Fatalf("staged batches = %#v, want one batch of one row", batches)
				}
				row := batches[0][0]
				if row.ExternalID != tc.id || row.ExternalIDNamespace != tc.ns {
					t.Errorf("staged identity = (%q, %q), want (%q, %q)", row.ExternalID, row.ExternalIDNamespace, tc.id, tc.ns)
				}
			})
		}
	})

	t.Run("X9_each_row_gets_its_own_identity_including_none", func(t *testing.T) {
		f := newExtidFixture(t)
		_, err := f.manager.AddAudio(context.Background(), extidRequest(
			extidFile(1, extidMBID, "musicbrainz"),
			extidFile(2, "", ""),
			extidFile(3, extidASIN, "asin"),
			extidFile(4, extidASIN, "asin"),
		))
		if err != nil {
			t.Fatalf("AddAudio() error = %v, want nil", err)
		}
		batches := f.registry.stagedBatches()
		if len(batches) != 1 {
			t.Fatalf("staged batches = %d, want 1", len(batches))
		}
		got := map[string][2]string{}
		for _, row := range batches[0] {
			got[row.VirtualPath] = [2]string{row.ExternalID, row.ExternalIDNamespace}
		}
		want := map[string][2]string{
			extidDest(1): {extidMBID, "musicbrainz"},
			extidDest(2): {"", ""},
			extidDest(3): {extidASIN, "asin"},
			extidDest(4): {extidASIN, "asin"},
		}
		if len(got) != len(want) {
			t.Fatalf("staged rows = %v, want %d rows", got, len(want))
		}
		for path, w := range want {
			if got[path] != w {
				t.Errorf("row %q identity = %v, want %v (an identity must not leak between rows)", path, got[path], w)
			}
		}
	})

	t.Run("X7_X8_unknown_namespace_and_repeated_pair_are_added", func(t *testing.T) {
		f := newExtidFixture(t)
		resp, err := f.manager.AddAudio(context.Background(), extidRequest(
			extidFile(1, "rel-1", "discogs"),
			extidFile(2, "rel-1", "discogs"),
			extidFile(3, "9780000000000", "isbn"),
		))
		if err != nil {
			t.Fatalf("AddAudio() error = %v, want nil", err)
		}
		if len(resp.Files) != 3 {
			t.Fatalf("response Files = %#v, want 3", resp.Files)
		}
		for _, file := range resp.Files {
			if file.State != AudioProjectionCreated {
				t.Errorf("file %q state = %q, want created", file.Path, file.State)
			}
		}
	})

	t.Run("X10_stub_carries_the_exact_identity", func(t *testing.T) {
		for _, tc := range extidOpaqueValues {
			t.Run(tc.name, func(t *testing.T) {
				f := newExtidFixture(t)
				if _, err := f.manager.AddAudio(context.Background(), extidRequest(extidFile(1, tc.id, tc.ns))); err != nil {
					t.Fatalf("AddAudio() error = %v, want nil", err)
				}
				stub := extidReadStub(t, f.root, 1)
				if got, ok := stub["external_id"]; !ok || got != tc.id {
					t.Errorf("stub external_id = %#v (present=%v), want %q", got, ok, tc.id)
				}
				if got, ok := stub["external_id_ns"]; !ok || got != tc.ns {
					t.Errorf("stub external_id_ns = %#v (present=%v), want %q", got, ok, tc.ns)
				}
			})
		}
	})

	t.Run("X10_stub_omits_both_keys_when_absent", func(t *testing.T) {
		f := newExtidFixture(t)
		if _, err := f.manager.AddAudio(context.Background(), extidRequest(extidFile(1, "", ""))); err != nil {
			t.Fatalf("AddAudio() error = %v, want nil", err)
		}
		stub := extidReadStub(t, f.root, 1)
		for _, key := range []string{"external_id", "external_id_ns"} {
			if v, present := stub[key]; present {
				t.Errorf("stub has %q = %#v, want the key omitted entirely rather than empty", key, v)
			}
		}
		for _, key := range []string{"url", "size", "magnet"} {
			if _, ok := stub[key]; !ok {
				t.Errorf("stub lost %q; the omission must not disturb the existing keys", key)
			}
		}
	})

	t.Run("X10_mixed_request_writes_each_stub_independently", func(t *testing.T) {
		f := newExtidFixture(t)
		_, err := f.manager.AddAudio(context.Background(), extidRequest(
			extidFile(1, extidMBID, "musicbrainz"),
			extidFile(2, "", ""),
		))
		if err != nil {
			t.Fatalf("AddAudio() error = %v, want nil", err)
		}
		with := extidReadStub(t, f.root, 1)
		if with["external_id"] != extidMBID || with["external_id_ns"] != "musicbrainz" {
			t.Errorf("identified stub = %#v, want the MBID pair", with)
		}
		without := extidReadStub(t, f.root, 2)
		if _, present := without["external_id"]; present {
			t.Errorf("unidentified stub carries external_id = %#v, want omitted", without["external_id"])
		}
		if _, present := without["external_id_ns"]; present {
			t.Errorf("unidentified stub carries external_id_ns = %#v, want omitted", without["external_id_ns"])
		}
	})

	t.Run("X11_response_echoes_the_identity_per_file", func(t *testing.T) {
		f := newExtidFixture(t)
		resp, err := f.manager.AddAudio(context.Background(), extidRequest(
			extidFile(1, extidMBID, "musicbrainz"),
			extidFile(2, "", ""),
			extidFile(3, " Mixed Case ", "Discogs"),
		))
		if err != nil {
			t.Fatalf("AddAudio() error = %v, want nil", err)
		}
		got := map[string][2]string{}
		for _, file := range resp.Files {
			got[file.Path] = [2]string{file.ExternalID, file.ExternalIDNamespace}
		}
		want := map[string][2]string{
			extidDest(1): {extidMBID, "musicbrainz"},
			extidDest(2): {"", ""},
			extidDest(3): {" Mixed Case ", "Discogs"},
		}
		if len(got) != len(want) {
			t.Fatalf("response identities = %v, want %d files", got, len(want))
		}
		for path, w := range want {
			if got[path] != w {
				t.Errorf("response %q identity = %v, want %v", path, got[path], w)
			}
		}
	})

	t.Run("X11_response_json_uses_the_request_field_names", func(t *testing.T) {
		f := newExtidFixture(t)
		resp, err := f.manager.AddAudio(context.Background(), extidRequest(extidFile(1, extidMBID, "musicbrainz")))
		if err != nil {
			t.Fatalf("AddAudio() error = %v, want nil", err)
		}
		raw, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("Marshal(response): %v", err)
		}
		var decoded struct {
			Files []map[string]any `json:"files"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("Unmarshal(response): %v", err)
		}
		if len(decoded.Files) != 1 || decoded.Files[0]["external_id"] != extidMBID || decoded.Files[0]["external_id_ns"] != "musicbrainz" {
			t.Errorf("response JSON files = %#v, want external_id and external_id_ns echoed", decoded.Files)
		}
	})

	t.Run("X1_request_json_decodes_the_documented_field_names", func(t *testing.T) {
		var file AudioFileRequest
		body := `{"source_path":"a.flac","path":"b.flac","external_id":"` + extidMBID + `","external_id_ns":"musicbrainz"}`
		if err := json.Unmarshal([]byte(body), &file); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if file.ExternalID != extidMBID || file.ExternalIDNamespace != "musicbrainz" {
			t.Errorf("decoded = %+v, want external_id/external_id_ns bound to the fields", file)
		}
	})

	t.Run("X2_request_json_omits_absent_identity", func(t *testing.T) {
		raw, err := json.Marshal(AudioFileRequest{SourcePath: "a.flac", Path: "b.flac"})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if strings.Contains(string(raw), "external_id") {
			t.Errorf("request JSON = %s, want the identity keys omitted when absent", raw)
		}
	})
}

func TestExternalIdentity_Reconciliation(t *testing.T) {
	seed := func(path string, index int, id, ns string) metadb.AudioProjection {
		return metadb.AudioProjection{
			Section:             string(SectionMusic),
			VirtualPath:         path,
			Hash:                extidHash,
			FileIndex:           index,
			SourcePath:          fmt.Sprintf("Release/%d.flac", index),
			Size:                int64(index),
			State:               metadb.AudioCommitted,
			ExternalID:          id,
			ExternalIDNamespace: ns,
		}
	}

	t.Run("X12_list_returns_identity_unchanged_from_the_row", func(t *testing.T) {
		rows := []metadb.AudioProjection{
			seed("a.flac", 1, extidMBID, "musicbrainz"),
			seed("b.flac", 2, "  Padded\tId ", "Discogs"),
			seed("c.flac", 3, "café", "isbn"),
		}
		manager := New(Config{AudioProjections: newExtidRegistry(rows...), Logger: log.New(io.Discard, "", 0)})
		resp, err := manager.ListAudio(AudioListRequest{Type: "music"})
		if err != nil {
			t.Fatalf("ListAudio() error = %v, want nil", err)
		}
		if len(resp.Items) != len(rows) {
			t.Fatalf("Items = %#v, want %d", resp.Items, len(rows))
		}
		for i, item := range resp.Items {
			if item.ExternalID != rows[i].ExternalID || item.ExternalIDNamespace != rows[i].ExternalIDNamespace {
				t.Errorf("item %q identity = (%q, %q), want (%q, %q)", item.Path,
					item.ExternalID, item.ExternalIDNamespace, rows[i].ExternalID, rows[i].ExternalIDNamespace)
			}
		}
	})

	t.Run("X12_list_json_uses_the_request_field_names", func(t *testing.T) {
		manager := New(Config{
			AudioProjections: newExtidRegistry(seed("a.flac", 1, extidMBID, "musicbrainz")),
			Logger:           log.New(io.Discard, "", 0),
		})
		resp, err := manager.ListAudio(AudioListRequest{Type: "music"})
		if err != nil {
			t.Fatalf("ListAudio() error = %v, want nil", err)
		}
		raw, err := json.Marshal(resp.Items[0])
		if err != nil {
			t.Fatalf("Marshal(item): %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("Unmarshal(item): %v", err)
		}
		if decoded["external_id"] != extidMBID || decoded["external_id_ns"] != "musicbrainz" {
			t.Errorf("item JSON = %s, want external_id and external_id_ns", raw)
		}
	})

	t.Run("X13_row_without_identity_lists_as_two_empty_strings_not_null", func(t *testing.T) {
		manager := New(Config{
			AudioProjections: newExtidRegistry(seed("a.flac", 1, "", ""), seed("b.flac", 2, extidASIN, "asin")),
			Logger:           log.New(io.Discard, "", 0),
		})
		resp, err := manager.ListAudio(AudioListRequest{Type: "music"})
		if err != nil {
			t.Fatalf("ListAudio() error = %v, want nil", err)
		}
		raw, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("Marshal(response): %v", err)
		}
		var decoded struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("Unmarshal(response): %v", err)
		}
		if len(decoded.Items) != 2 {
			t.Fatalf("items = %#v, want 2", decoded.Items)
		}
		bare := decoded.Items[0]
		for _, key := range []string{"external_id", "external_id_ns"} {
			v, present := bare[key]
			if !present {
				t.Errorf("unidentified item lacks %q, want present as an empty string; JSON: %s", key, raw)
				continue
			}
			if s, isString := v.(string); !isString || s != "" {
				t.Errorf("unidentified item %q = %#v, want the empty string", key, v)
			}
		}
		if decoded.Items[1]["external_id"] != extidASIN {
			t.Errorf("identified neighbour external_id = %#v, want %q", decoded.Items[1]["external_id"], extidASIN)
		}
	})

	t.Run("X12_X13_what_was_added_is_what_lists", func(t *testing.T) {
		f := newExtidFixture(t)
		_, err := f.manager.AddAudio(context.Background(), extidRequest(
			extidFile(1, extidMBID, "musicbrainz"),
			extidFile(2, "", ""),
			extidFile(3, " Odd-Case ", "Custom NS"),
		))
		if err != nil {
			t.Fatalf("AddAudio() error = %v, want nil", err)
		}
		resp, err := f.manager.ListAudio(AudioListRequest{Type: "music"})
		if err != nil {
			t.Fatalf("ListAudio() error = %v, want nil", err)
		}
		got := map[string][2]string{}
		for _, item := range resp.Items {
			got[item.Path] = [2]string{item.ExternalID, item.ExternalIDNamespace}
		}
		want := map[string][2]string{
			extidDest(1): {extidMBID, "musicbrainz"},
			extidDest(2): {"", ""},
			extidDest(3): {" Odd-Case ", "Custom NS"},
		}
		if len(got) != len(want) {
			t.Fatalf("listed identities = %v, want %d items", got, len(want))
		}
		for path, w := range want {
			if got[path] != w {
				t.Errorf("listed %q identity = %v, want %v", path, got[path], w)
			}
		}
	})
}

func TestExternalIdentity_ErrorModel(t *testing.T) {
	t.Run("X14_status_for_incomplete_identity_is_400", func(t *testing.T) {
		if got := StatusForError(metadb.ErrAudioIdentityIncomplete); got != http.StatusBadRequest {
			t.Errorf("StatusForError(ErrAudioIdentityIncomplete) = %d, want %d", got, http.StatusBadRequest)
		}
	})

	t.Run("X14_wrapped_instance_maps_the_same", func(t *testing.T) {
		wrapped := fmt.Errorf("stage music/a.flac: %w", metadb.ErrAudioIdentityIncomplete)
		if got := StatusForError(wrapped); got != http.StatusBadRequest {
			t.Errorf("StatusForError(wrapped) = %d, want %d", got, http.StatusBadRequest)
		}
		doubly := fmt.Errorf("outer: %w", wrapped)
		if got := StatusForError(doubly); got != http.StatusBadRequest {
			t.Errorf("StatusForError(doubly wrapped) = %d, want %d", got, http.StatusBadRequest)
		}
	})

	t.Run("X14_registry_refusal_reaches_the_caller_as_400_and_cleans_up", func(t *testing.T) {
		// Defence in depth: should the registry ever reject the pair, the caller
		// hears 400 rather than a 500, and the engine's torrent is not orphaned.
		f := newExtidFixture(t)
		f.registry.stageErr = fmt.Errorf("stage: %w", metadb.ErrAudioIdentityIncomplete)
		resp, err := f.manager.AddAudio(context.Background(), extidRequest(extidFile(1, extidMBID, "musicbrainz")))
		extidRequireStatus(t, err, http.StatusBadRequest)
		if !errors.Is(err, metadb.ErrAudioIdentityIncomplete) {
			t.Errorf("errors.Is(err, ErrAudioIdentityIncomplete) = false for %v, want the sentinel reachable", err)
		}
		if resp != nil {
			t.Errorf("response = %#v, want nil", resp)
		}
		removed := false
		for _, call := range f.engine.callLog() {
			removed = removed || call == "RemoveTorrent"
		}
		if !removed {
			t.Errorf("engine calls = %v, want RemoveTorrent after the failed stage", f.engine.callLog())
		}
	})

	t.Run("X14_existing_statuses_are_unchanged", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			err  error
			want int
		}{
			{"path_invalid", ErrPathInvalid, http.StatusBadRequest},
			{"source_not_found", ErrSourceNotFound, http.StatusUnprocessableEntity},
			{"path_conflict", metadb.ErrAudioPathConflict, http.StatusConflict},
			{"unknown_is_500", errors.New("boom"), http.StatusInternalServerError},
		} {
			if got := StatusForError(tc.err); got != tc.want {
				t.Errorf("%s: StatusForError = %d, want %d", tc.name, got, tc.want)
			}
		}
	})
}
