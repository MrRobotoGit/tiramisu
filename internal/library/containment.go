package library

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

var (
	// ErrPathEscapesSection means the path did not resolve beneath the root.
	ErrPathEscapesSection = errors.New("library: path escapes the section root")
	// ErrDestinationExists means something already occupies the destination.
	ErrDestinationExists = errors.New("library: destination already exists")
)

// resolveBeneath refuses both an escape and any symlink on the way, so a path
// that passed string validation still cannot be redirected by one planted later.
const resolveBeneath = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS

// SectionWriter anchors every mutation to a pre-opened section root: spec §14.3
// requires the parents be re-resolved by the kernel on each operation.
type SectionWriter struct {
	root *os.File
}

// OpenSectionWriter pins sectionRoot. The root itself is operator configuration
// and is resolved once; nothing beneath it may escape afterwards.
func OpenSectionWriter(sectionRoot string) (*SectionWriter, error) {
	root, err := os.Open(sectionRoot)
	if err != nil {
		return nil, err
	}
	info, err := root.Stat()
	if err != nil {
		root.Close()
		return nil, err
	}
	if !info.IsDir() {
		root.Close()
		return nil, fmt.Errorf("library: section root %q is not a directory", sectionRoot)
	}
	return &SectionWriter{root: root}, nil
}

func (w *SectionWriter) Close() error {
	if w == nil || w.root == nil {
		return nil
	}
	err := w.root.Close()
	w.root = nil
	return err
}

func (w *SectionWriter) usable() error {
	if w == nil || w.root == nil {
		return errors.New("library: section writer is closed")
	}
	return nil
}

// beneathErr keeps a refusal by the kernel distinguishable from an ordinary
// filesystem error: ELOOP and EXDEV are how openat2 reports a blocked escape.
func beneathErr(err error, rel string) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EXDEV) {
		return fmt.Errorf("%w: %s", ErrPathEscapesSection, rel)
	}
	return err
}

func checkRel(rel string) error {
	if rel == "" || strings.HasPrefix(rel, "/") {
		return fmt.Errorf("%w: %q", ErrPathEscapesSection, rel)
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%w: %q", ErrPathEscapesSection, rel)
		}
	}
	return nil
}

// openParent walks rel's directories from the root fd, optionally creating them,
// and returns a descriptor for the parent plus the final component.
func (w *SectionWriter) openParent(rel string, create bool) (int, string, error) {
	parts := strings.Split(rel, "/")
	leaf := parts[len(parts)-1]
	cur, err := unix.Openat2(int(w.root.Fd()), ".", &unix.OpenHow{
		Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: resolveBeneath,
	})
	if err != nil {
		return -1, "", beneathErr(err, rel)
	}
	for _, part := range parts[:len(parts)-1] {
		if create {
			if err := unix.Mkdirat(cur, part, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
				unix.Close(cur)
				return -1, "", beneathErr(err, rel)
			}
		}
		next, err := unix.Openat2(cur, part, &unix.OpenHow{
			Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: resolveBeneath,
		})
		unix.Close(cur)
		if err != nil {
			return -1, "", beneathErr(err, rel)
		}
		cur = next
	}
	return cur, leaf, nil
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
	dirfd, leaf, err := w.openParent(rel, true)
	if err != nil {
		return err
	}
	defer unix.Close(dirfd)

	fd, err := unix.Openat2(dirfd, leaf, &unix.OpenHow{
		Flags: unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_CLOEXEC,
		Mode:  0o644, Resolve: resolveBeneath,
	})
	if err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("%w: %s", ErrDestinationExists, rel)
		}
		return beneathErr(err, rel)
	}
	file := os.NewFile(uintptr(fd), rel)
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

// Publish renames stagedRel onto finalRel without replacing anything already
// there, so an unregistered file is a conflict the caller sees.
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
	stagedDir, stagedLeaf, err := w.openParent(stagedRel, false)
	if err != nil {
		return err
	}
	defer unix.Close(stagedDir)
	finalDir, finalLeaf, err := w.openParent(finalRel, true)
	if err != nil {
		return err
	}
	defer unix.Close(finalDir)

	if err := unix.Renameat2(stagedDir, stagedLeaf, finalDir, finalLeaf, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("%w: %s", ErrDestinationExists, finalRel)
		}
		return beneathErr(err, finalRel)
	}
	return nil
}

// RemoveStaged deletes rel beneath the root. A rollback that followed a symlink
// would delete a file outside the section, so the walk refuses one.
func (w *SectionWriter) RemoveStaged(rel string) error {
	if err := w.usable(); err != nil {
		return err
	}
	if err := checkRel(rel); err != nil {
		return err
	}
	dirfd, leaf, err := w.openParent(rel, false)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	defer unix.Close(dirfd)
	if err := unix.Unlinkat(dirfd, leaf, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return beneathErr(err, rel)
	}
	return nil
}

// PruneEmptyDirs removes now-empty directories from relPath's parent up to the
// root. rmdir cannot remove a non-empty one, so nothing in use is lost.
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
		parent, leaf, err := w.openParent(dir, false)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return err
		}
		err = unix.Unlinkat(parent, leaf, unix.AT_REMOVEDIR)
		unix.Close(parent)
		if err != nil {
			// ENOTEMPTY stops the walk: an ancestor of a directory still in use
			// cannot be empty either.
			if errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ENOENT) {
				return nil
			}
			return beneathErr(err, dir)
		}
	}
	return nil
}
