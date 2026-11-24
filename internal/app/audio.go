package app

import (
	"context"
	"errors"
	"fmt"
	_ "net/http/pprof"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"tiramisu/internal/library"
	"tiramisu/internal/vfs"
)

// pathUsesSSDWarmup reports whether a projection at this physical path takes the
// movie/TV head/tail SSD warmup. A path outside every known section keeps the
// existing behavior; only the audio sections opt out.
func pathUsesSSDWarmup(path string) bool {
	section, ok := library.SectionForPath(physicalSourcePath, path)
	if !ok {
		return true
	}
	return library.UsesSSDWarmup(section)
}

// classifyVFSPath is the single dispatch decision every FUSE entry point shares:
// Readdir, Lookup, Getattr and Open must agree about what is a virtual file, or a
// scanner sees names it cannot open. physicalSourcePath is the effective root,
// which is the CLI argument when one was given.
// globalAudioNamespace is the committed audio namespace the VFS dispatches on.
// Extension alone must not publish a projection: a stub the registry disowned,
// or one nobody ever committed, would otherwise be listed and served.
var globalAudioNamespace = library.NewAudioNamespace()

func classifyVFSPath(fullPath string) library.VFSClass {
	return library.ClassifyProjection(physicalSourcePath, fullPath, globalAudioNamespace)
}

// stubSizeLimits picks the size guard for a stub. Audio drops the movie-sized
// floor: a valid track or book part is legitimately small.
// Size limits follow the file format, not ownership: reading a stub must work
// even for a projection the registry has disowned, or recovery could not see it.
func stubSizeLimits(fullPath string) vfs.SizeLimits {
	if library.ClassifyPath(physicalSourcePath, fullPath).Audio {
		return vfs.AudioSizeLimits
	}
	return vfs.VideoSizeLimits
}

// audioOwnershipRegistry is what every drop guard consults. The nil case means
// audio is genuinely not configured; when the state DB was expected but could not
// be opened, an unavailable registry is returned instead so a tolerated
// persistence failure cannot become permission to delete shared torrents.
func audioOwnershipRegistry() library.AudioRegistry {
	if stateDB != nil {
		return stateDB
	}
	// The state DB is always on now: nil only when its migration failed.
	return library.UnavailableAudioRegistry{Err: errStateDBUnavailable}
}

// audioNamespaceState reports the audio namespace readiness to external observers:
// "Ready" only once reconciliation published a coherent committed set, never during
// a partial rebuild (spec 8).
func audioNamespaceState() string {
	if globalAudioNamespace == nil {
		return "Uninitialized"
	}
	return globalAudioNamespace.State().String()
}

func audioNamespaceEntries() int {
	if globalAudioNamespace == nil {
		return 0
	}
	return globalAudioNamespace.Len()
}

// audioProjectionRegistry returns the projection registry only when it is really
// open: a typed-nil *metadb.DB would satisfy the interface and then panic.
func audioProjectionRegistry() library.AudioProjectionRegistry {
	if stateDB == nil {
		return nil
	}
	return stateDB
}

// diagnosticsDB is the read-only consistency check's view of the state DB, nil without one.
func diagnosticsDB() library.DiagnosticsDB {
	if stateDB == nil {
		return nil
	}
	return stateDB
}

// audioRemovalRegistry mirrors audioProjectionRegistry for the removal half of the
// registry, so a boot whose DB failed to open cannot half-enable the endpoint.
func audioRemovalRegistry() library.AudioRemovalRegistry {
	if stateDB == nil {
		return nil
	}
	return stateDB
}

// unpublishAudioProjectionLive drops a removed projection from the live VFS and clears
// the caches that still answer for it, before its stub is unlinked.
func unpublishAudioProjectionLive(p library.AudioProjection) {
	if globalAudioNamespace != nil {
		globalAudioNamespace.Remove(p.Path())
	}
	invalidateSyncRemovedPath(filepath.Join(physicalSourcePath, string(p.Section), filepath.FromSlash(p.VirtualPath)))
}

var errStateDBUnavailable = errors.New("state DB is enabled but was not opened")

// audioReadyWait bounds how long a VFS operation waits for reconciliation. The
// wait exists because reconciliation always finishes; the bound exists because
// "always" is a claim about code that could regress. Without it, a builder that
// never runs would hang the mount for the life of the process, and a hung mount
// is harder to diagnose than an I/O error.
const audioReadyWait = 2 * time.Minute

// audioWaitServable blocks until an audio directory may be served, and reports
// the errno to fail with when it may not.
//
// Movies and tv return immediately in every state: they are read straight from
// disk and must never wait on the audio registry.
//
// This replaced a bare syscall.EAGAIN. Pass 4 read the scanner sources: GNU ls
// reports and stops, Navidrome retries once then returns the entries it has
// (none), and Audiobookshelf converts the error to an empty slice and then marks
// existing items missing. The errno chosen to avoid an apparent-empty library
// produced exactly that on two supported scanners. Blocking is what a network
// filesystem does, and scanners tolerate slow far better than empty.
func audioWaitServable(ctx context.Context, dir string) syscall.Errno {
	section, ok := library.SectionForPath(physicalSourcePath, filepath.Join(dir, "x"))
	if !ok || !library.IsAudioSection(section) {
		return 0
	}
	waitCtx, cancel := context.WithTimeout(ctx, audioReadyWait)
	defer cancel()

	err := globalAudioNamespace.WaitServable(waitCtx)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, library.ErrAudioReconcileFailed):
		// Terminal. Never ENOENT: a scanner reads absence as a deletion.
		return syscall.EIO
	case ctx.Err() != nil:
		// The caller gave up, not us. EINTR says interrupted rather than failed.
		return syscall.EINTR
	default:
		// Our own deadline expired, so reconciliation never terminated.
		logger.Printf("AUDIO: readiness wait expired after %s for %s", audioReadyWait, dir)
		return syscall.EIO
	}
}

// audioProjectionMeta builds an audio file's metadata from its committed registry
// row instead of the physical stub. The registry is authoritative for audio
// (spec §5.1), and the stream URL is recomputed from (hash, file_index) rather
// than read back from the stub, which spec §5.2 calls a second potentially stale
// source of truth. A stub swapped underneath therefore cannot change what is
// served, and a stub with an unparseable URL cannot make a committed row
// unreadable.
func audioProjectionMeta(fullPath string, p library.AudioProjection) *vfs.Metadata {
	return &vfs.Metadata{
		URL:                 fmt.Sprintf("%s/stream?link=%s&index=%d&play", strings.TrimRight(gc().GoStormBaseURL, "/"), p.Hash, p.FileIndex),
		Size:                p.Size,
		Mtime:               time.Unix(0, p.MtimeNS),
		Path:                fullPath,
		ExternalID:          p.ExternalID,
		ExternalIDNamespace: p.ExternalIDNamespace,
		Audio:               true,
		SegmentTrack:        p.CueTrack,
		SegmentOffset:       p.ByteOffset,
		SegmentHeader:       p.Header,
	}
}

// committedAudioMeta returns registry-derived metadata when the namespace owns
// the path. ok is false for video and for anything uncommitted.
func committedAudioMeta(fullPath string) (*vfs.Metadata, bool) {
	p, ok := library.CommittedProjectionFor(physicalSourcePath, fullPath, globalAudioNamespace)
	if !ok {
		return nil, false
	}
	return audioProjectionMeta(fullPath, p), true
}

// stubMeta returns a virtual file's metadata. Audio comes from its committed
// registry row; video keeps reading its physical stub exactly as before.
func stubMeta(fullPath string) (*vfs.Metadata, error) {
	if meta, ok := committedAudioMeta(fullPath); ok {
		return meta, nil
	}
	return getOrReadMeta(fullPath)
}
