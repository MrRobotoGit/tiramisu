//go:build linux

package library

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// resolveBeneath refuses both an escape and any symlink on the way, so a path
// that passed string validation still cannot be redirected by one planted later.
const resolveBeneath = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS

// beneathErr keeps a refusal by the kernel distinguishable from an ordinary
// filesystem error: ELOOP and EXDEV are how openat2 reports a blocked escape, and
// EAGAIN is the ".." race it could not rule out (unreachable while checkRel rejects
// "..", mapped anyway so a relaxed checkRel cannot leak a raw errno).
func beneathErr(err error, rel string) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EAGAIN) {
		return fmt.Errorf("%w: %s", ErrPathEscapesSection, rel)
	}
	return err
}

// openDirAt resolves a directory path against the section root. Without create it is
// a single openat2, so a rename after resolution moves the descriptor with its own
// directory but cannot redirect the walk. With create, a missing chain is built one
// component at a time and every step re-resolves the accumulated path from the root -
// never from the previous component's descriptor, which a rename could carry outside
// the section. The remaining windows are one per syscall, not one in total: each new
// component adds a resolution-to-Mkdirat window, and its parent descriptor exists only
// for that single step.
func (w *SectionWriter) openDirAt(rel string, create bool) (int, error) {
	open := func(path string) (int, error) {
		return unix.Openat2(int(w.root.Fd()), path, &unix.OpenHow{
			Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: resolveBeneath,
		})
	}
	fd, err := open(rel)
	if err == nil {
		return fd, nil
	}
	if !create || !errors.Is(err, unix.ENOENT) {
		return -1, beneathErr(err, rel)
	}
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		acc := strings.Join(parts[:i+1], "/")
		fd, err := open(acc)
		if err == nil {
			unix.Close(fd)
			continue
		}
		if !errors.Is(err, unix.ENOENT) {
			return -1, beneathErr(err, acc)
		}
		parentRel := strings.Join(parts[:i], "/")
		if parentRel == "" {
			parentRel = "."
		}
		parent, err := open(parentRel)
		if err != nil {
			return -1, beneathErr(err, parentRel)
		}
		err = unix.Mkdirat(parent, part, 0o755)
		unix.Close(parent)
		if err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, beneathErr(err, acc)
		}
		if err == nil {
			w.recordCreatedDir(acc)
		}
	}
	fd, err = open(rel)
	if err != nil {
		return -1, beneathErr(err, rel)
	}
	return fd, nil
}

// openParent returns the parent directory descriptor and the final component for rel.
// The parent is resolved through openDirAt, so the path stays anchored to the section
// root instead of becoming a new root descriptor at every component.
func (w *SectionWriter) openParent(rel string, create bool) (int, string, error) {
	parts := strings.Split(rel, "/")
	leaf := parts[len(parts)-1]
	dir := strings.Join(parts[:len(parts)-1], "/")
	if dir == "" {
		dir = "."
	}
	fd, err := w.openDirAt(dir, create)
	if err != nil {
		return -1, "", err
	}
	return fd, leaf, nil
}

// WriteStaged creates rel beneath the root exclusively: an existing file is a
// conflict, never a truncation.
func (w *SectionWriter) WriteStaged(rel string, data []byte) error {
	_, err := w.WriteStagedIdentity(rel, data)
	return err
}

// WriteStagedIdentity is WriteStaged plus the identity of the object it created, which
// a rollback needs to unlink the right file later.
func (w *SectionWriter) WriteStagedIdentity(rel string, data []byte) (id FileIdentity, err error) {
	if err := w.usable(); err != nil {
		return FileIdentity{}, err
	}
	if err := checkRel(rel); err != nil {
		return FileIdentity{}, err
	}
	dirfd, leaf, err := w.openParent(rel, true)
	if err != nil {
		return FileIdentity{}, err
	}
	defer unix.Close(dirfd)

	fd, err := unix.Openat2(dirfd, leaf, &unix.OpenHow{
		Flags: unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_CLOEXEC,
		Mode:  0o644, Resolve: resolveBeneath,
	})
	if err != nil {
		if errors.Is(err, unix.EEXIST) {
			return FileIdentity{}, fmt.Errorf("%w: %s", ErrDestinationExists, rel)
		}
		return FileIdentity{}, beneathErr(err, rel)
	}
	// From here the object exists and belongs to this call: a failure below must not
	// leave behind a name nothing owns.
	defer func() {
		if err != nil {
			_ = unix.Unlinkat(dirfd, leaf, 0)
		}
	}()
	file := os.NewFile(uintptr(fd), rel)
	defer file.Close()
	if _, err = file.Write(data); err != nil {
		return FileIdentity{}, err
	}
	if err = stagedFileSync(file); err != nil {
		return FileIdentity{}, err
	}
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		return FileIdentity{}, beneathErr(err, rel)
	}
	return FileIdentity{Dev: uint64(st.Dev), Ino: st.Ino}, nil
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
	// Resolve the source once to fail before any final directory is created, then
	// resolve it again immediately before the rename: holding the first descriptor
	// across the final chain's creation would widen its rename window from one
	// syscall to the whole walk.
	stagedDir, stagedLeaf, err := w.openParent(stagedRel, false)
	if err != nil {
		return err
	}
	unix.Close(stagedDir)
	finalDir, finalLeaf, err := w.openParent(finalRel, true)
	if err != nil {
		return err
	}
	defer unix.Close(finalDir)
	stagedDir, stagedLeaf, err = w.openParent(stagedRel, false)
	if err != nil {
		return err
	}
	defer unix.Close(stagedDir)

	if err := unix.Renameat2(stagedDir, stagedLeaf, finalDir, finalLeaf, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("%w: %s", ErrDestinationExists, finalRel)
		}
		return beneathErr(err, finalRel)
	}
	// Durability: without a directory fsync a power loss can leave a committed row whose
	// directory entry never reached the disk. Both ends of the rename need it: the
	// source lost a name and the destination gained one.
	if err := unix.Fsync(finalDir); err != nil {
		return beneathErr(err, finalRel)
	}
	if err := unix.Fsync(stagedDir); err != nil {
		return beneathErr(err, stagedRel)
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

// Identity reports rel's device/inode pair without following a symlink.
func (w *SectionWriter) Identity(rel string) (FileIdentity, error) {
	if err := w.usable(); err != nil {
		return FileIdentity{}, err
	}
	if err := checkRel(rel); err != nil {
		return FileIdentity{}, err
	}
	dirfd, leaf, err := w.openParent(rel, false)
	if err != nil {
		return FileIdentity{}, err
	}
	defer unix.Close(dirfd)
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, leaf, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return FileIdentity{}, beneathErr(err, rel)
	}
	return FileIdentity{Dev: uint64(st.Dev), Ino: st.Ino}, nil
}

// RemoveStagedIfIdentity unlinks rel only while it still holds want, so a name reused
// by another object is left alone. Reports whether the object was removed.
func (w *SectionWriter) RemoveStagedIfIdentity(rel string, want FileIdentity) (bool, error) {
	if err := w.usable(); err != nil {
		return false, err
	}
	if err := checkRel(rel); err != nil {
		return false, err
	}
	dirfd, leaf, err := w.openParent(rel, false)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, err
	}
	defer unix.Close(dirfd)
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, leaf, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, beneathErr(err, rel)
	}
	if uint64(st.Dev) != want.Dev || st.Ino != want.Ino {
		return false, nil
	}
	if err := unix.Unlinkat(dirfd, leaf, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return false, beneathErr(err, rel)
	}
	return true, nil
}

// removeDirIfEmpty removes one directory this writer created; a directory that is not
// empty (or already gone) is left in place, and its ancestors cannot be empty either.
func (w *SectionWriter) removeDirIfEmpty(rel string) error {
	parent, leaf, err := w.openParent(rel, false)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	defer unix.Close(parent)
	err = unix.Unlinkat(parent, leaf, unix.AT_REMOVEDIR)
	if err != nil && !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.ENOTEMPTY) && !errors.Is(err, unix.EEXIST) {
		return beneathErr(err, rel)
	}
	return nil
}

// PruneEmptyDirs removes now-empty directories from relPath's parent up to the root,
// regardless of who created them. Production rollback uses PruneCreatedDirs instead:
// this blind walk is kept for the primitives it exercises.
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
