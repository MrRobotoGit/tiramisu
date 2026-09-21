package library

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"tiramisu/internal/metadb"
)

type recoverySourceFake struct {
	rows      []metadb.AudioProjection
	rowsErr   error
	rollbacks []string
	rollErr   error
}

func (f *recoverySourceFake) AudioProjectionsByState(metadb.AudioProjectionState) ([]metadb.AudioProjection, error) {
	return append([]metadb.AudioProjection(nil), f.rows...), f.rowsErr
}

func (f *recoverySourceFake) RollbackAudioProjections(txnID string) (int, error) {
	f.rollbacks = append(f.rollbacks, txnID)
	return 1, f.rollErr
}

func newRecoveryFixture(t *testing.T) (root string, logger *log.Logger) {
	t.Helper()
	root = t.TempDir()
	for _, section := range []string{string(SectionMusic), string(SectionAudiobooks)} {
		if err := os.Mkdir(filepath.Join(root, section), 0o755); err != nil {
			t.Fatalf("create %s: %v", section, err)
		}
	}
	return root, log.New(io.Discard, "", 0)
}

// B1: a crash between the final rename and the registry commit leaves final names on
// disk with staged rows; startup rolls the transaction back so the path is usable again.
func TestRecoverStagedAudioTransactions_B1(t *testing.T) {
	t.Run("B1a_removes_final_and_staging_names_then_rolls_back", func(t *testing.T) {
		root, logger := newRecoveryFixture(t)
		dir := filepath.Join(root, string(SectionMusic), "Artist", "Album")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		final := filepath.Join(dir, "01 - Track_01234567.flac")
		staging := filepath.Join(dir, ".tiramisu-txn-0-0")
		for _, path := range []string{final, staging} {
			if err := os.WriteFile(path, []byte("stub"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		src := &recoverySourceFake{rows: []metadb.AudioProjection{{
			Section: string(SectionMusic), VirtualPath: "Artist/Album/01 - Track_01234567.flac",
			StagingName: ".tiramisu-txn-0-0", TxnID: "txn-0",
		}}}

		recovered, err := RecoverStagedAudioTransactions(src, root, logger)
		if err != nil {
			t.Fatalf("RecoverStagedAudioTransactions() error = %v", err)
		}
		if recovered != 1 {
			t.Fatalf("recovered = %d, want 1", recovered)
		}
		for _, path := range []string{final, staging} {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s lstat error = %v, want os.ErrNotExist", path, err)
			}
		}
		if _, err := os.Lstat(filepath.Join(root, string(SectionMusic), "Artist")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("empty artist chain survived: lstat error = %v, want os.ErrNotExist", err)
		}
		if len(src.rollbacks) != 1 || src.rollbacks[0] != "txn-0" {
			t.Errorf("rollbacks = %v, want [txn-0]", src.rollbacks)
		}
	})

	t.Run("B1b_a_filesystem_failure_keeps_the_rows", func(t *testing.T) {
		root, logger := newRecoveryFixture(t)
		// A file where the row's parent directory belongs: removal cannot resolve.
		if err := os.WriteFile(filepath.Join(root, string(SectionMusic), "Artist"), []byte("file"), 0o644); err != nil {
			t.Fatal(err)
		}
		src := &recoverySourceFake{rows: []metadb.AudioProjection{{
			Section: string(SectionMusic), VirtualPath: "Artist/Album/01 - Track_01234567.flac",
			StagingName: ".tiramisu-txn-0-0", TxnID: "txn-0",
		}}}

		recovered, err := RecoverStagedAudioTransactions(src, root, logger)
		if err == nil {
			t.Fatal("RecoverStagedAudioTransactions() error = nil, want the removal failure")
		}
		if recovered != 0 {
			t.Errorf("recovered = %d, want 0", recovered)
		}
		if len(src.rollbacks) != 0 {
			t.Errorf("rollbacks = %v, want none while the rows still prove ownership", src.rollbacks)
		}
	})

	t.Run("B1c_one_broken_transaction_does_not_stop_the_others", func(t *testing.T) {
		root, logger := newRecoveryFixture(t)
		goodDir := filepath.Join(root, string(SectionMusic), "Good")
		if err := os.MkdirAll(goodDir, 0o755); err != nil {
			t.Fatal(err)
		}
		goodFinal := filepath.Join(goodDir, "01_01234567.flac")
		if err := os.WriteFile(goodFinal, []byte("stub"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, string(SectionMusic), "Broken"), []byte("file"), 0o644); err != nil {
			t.Fatal(err)
		}
		src := &recoverySourceFake{rows: []metadb.AudioProjection{
			{Section: string(SectionMusic), VirtualPath: "Good/01_01234567.flac", TxnID: "txn-good"},
			{Section: string(SectionMusic), VirtualPath: "Broken/Album/01_01234567.flac", TxnID: "txn-broken"},
		}}

		recovered, err := RecoverStagedAudioTransactions(src, root, logger)
		if err == nil {
			t.Fatal("error = nil, want the broken transaction reported")
		}
		if recovered != 1 {
			t.Errorf("recovered = %d, want 1", recovered)
		}
		if len(src.rollbacks) != 1 || src.rollbacks[0] != "txn-good" {
			t.Errorf("rollbacks = %v, want [txn-good]", src.rollbacks)
		}
		if _, statErr := os.Lstat(goodFinal); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("good final lstat error = %v, want os.ErrNotExist", statErr)
		}
	})
}
