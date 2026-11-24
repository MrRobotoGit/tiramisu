package app

import (
	"fmt"
	_ "net/http/pprof"
	"strconv"
	"strings"
	"tiramisu/internal/library"

	"github.com/cespare/xxhash/v2"
)

// resolveTargetFile finds the torrent hash and file index for a given URL and size.
// resolveTargetFileFn is resolveTargetFile, a var so the FUSE-layer simulator can resolve without
// the BitTorrent engine.
var resolveTargetFileFn = resolveTargetFile

func resolveTargetFile(url string, targetSize int64, physicalPath string) (string, int, error) {
	if nativeBridge == nil {
		return "", 0, fmt.Errorf("nativeBridge is nil")
	}
	if !strings.Contains(url, "link=") {
		return "", 0, fmt.Errorf("file not found in torrent")
	}

	hashStr := urlQueryValue(url, "link=")
	urlIndex := 0
	if raw := urlQueryValue(url, "index="); raw != "" {
		if idx, err := strconv.Atoi(raw); err == nil {
			urlIndex = idx
		}
	}

	section, _ := library.SectionForPath(physicalSourcePath, physicalPath)

	// The engine lists the files in ID order (fileorder). Audio never reads this list - its
	// identity comes from the registry - so a music scan does not pay for it on every Open.
	var files []library.TorrentFile
	if !library.IsAudioSection(section) {
		if t := eng.Get(hashStr); t != nil {
			for _, f := range t.Files() {
				files = append(files, library.TorrentFile{Index: f.ID, Path: f.Path, Length: f.Length})
			}
		}
	}

	target, err := library.ResolveOpenTarget(section, hashStr, urlIndex, targetSize, physicalPath, files)
	if err != nil {
		return "", 0, err
	}
	return target.Hash, target.FileIndex, nil
}

// urlQueryValue reads a bare query value without parsing the whole URL, which
// this FUSE path does on every Open.
func urlQueryValue(url, key string) string {
	start := strings.Index(url, key)
	if start < 0 {
		return ""
	}
	start += len(key)
	if end := strings.Index(url[start:], "&"); end >= 0 {
		return url[start : start+end]
	}
	return url[start:]
}

// Fast deterministic inode from FNV-1a hash to avoid syscalls in Readdir.
// Uses POSIX bits (syscall.S_IFDIR/S_IFREG) for FUSE/Samba/kernel compatibility.
func hashFilenameToInode(name string) uint64 {
	return xxhash.Sum64String(name)
}
