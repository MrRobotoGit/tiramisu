package library

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var (
	// ErrPathEscapesSection means the path did not resolve beneath the root.
	ErrPathEscapesSection = errors.New("library: path escapes the section root")
	// ErrDestinationExists means something already occupies the destination.
	ErrDestinationExists = errors.New("library: destination already exists")
)

// FileIdentity is an object's device/inode pair, captured when the writer creates the
// object. A rollback unlinks a name only while it still holds this identity, so a name
// reused by anything else survives.
type FileIdentity struct {
	Dev uint64
	Ino uint64
}

// stagedFileSync is the injection seam the cleanup test uses to fail a write after the
// file exists; production always calls Sync directly.
var stagedFileSync = func(f *os.File) error { return f.Sync() }

// SectionWriter anchors every mutation to a pre-opened section root: spec §14.3
// requires the parents be re-resolved by the kernel on each operation.
//
// The mutating methods are per-platform. On Linux they use openat2 with
// RESOLVE_BENEATH, which makes containment a kernel guarantee and immune to a
// symlink planted between check and use. Elsewhere (developer machines) the
// same surface is reimplemented in portable Go, which checks each component
// instead and is therefore TOCTOU-racy by construction. Production is Linux;
// the fallback exists so the package builds and its tests run on macOS.
type SectionWriter struct {
	root *os.File
	// rootPath is the section root with symlinks resolved at open time. The
	// Linux path never reads it - its fd already pins the resolved inode - but
	// the portable fallback has no fd-relative syscalls and needs somewhere to
	// anchor, and re-resolving per call would follow a root symlink repointed
	// after the writer was opened.
	rootPath string
	// createdDirs is every directory this writer created, deepest last. A rollback
	// prunes only these: a pre-existing empty ancestor is not this request's to
	// remove, even when the request emptied it.
	createdDirs []string
}

// splitParent splits a relative path into its parent directory (".", when there is
// none) and its final component.
func splitParent(rel string) (string, string) {
	i := strings.LastIndex(rel, "/")
	if i < 0 {
		return ".", rel
	}
	return rel[:i], rel[i+1:]
}

// recordCreatedDir remembers a directory made by this writer. Creation order is not
// guaranteed to be deepest-last for every caller, so pruning sorts by depth.
func (w *SectionWriter) recordCreatedDir(rel string) {
	for _, existing := range w.createdDirs {
		if existing == rel {
			return
		}
	}
	w.createdDirs = append(w.createdDirs, rel)
}

// PruneCreatedDirs removes the directories this writer created, deepest first. A
// directory that is not empty (or already gone) stops that branch: an ancestor of a
// directory still in use cannot be empty either. Pre-existing directories are never
// touched, because they were never recorded.
func (w *SectionWriter) PruneCreatedDirs() error {
	if err := w.usable(); err != nil {
		return err
	}
	dirs := append([]string(nil), w.createdDirs...)
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, rel := range dirs {
		if err := w.removeDirIfEmpty(rel); err != nil {
			return err
		}
	}
	w.createdDirs = nil
	return nil
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
	resolved, err := filepath.EvalSymlinks(sectionRoot)
	if err != nil {
		root.Close()
		return nil, err
	}
	return &SectionWriter{root: root, rootPath: resolved}, nil
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
