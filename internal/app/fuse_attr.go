package app

import (
	_ "net/http/pprof"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"tiramisu/internal/library"
	"tiramisu/internal/vfs"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// sanitizeTime ensures a time is never "zero" (1970), falling back to current time
func sanitizeTime(t time.Time) uint64 {
	if t.IsZero() || t.Unix() <= 0 {
		return uint64(time.Now().Unix())
	}
	return uint64(t.Unix())
}

// fillAttrFromStat populates FUSE attributes from a standard syscall.Stat_t.
func fillAttrFromStat(st *syscall.Stat_t, out *fuse.Attr) {
	out.Ino = st.Ino
	out.Nlink = uint32(st.Nlink)
	out.Mode = uint32(st.Mode)
	out.Uid = st.Uid
	out.Gid = st.Gid
	out.Rdev = uint32(st.Rdev)
	// out.Blksize = uint32(st.Blksize) // CRITICAL: Samba uses for buffer sizing
	out.Blksize = uint32(gc().FuseBlockSize) // Configurable block size (default 1MB)
	out.Blocks = uint64(st.Blocks)           // CRITICAL: Samba uses for throughput calc
	out.Size = uint64(st.Size)

	// The physical directory's times: they move only when a stub is added or removed, so a
	// scanner can skip unchanged folders. time.Now() made every folder look modified.
	mtime, ctime := vfs.StatTimes(st)
	out.Mtime = sanitizeTime(mtime)
	out.Atime = out.Mtime
	out.Ctime = sanitizeTime(ctime)
}

// fillAttrFromMetadata populates FUSE attributes from our internal Metadata.
func fillAttrFromMetadata(m *vfs.Metadata, out *fuse.Attr) {
	out.Size = uint64(m.Size)
	out.Mode = syscall.S_IFREG | 0644
	// Audio projections are read-only to their clients (spec 4): the API owns every
	// mutation, so a conventional scanner must never see a writable stub.
	if m.Audio {
		out.Mode = syscall.S_IFREG | 0444
	}
	out.Uid, out.Gid = gc().UID, gc().GID
	out.Nlink = 1
	// out.Blksize = 4096                                 // Standard block size
	out.Blksize = uint32(gc().FuseBlockSize)  // Configurable block size (default 1MB)
	out.Blocks = (uint64(m.Size) + 511) / 512 // Estimate blocks based on size

	ts := sanitizeTime(m.Mtime)
	out.Mtime = ts
	out.Atime = ts
	out.Ctime = ts
}

// isAudioSectionPath reports whether an absolute path lives under a Phase 1 audio
// section root (music/ or audiobooks/). Prefix matching is component-wise, so a
// sibling like "music-videos/" is not an audio section.
func isAudioSectionPath(fullPath string) bool {
	rel, err := filepath.Rel(physicalSourcePath, fullPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 0 || parts[0] == "." {
		return false
	}
	return library.IsAudioSection(library.Section(parts[0]))
}

// audioWriteIntent reports whether open flags ask for anything a read-only
// projection cannot give.
func audioWriteIntent(flags uint32) bool {
	return flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_TRUNC|syscall.O_APPEND) != 0
}

// audioMutationErrno keeps every audio mutation on the deliberate EROFS the spec
// requires; video keeps the untouched ENOSYS it has always answered with.
func audioMutationErrno(fullPath string) syscall.Errno {
	if isAudioSectionPath(fullPath) {
		return syscall.EROFS
	}
	return syscall.ENOSYS
}
