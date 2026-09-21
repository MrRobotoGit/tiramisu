package library

import (
	"errors"
	"path/filepath"
	"strings"
)

// ErrOpenTargetUnresolved means no file could be chosen for an Open.
var ErrOpenTargetUnresolved = errors.New("library: cannot resolve the open target")

// OpenTarget is the torrent file an Open should serve.
type OpenTarget struct {
	Hash      string
	FileIndex int
}

// TorrentFile is one resident torrent file in the engine's sorted order.
// Index is 1-based, matching the ID GoStorm assigns.
type TorrentFile struct {
	Index  int
	Path   string
	Length int64
}

// ResolveOpenTarget picks the torrent file an Open serves. Audio takes the
// registry's index: its name is caller-chosen, so a heuristic can pick another.
func ResolveOpenTarget(section Section, hash string, urlIndex int, size int64, virtualPath string, files []TorrentFile) (OpenTarget, error) {
	if IsAudioSection(section) {
		if urlIndex <= 0 {
			return OpenTarget{}, ErrOpenTargetUnresolved
		}
		return OpenTarget{Hash: hash, FileIndex: urlIndex}, nil
	}

	// Video keeps the existing heuristic: engine-built names drift when a media
	// server renames a file, and the index in an older stub can be stale.
	if index, ok := matchVideoFile(hash, size, virtualPath, files); ok {
		return OpenTarget{Hash: hash, FileIndex: index}, nil
	}
	if urlIndex > 0 {
		return OpenTarget{Hash: hash, FileIndex: urlIndex}, nil
	}
	return OpenTarget{}, ErrOpenTargetUnresolved
}

// matchVideoFile reproduces the size-and-normalised-name match. It reads files
// in the order given and never reorders them: the engine's list is shared.
func matchVideoFile(hash string, size int64, virtualPath string, files []TorrentFile) (int, bool) {
	if len(files) == 0 {
		return 0, false
	}
	wanted := normaliseVirtualName(hash, virtualPath)
	matchIndex, matches := 0, 0
	for _, file := range files {
		if file.Length != size {
			continue
		}
		matches++
		matchIndex = file.Index
		candidate := strings.ReplaceAll(strings.ToLower(file.Path), "_", ".")
		if strings.HasSuffix(wanted, candidate) || strings.HasSuffix(candidate, wanted) || candidate == wanted {
			return file.Index, true
		}
	}
	// A lone size match is trusted even when the name does not normalise, which
	// is what survives a media-server rename.
	if matches == 1 {
		return matchIndex, true
	}
	return 0, false
}

// normaliseVirtualName strips the hash suffix Tiramisu appends and folds the
// separators a media server may have rewritten.
func normaliseVirtualName(hash, virtualPath string) string {
	name := strings.ToLower(filepath.Base(virtualPath))
	if len(hash) >= 8 {
		full := strings.ToLower(hash)
		short := full[:8]
		for _, token := range []string{"_" + full, "." + full, "_" + short, "." + short} {
			name = strings.ReplaceAll(name, token, "")
		}
	}
	name = strings.ReplaceAll(name, "_", ".")
	return strings.ReplaceAll(name, " ", ".")
}
