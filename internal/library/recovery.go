package library

import (
	"fmt"
	"log"
	"path/filepath"

	"tiramisu/internal/metadb"
)

// AudioRecoverySource is the registry subset recovery needs: the rows an interrupted
// mutation left behind, the rollback that removes a staged transaction, and the delete
// that finishes an interrupted removal.
type AudioRecoverySource interface {
	AudioProjectionsByState(state metadb.AudioProjectionState) ([]metadb.AudioProjection, error)
	RollbackAudioProjections(txnID string) (int, error)
	DeleteAudioProjection(section, virtualPath string) error
}

// RecoverStagedAudioTransactions rolls back transactions that crashed between the final
// rename and the registry commit. Without it the final names are on disk while every row
// is still staged: startup publishes only committed rows, RENAME_NOREPLACE turns each
// retry into a 409 against a file no row owns, and Remove has no row to delete, so the
// path is unrecoverable through the API.
//
// The filesystem work happens before the registry rollback: a crash during recovery must
// leave the rows that prove what the files belong to. Only names derivable from a row are
// touched, never a file that merely looks like audio.
func RecoverStagedAudioTransactions(src AudioRecoverySource, sourcePath string, logger *log.Logger) (int, error) {
	rows, err := src.AudioProjectionsByState(metadb.AudioStaged)
	if err != nil {
		return 0, fmt.Errorf("cannot list staged audio transactions: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	byTxn := make(map[string][]metadb.AudioProjection)
	var order []string
	for _, row := range rows {
		if row.TxnID == "" {
			continue
		}
		if _, seen := byTxn[row.TxnID]; !seen {
			order = append(order, row.TxnID)
		}
		byTxn[row.TxnID] = append(byTxn[row.TxnID], row)
	}

	recovered := 0
	var firstErr error
	for _, txnID := range order {
		if err := recoverStagedTransaction(src, sourcePath, txnID, byTxn[txnID], logger); err != nil {
			if logger != nil {
				logger.Printf("Audio recovery: transaction %s left staged: %v", txnID, err)
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		recovered++
	}
	return recovered, firstErr
}

// recoverStagedTransaction removes one transaction's names through a SectionWriter, so
// containment stays anchored to the section root, and only then drops its rows.
func recoverStagedTransaction(src AudioRecoverySource, sourcePath, txnID string, rows []metadb.AudioProjection, logger *log.Logger) error {
	writers := make(map[string]*SectionWriter)
	defer closeSectionWriters(writers)
	writerFor := func(section string) (*SectionWriter, error) {
		return sectionWriterMemo(writers, sourcePath, section)
	}

	for _, row := range rows {
		writer, err := writerFor(row.Section)
		if err != nil {
			return err
		}
		// The transaction never reached commit, so no dispatcher ever served these
		// names: neither can belong to a committed projection.
		if err := writer.RemoveStaged(row.VirtualPath); err != nil {
			return fmt.Errorf("remove %s: %w", row.VirtualPath, err)
		}
		if row.StagingName != "" {
			if err := writer.RemoveStaged(stagingRelPath(row)); err != nil {
				return fmt.Errorf("remove the staging name of %s: %w", row.VirtualPath, err)
			}
		}
	}
	for _, row := range rows {
		writer := writers[row.Section]
		if writer == nil {
			continue
		}
		// Best effort and cosmetic: an empty directory left behind is harmless, and
		// a failed prune must not keep the rows staged a second time.
		if err := writer.PruneEmptyDirs(row.VirtualPath); err != nil && logger != nil {
			logger.Printf("Audio recovery: cannot prune directories for %s: %v", row.VirtualPath, err)
		}
	}
	if _, err := src.RollbackAudioProjections(txnID); err != nil {
		return fmt.Errorf("roll back transaction %s: %w", txnID, err)
	}
	if logger != nil {
		logger.Printf("Audio recovery: rolled back transaction %s after removing %d projection(s)", txnID, len(rows))
	}
	return nil
}

// RecoverRemovingAudioProjections finishes removals that crashed between the removal
// mark and the registry delete. A removing row is already unpublished, so the sweep only
// has to take the leftover stub away and drop the row. Without it the row stays in place
// forever: it keeps the virtual path and (hash, file_index) reserved, keeps counting as a
// torrent reference, and no request can complete it because the namespace stopped
// answering for the path when the claim was recorded.
func RecoverRemovingAudioProjections(src AudioRecoverySource, sourcePath string, logger *log.Logger) (int, error) {
	rows, err := src.AudioProjectionsByState(metadb.AudioRemoving)
	if err != nil {
		return 0, fmt.Errorf("cannot list interrupted audio removals: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	writers := make(map[string]*SectionWriter)
	defer closeSectionWriters(writers)

	recovered := 0
	var firstErr error
	for _, row := range rows {
		writer, err := sectionWriterMemo(writers, sourcePath, row.Section)
		if err == nil {
			// The row itself is the claim: the projection was marked for removal and
			// the namespace already stopped serving it before the crash.
			if err = writer.RemoveStaged(row.VirtualPath); err == nil {
				if prErr := writer.PruneEmptyDirs(row.VirtualPath); prErr != nil && logger != nil {
					logger.Printf("Audio recovery: cannot prune directories for %s: %v", row.VirtualPath, prErr)
				}
				err = src.DeleteAudioProjection(row.Section, row.VirtualPath)
			}
		}
		if err != nil {
			if logger != nil {
				logger.Printf("Audio recovery: removal of %s/%s left for the next boot: %v", row.Section, row.VirtualPath, err)
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		recovered++
	}
	return recovered, firstErr
}

// sectionWriterMemo opens one writer per section root and reuses it across rows.
func sectionWriterMemo(memo map[string]*SectionWriter, sourcePath, section string) (*SectionWriter, error) {
	if writer, ok := memo[section]; ok {
		return writer, nil
	}
	writer, err := OpenSectionWriter(filepath.Join(sourcePath, section))
	if err != nil {
		return nil, fmt.Errorf("open section %q: %w", section, err)
	}
	memo[section] = writer
	return writer, nil
}

func closeSectionWriters(memo map[string]*SectionWriter) {
	for _, writer := range memo {
		_ = writer.Close()
	}
}
