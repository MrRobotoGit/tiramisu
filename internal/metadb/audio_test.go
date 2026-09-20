package metadb

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
)

func openAudioTestDB(t *testing.T, path string) *DB {
	t.Helper()
	db, err := New(path, nil)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func audioProjection(tag string) AudioProjection {
	return AudioProjection{
		Section:         "music",
		VirtualPath:     "Artist/Album/" + tag + ".flac",
		PortablePathKey: "artist/album/" + tag + ".flac",
		Hash:            "hash-" + tag,
		FileIndex:       1,
		SourcePath:      "source/" + tag + ".flac",
		Size:            12345,
		MtimeNS:         1700000000000000001,
		Title:           "Title " + tag,
		Magnet:          "magnet:?xt=urn:btih:" + tag,
		State:           AudioCommitted,
		TxnID:           "txn-" + tag,
		StagingName:     ".stage-" + tag,
		CreatedAtNS:     1700000000000000002,
		UpdatedAtNS:     1700000000000000003,
	}
}

func requireProjectionFields(t *testing.T, got *AudioProjection, want AudioProjection, wantState AudioProjectionState) {
	t.Helper()
	if got.ID <= 0 {
		t.Errorf("projection ID = %d, want a positive persistent ID", got.ID)
	}
	want.ID = got.ID
	want.State = wantState
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("projection fields differ (-got +want):\n got: %#v\nwant: %#v", *got, want)
	}
}

func requireFoundProjection(t *testing.T, db *DB, section, path string) *AudioProjection {
	t.Helper()
	p, found, err := db.GetAudioProjection(section, path)
	if err != nil {
		t.Fatalf("GetAudioProjection(%q, %q): %v", section, path, err)
	}
	if !found {
		t.Fatalf("GetAudioProjection(%q, %q): not found", section, path)
	}
	return p
}

func requireNoProjection(t *testing.T, db *DB, section, path string) {
	t.Helper()
	_, found, err := db.GetAudioProjection(section, path)
	if err != nil {
		t.Fatalf("GetAudioProjection(%q, %q): %v", section, path, err)
	}
	if found {
		t.Fatalf("GetAudioProjection(%q, %q): found, want absent", section, path)
	}
}

func TestAudioProjectionSchemaMigrationPreservesExistingData(t *testing.T) {
	// R2: simulate an installation whose established tables contain data but whose
	// audio registry did not exist yet.
	path := filepath.Join(t.TempDir(), "legacy.db")
	db := openAudioTestDB(t, path)
	statements := []string{
		`INSERT INTO inodes(type, infohash, file_idx, full_path, rel_path, basename, inode_value) VALUES ('file', 'ih', 7, '/old/file.mkv', NULL, 'file.mkv', 101)`,
		`INSERT INTO sync_caches(hash, cache_type, title, timestamp) VALUES ('cache-hash', 'negative', 'reason', '2025-01-02T03:04:05Z')`,
		`INSERT INTO tv_episodes(episode_key, quality_score, hash, file_path, source, created, show_imdb) VALUES ('show:S01E01', 900, 'episode-hash', '/old/e.mkv', 'legacy', 123, 'tt123')`,
		`INSERT INTO playback_states(path, hash, imdb_id, opened_at, is_healthy, read_count) VALUES ('/old/play.mkv', 'play-hash', 'tt456', '2025-01-02T03:04:05Z', 1, 4)`,
		`INSERT INTO episode_gaps(episode_key, show_imdb, season, file_path, dead_hash, removed_at, last_attempt) VALUES ('gap:S02E03', 'tt789', 2, '/old/gap.mkv', 'dead-hash', 456, 789)`,
		`INSERT INTO metadata_failures(hash, fail_count, first_fail, last_fail) VALUES ('failed-hash', 3, 100, 200)`,
		`INSERT INTO v304_bans(ip, banned_at) VALUES ('192.0.2.10', 987)`,
	}
	for _, statement := range statements {
		if _, err := db.SQL().Exec(statement); err != nil {
			t.Fatalf("seed legacy database with %q: %v", statement, err)
		}
	}
	if _, err := db.SQL().Exec(`DROP TABLE audio_projections`); err != nil {
		t.Fatalf("remove registry to simulate pre-audio installation: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	reopened := openAudioTestDB(t, path)
	checks := []struct {
		name  string
		query string
		want  any
	}{
		{"R2 inode survives", `SELECT inode_value FROM inodes WHERE full_path='/old/file.mkv'`, int64(101)},
		{"R2 sync cache survives", `SELECT title FROM sync_caches WHERE hash='cache-hash'`, "reason"},
		{"R2 episode survives", `SELECT show_imdb FROM tv_episodes WHERE episode_key='show:S01E01'`, "tt123"},
		{"R2 playback state survives", `SELECT read_count FROM playback_states WHERE path='/old/play.mkv'`, int64(4)},
		{"R2 episode gap survives", `SELECT last_attempt FROM episode_gaps WHERE episode_key='gap:S02E03'`, int64(789)},
		{"R2 metadata failure survives", `SELECT fail_count FROM metadata_failures WHERE hash='failed-hash'`, int64(3)},
		{"R2 v304 ban survives", `SELECT banned_at FROM v304_bans WHERE ip='192.0.2.10'`, int64(987)},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			var got any
			switch tc.want.(type) {
			case string:
				var value string
				if err := reopened.SQL().QueryRow(tc.query).Scan(&value); err != nil {
					t.Fatalf("legacy row missing: %v", err)
				}
				got = value
			default:
				var value int64
				if err := reopened.SQL().QueryRow(tc.query).Scan(&value); err != nil {
					t.Fatalf("legacy row missing: %v", err)
				}
				got = value
			}
			if got != tc.want {
				t.Errorf("preserved value = %#v, want %#v", got, tc.want)
			}
		})
	}
	if err := reopened.StageAudioProjections("migration-probe", []AudioProjection{audioProjection("migration-probe")}); err != nil {
		t.Fatalf("R2 registry was not recreated after upgrade: %v", err)
	}
}

func TestAudioProjectionSchemaVersionIsNewAndIdempotent(t *testing.T) {
	// R1, R3: every open applies the schema safely, records one new migration,
	// and preserves the established migration descriptions.
	path := filepath.Join(t.TempDir(), "versions.db")
	db := openAudioTestDB(t, path)
	wantDescriptions := map[int]string{
		1: "initial schema",
		2: "add playback_states table",
		3: "add v304_bans table",
		5: "add metadata_failures table",
		6: "add tv_episodes.show_imdb",
		7: "add episode_gaps table",
		8: "add episode_gaps.last_attempt",
	}
	assertVersions := func(t *testing.T, db *DB) int {
		t.Helper()
		rows, err := db.SQL().Query(`SELECT version, description FROM schema_version ORDER BY version`)
		if err != nil {
			t.Fatalf("query schema versions: %v", err)
		}
		defer rows.Close()
		newRows := 0
		newVersion := 0
		seenOld := make(map[int]string)
		for rows.Next() {
			var version int
			var description string
			if err := rows.Scan(&version, &description); err != nil {
				t.Fatalf("scan schema version: %v", err)
			}
			if version > 8 {
				newRows++
				newVersion = version
			} else {
				seenOld[version] = description
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate schema versions: %v", err)
		}
		if !reflect.DeepEqual(seenOld, wantDescriptions) {
			t.Errorf("R3 prior schema versions changed: got %#v, want %#v", seenOld, wantDescriptions)
		}
		if newRows != 1 || newVersion <= 8 {
			t.Errorf("R3 audio migration rows above version 8 = %d (version %d), want exactly one", newRows, newVersion)
		}
		return newVersion
	}
	firstVersion := assertVersions(t, db)
	if err := db.ExecSchema(); err != nil {
		t.Fatalf("R1 repeated ExecSchema: %v", err)
	}
	if got := assertVersions(t, db); got != firstVersion {
		t.Errorf("R3 migration version changed after ExecSchema: got %d, want %d", got, firstVersion)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close before idempotent reopen: %v", err)
	}
	reopened := openAudioTestDB(t, path)
	if got := assertVersions(t, reopened); got != firstVersion {
		t.Errorf("R1/R3 migration version changed after reopen: got %d, want %d", got, firstVersion)
	}
}

func TestStageAudioProjectionsPersistsAllFieldsAndRestart(t *testing.T) {
	// R1, R4, J7: storage is verbatim except that staging always establishes the
	// staged lifecycle state, and values survive a close/reopen cycle.
	path := filepath.Join(t.TempDir(), "restart.db")
	db := openAudioTestDB(t, path)
	want := audioProjection("verbatim")
	if err := db.StageAudioProjections(want.TxnID, []AudioProjection{want}); err != nil {
		t.Fatalf("R4 stage projection: %v", err)
	}
	got := requireFoundProjection(t, db, want.Section, want.VirtualPath)
	requireProjectionFields(t, got, want, AudioStaged)
	firstID := got.ID
	if err := db.Close(); err != nil {
		t.Fatalf("J7 close database: %v", err)
	}

	reopened := openAudioTestDB(t, path)
	got = requireFoundProjection(t, reopened, want.Section, want.VirtualPath)
	requireProjectionFields(t, got, want, AudioStaged)
	if got.ID != firstID {
		t.Errorf("R1 stable row ID after reopen = %d, want %d", got.ID, firstID)
	}

	// R4 boundary: optional magnet and legacy transaction/staging identifiers may
	// all be empty and must not be synthesized.
	empty := audioProjection("empty-optionals")
	empty.Magnet = ""
	empty.TxnID = ""
	empty.StagingName = ""
	if err := reopened.StageAudioProjections("", []AudioProjection{empty}); err != nil {
		t.Fatalf("R4 stage projection with empty optional/legacy fields: %v", err)
	}
	requireProjectionFields(t, requireFoundProjection(t, reopened, empty.Section, empty.VirtualPath), empty, AudioStaged)
}

func TestStageAudioProjectionsUsesBatchTxnID(t *testing.T) {
	// R14: the request's txnID is authoritative for every row; per-projection
	// values are ignored just like the caller-supplied State field.
	db := openAudioTestDB(t, filepath.Join(t.TempDir(), "authoritative-txn.db"))
	commitRows := []AudioProjection{audioProjection("txn-argument-a"), audioProjection("txn-argument-b")}
	commitRows[0].TxnID = "ignored-struct-txn"
	commitRows[1].TxnID = ""
	const commitTxn = "authoritative-commit-txn"
	if err := db.StageAudioProjections(commitTxn, commitRows); err != nil {
		t.Fatalf("R14 stage commit batch: %v", err)
	}
	if changed, err := db.CommitAudioProjections("ignored-struct-txn", 7000); err != nil || changed != 0 {
		t.Errorf("R14 commit by ignored struct txn = %d, err %v; want 0, nil", changed, err)
	}
	const committedAt = int64(7001)
	changed, err := db.CommitAudioProjections(commitTxn, committedAt)
	if err != nil {
		t.Fatalf("R14 commit by batch txn: %v", err)
	}
	if changed != len(commitRows) {
		t.Errorf("R14 committed rows = %d, want full batch size %d", changed, len(commitRows))
	}
	for _, p := range commitRows {
		got := requireFoundProjection(t, db, p.Section, p.VirtualPath)
		if got.TxnID != commitTxn || got.State != AudioCommitted || got.UpdatedAtNS != committedAt {
			t.Errorf("R14 committed row txn/state/time = %q/%q/%d, want %q/%q/%d", got.TxnID, got.State, got.UpdatedAtNS, commitTxn, AudioCommitted, committedAt)
		}
	}

	rollbackRows := []AudioProjection{audioProjection("txn-rollback-a"), audioProjection("txn-rollback-b")}
	rollbackRows[0].TxnID = "ignored-rollback-txn"
	rollbackRows[1].TxnID = ""
	const rollbackTxn = "authoritative-rollback-txn"
	if err := db.StageAudioProjections(rollbackTxn, rollbackRows); err != nil {
		t.Fatalf("R14 stage rollback batch: %v", err)
	}
	removed, err := db.RollbackAudioProjections(rollbackTxn)
	if err != nil {
		t.Fatalf("R14 rollback by batch txn: %v", err)
	}
	if removed != len(rollbackRows) {
		t.Errorf("R14 rolled back rows = %d, want full batch size %d", removed, len(rollbackRows))
	}
	for _, p := range rollbackRows {
		requireNoProjection(t, db, p.Section, p.VirtualPath)
	}
}

func TestStageAudioProjectionsConflictsAndSectionScope(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*AudioProjection)
		wantErr error
	}{
		{
			name: "J1 same section and virtual path conflicts without overwrite",
			mutate: func(p *AudioProjection) {
				p.PortablePathKey = "different-key"
				p.Hash = "different-hash"
				p.FileIndex = 2
			},
			wantErr: ErrAudioPathConflict,
		},
		{
			name: "J2 letter-case spellings with caller-supplied folded key conflict",
			mutate: func(p *AudioProjection) {
				p.VirtualPath = "artist/album/original.flac"
				p.Hash = "case-hash"
				p.FileIndex = 2
			},
			wantErr: ErrAudioPathConflict,
		},
		{
			name: "J2 Unicode-normalized spellings with caller-supplied key conflict",
			mutate: func(p *AudioProjection) {
				p.VirtualPath = "Artist/Album/Cafe\u0301.flac"
				p.PortablePathKey = "artist/album/café.flac"
				p.Hash = "unicode-hash"
				p.FileIndex = 2
			},
			wantErr: ErrAudioPathConflict,
		},
		{
			name: "J3 same torrent file identity conflicts",
			mutate: func(p *AudioProjection) {
				p.VirtualPath = "Artist/Album/different.flac"
				p.PortablePathKey = "artist/album/different.flac"
			},
			wantErr: ErrAudioSourceConflict,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := openAudioTestDB(t, filepath.Join(t.TempDir(), "conflict.db"))
			original := audioProjection("original")
			original.TxnID = "first"
			if tc.name == "J2 Unicode-normalized spellings with caller-supplied key conflict" {
				original.VirtualPath = "Artist/Album/Café.flac"
				original.PortablePathKey = "artist/album/café.flac"
			}
			if err := db.StageAudioProjections("first", []AudioProjection{original}); err != nil {
				t.Fatalf("stage original projection: %v", err)
			}
			conflict := original
			conflict.Title = "must not overwrite"
			tc.mutate(&conflict)
			err := db.StageAudioProjections("conflict", []AudioProjection{conflict})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("J4 conflict error = %v, want errors.Is(_, %v)", err, tc.wantErr)
			}
			if tc.wantErr == ErrAudioPathConflict && errors.Is(err, ErrAudioSourceConflict) {
				t.Errorf("J4 path conflict also identifies as source conflict")
			}
			if tc.wantErr == ErrAudioSourceConflict && errors.Is(err, ErrAudioPathConflict) {
				t.Errorf("J4 source conflict also identifies as path conflict")
			}
			stored := requireFoundProjection(t, db, original.Section, original.VirtualPath)
			requireProjectionFields(t, stored, original, AudioStaged)
			if conflict.VirtualPath != original.VirtualPath {
				requireNoProjection(t, db, conflict.Section, conflict.VirtualPath)
			}

			// F5: rejection must release the transaction/connection for later work.
			valid := audioProjection("valid-after-conflict")
			if err := db.StageAudioProjections("after", []AudioProjection{valid}); err != nil {
				t.Fatalf("F5 valid stage after rejected conflict: %v", err)
			}
			requireFoundProjection(t, db, valid.Section, valid.VirtualPath)
		})
	}

	t.Run("J1 J2 same path and portable key are independent across sections", func(t *testing.T) {
		db := openAudioTestDB(t, filepath.Join(t.TempDir(), "sections.db"))
		music := audioProjection("shared")
		audiobook := music
		audiobook.Section = "audiobooks"
		audiobook.Hash = "audiobook-hash"
		audiobook.FileIndex = 9
		if err := db.StageAudioProjections("cross-section", []AudioProjection{music, audiobook}); err != nil {
			t.Fatalf("cross-section projections should coexist: %v", err)
		}
		requireFoundProjection(t, db, "music", music.VirtualPath)
		requireFoundProjection(t, db, "audiobooks", music.VirtualPath)
	})

	t.Run("J3 source identity is globally unique across sections", func(t *testing.T) {
		db := openAudioTestDB(t, filepath.Join(t.TempDir(), "global-source.db"))
		music := audioProjection("global-source-music")
		music.TxnID = "music-source"
		audiobook := music
		audiobook.Section = "audiobooks"
		audiobook.VirtualPath = "Author/Book/chapter.flac"
		audiobook.PortablePathKey = "author/book/chapter.flac"
		audiobook.TxnID = "audiobook-source"
		if err := db.StageAudioProjections("music-source", []AudioProjection{music}); err != nil {
			t.Fatalf("stage music source owner: %v", err)
		}
		err := db.StageAudioProjections("audiobook-source", []AudioProjection{audiobook})
		if !errors.Is(err, ErrAudioSourceConflict) {
			t.Fatalf("J3 cross-section source conflict = %v, want errors.Is(_, ErrAudioSourceConflict)", err)
		}
		requireFoundProjection(t, db, music.Section, music.VirtualPath)
		requireNoProjection(t, db, audiobook.Section, audiobook.VirtualPath)
	})
}

func TestStageAudioProjectionsBatchIsAtomic(t *testing.T) {
	// R5 and F6 cover conflicts against stored rows and within the new batch.
	tests := []struct {
		name    string
		batch   func() []AudioProjection
		wantErr error
	}{
		{
			name: "R5 conflict with existing row rolls back otherwise valid batch members",
			batch: func() []AudioProjection {
				valid := audioProjection("batch-valid")
				conflict := audioProjection("existing")
				conflict.Hash = "new-hash"
				conflict.FileIndex = 8
				return []AudioProjection{valid, conflict}
			},
			wantErr: ErrAudioPathConflict,
		},
		{
			name: "F6 duplicate paths inside batch reject whole batch",
			batch: func() []AudioProjection {
				a := audioProjection("in-batch-path")
				b := audioProjection("in-batch-path")
				b.Hash = "second-hash"
				b.FileIndex = 2
				return []AudioProjection{a, b}
			},
			wantErr: ErrAudioPathConflict,
		},
		{
			name: "F6 duplicate portable keys inside batch reject whole batch",
			batch: func() []AudioProjection {
				a := audioProjection("in-batch-key-a")
				b := audioProjection("in-batch-key-b")
				b.PortablePathKey = a.PortablePathKey
				return []AudioProjection{a, b}
			},
			wantErr: ErrAudioPathConflict,
		},
		{
			name: "F6 duplicate source identities inside batch reject whole batch",
			batch: func() []AudioProjection {
				a := audioProjection("in-batch-source-a")
				b := audioProjection("in-batch-source-b")
				b.Hash = a.Hash
				b.FileIndex = a.FileIndex
				return []AudioProjection{a, b}
			},
			wantErr: ErrAudioSourceConflict,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := openAudioTestDB(t, filepath.Join(t.TempDir(), "atomic.db"))
			existing := audioProjection("existing")
			existing.TxnID = "unrelated"
			if err := db.StageAudioProjections("unrelated", []AudioProjection{existing}); err != nil {
				t.Fatalf("stage unrelated row: %v", err)
			}
			batch := tc.batch()
			err := db.StageAudioProjections("failing-batch", batch)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("batch error = %v, want errors.Is(_, %v)", err, tc.wantErr)
			}
			for _, p := range batch {
				if p.VirtualPath != existing.VirtualPath {
					requireNoProjection(t, db, p.Section, p.VirtualPath)
				}
			}
			requireProjectionFields(t, requireFoundProjection(t, db, existing.Section, existing.VirtualPath), existing, AudioStaged)
		})
	}
}

func TestAudioProjectionValidationBoundaries(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*AudioProjection)
	}{
		{"J6 file index zero is rejected", func(p *AudioProjection) { p.FileIndex = 0 }},
		{"J6 negative file index is rejected", func(p *AudioProjection) { p.FileIndex = -1 }},
		{"J6 size zero is rejected", func(p *AudioProjection) { p.Size = 0 }},
		{"J6 negative size is rejected", func(p *AudioProjection) { p.Size = -1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := openAudioTestDB(t, filepath.Join(t.TempDir(), "invalid.db"))
			p := audioProjection("invalid")
			tc.mutate(&p)
			if err := db.StageAudioProjections("invalid", []AudioProjection{p}); err == nil {
				t.Fatalf("invalid projection was accepted: %#v", p)
			}
			requireNoProjection(t, db, p.Section, p.VirtualPath)
		})
	}

	t.Run("J6 one-byte audio is valid", func(t *testing.T) {
		db := openAudioTestDB(t, filepath.Join(t.TempDir(), "one-byte.db"))
		p := audioProjection("one-byte")
		p.Size = 1
		if err := db.StageAudioProjections("one-byte", []AudioProjection{p}); err != nil {
			t.Fatalf("one-byte projection rejected: %v", err)
		}
		if got := requireFoundProjection(t, db, p.Section, p.VirtualPath); got.Size != 1 {
			t.Errorf("stored size = %d, want 1", got.Size)
		}
	})

	t.Run("J5 database rejects state outside lifecycle", func(t *testing.T) {
		db := openAudioTestDB(t, filepath.Join(t.TempDir(), "state-check.db"))
		p := audioProjection("state-check")
		if err := db.StageAudioProjections("state-check", []AudioProjection{p}); err != nil {
			t.Fatalf("stage projection: %v", err)
		}
		if _, err := db.SQL().Exec(`UPDATE audio_projections SET state='corrupt' WHERE section=? AND virtual_path=?`, p.Section, p.VirtualPath); err == nil {
			t.Fatal("database accepted an invalid audio projection state")
		}
		if got := requireFoundProjection(t, db, p.Section, p.VirtualPath); got.State != AudioStaged {
			t.Errorf("state after rejected invalid update = %q, want %q", got.State, AudioStaged)
		}
	})
}

func TestCommitAndRollbackAudioProjectionsScopeTransitions(t *testing.T) {
	db := openAudioTestDB(t, filepath.Join(t.TempDir(), "transitions.db"))
	commitA := audioProjection("commit-a")
	commitB := audioProjection("commit-b")
	other := audioProjection("other-txn")
	commitA.TxnID = "commit-me"
	commitB.TxnID = "commit-me"
	other.TxnID = "other"
	if err := db.StageAudioProjections("commit-me", []AudioProjection{commitA, commitB}); err != nil {
		t.Fatalf("stage commit rows: %v", err)
	}
	if err := db.StageAudioProjections("other", []AudioProjection{other}); err != nil {
		t.Fatalf("stage other transaction: %v", err)
	}
	// Establish a non-staged row carrying the same txn id, as recovery can observe.
	if _, err := db.SQL().Exec(`UPDATE audio_projections SET state='removing' WHERE virtual_path=?`, commitB.VirtualPath); err != nil {
		t.Fatalf("establish recovered removing row: %v", err)
	}
	const committedAt = int64(1800000000000000001)
	changed, err := db.CommitAudioProjections("commit-me", committedAt)
	if err != nil {
		t.Fatalf("R6 commit transaction: %v", err)
	}
	if changed != 1 {
		t.Errorf("R6 changed rows = %d, want 1 staged row only", changed)
	}
	if got := requireFoundProjection(t, db, commitA.Section, commitA.VirtualPath); got.State != AudioCommitted || got.UpdatedAtNS != committedAt {
		t.Errorf("R6 committed row state/time = %q/%d, want %q/%d", got.State, got.UpdatedAtNS, AudioCommitted, committedAt)
	}
	if got := requireFoundProjection(t, db, commitB.Section, commitB.VirtualPath); got.State != AudioRemoving || got.UpdatedAtNS != commitB.UpdatedAtNS {
		t.Errorf("R6 non-staged same-txn row changed: %#v", got)
	}
	if got := requireFoundProjection(t, db, other.Section, other.VirtualPath); got.State != AudioStaged || got.UpdatedAtNS != other.UpdatedAtNS {
		t.Errorf("R6 other transaction changed: %#v", got)
	}

	rollbackStaged := audioProjection("rollback-staged")
	rollbackCommitted := audioProjection("rollback-committed")
	rollbackRemoving := audioProjection("rollback-removing")
	rollbackStaged.TxnID = "rollback-me"
	rollbackCommitted.TxnID = "rollback-me"
	rollbackRemoving.TxnID = "rollback-me"
	if err := db.StageAudioProjections("rollback-me", []AudioProjection{rollbackStaged, rollbackCommitted, rollbackRemoving}); err != nil {
		t.Fatalf("stage rollback rows: %v", err)
	}
	if _, err := db.SQL().Exec(`UPDATE audio_projections SET state='committed' WHERE virtual_path=?`, rollbackCommitted.VirtualPath); err != nil {
		t.Fatalf("establish committed recovery row: %v", err)
	}
	if _, err := db.SQL().Exec(`UPDATE audio_projections SET state='removing' WHERE virtual_path=?`, rollbackRemoving.VirtualPath); err != nil {
		t.Fatalf("establish removing recovery row: %v", err)
	}
	removed, err := db.RollbackAudioProjections("rollback-me")
	if err != nil {
		t.Fatalf("R7 rollback transaction: %v", err)
	}
	if removed != 1 {
		t.Errorf("R7 removed rows = %d, want 1 staged row only", removed)
	}
	requireNoProjection(t, db, rollbackStaged.Section, rollbackStaged.VirtualPath)
	if got := requireFoundProjection(t, db, rollbackCommitted.Section, rollbackCommitted.VirtualPath); got.State != AudioCommitted {
		t.Errorf("R7 committed row state = %q, want retained", got.State)
	}
	if got := requireFoundProjection(t, db, rollbackRemoving.Section, rollbackRemoving.VirtualPath); got.State != AudioRemoving {
		t.Errorf("R7 removing row state = %q, want retained", got.State)
	}
}

func TestAudioProjectionQueries(t *testing.T) {
	db := openAudioTestDB(t, filepath.Join(t.TempDir(), "queries.db"))

	t.Run("R8 source identity includes file index", func(t *testing.T) {
		p := audioProjection("source")
		p.TxnID = "source"
		p.FileIndex = 4
		if err := db.StageAudioProjections("source", []AudioProjection{p}); err != nil {
			t.Fatalf("stage source row: %v", err)
		}
		got, found, err := db.AudioProjectionBySource(p.Hash, 4)
		if err != nil || !found {
			t.Fatalf("lookup exact source = found %v, err %v", found, err)
		}
		requireProjectionFields(t, got, p, AudioStaged)
		if _, found, err := db.AudioProjectionBySource(p.Hash, 5); err != nil || found {
			t.Errorf("lookup different file index = found %v, err %v; want false, nil", found, err)
		}
	})

	t.Run("R9 hash query includes every state and track", func(t *testing.T) {
		rows := []AudioProjection{audioProjection("hash-staged"), audioProjection("hash-committed"), audioProjection("hash-removing")}
		for i := range rows {
			rows[i].Hash = "shared-torrent"
			rows[i].FileIndex = i + 1
			rows[i].TxnID = "hash-state-" + fmt.Sprint(i)
			if err := db.StageAudioProjections("hash-state-"+fmt.Sprint(i), []AudioProjection{rows[i]}); err != nil {
				t.Fatalf("stage hash row %d: %v", i, err)
			}
		}
		if _, err := db.CommitAudioProjections("hash-state-1", 2001); err != nil {
			t.Fatalf("commit hash row: %v", err)
		}
		if _, err := db.CommitAudioProjections("hash-state-2", 2002); err != nil {
			t.Fatalf("commit row before removing: %v", err)
		}
		if ok, err := db.MarkAudioProjectionRemoving(rows[2].Section, rows[2].VirtualPath, 2003); err != nil || !ok {
			t.Fatalf("mark hash row removing = ok %v, err %v", ok, err)
		}
		got, err := db.AudioProjectionsByHash("shared-torrent")
		if err != nil {
			t.Fatalf("R9 query by hash: %v", err)
		}
		states := make(map[AudioProjectionState]int)
		for _, p := range got {
			states[p.State]++
		}
		want := map[AudioProjectionState]int{AudioStaged: 1, AudioCommitted: 1, AudioRemoving: 1}
		if !reflect.DeepEqual(states, want) || len(got) != 3 {
			t.Errorf("R9 rows/states = %d/%v, want 3/%v", len(got), states, want)
		}
	})

	t.Run("R10 committed query filters section and orders path", func(t *testing.T) {
		z := audioProjection("list-z")
		z.VirtualPath = "z.flac"
		z.PortablePathKey = "z.flac"
		a := audioProjection("list-a")
		a.VirtualPath = "a.flac"
		a.PortablePathKey = "a.flac"
		book := audioProjection("list-book")
		book.Section = "audiobooks"
		staged := audioProjection("list-staged")
		z.TxnID = "list-committed"
		a.TxnID = "list-committed"
		book.TxnID = "list-committed"
		staged.TxnID = "list-staged"
		if err := db.StageAudioProjections("list-committed", []AudioProjection{z, a, book}); err != nil {
			t.Fatalf("stage committed list rows: %v", err)
		}
		if _, err := db.CommitAudioProjections("list-committed", 3000); err != nil {
			t.Fatalf("commit list rows: %v", err)
		}
		if err := db.StageAudioProjections("list-staged", []AudioProjection{staged}); err != nil {
			t.Fatalf("stage excluded list row: %v", err)
		}
		music, err := db.CommittedAudioProjections("music")
		if err != nil {
			t.Fatalf("R10 list music: %v", err)
		}
		var paths []string
		for _, p := range music {
			if p.Section != "music" || p.State != AudioCommitted {
				t.Errorf("R10 section list leaked row: %#v", p)
			}
			paths = append(paths, p.VirtualPath)
		}
		if !sort.StringsAreSorted(paths) {
			t.Errorf("R10 music paths not ordered: %v", paths)
		}
		if !containsAll(paths, "a.flac", "z.flac") {
			t.Errorf("R10 music paths %v do not include created committed rows", paths)
		}
		all, err := db.CommittedAudioProjections("")
		if err != nil {
			t.Fatalf("R10 list all sections: %v", err)
		}
		foundBook := false
		var allPaths []string
		for _, p := range all {
			if p.State != AudioCommitted {
				t.Errorf("R10 all-sections list leaked %q row", p.State)
			}
			allPaths = append(allPaths, p.VirtualPath)
			foundBook = foundBook || p.Section == "audiobooks" && p.VirtualPath == book.VirtualPath
		}
		if !sort.StringsAreSorted(allPaths) {
			t.Errorf("R10 all-section paths not ordered: %v", allPaths)
		}
		if !foundBook {
			t.Errorf("R10 empty section did not span audiobooks: %#v", all)
		}
	})
}

func containsAll(got []string, values ...string) bool {
	set := make(map[string]bool, len(got))
	for _, value := range got {
		set[value] = true
	}
	for _, value := range values {
		if !set[value] {
			return false
		}
	}
	return true
}

func TestMarkRemovingReferenceCountingAndDelete(t *testing.T) {
	db := openAudioTestDB(t, filepath.Join(t.TempDir(), "reference.db"))
	p := audioProjection("lifecycle")
	p.TxnID = "lifecycle"
	if err := db.StageAudioProjections("lifecycle", []AudioProjection{p}); err != nil {
		t.Fatalf("stage lifecycle row: %v", err)
	}
	assertReferenced := func(want bool, phase string) {
		t.Helper()
		got, err := db.AudioHashReferenced(p.Hash)
		if err != nil {
			t.Fatalf("R12 reference query during %s: %v", phase, err)
		}
		if got != want {
			t.Errorf("R12 referenced during %s = %v, want %v", phase, got, want)
		}
	}
	assertReferenced(true, "staged")
	if ok, err := db.MarkAudioProjectionRemoving(p.Section, p.VirtualPath, 4000); err != nil || ok {
		t.Errorf("R11 staged row mark removing = ok %v, err %v; want false, nil", ok, err)
	}
	if got := requireFoundProjection(t, db, p.Section, p.VirtualPath); got.State != AudioStaged {
		t.Errorf("R11 staged row changed to %q", got.State)
	}
	if _, err := db.CommitAudioProjections("lifecycle", 4001); err != nil {
		t.Fatalf("commit lifecycle row: %v", err)
	}
	assertReferenced(true, "committed")
	if ok, err := db.MarkAudioProjectionRemoving(p.Section, p.VirtualPath, 4002); err != nil || !ok {
		t.Fatalf("R11 committed row mark removing = ok %v, err %v", ok, err)
	}
	got := requireFoundProjection(t, db, p.Section, p.VirtualPath)
	if got.State != AudioRemoving || got.UpdatedAtNS != 4002 {
		t.Errorf("R11 removing state/time = %q/%d, want %q/4002", got.State, got.UpdatedAtNS, AudioRemoving)
	}
	assertReferenced(true, "removing")
	if ok, err := db.MarkAudioProjectionRemoving("music", "unknown.flac", 4003); err != nil || ok {
		t.Errorf("R11 unknown path mark removing = ok %v, err %v; want false, nil", ok, err)
	}
	if err := db.DeleteAudioProjection(p.Section, p.VirtualPath); err != nil {
		t.Fatalf("delete lifecycle row: %v", err)
	}
	assertReferenced(false, "deleted")
	if err := db.DeleteAudioProjection(p.Section, p.VirtualPath); err != nil {
		t.Errorf("F4 repeated delete of unknown path: %v", err)
	}
}

func TestAudioProjectionsByState(t *testing.T) {
	db := openAudioTestDB(t, filepath.Join(t.TempDir(), "states.db"))
	staged := audioProjection("state-staged")
	committed := audioProjection("state-committed")
	removing := audioProjection("state-removing")
	for i, p := range []AudioProjection{staged, committed, removing} {
		p.TxnID = fmt.Sprintf("state-%d", i)
		if err := db.StageAudioProjections(fmt.Sprintf("state-%d", i), []AudioProjection{p}); err != nil {
			t.Fatalf("stage state row %d: %v", i, err)
		}
	}
	if _, err := db.CommitAudioProjections("state-1", 5001); err != nil {
		t.Fatalf("commit state row: %v", err)
	}
	if _, err := db.CommitAudioProjections("state-2", 5002); err != nil {
		t.Fatalf("commit removing precursor: %v", err)
	}
	if ok, err := db.MarkAudioProjectionRemoving(removing.Section, removing.VirtualPath, 5003); err != nil || !ok {
		t.Fatalf("mark state row removing = ok %v, err %v", ok, err)
	}
	wants := map[AudioProjectionState]string{
		AudioStaged: staged.VirtualPath, AudioCommitted: committed.VirtualPath, AudioRemoving: removing.VirtualPath,
	}
	for state, wantPath := range wants {
		t.Run(string(state), func(t *testing.T) {
			got, err := db.AudioProjectionsByState(state)
			if err != nil {
				t.Fatalf("R13 query state %q: %v", state, err)
			}
			if len(got) != 1 || got[0].VirtualPath != wantPath || got[0].State != state {
				t.Errorf("R13 rows for %q = %#v, want only %q", state, got, wantPath)
			}
		})
	}
}

func TestAudioProjectionEmptyAndMissingBoundaries(t *testing.T) {
	db := openAudioTestDB(t, filepath.Join(t.TempDir(), "empty.db"))
	if err := db.StageAudioProjections("empty", nil); err != nil {
		t.Errorf("F1 nil batch: %v", err)
	}
	if err := db.StageAudioProjections("empty", []AudioProjection{}); err != nil {
		t.Errorf("F1 empty batch: %v", err)
	}
	if _, found, err := db.GetAudioProjection("music", "missing.flac"); err != nil || found {
		t.Errorf("F2 missing path = found %v, err %v; want false, nil", found, err)
	}
	if _, found, err := db.AudioProjectionBySource("missing", 1); err != nil || found {
		t.Errorf("F2 missing source = found %v, err %v; want false, nil", found, err)
	}
	lists := []struct {
		name string
		call func() ([]AudioProjection, error)
	}{
		{"F2 hash list", func() ([]AudioProjection, error) { return db.AudioProjectionsByHash("missing") }},
		{"F2 state list", func() ([]AudioProjection, error) { return db.AudioProjectionsByState(AudioCommitted) }},
		{"F2 committed list", func() ([]AudioProjection, error) { return db.CommittedAudioProjections("") }},
	}
	for _, tc := range lists {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.call()
			if err != nil || len(got) != 0 {
				t.Errorf("empty list = %#v, err %v; want empty, nil", got, err)
			}
		})
	}
	if changed, err := db.CommitAudioProjections("unknown", 6001); err != nil || changed != 0 {
		t.Errorf("F3 unknown commit = %d, err %v; want 0, nil", changed, err)
	}
	if removed, err := db.RollbackAudioProjections("unknown"); err != nil || removed != 0 {
		t.Errorf("F3 unknown rollback = %d, err %v; want 0, nil", removed, err)
	}
	if err := db.DeleteAudioProjection("music", "missing.flac"); err != nil {
		t.Errorf("F4 delete unknown path: %v", err)
	}
}

func TestAudioProjectionConcurrentStagesAndReaders(t *testing.T) {
	// J8: one DB connection must remain safe and live when callers do not
	// serialize distinct writers and readers. Assertions depend only on completed
	// calls, never on their interleaving.
	db := openAudioTestDB(t, filepath.Join(t.TempDir(), "concurrent.db"))
	const writers = 16
	const readers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	type result struct {
		projection AudioProjection
		err        error
	}
	results := make(chan result, writers)
	errs := make(chan error, readers)
	for i := 0; i < writers; i++ {
		p := audioProjection(fmt.Sprintf("concurrent-%02d", i))
		p.FileIndex = i + 1
		wg.Add(1)
		go func(i int, p AudioProjection) {
			defer wg.Done()
			<-start
			err := db.StageAudioProjections(fmt.Sprintf("txn-%02d", i), []AudioProjection{p})
			results <- result{projection: p, err: err}
		}(i, p)
	}
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := db.AudioProjectionsByState(AudioStaged)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("J8 concurrent reader: %v", err)
		}
	}
	succeeded := make(map[string]AudioProjection)
	for result := range results {
		if result.err != nil {
			t.Errorf("J8 concurrent distinct stage %q: %v", result.projection.VirtualPath, result.err)
			continue
		}
		succeeded[result.projection.VirtualPath] = result.projection
	}
	got, err := db.AudioProjectionsByState(AudioStaged)
	if err != nil {
		t.Fatalf("J8 final state query: %v", err)
	}
	if len(got) != len(succeeded) || len(succeeded) != writers {
		t.Fatalf("J8 final/successful/wanted rows = %d/%d/%d", len(got), len(succeeded), writers)
	}
	for _, p := range got {
		if _, ok := succeeded[p.VirtualPath]; !ok {
			t.Errorf("J8 final set contains unclaimed row %q", p.VirtualPath)
		}
	}
}

func TestAudioProjectionConcurrentPathConflict(t *testing.T) {
	// J2/J3 + J8: collision enforcement must remain atomic when callers race;
	// assertions intentionally do not depend on which goroutine wins.
	db := openAudioTestDB(t, filepath.Join(t.TempDir(), "concurrent-conflict.db"))
	const contenders = 8
	start := make(chan struct{})
	results := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		p := audioProjection("contended-path")
		p.Hash = fmt.Sprintf("contender-hash-%d", i)
		p.FileIndex = i + 1
		p.TxnID = fmt.Sprintf("contender-txn-%d", i)
		wg.Add(1)
		go func(p AudioProjection) {
			defer wg.Done()
			<-start
			results <- db.StageAudioProjections(p.TxnID, []AudioProjection{p})
		}(p)
	}
	close(start)
	wg.Wait()
	close(results)

	successes := 0
	pathConflicts := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrAudioPathConflict):
			pathConflicts++
		default:
			t.Errorf("J2/J8 concurrent contender returned unexpected error: %v", err)
		}
	}
	if successes != 1 || pathConflicts != contenders-1 {
		t.Errorf("J2/J8 concurrent results: successes=%d path_conflicts=%d, want 1 and %d", successes, pathConflicts, contenders-1)
	}
	rows, err := db.AudioProjectionsByState(AudioStaged)
	if err != nil {
		t.Fatalf("J2/J8 query surviving rows: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("J2/J8 surviving rows = %d, want exactly 1", len(rows))
	}
}
