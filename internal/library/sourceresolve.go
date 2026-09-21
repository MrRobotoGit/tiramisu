package library

import (
	"errors"
	"fmt"
)

// Resolution failures are distinguished by sentinel so the Library API can map
// each to its own status. Every failure wraps exactly one of these.
var (
	ErrSourceNotFound  = errors.New("library: source path not found in torrent")
	ErrSourceDuplicate = errors.New("library: source selected twice in one request")
	ErrSourceAmbiguous = errors.New("library: torrent file list is malformed")
	ErrSourceDrift     = errors.New("library: source identity changed since it was resolved")
)

// ResolvedSource is a caller's source_path bound to the torrent file it names.
// Both halves are persisted so a later disagreement is detectable.
type ResolvedSource struct {
	SourcePath string
	FileIndex  int
	Size       int64
}

// matchEntry returns the one list entry carrying sourcePath.
func matchEntry(files []FileStat, sourcePath string) (*FileStat, error) {
	if sourcePath == "" {
		return nil, fmt.Errorf("empty source path: %w", ErrSourceNotFound)
	}
	var match *FileStat
	matches := 0
	for i := range files {
		if files[i].Path == sourcePath {
			match = &files[i]
			matches++
		}
	}
	switch {
	case matches == 0:
		return nil, fmt.Errorf("no file named %q: %w", sourcePath, ErrSourceNotFound)
	case matches > 1:
		return nil, fmt.Errorf("%d files named %q: %w", matches, sourcePath, ErrSourceAmbiguous)
	}
	return match, nil
}

// checkEntry rejects an index that cannot address exactly one file: the stream
// lookup stops at the first hit, so a shared index would serve the wrong file.
func checkEntry(files []FileStat, match *FileStat, sourcePath string) error {
	// GoStorm indexes are 1-based; a non-positive one is a malformed answer.
	if match.ID <= 0 {
		return fmt.Errorf("file %q has non-positive index %d: %w", sourcePath, match.ID, ErrSourceAmbiguous)
	}
	if match.Length < 0 {
		return fmt.Errorf("file %q has negative length %d: %w", sourcePath, match.Length, ErrSourceAmbiguous)
	}
	sharing := 0
	for i := range files {
		if files[i].ID == match.ID {
			sharing++
		}
	}
	if sharing > 1 {
		return fmt.Errorf("index %d of %q is carried by %d files: %w", match.ID, sourcePath, sharing, ErrSourceAmbiguous)
	}
	return nil
}

// ResolveSource binds one source_path to its torrent file. Matching is exact and
// case-sensitive; a miss is an error, never a substitution.
func ResolveSource(files []FileStat, sourcePath string) (ResolvedSource, error) {
	match, err := matchEntry(files, sourcePath)
	if err != nil {
		return ResolvedSource{}, err
	}
	if err := checkEntry(files, match, sourcePath); err != nil {
		return ResolvedSource{}, err
	}
	return ResolvedSource{SourcePath: sourcePath, FileIndex: match.ID, Size: match.Length}, nil
}

// ResolveSources resolves a whole request, in request order. It is fail-closed:
// one bad entry fails the call and returns nothing.
func ResolveSources(files []FileStat, sourcePaths []string) ([]ResolvedSource, error) {
	// Repeated paths are rejected first so a request that both repeats a path
	// and names an unknown one reports the duplicate.
	seenPath := make(map[string]bool, len(sourcePaths))
	for _, sourcePath := range sourcePaths {
		if seenPath[sourcePath] {
			return nil, fmt.Errorf("source path %q requested twice: %w", sourcePath, ErrSourceDuplicate)
		}
		seenPath[sourcePath] = true
	}

	// Every path is matched before any is reported, so duplicate-index detection
	// does not depend on where an unresolvable path sits in the request.
	matches := make([]*FileStat, len(sourcePaths))
	var firstErr error
	for i, sourcePath := range sourcePaths {
		match, err := matchEntry(files, sourcePath)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		matches[i] = match
	}
	seenIndex := make(map[int]bool, len(sourcePaths))
	for i, match := range matches {
		if match == nil {
			continue
		}
		// Two distinct paths can still name one file in a malformed list, which
		// would project the same torrent file at two virtual paths.
		if seenIndex[match.ID] {
			return nil, fmt.Errorf("file index %d requested twice, last as %q: %w", match.ID, sourcePaths[i], ErrSourceDuplicate)
		}
		seenIndex[match.ID] = true
	}
	if firstErr != nil {
		return nil, firstErr
	}

	resolved := make([]ResolvedSource, 0, len(sourcePaths))
	for i, match := range matches {
		if err := checkEntry(files, match, sourcePaths[i]); err != nil {
			return nil, err
		}
		resolved = append(resolved, ResolvedSource{SourcePath: sourcePaths[i], FileIndex: match.ID, Size: match.Length})
	}
	return resolved, nil
}

// VerifyResolvedSource re-checks a persisted triple, reporting a changed index or
// size rather than adopting it. files must belong to the same infohash.
func VerifyResolvedSource(files []FileStat, prior ResolvedSource) error {
	current, err := ResolveSource(files, prior.SourcePath)
	if err != nil {
		return err
	}
	if current.FileIndex != prior.FileIndex {
		return fmt.Errorf("source %q moved from index %d to %d: %w", prior.SourcePath, prior.FileIndex, current.FileIndex, ErrSourceDrift)
	}
	if current.Size != prior.Size {
		return fmt.Errorf("source %q changed size from %d to %d: %w", prior.SourcePath, prior.Size, current.Size, ErrSourceDrift)
	}
	return nil
}
