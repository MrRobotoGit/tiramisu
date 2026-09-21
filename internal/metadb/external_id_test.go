package metadb

import (
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestAudioExternalIDSchemaFreshDatabase(t *testing.T) {
	db := openAudioTestDB(t, filepath.Join(t.TempDir(), "fresh.db"))

	if got := audioExternalIDColumns(t, db); !reflect.DeepEqual(got, []string{"external_id", "external_id_ns"}) {
		t.Errorf("X1 external-ID columns = %v, want both columns", got)
	}
	var maxVersion int
	if err := db.SQL().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&maxVersion); err != nil {
		t.Fatalf("X1 query maximum schema version: %v", err)
	}
	if maxVersion != 10 {
		t.Errorf("X1 maximum schema version = %d, want 10", maxVersion)
	}
	var versionNineDescription, versionTenDescription string
	if err := db.SQL().QueryRow(`SELECT description FROM schema_version WHERE version = 9`).Scan(&versionNineDescription); err != nil {
		t.Fatalf("X1 query schema version 9: %v", err)
	}
	if err := db.SQL().QueryRow(`SELECT description FROM schema_version WHERE version = 10`).Scan(&versionTenDescription); err != nil {
		t.Fatalf("X1 query schema version 10: %v", err)
	}
	if versionTenDescription == "" || versionTenDescription == versionNineDescription {
		t.Errorf("X1 schema version 10 description = %q, want non-empty and distinct from version 9 %q", versionTenDescription, versionNineDescription)
	}
}

func TestAudioExternalIDMigrationFromSchemaNine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema-nine.db")
	db := openAudioTestDB(t, path)
	want := audioProjection("schema-nine-existing")
	if err := db.StageAudioProjections(want.TxnID, []AudioProjection{want}); err != nil {
		t.Fatalf("X2 stage pre-migration projection: %v", err)
	}
	const committedAt = int64(1800000000000000010)
	if changed, err := db.CommitAudioProjections(want.TxnID, committedAt); err != nil || changed != 1 {
		t.Fatalf("X2 commit pre-migration projection = %d, err %v; want 1, nil", changed, err)
	}
	want.UpdatedAtNS = committedAt

	// Derive the old database from the real schema rather than duplicating its DDL:
	// only the v10 additions are removed after seeding through the public API.
	for _, statement := range []string{
		`ALTER TABLE audio_projections DROP COLUMN external_id_ns`,
		`ALTER TABLE audio_projections DROP COLUMN external_id`,
		`DELETE FROM schema_version WHERE version = 10`,
	} {
		if _, err := db.SQL().Exec(statement); err != nil {
			t.Fatalf("X2 establish schema 9 with %q: %v", statement, err)
		}
	}
	if got := audioExternalIDColumns(t, db); len(got) != 0 {
		t.Fatalf("X2 schema-9 columns = %v, want neither external-ID column", got)
	}
	var beforeVersion int
	if err := db.SQL().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&beforeVersion); err != nil {
		t.Fatalf("X2 query schema-9 version: %v", err)
	}
	if beforeVersion != 9 {
		t.Fatalf("X2 pre-migration version = %d, want 9", beforeVersion)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("X2 close schema-9 database: %v", err)
	}

	reopened := openAudioTestDB(t, path)
	if got := audioExternalIDColumns(t, reopened); !reflect.DeepEqual(got, []string{"external_id", "external_id_ns"}) {
		t.Fatalf("X2 migrated columns = %v, want both external-ID columns", got)
	}
	restored := requireFoundProjection(t, reopened, want.Section, want.VirtualPath)
	requireProjectionFields(t, restored, want, AudioCommitted)
	if restored.ExternalID != "" || restored.ExternalIDNamespace != "" {
		t.Errorf("X2 migrated legacy identity = %q/%q, want empty defaults", restored.ExternalIDNamespace, restored.ExternalID)
	}

	var beforeCount int
	var beforeDescription, beforeAppliedAt string
	if err := reopened.SQL().QueryRow(`SELECT COUNT(*), description, applied_at FROM schema_version WHERE version = 10`).Scan(&beforeCount, &beforeDescription, &beforeAppliedAt); err != nil {
		t.Fatalf("X2 snapshot migration row: %v", err)
	}
	var beforeChanges int64
	if err := reopened.SQL().QueryRow(`SELECT total_changes()`).Scan(&beforeChanges); err != nil {
		t.Fatalf("X2 query change count before second ExecSchema: %v", err)
	}
	if err := reopened.ExecSchema(); err != nil {
		t.Fatalf("X2 second ExecSchema: %v", err)
	}
	var afterChanges int64
	if err := reopened.SQL().QueryRow(`SELECT total_changes()`).Scan(&afterChanges); err != nil {
		t.Fatalf("X2 query change count after second ExecSchema: %v", err)
	}
	if afterChanges != beforeChanges {
		t.Errorf("X2 second ExecSchema changed %d database rows, want zero", afterChanges-beforeChanges)
	}
	var afterCount int
	var afterDescription, afterAppliedAt string
	if err := reopened.SQL().QueryRow(`SELECT COUNT(*), description, applied_at FROM schema_version WHERE version = 10`).Scan(&afterCount, &afterDescription, &afterAppliedAt); err != nil {
		t.Fatalf("X2 query migration row after second ExecSchema: %v", err)
	}
	if beforeCount != 1 || afterCount != beforeCount || afterDescription != beforeDescription || afterAppliedAt != beforeAppliedAt {
		t.Errorf("X2 schema migration row changed on second ExecSchema: before=%d/%q/%q after=%d/%q/%q", beforeCount, beforeDescription, beforeAppliedAt, afterCount, afterDescription, afterAppliedAt)
	}
	requireProjectionFields(t, requireFoundProjection(t, reopened, want.Section, want.VirtualPath), want, AudioCommitted)
}

func TestAudioExternalIDPersistenceAndValidation(t *testing.T) {
	t.Run("X3_X6_X7 known and unknown namespaces round trip opaque values", func(t *testing.T) {
		db := openAudioTestDB(t, filepath.Join(t.TempDir(), "opaque.db"))
		tests := []struct {
			name       string
			tag        string
			externalID string
			namespace  string
		}{
			{"X3 known namespace", "known", "release-MBID-ABC123", "musicbrainz"},
			{"X6 X7 arbitrary namespace and verbatim ID", "opaque", "  Release/AbC:DéF?x=Y  ", "provider.example/Unknown-V1"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				p := audioProjection(tc.tag)
				p.ExternalID = tc.externalID
				p.ExternalIDNamespace = tc.namespace
				if err := db.StageAudioProjections(p.TxnID, []AudioProjection{p}); err != nil {
					t.Fatalf("stage projection: %v", err)
				}
				if changed, err := db.CommitAudioProjections(p.TxnID, p.UpdatedAtNS+1); err != nil || changed != 1 {
					t.Fatalf("commit projection = %d, err %v; want 1, nil", changed, err)
				}
				got := requireFoundProjection(t, db, p.Section, p.VirtualPath)
				if got.ExternalID != tc.externalID || got.ExternalIDNamespace != tc.namespace {
					t.Errorf("external identity = %q/%q, want byte-for-byte %q/%q", got.ExternalIDNamespace, got.ExternalID, tc.namespace, tc.externalID)
				}
			})
		}
	})

	t.Run("X4 neither identity field is valid", func(t *testing.T) {
		db := openAudioTestDB(t, filepath.Join(t.TempDir(), "absent.db"))
		p := audioProjection("absent")
		if err := db.StageAudioProjections(p.TxnID, []AudioProjection{p}); err != nil {
			t.Fatalf("stage projection without external identity: %v", err)
		}
		got := requireFoundProjection(t, db, p.Section, p.VirtualPath)
		if got.ExternalID != "" || got.ExternalIDNamespace != "" {
			t.Errorf("X4 absent external identity = %q/%q, want both empty", got.ExternalIDNamespace, got.ExternalID)
		}
	})

	t.Run("X5 both identity fields are required together", func(t *testing.T) {
		tests := []struct {
			name       string
			externalID string
			namespace  string
		}{
			{"ID without namespace", "release-id", ""},
			{"namespace without ID", "", "musicbrainz"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				db := openAudioTestDB(t, filepath.Join(t.TempDir(), "invalid-pair.db"))
				p := audioProjection("invalid-pair")
				p.ExternalID = tc.externalID
				p.ExternalIDNamespace = tc.namespace
				if err := db.StageAudioProjections(p.TxnID, []AudioProjection{p}); err == nil {
					t.Fatal("X5 StageAudioProjections error = nil for a partial external identity")
				}
				requireNoProjection(t, db, p.Section, p.VirtualPath)
			})
		}
	})

	t.Run("X8 shared external identity is not unique", func(t *testing.T) {
		db := openAudioTestDB(t, filepath.Join(t.TempDir(), "shared.db"))
		const (
			externalID = "shared-release-mbid"
			namespace  = "musicbrainz"
		)
		rows := []AudioProjection{audioProjection("track-01"), audioProjection("track-02")}
		for i := range rows {
			rows[i].FileIndex = i + 1
			rows[i].ExternalID = externalID
			rows[i].ExternalIDNamespace = namespace
		}
		const txnID = "shared-release"
		if err := db.StageAudioProjections(txnID, rows); err != nil {
			t.Fatalf("X8 stage tracks sharing an external identity: %v", err)
		}
		if changed, err := db.CommitAudioProjections(txnID, 1900000000000000000); err != nil || changed != len(rows) {
			t.Fatalf("X8 commit shared identity rows = %d, err %v; want %d, nil", changed, err, len(rows))
		}
		for _, p := range rows {
			got := requireFoundProjection(t, db, p.Section, p.VirtualPath)
			if got.ExternalID != externalID || got.ExternalIDNamespace != namespace {
				t.Errorf("X8 %q identity = %q/%q, want shared %q/%q", p.VirtualPath, got.ExternalIDNamespace, got.ExternalID, namespace, externalID)
			}
		}
	})
}

func audioExternalIDColumns(t *testing.T, db *DB) []string {
	t.Helper()
	rows, err := db.SQL().Query(`SELECT name FROM pragma_table_info('audio_projections') WHERE name IN ('external_id', 'external_id_ns')`)
	if err != nil {
		t.Fatalf("query audio projection columns: %v", err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatalf("scan audio projection column: %v", err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audio projection columns: %v", err)
	}
	sort.Strings(columns)
	return columns
}
