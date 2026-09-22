package vfs

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"tiramisu/internal/metadb"
)

// SizeLimits bounds the declared size of a virtual stub.
type SizeLimits struct {
	Min int64
	Max int64
}

// The movie floor exists to catch a truncated 4K remux. Audio has no such floor:
// a valid track or book part is legitimately small, so corruption is caught by
// size agreement with the registry instead.
var (
	VideoSizeLimits = SizeLimits{Min: MinFileSize, Max: MaxFileSize}
	AudioSizeLimits = SizeLimits{Min: 1, Max: MaxFileSize}
)

// ReadMetadataFromFileWithLimits reads a virtual stub, validating its declared
// size against limits. Format handling matches ReadMetadataFromFile.
func ReadMetadataFromFileWithLimits(path string, limits SizeLimits) (*FileMetadata, error) {
	return readMetadataFromFile(path, limits)
}

// AudioProjectionSource is the registry subset reconciliation reads.
type AudioProjectionSource interface {
	CommittedAudioProjections(section string) ([]metadb.AudioProjection, error)
}

// AudioReconcileResult reports one reconciliation pass. The two problem lists
// carry "<section>/<virtual_path>" and are sorted, so two passes over unchanged
// state compare equal.
type AudioReconcileResult struct {
	Registered       int
	MissingStub      []string
	SizeMismatch     []string
	ReferencedHashes []string
}

// audioSections is the set reconciliation walks, in the order it walks them.
var audioSections = []string{"music", "audiobooks"}

// ReconcileAudio rebuilds the VFS state the committed audio registry implies:
// it registers each projection's full path against its content identity and
// reports what it could not account for. It never writes to disk and never
// touches the registry - the registry is authoritative, and repairing a missing
// stub is a later decision, not a side effect of looking.
func ReconcileAudio(src AudioProjectionSource, im *InodeMap, sourcePath string) (*AudioReconcileResult, error) {
	result := &AudioReconcileResult{}
	if src == nil || im == nil {
		return result, nil
	}

	hashes := make(map[string]struct{})
	for _, section := range audioSections {
		projections, err := src.CommittedAudioProjections(section)
		if err != nil {
			return nil, err
		}
		for _, p := range projections {
			if p.State != metadb.AudioCommitted {
				continue
			}
			// Collected before the health check: an unhealthy projection still
			// holds its torrent, and dropping it would strand the repair.
			hashes[p.Hash] = struct{}{}

			key := p.Section + "/" + p.VirtualPath
			full := filepath.Join(sourcePath, p.Section, filepath.FromSlash(p.VirtualPath))
			meta, err := ReadMetadataFromFileWithLimits(full, AudioSizeLimits)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					result.MissingStub = append(result.MissingStub, key)
				} else {
					result.SizeMismatch = append(result.SizeMismatch, key)
				}
				continue
			}
			// Size agreement is not identity. A stub of the right size whose URL
			// addresses another torrent file would be registered under this row's
			// inode while Open served the other object: content substitution under
			// a stable path, which the registry's authority exists to prevent.
			//
			// Only compared when the stub actually declares an identity. A URL with
			// no resolvable link carries nothing to disagree with, and Open cannot
			// serve it either, so rejecting on that would fail legacy stubs without
			// closing any substitution.
			if stubHash, stubIndex := ExtractHashAndIndex(meta.URL); stubHash != "" &&
				(!strings.EqualFold(stubHash, p.Hash) || stubIndex != p.FileIndex) {
				result.SizeMismatch = append(result.SizeMismatch, key)
				continue
			}
			if meta.Size != p.Size {
				result.SizeMismatch = append(result.SizeMismatch, key)
				continue
			}
			// The full path, not the basename: two albums both holding
			// "01 - Intro.flac" share a basename but never an identity.
			im.AddFile(full, SegmentInodeHash(p.Hash, p.CueTrack), p.FileIndex)
			result.Registered++
		}
	}

	result.ReferencedHashes = make([]string, 0, len(hashes))
	for hash := range hashes {
		result.ReferencedHashes = append(result.ReferencedHashes, hash)
	}
	sort.Strings(result.ReferencedHashes)
	sort.Strings(result.MissingStub)
	sort.Strings(result.SizeMismatch)
	return result, nil
}
