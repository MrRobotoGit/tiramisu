//go:build !linux

package library

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Portable SectionWriter for platforms without openat2 (macOS during
// development). It reproduces the Linux surface and its error values, but not
// its safety model: containment here is a per-component Lstat check, so a
// symlink planted between the check and the operation is not caught. That race
// is exactly what RESOLVE_BENEATH removes on Linux.
//
// This is acceptable only because production is Linux — the Pi and the Docker
// image — and this file never ships there. It exists so `go build`, `go vet`
// and `go test` work on a developer machine, which the project workflow
// depends on.

// resolveUnder mirrors the Linux openDirAt/openParent pair: every component is
// re-joined from the section root (never chained from the previously resolved
// component), missing intermediates are created one at a time, and a symlink
// anywhere on the way is refused. The check-to-use race between the Lstat and the
// operation stays open - only openat2's RESOLVE_BENEATH closes it - which is why
// this file never ships to production.
func (w *SectionWriter) resolveUnder(rel string, createDirs bool) (string, error) {
	parts := strings.Split(rel, "/")
	for i := range parts {
		acc := strings.Join(parts[:i+1], "/")
		next := filepath.Join(w.rootPath, filepath.FromSlash(acc))
		last := i == len(parts)-1
		info, err := os.Lstat(next)
		switch {
		case err == nil:
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("%w: %s", ErrPathEscapesSection, rel)
			}
			if !last && !info.IsDir() {
				return "", fmt.Errorf("%w: %s", ErrPathEscapesSection, rel)
			}
		case errors.Is(err, os.ErrNotExist):
			if !last && createDirs {
				if err := os.Mkdir(next, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
					return "", err
				}
			} else if !last {
				return "", err
			}
		default:
			return "", err
		}
	}
	return filepath.Join(w.rootPath, filepath.FromSlash(rel)), nil
}

// WriteStaged creates rel beneath the root exclusively: an existing file is a
// conflict, never a truncation.
func (w *SectionWriter) WriteStaged(rel string, data []byte) error {
	if err := w.usable(); err != nil {
		return err
	}
	if err := checkRel(rel); err != nil {
		return err
	}
	path, err := w.resolveUnder(rel, true)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", ErrDestinationExists, rel)
		}
		return err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

// Publish renames stagedRel onto finalRel without replacing anything already
// there. Without RENAME_NOREPLACE the check and the rename are two steps, so
// the refusal is advisory rather than atomic.
func (w *SectionWriter) Publish(stagedRel, finalRel string) error {
	if err := w.usable(); err != nil {
		return err
	}
	if err := checkRel(stagedRel); err != nil {
		return err
	}
	if err := checkRel(finalRel); err != nil {
		return err
	}
	staged, err := w.resolveUnder(stagedRel, false)
	if err != nil {
		return err
	}
	final, err := w.resolveUnder(finalRel, true)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(final); err == nil {
		return fmt.Errorf("%w: %s", ErrDestinationExists, finalRel)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(staged, final)
}

// RemoveStaged deletes rel beneath the root.
func (w *SectionWriter) RemoveStaged(rel string) error {
	if err := w.usable(); err != nil {
		return err
	}
	if err := checkRel(rel); err != nil {
		return err
	}
	path, err := w.resolveUnder(rel, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// PruneEmptyDirs removes now-empty directories from relPath's parent up to the
// root. Remove refuses a non-empty directory, so nothing in use is lost.
func (w *SectionWriter) PruneEmptyDirs(relPath string) error {
	if err := w.usable(); err != nil {
		return err
	}
	if err := checkRel(relPath); err != nil {
		return err
	}
	parts := strings.Split(relPath, "/")
	for i := len(parts) - 1; i > 0; i-- {
		dir := strings.Join(parts[:i], "/")
		path, err := w.resolveUnder(dir, false)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if err := os.Remove(path); err != nil {
			// A non-empty directory stops the walk: an ancestor of a directory
			// still in use cannot be empty either.
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if pe, ok := err.(*os.PathError); ok && isNotEmpty(pe.Err) {
				return nil
			}
			return err
		}
	}
	return nil
}

// isNotEmpty reports the ENOTEMPTY/EEXIST pair that rmdir uses for a directory
// that still has entries; the two differ by platform.
func isNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}
