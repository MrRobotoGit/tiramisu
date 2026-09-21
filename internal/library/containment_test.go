package library

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

func TestSectionWriterWriteStaged_BeneathRule(t *testing.T) {
	t.Run("C1_creates_parent_directories_and_exact_file_content", func(t *testing.T) {
		root := t.TempDir()
		writer := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, writer)

		relPath := "Artist/Album/Track_01234567.flac"
		want := []byte("exact staged audio stub")
		if err := writer.WriteStaged(relPath, want); err != nil {
			t.Fatalf("WriteStaged(%q): %v", relPath, err)
		}

		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relPath)))
		if err != nil {
			t.Fatalf("read staged file: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("staged content = %q, want %q", got, want)
		}
	})

	t.Run("C1_section_root_symlink_is_resolved_once_when_opened", func(t *testing.T) {
		configuredParent := t.TempDir()
		configuredRoot := filepath.Join(configuredParent, "music")
		originalTarget := t.TempDir()
		retargetedRoot := t.TempDir()
		if err := os.Symlink(originalTarget, configuredRoot); err != nil {
			t.Fatalf("create configured root symlink: %v", err)
		}

		writer := mustOpenSectionWriter(t, configuredRoot)
		closeSectionWriterAtCleanup(t, writer)

		if err := os.Remove(configuredRoot); err != nil {
			t.Fatalf("remove configured root symlink: %v", err)
		}
		if err := os.Symlink(retargetedRoot, configuredRoot); err != nil {
			t.Fatalf("retarget configured root symlink: %v", err)
		}

		relPath := "Artist/Album/Track_01234567.flac"
		want := []byte("anchored to the originally opened root")
		if err := writer.WriteStaged(relPath, want); err != nil {
			t.Fatalf("WriteStaged(%q): %v", relPath, err)
		}
		got, err := os.ReadFile(filepath.Join(originalTarget, filepath.FromSlash(relPath)))
		if err != nil {
			t.Fatalf("read file from original root target: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("original-root content = %q, want %q", got, want)
		}
		assertDirectoryEmptyByWalk(t, retargetedRoot)
	})

	t.Run("C2_C5_symlinked_parent_cannot_write_outside", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(root, "Artist")); err != nil {
			t.Fatalf("create escaping parent symlink: %v", err)
		}
		writer := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, writer)

		err := writer.WriteStaged("Artist/Album/Track_01234567.flac", []byte("must not escape"))
		assertErrorIs(t, err, ErrPathEscapesSection)
		assertDirectoryEmptyByWalk(t, outside)
	})

	t.Run("C3_C5_symlinked_leaf_cannot_write_outside", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		parent := filepath.Join(root, "Artist", "Album")
		if err := os.MkdirAll(parent, 0o755); err != nil {
			t.Fatalf("create in-root parent: %v", err)
		}
		leaf := filepath.Join(parent, "Track_01234567.flac")
		if err := os.Symlink(filepath.Join(outside, "escaped.flac"), leaf); err != nil {
			t.Fatalf("create escaping leaf symlink: %v", err)
		}
		writer := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, writer)

		err := writer.WriteStaged("Artist/Album/Track_01234567.flac", []byte("must not escape"))
		if err == nil {
			t.Fatal("WriteStaged through a symlinked leaf error = nil, want rejection")
		}
		info, statErr := os.Lstat(leaf)
		if statErr != nil {
			t.Fatalf("lstat leaf after rejected write: %v", statErr)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("leaf mode = %v, want the original symlink preserved", info.Mode())
		}
		assertDirectoryEmptyByWalk(t, outside)
	})
}

func TestSectionWriterWriteStaged_RejectsInvalidRelativePaths_C4_C5(t *testing.T) {
	tests := []struct {
		name    string
		relPath string
	}{
		{"C4_C5_dot_dot_component", "Artist/../Track_01234567.flac"},
		{"C4_C5_absolute_path", "/Artist/Album/Track_01234567.flac"},
		{"C4_C5_empty_path", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			writer := mustOpenSectionWriter(t, root)
			closeSectionWriterAtCleanup(t, writer)

			if err := writer.WriteStaged(tt.relPath, []byte("must not be written")); err == nil {
				t.Fatalf("WriteStaged(%q) error = nil, want rejection", tt.relPath)
			}
			assertDirectoryEmptyByWalk(t, root)
			assertDirectoryEmptyByWalk(t, outside)
		})
	}
}

func TestSectionWriterWriteStaged_ExclusiveCreation(t *testing.T) {
	t.Run("C6_C5_existing_staged_file_is_not_truncated", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		relPath := "Artist/Album/Track_01234567.flac"
		path := filepath.Join(root, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create staged parent: %v", err)
		}
		want := []byte("pre-existing unregistered content must survive")
		if err := os.WriteFile(path, want, 0o600); err != nil {
			t.Fatalf("write existing staged file: %v", err)
		}
		writer := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, writer)

		err := writer.WriteStaged(relPath, []byte("replacement content"))
		assertErrorIs(t, err, ErrDestinationExists)
		assertFileContent(t, path, want)
		assertDirectoryEmptyByWalk(t, outside)
	})

	t.Run("C7_two_writers_racing_one_staged_name_have_exactly_one_winner", func(t *testing.T) {
		root := t.TempDir()
		first := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, first)
		second := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, second)

		relPath := "Artist/Album/Track_01234567.flac"
		payloads := [][]byte{[]byte("first writer"), []byte("second writer")}
		writers := []*SectionWriter{first, second}
		start := make(chan struct{})
		type result struct {
			writer int
			err    error
		}
		results := make(chan result, len(writers))
		for i := range writers {
			go func(i int) {
				<-start
				results <- result{writer: i, err: writers[i].WriteStaged(relPath, payloads[i])}
			}(i)
		}
		close(start)

		winner := -1
		destinationExists := 0
		for range writers {
			got := <-results
			switch {
			case got.err == nil:
				if winner != -1 {
					t.Errorf("writers %d and %d both succeeded", winner, got.writer)
				}
				winner = got.writer
			case errors.Is(got.err, ErrDestinationExists):
				destinationExists++
			default:
				t.Errorf("writer %d error = %v, want nil or ErrDestinationExists", got.writer, got.err)
			}
		}
		if winner == -1 || destinationExists != 1 {
			t.Fatalf("race result: winner = %d, ErrDestinationExists count = %d; want one of each", winner, destinationExists)
		}
		assertFileContent(t, filepath.Join(root, filepath.FromSlash(relPath)), payloads[winner])
	})
}

func TestSectionWriterPublish(t *testing.T) {
	t.Run("C8_moves_staged_file_to_final_name", func(t *testing.T) {
		root := t.TempDir()
		writer := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, writer)
		stagedRel := "Artist/Album/staged.flac"
		finalRel := "Artist/Album/Track_01234567.flac"
		want := []byte("published content")
		if err := writer.WriteStaged(stagedRel, want); err != nil {
			t.Fatalf("WriteStaged(%q): %v", stagedRel, err)
		}

		if err := writer.Publish(stagedRel, finalRel); err != nil {
			t.Fatalf("Publish(%q, %q): %v", stagedRel, finalRel, err)
		}
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(stagedRel))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("staged path after Publish: error = %v, want os.ErrNotExist", err)
		}
		assertFileContent(t, filepath.Join(root, filepath.FromSlash(finalRel)), want)
	})

	t.Run("C9_C5_existing_final_is_preserved_and_staged_file_remains", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		writer := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, writer)
		stagedRel := "Artist/Album/staged.flac"
		finalRel := "Artist/Album/Track_01234567.flac"
		stagedContent := []byte("staged content remains available for rollback")
		finalContent := []byte("unregistered final content must survive")
		if err := writer.WriteStaged(stagedRel, stagedContent); err != nil {
			t.Fatalf("WriteStaged(%q): %v", stagedRel, err)
		}
		finalPath := filepath.Join(root, filepath.FromSlash(finalRel))
		if err := os.WriteFile(finalPath, finalContent, 0o600); err != nil {
			t.Fatalf("write pre-existing final file: %v", err)
		}

		err := writer.Publish(stagedRel, finalRel)
		assertErrorIs(t, err, ErrDestinationExists)
		assertFileContent(t, finalPath, finalContent)
		assertFileContent(t, filepath.Join(root, filepath.FromSlash(stagedRel)), stagedContent)
		assertDirectoryEmptyByWalk(t, outside)
	})

	t.Run("C10_C5_final_path_with_symlinked_parent_cannot_escape", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		writer := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, writer)
		stagedRel := "Safe/staged.flac"
		stagedContent := []byte("staged content")
		if err := writer.WriteStaged(stagedRel, stagedContent); err != nil {
			t.Fatalf("WriteStaged(%q): %v", stagedRel, err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "Escaping")); err != nil {
			t.Fatalf("create escaping final parent symlink: %v", err)
		}

		err := writer.Publish(stagedRel, "Escaping/Track_01234567.flac")
		assertErrorIs(t, err, ErrPathEscapesSection)
		assertFileContent(t, filepath.Join(root, filepath.FromSlash(stagedRel)), stagedContent)
		assertDirectoryEmptyByWalk(t, outside)
	})
}

func TestSectionWriterRemoveStaged(t *testing.T) {
	t.Run("C11_deletes_an_existing_file_and_ignores_absence", func(t *testing.T) {
		root := t.TempDir()
		writer := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, writer)
		relPath := "Artist/Album/staged.flac"
		path := filepath.Join(root, filepath.FromSlash(relPath))
		if err := writer.WriteStaged(relPath, []byte("remove me")); err != nil {
			t.Fatalf("WriteStaged(%q): %v", relPath, err)
		}

		if err := writer.RemoveStaged(relPath); err != nil {
			t.Fatalf("first RemoveStaged(%q): %v", relPath, err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("removed path lstat error = %v, want os.ErrNotExist", err)
		}
		if err := writer.RemoveStaged(relPath); err != nil {
			t.Errorf("second RemoveStaged(%q) = %v, want nil", relPath, err)
		}
	})

	t.Run("C12_C5_refuses_escaping_cleanup_and_preserves_outside_victim", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		victimRel := "Album/someone-elses-file.flac"
		victimPath := filepath.Join(outside, filepath.FromSlash(victimRel))
		if err := os.MkdirAll(filepath.Dir(victimPath), 0o755); err != nil {
			t.Fatalf("create outside victim parent: %v", err)
		}
		want := []byte("outside owner data must survive rollback")
		if err := os.WriteFile(victimPath, want, 0o600); err != nil {
			t.Fatalf("write outside victim: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "Artist")); err != nil {
			t.Fatalf("create escaping cleanup parent symlink: %v", err)
		}
		outsideBefore := snapshotDirectory(t, outside)
		writer := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, writer)

		err := writer.RemoveStaged("Artist/" + victimRel)
		assertErrorIs(t, err, ErrPathEscapesSection)
		assertFileContent(t, victimPath, want)
		outsideAfter := snapshotDirectory(t, outside)
		if !reflect.DeepEqual(outsideAfter, outsideBefore) {
			t.Errorf("outside tree changed during rejected cleanup:\n before: %#v\n  after: %#v", outsideBefore, outsideAfter)
		}
	})
}

func TestOpenSectionWriter_InvalidRoots_C13_C5(t *testing.T) {
	t.Run("C13_C5_nonexistent_root_is_an_error", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "missing")
		outside := t.TempDir()

		writer, err := OpenSectionWriter(root)
		if writer != nil {
			_ = writer.Close()
		}
		if err == nil {
			t.Fatal("OpenSectionWriter(nonexistent root) error = nil, want error")
		}
		if _, statErr := os.Lstat(root); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("nonexistent root lstat error = %v, want os.ErrNotExist", statErr)
		}
		assertDirectoryEmptyByWalk(t, parent)
		assertDirectoryEmptyByWalk(t, outside)
	})

	t.Run("C13_C5_regular_file_root_is_an_error", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "not-a-directory")
		outside := t.TempDir()
		want := []byte("root file content")
		if err := os.WriteFile(root, want, 0o600); err != nil {
			t.Fatalf("write root fixture: %v", err)
		}

		writer, err := OpenSectionWriter(root)
		if writer != nil {
			_ = writer.Close()
		}
		if err == nil {
			t.Fatal("OpenSectionWriter(regular file) error = nil, want error")
		}
		assertFileContent(t, root, want)
		assertDirectoryEmptyByWalk(t, outside)
	})
}

func TestSectionWriter_OperationsAfterClose_C14_C5(t *testing.T) {
	t.Run("C14_C5_WriteStaged_returns_error_without_mutating_a_recycled_root", func(t *testing.T) {
		originalRoot := t.TempDir()
		closedWriter := mustOpenSectionWriter(t, originalRoot)
		if err := closedWriter.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		recycledRoot := t.TempDir()
		replacement := mustOpenSectionWriter(t, recycledRoot)
		closeSectionWriterAtCleanup(t, replacement)

		if err := closedWriter.WriteStaged("Artist/after-close.flac", []byte("must not be written")); err == nil {
			t.Fatal("WriteStaged after Close error = nil, want error")
		}
		assertDirectoryEmptyByWalk(t, originalRoot)
		assertDirectoryEmptyByWalk(t, recycledRoot)
	})

	t.Run("C14_C5_Publish_returns_error_without_mutating_a_recycled_root", func(t *testing.T) {
		originalRoot := t.TempDir()
		closedWriter := mustOpenSectionWriter(t, originalRoot)
		stagedRel := "Artist/staged.flac"
		finalRel := "Artist/final.flac"
		if err := closedWriter.WriteStaged(stagedRel, []byte("original staged")); err != nil {
			t.Fatalf("seed original staged file: %v", err)
		}
		if err := closedWriter.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		recycledRoot := t.TempDir()
		replacement := mustOpenSectionWriter(t, recycledRoot)
		closeSectionWriterAtCleanup(t, replacement)
		if err := replacement.WriteStaged(stagedRel, []byte("recycled descriptor decoy")); err != nil {
			t.Fatalf("seed recycled-root staged file: %v", err)
		}
		originalBefore := snapshotDirectory(t, originalRoot)
		recycledBefore := snapshotDirectory(t, recycledRoot)

		if err := closedWriter.Publish(stagedRel, finalRel); err == nil {
			t.Fatal("Publish after Close error = nil, want error")
		}
		assertDirectorySnapshot(t, originalRoot, originalBefore)
		assertDirectorySnapshot(t, recycledRoot, recycledBefore)
	})

	t.Run("C14_C5_RemoveStaged_returns_error_without_mutating_a_recycled_root", func(t *testing.T) {
		originalRoot := t.TempDir()
		closedWriter := mustOpenSectionWriter(t, originalRoot)
		relPath := "Artist/staged.flac"
		if err := closedWriter.WriteStaged(relPath, []byte("original staged")); err != nil {
			t.Fatalf("seed original staged file: %v", err)
		}
		if err := closedWriter.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		recycledRoot := t.TempDir()
		replacement := mustOpenSectionWriter(t, recycledRoot)
		closeSectionWriterAtCleanup(t, replacement)
		if err := replacement.WriteStaged(relPath, []byte("recycled descriptor decoy")); err != nil {
			t.Fatalf("seed recycled-root staged file: %v", err)
		}
		originalBefore := snapshotDirectory(t, originalRoot)
		recycledBefore := snapshotDirectory(t, recycledRoot)

		if err := closedWriter.RemoveStaged(relPath); err == nil {
			t.Fatal("RemoveStaged after Close error = nil, want error")
		}
		assertDirectorySnapshot(t, originalRoot, originalBefore)
		assertDirectorySnapshot(t, recycledRoot, recycledBefore)
	})
}

func TestSectionWriterRejectsInwardSymlinks(t *testing.T) {
	t.Run("C15_WriteStaged_refuses_an_inward_symlink", func(t *testing.T) {
		root, inside, writer := newInwardSymlinkFixture(t)
		before := snapshotDirectory(t, root)

		err := writer.WriteStaged("Artist/Album/Track_01234567.flac", []byte("must not be redirected"))
		if err == nil {
			t.Fatal("WriteStaged through an inward symlink error = nil, want rejection")
		}
		assertDirectoryEmptyByWalk(t, inside)
		assertDirectorySnapshot(t, root, before)
	})

	t.Run("C16_Publish_refuses_an_inward_symlink_on_the_staged_side", func(t *testing.T) {
		root, inside, writer := newInwardSymlinkFixture(t)
		stagedPath := filepath.Join(inside, "Album", "staged.flac")
		if err := os.MkdirAll(filepath.Dir(stagedPath), 0o755); err != nil {
			t.Fatalf("create inward staged parent: %v", err)
		}
		want := []byte("file not created by this request")
		if err := os.WriteFile(stagedPath, want, 0o600); err != nil {
			t.Fatalf("write inward staged fixture: %v", err)
		}
		if err := os.Mkdir(filepath.Join(root, "Safe"), 0o755); err != nil {
			t.Fatalf("create safe final parent: %v", err)
		}
		before := snapshotDirectory(t, root)

		err := writer.Publish("Artist/Album/staged.flac", "Safe/final.flac")
		if err == nil {
			t.Fatal("Publish from an inward-symlinked staged path error = nil, want rejection")
		}
		assertFileContent(t, stagedPath, want)
		assertDirectorySnapshot(t, root, before)
	})

	t.Run("C16_Publish_refuses_an_inward_symlink_on_the_final_side", func(t *testing.T) {
		root, inside, writer := newInwardSymlinkFixture(t)
		stagedRel := "Safe/staged.flac"
		want := []byte("staged content must remain in place")
		if err := writer.WriteStaged(stagedRel, want); err != nil {
			t.Fatalf("seed safe staged file: %v", err)
		}
		before := snapshotDirectory(t, root)

		err := writer.Publish(stagedRel, "Artist/Album/final.flac")
		if err == nil {
			t.Fatal("Publish to an inward-symlinked final path error = nil, want rejection")
		}
		assertFileContent(t, filepath.Join(root, filepath.FromSlash(stagedRel)), want)
		assertDirectoryEmptyByWalk(t, inside)
		assertDirectorySnapshot(t, root, before)
	})

	t.Run("C16_RemoveStaged_refuses_an_inward_symlink", func(t *testing.T) {
		root, inside, writer := newInwardSymlinkFixture(t)
		victimPath := filepath.Join(inside, "Album", "someone-elses-file.flac")
		if err := os.MkdirAll(filepath.Dir(victimPath), 0o755); err != nil {
			t.Fatalf("create inward victim parent: %v", err)
		}
		want := []byte("in-root file not created by this request")
		if err := os.WriteFile(victimPath, want, 0o600); err != nil {
			t.Fatalf("write inward victim: %v", err)
		}
		before := snapshotDirectory(t, root)

		err := writer.RemoveStaged("Artist/Album/someone-elses-file.flac")
		if err == nil {
			t.Fatal("RemoveStaged through an inward symlink error = nil, want rejection")
		}
		assertFileContent(t, victimPath, want)
		assertDirectorySnapshot(t, root, before)
	})
}

func TestSectionWriterPruneEmptyDirs(t *testing.T) {
	t.Run("C17a_removes_empty_parents_and_never_the_section_root", func(t *testing.T) {
		root := t.TempDir()
		rootBefore, err := os.Stat(root)
		if err != nil {
			t.Fatalf("stat section root before prune: %v", err)
		}
		writer := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, writer)
		relPath := "A/B/track.flac"
		if err := writer.WriteStaged(relPath, []byte("temporary")); err != nil {
			t.Fatalf("WriteStaged(%q): %v", relPath, err)
		}
		if err := writer.RemoveStaged(relPath); err != nil {
			t.Fatalf("RemoveStaged(%q): %v", relPath, err)
		}

		if err := writer.PruneEmptyDirs(relPath); err != nil {
			t.Fatalf("PruneEmptyDirs(%q): %v", relPath, err)
		}
		for _, path := range []string{filepath.Join(root, "A", "B"), filepath.Join(root, "A")} {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("pruned directory %q lstat error = %v, want os.ErrNotExist", path, err)
			}
		}
		rootAfter, err := os.Stat(root)
		if err != nil {
			t.Fatalf("section root was removed: %v", err)
		}
		if !os.SameFile(rootBefore, rootAfter) {
			t.Error("section root changed during pruning")
		}
		assertDirectoryEmptyByWalk(t, root)
	})

	t.Run("C17b_nonempty_directory_stops_pruning", func(t *testing.T) {
		root := t.TempDir()
		writer := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, writer)
		removedRel := "A/B/track.flac"
		keptRel := "A/B/keep.flac"
		keptContent := []byte("still in use")
		if err := writer.WriteStaged(removedRel, []byte("temporary")); err != nil {
			t.Fatalf("write removable file: %v", err)
		}
		if err := writer.WriteStaged(keptRel, keptContent); err != nil {
			t.Fatalf("write retained file: %v", err)
		}
		if err := writer.RemoveStaged(removedRel); err != nil {
			t.Fatalf("remove staged file: %v", err)
		}
		before := snapshotDirectory(t, root)

		if err := writer.PruneEmptyDirs(removedRel); err != nil {
			t.Fatalf("PruneEmptyDirs(%q): %v", removedRel, err)
		}
		assertFileContent(t, filepath.Join(root, filepath.FromSlash(keptRel)), keptContent)
		assertDirectorySnapshot(t, root, before)
	})

	t.Run("C17c_preexisting_ancestor_survives_and_only_created_dirs_are_pruned", func(t *testing.T) {
		root := t.TempDir()
		preexistingAncestor := filepath.Join(root, "A")
		unrelated := filepath.Join(root, "Unrelated")
		if err := os.Mkdir(preexistingAncestor, 0o700); err != nil {
			t.Fatalf("create pre-existing ancestor: %v", err)
		}
		if err := os.Mkdir(unrelated, 0o700); err != nil {
			t.Fatalf("create unrelated empty directory: %v", err)
		}
		writer := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, writer)
		relPath := "A/B/track.flac"
		if err := writer.WriteStaged(relPath, []byte("temporary")); err != nil {
			t.Fatalf("WriteStaged(%q): %v", relPath, err)
		}
		if err := writer.RemoveStaged(relPath); err != nil {
			t.Fatalf("RemoveStaged(%q): %v", relPath, err)
		}

		if err := writer.PruneCreatedDirs(); err != nil {
			t.Fatalf("PruneCreatedDirs(): %v", err)
		}
		// The request created B but not A: only B is pruned, and the empty
		// pre-existing ancestor is left alone.
		if _, err := os.Lstat(filepath.Join(preexistingAncestor, "B")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("request-created directory lstat error = %v, want os.ErrNotExist", err)
		}
		info, err := os.Stat(preexistingAncestor)
		if err != nil {
			t.Fatalf("pre-existing ancestor was removed: %v", err)
		}
		if !info.IsDir() {
			t.Errorf("pre-existing ancestor mode = %v, want directory", info.Mode())
		}
		unrelatedInfo, err := os.Stat(unrelated)
		if err != nil {
			t.Fatalf("unrelated directory was removed: %v", err)
		}
		if !unrelatedInfo.IsDir() {
			t.Errorf("unrelated path mode = %v, want directory", unrelatedInfo.Mode())
		}
		assertDirectoryEmptyByWalk(t, unrelated)
	})

	t.Run("C17d_outward_symlink_refuses_prune_and_preserves_outside", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		if err := os.Mkdir(filepath.Join(outside, "B"), 0o755); err != nil {
			t.Fatalf("create outside empty directory: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "A")); err != nil {
			t.Fatalf("create outward symlink: %v", err)
		}
		writer := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, writer)
		rootBefore := snapshotDirectory(t, root)
		outsideBefore := snapshotDirectory(t, outside)

		err := writer.PruneEmptyDirs("A/B/track.flac")
		if err == nil {
			t.Fatal("PruneEmptyDirs through an outward symlink error = nil, want rejection")
		}
		assertDirectorySnapshot(t, root, rootBefore)
		assertDirectorySnapshot(t, outside, outsideBefore)
	})

	t.Run("C17e_inward_symlink_refuses_prune", func(t *testing.T) {
		root, inside, writer := newInwardSymlinkFixture(t)
		if err := os.Mkdir(filepath.Join(inside, "B"), 0o755); err != nil {
			t.Fatalf("create inward empty directory: %v", err)
		}
		before := snapshotDirectory(t, root)

		err := writer.PruneEmptyDirs("Artist/B/track.flac")
		if err == nil {
			t.Fatal("PruneEmptyDirs through an inward symlink error = nil, want rejection")
		}
		assertDirectorySnapshot(t, root, before)
	})

	t.Run("C17f_already_gone_directories_are_a_noop", func(t *testing.T) {
		root := t.TempDir()
		writer := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, writer)

		if err := writer.PruneEmptyDirs("A/B/track.flac"); err != nil {
			t.Errorf("PruneEmptyDirs for absent directories = %v, want nil", err)
		}
		assertDirectoryEmptyByWalk(t, root)
	})

	invalidPaths := []struct {
		name    string
		relPath string
	}{
		{"traversal", "A/../B/track.flac"},
		{"absolute", "/A/B/track.flac"},
		{"empty", ""},
	}
	for _, tt := range invalidPaths {
		t.Run("C17g_refuses_"+tt.name+"_path", func(t *testing.T) {
			root := t.TempDir()
			writer := mustOpenSectionWriter(t, root)
			closeSectionWriterAtCleanup(t, writer)
			before := snapshotDirectory(t, root)

			if err := writer.PruneEmptyDirs(tt.relPath); err == nil {
				t.Fatalf("PruneEmptyDirs(%q) error = nil, want rejection", tt.relPath)
			}
			assertDirectorySnapshot(t, root, before)
		})
	}

	t.Run("C17h_after_Close_returns_error_without_mutating_a_recycled_root", func(t *testing.T) {
		originalRoot := t.TempDir()
		if err := os.MkdirAll(filepath.Join(originalRoot, "A", "B"), 0o755); err != nil {
			t.Fatalf("create original empty directories: %v", err)
		}
		closedWriter := mustOpenSectionWriter(t, originalRoot)
		if err := closedWriter.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		recycledRoot := t.TempDir()
		if err := os.MkdirAll(filepath.Join(recycledRoot, "A", "B"), 0o755); err != nil {
			t.Fatalf("create recycled-root empty directories: %v", err)
		}
		replacement := mustOpenSectionWriter(t, recycledRoot)
		closeSectionWriterAtCleanup(t, replacement)
		originalBefore := snapshotDirectory(t, originalRoot)
		recycledBefore := snapshotDirectory(t, recycledRoot)

		if err := closedWriter.PruneEmptyDirs("A/B/track.flac"); err == nil {
			t.Fatal("PruneEmptyDirs after Close error = nil, want error")
		}
		assertDirectorySnapshot(t, originalRoot, originalBefore)
		assertDirectorySnapshot(t, recycledRoot, recycledBefore)
	})
}

func mustOpenSectionWriter(t *testing.T, root string) *SectionWriter {
	t.Helper()
	writer, err := OpenSectionWriter(root)
	if err != nil {
		t.Fatalf("OpenSectionWriter(%q): %v", root, err)
	}
	return writer
}

func closeSectionWriterAtCleanup(t *testing.T, writer *SectionWriter) {
	t.Helper()
	t.Cleanup(func() {
		if err := writer.Close(); err != nil {
			t.Errorf("Close SectionWriter: %v", err)
		}
	})
}

func assertErrorIs(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("error = %v, want errors.Is(_, %v)", got, want)
	}
}

func assertFileContent(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("content of %q = %q, want %q", path, got, want)
	}
}

type containmentTreeEntry struct {
	Path       string
	Mode       os.FileMode
	LinkTarget string
	Content    string
}

func snapshotDirectory(t *testing.T, root string) []containmentTreeEntry {
	t.Helper()
	var entries []containmentTreeEntry
	err := filepath.WalkDir(root, func(path string, dirEntry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		info, err := dirEntry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entry := containmentTreeEntry{Path: rel, Mode: info.Mode()}
		if info.Mode()&os.ModeSymlink != 0 {
			entry.LinkTarget, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entry.Content = string(data)
		}
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %q: %v", root, err)
	}
	return entries
}

func assertDirectoryEmptyByWalk(t *testing.T, root string) {
	t.Helper()
	if entries := snapshotDirectory(t, root); len(entries) != 0 {
		t.Errorf("directory %q is not empty after rejected operation: %#v", root, entries)
	}
}

func assertDirectorySnapshot(t *testing.T, root string, want []containmentTreeEntry) {
	t.Helper()
	got := snapshotDirectory(t, root)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("directory %q changed:\n want: %#v\n  got: %#v", root, want, got)
	}
}

func newInwardSymlinkFixture(t *testing.T) (root, inside string, writer *SectionWriter) {
	t.Helper()
	root = t.TempDir()
	inside = filepath.Join(root, "Inside")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatalf("create inward symlink target: %v", err)
	}
	if err := os.Symlink("Inside", filepath.Join(root, "Artist")); err != nil {
		t.Fatalf("create inward symlink: %v", err)
	}
	writer = mustOpenSectionWriter(t, root)
	closeSectionWriterAtCleanup(t, writer)
	return root, inside, writer
}

// H5: the path is re-resolved from the section root on every operation, so an
// ancestor that has been moved out of the section is not followed: the old path no
// longer resolves and the moved tree stays untouched. The mid-walk window (a rename
// landing between two resolution steps) is closed by construction on Linux, since
// each step resolves the accumulated path from the root instead of chaining the
// previous component's descriptor.
func TestSectionWriterDoesNotFollowAMovedAncestor_H5(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writer := mustOpenSectionWriter(t, root)
	closeSectionWriterAtCleanup(t, writer)

	relPath := "Artist/Album/Track_01234567.flac"
	if err := writer.WriteStaged(relPath, []byte("staged bytes")); err != nil {
		t.Fatalf("WriteStaged(%q): %v", relPath, err)
	}
	if err := os.Rename(filepath.Join(root, "Artist"), filepath.Join(outside, "Artist")); err != nil {
		t.Fatalf("move the ancestor out of the section: %v", err)
	}

	if err := writer.RemoveStaged(relPath); err != nil {
		t.Fatalf("RemoveStaged(%q) after the move: %v", relPath, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "Artist", "Album", "Track_01234567.flac")); err != nil {
		t.Fatalf("file under the moved ancestor was touched: %v", err)
	}

	if err := writer.PruneEmptyDirs(relPath); err != nil {
		t.Fatalf("PruneEmptyDirs(%q) after the move: %v", relPath, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "Artist", "Album")); err != nil {
		t.Fatalf("directory under the moved ancestor was pruned: %v", err)
	}
}

// H5 follow-up: the create and publish paths must re-anchor to the root too, not just
// the remove and prune paths the first H5 test exercises.
func TestSectionWriterCreateAndPublishAfterMovedAncestor_H5(t *testing.T) {
	t.Run("H5b_create_after_a_moved_ancestor_rebuilds_in_section", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "Artist"), 0o755); err != nil {
			t.Fatalf("create the ancestor: %v", err)
		}
		writer := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, writer)
		if err := os.Rename(filepath.Join(root, "Artist"), filepath.Join(outside, "Artist")); err != nil {
			t.Fatalf("move the ancestor out of the section: %v", err)
		}

		rel := "Artist/Album/Track_01234567.flac"
		if _, err := writer.WriteStagedIdentity(rel, []byte("staged")); err != nil {
			t.Fatalf("WriteStagedIdentity(%q) after the move: %v", rel, err)
		}
		// The in-section tree is rebuilt from the root; the moved one gains nothing.
		assertFileContent(t, filepath.Join(root, filepath.FromSlash(rel)), []byte("staged"))
		if _, err := os.Stat(filepath.Join(outside, "Artist", "Album")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("moved tree gained directories: lstat error = %v, want os.ErrNotExist", err)
		}
	})

	t.Run("H5c_publish_after_a_moved_ancestor_touches_nothing", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		writer := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, writer)
		stagedRel := "Artist/Album/.tiramisu-txn-0"
		if err := writer.WriteStaged(stagedRel, []byte("staged")); err != nil {
			t.Fatalf("WriteStaged(%q): %v", stagedRel, err)
		}
		if err := os.Rename(filepath.Join(root, "Artist"), filepath.Join(outside, "Artist")); err != nil {
			t.Fatalf("move the ancestor out of the section: %v", err)
		}

		finalRel := "Artist/Album/Track_01234567.flac"
		if err := writer.Publish(stagedRel, finalRel); err == nil {
			t.Fatal("Publish after the move = nil, want a resolution failure")
		}
		// The moved staged file survives, and no destination appears in the section.
		assertFileContent(t, filepath.Join(outside, "Artist", "Album", ".tiramisu-txn-0"), []byte("staged"))
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(finalRel))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("destination appeared in the section: lstat error = %v, want os.ErrNotExist", err)
		}
	})
}

// H5 follow-up: the portable fallback promises to reproduce the Linux surface and its
// error values, so the two implementations must agree on these inputs.
func TestSectionWriterErrorParity_H5(t *testing.T) {
	t.Run("P1_remove_staged_removes_a_final_symlink_without_following_it", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		target := filepath.Join(outside, "target")
		if err := os.WriteFile(target, []byte("outside"), 0o644); err != nil {
			t.Fatalf("write the symlink target: %v", err)
		}
		if err := os.Mkdir(filepath.Join(root, "A"), 0o755); err != nil {
			t.Fatalf("create the parent: %v", err)
		}
		if err := os.Symlink(target, filepath.Join(root, "A", "link.flac")); err != nil {
			t.Fatalf("create the final symlink: %v", err)
		}
		writer := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, writer)

		if err := writer.RemoveStaged("A/link.flac"); err != nil {
			t.Fatalf("RemoveStaged on a final symlink: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(root, "A", "link.flac")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("symlink lstat error = %v, want os.ErrNotExist", err)
		}
		assertFileContent(t, target, []byte("outside"))
	})

	t.Run("P2_a_file_where_a_directory_belongs_is_ENOTDIR", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "A"), []byte("file"), 0o644); err != nil {
			t.Fatalf("create the file component: %v", err)
		}
		writer := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, writer)
		if _, err := writer.WriteStagedIdentity("A/B/track.flac", []byte("data")); !errors.Is(err, syscall.ENOTDIR) {
			t.Fatalf("WriteStagedIdentity through a file = %v, want ENOTDIR", err)
		}
	})

	t.Run("P3_an_existing_destination_directory_is_a_conflict", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "A"), 0o755); err != nil {
			t.Fatalf("create the destination directory: %v", err)
		}
		writer := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, writer)
		if _, err := writer.WriteStagedIdentity("A", []byte("data")); !errors.Is(err, ErrDestinationExists) {
			t.Fatalf("WriteStagedIdentity onto a directory = %v, want ErrDestinationExists", err)
		}
	})
}

// H7: a failed write must not leave a hidden file nothing owns. The function created
// the object, so it removes it before returning the error.
func TestSectionWriterWriteStagedRemovesOnFailure_H7(t *testing.T) {
	root := t.TempDir()
	writer := mustOpenSectionWriter(t, root)
	closeSectionWriterAtCleanup(t, writer)

	original := stagedFileSync
	stagedFileSync = func(*os.File) error { return errors.New("sync failed") }
	defer func() { stagedFileSync = original }()

	rel := "A/track.flac"
	if _, err := writer.WriteStagedIdentity(rel, []byte("data")); err == nil {
		t.Fatal("WriteStagedIdentity() error = nil, want the injected sync failure")
	}
	if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("failed write left %q behind: lstat error = %v, want os.ErrNotExist", rel, err)
	}
}

// H6: a rollback unlinks a name only while it still holds the object the request
// created, so a replacement that has taken the name survives.
func TestSectionWriterIdentityGuardsRollback_H6(t *testing.T) {
	t.Run("H6a_remove_if_identity_refuses_a_replacement", func(t *testing.T) {
		root := t.TempDir()
		writer := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, writer)
		rel := "A/track.flac"
		id, err := writer.WriteStagedIdentity(rel, []byte("ours"))
		if err != nil {
			t.Fatalf("WriteStagedIdentity(%q): %v", rel, err)
		}
		// The replacement is created while the original still exists, so it cannot
		// reuse its inode: the identity check has a deterministic mismatch.
		replacementPath := filepath.Join(root, "replacement.tmp")
		replacement := []byte("someone else's file")
		if err := os.WriteFile(replacementPath, replacement, 0o644); err != nil {
			t.Fatalf("write replacement: %v", err)
		}
		if err := os.Rename(replacementPath, filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("replace the staged name: %v", err)
		}

		removed, err := writer.RemoveStagedIfIdentity(rel, id)
		if err != nil {
			t.Fatalf("RemoveStagedIfIdentity(%q): %v", rel, err)
		}
		if removed {
			t.Error("RemoveStagedIfIdentity removed a replacement that is not this request's object")
		}
		assertFileContent(t, filepath.Join(root, filepath.FromSlash(rel)), replacement)
	})

	t.Run("H6b_remove_if_identity_removes_its_own_object", func(t *testing.T) {
		root := t.TempDir()
		writer := mustOpenSectionWriter(t, root)
		closeSectionWriterAtCleanup(t, writer)
		rel := "A/track.flac"
		id, err := writer.WriteStagedIdentity(rel, []byte("ours"))
		if err != nil {
			t.Fatalf("WriteStagedIdentity(%q): %v", rel, err)
		}
		removed, err := writer.RemoveStagedIfIdentity(rel, id)
		if err != nil || !removed {
			t.Fatalf("RemoveStagedIfIdentity(%q) = (%v, %v), want (true, nil)", rel, removed, err)
		}
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("object lstat error = %v, want os.ErrNotExist", err)
		}
	})
}
