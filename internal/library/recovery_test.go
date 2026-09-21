package library

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"tiramisu/internal/metadb"
)

type recoverySourceFake struct {
	rows      []metadb.AudioProjection
	rowsErr   error
	rollbacks []string
	rollErr   error
	deleted   []string
	deleteErr error
}

func (f *recoverySourceFake) AudioProjectionsByState(metadb.AudioProjectionState) ([]metadb.AudioProjection, error) {
	return append([]metadb.AudioProjection(nil), f.rows...), f.rowsErr
}

func (f *recoverySourceFake) RollbackAudioProjections(txnID string) (int, error) {
	f.rollbacks = append(f.rollbacks, txnID)
	return 1, f.rollErr
}

func (f *recoverySourceFake) DeleteAudioProjection(section, virtualPath string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, section+"/"+virtualPath)
	return nil
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

// A removal that crashed between the removing mark and the row delete leaves a stub and
// a row nothing else can finish: the namespace already dropped the path, so the boot
// sweep is the only actor that can complete it.
func TestRecoverRemovingAudioProjections_sweeps_interrupted_removals(t *testing.T) {
	root, logger := newRecoveryFixture(t)
	dir := filepath.Join(root, string(SectionAudiobooks), "Author", "Book")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(dir, "Part 01_01234567.m4b")
	if err := os.WriteFile(stub, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := &recoverySourceFake{rows: []metadb.AudioProjection{{
		Section:     string(SectionAudiobooks),
		VirtualPath: "Author/Book/Part 01_01234567.m4b",
		Hash:        "hash",
		State:       metadb.AudioRemoving,
	}}}

	swept, err := RecoverRemovingAudioProjections(src, root, logger)
	if err != nil {
		t.Fatalf("RecoverRemovingAudioProjections() error = %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept = %d, want 1", swept)
	}
	if _, err := os.Lstat(stub); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stub lstat error = %v, want os.ErrNotExist", err)
	}
	if _, err := os.Lstat(filepath.Join(root, string(SectionAudiobooks), "Author")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("empty author chain survived: lstat error = %v, want os.ErrNotExist", err)
	}
	if _, err := os.Lstat(filepath.Join(root, string(SectionAudiobooks))); err != nil {
		t.Errorf("section root must survive the sweep: %v", err)
	}
	want := []string{string(SectionAudiobooks) + "/Author/Book/Part 01_01234567.m4b"}
	if !reflect.DeepEqual(src.deleted, want) {
		t.Errorf("deleted = %v, want %v", src.deleted, want)
	}
}

// A failed sweep keeps the row so the next boot retries it, exactly like B1b.
func TestRecoverRemovingAudioProjections_keeps_the_row_on_failure(t *testing.T) {
	root, logger := newRecoveryFixture(t)
	// A file where the row's parent directory belongs: removal cannot resolve.
	if err := os.WriteFile(filepath.Join(root, string(SectionMusic), "Artist"), []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := &recoverySourceFake{rows: []metadb.AudioProjection{{
		Section:     string(SectionMusic),
		VirtualPath: "Artist/Album/01 - Track_01234567.flac",
		Hash:        "hash",
		State:       metadb.AudioRemoving,
	}}}

	swept, err := RecoverRemovingAudioProjections(src, root, logger)
	if err == nil {
		t.Fatal("RecoverRemovingAudioProjections() error = nil, want the filesystem failure")
	}
	if swept != 0 {
		t.Errorf("swept = %d, want 0", swept)
	}
	if len(src.deleted) != 0 {
		t.Errorf("deleted = %v, want none while the stub could not be removed", src.deleted)
	}
}
