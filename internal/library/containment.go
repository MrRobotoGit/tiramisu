package library

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrPathEscapesSection means the path did not resolve beneath the root.
	ErrPathEscapesSection = errors.New("library: path escapes the section root")
	// ErrDestinationExists means something already occupies the destination.
	ErrDestinationExists = errors.New("library: destination already exists")
)

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
