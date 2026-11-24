package app

import (
	"context"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"tiramisu/internal/library"
	"tiramisu/internal/registry"
	"tiramisu/internal/vfs"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// VirtualDirNode - nodo per directory (movies, tv) con file .mkv virtuali
type VirtualDirNode struct {
	fs.Inode
	physicalPath string // Path fisico della directory (es. /mnt/torrserver/movies)
}

// Compile-time interface checks for VirtualDirNode
var _ fs.NodeReaddirer = (*VirtualDirNode)(nil)
var _ fs.NodeLookuper = (*VirtualDirNode)(nil)
var _ fs.NodeGetattrer = (*VirtualDirNode)(nil)
var _ fs.NodeUnlinker = (*VirtualDirNode)(nil)
var _ fs.NodeMkdirer = (*VirtualDirNode)(nil)
var _ fs.NodeCreater = (*VirtualDirNode)(nil)
var _ fs.NodeRenamer = (*VirtualDirNode)(nil)
var _ fs.NodeSetattrer = (*VirtualDirNode)(nil)

// Every mutation of an audio directory answers EROFS (spec 4); video directories keep
// the untouched ENOSYS they have always answered with.
func (d *VirtualDirNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, audioMutationErrno(filepath.Join(d.physicalPath, name))
}

func (d *VirtualDirNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	return nil, nil, 0, audioMutationErrno(filepath.Join(d.physicalPath, name))
}

func (d *VirtualDirNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	return audioMutationErrno(filepath.Join(d.physicalPath, name))
}

func (d *VirtualDirNode) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if isAudioSectionPath(d.physicalPath) {
		return syscall.EROFS
	}
	return syscall.ENOSYS
}

func (d *VirtualDirNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	// Inside an audio section before reconciliation has published, the committed
	// set is unknown. Wait for it rather than answering: an empty-but-successful
	// listing tells a scanner the library was deleted (spec 9: never a plausible
	// but incomplete tree).
	//
	// This check stays AHEAD of the directory cache deliberately. A listing cached
	// while the namespace was Ready would otherwise still be served after a later
	// MarkFailed, masking a terminal failure behind stale entries.
	if errno := audioWaitServable(ctx, d.physicalPath); errno != 0 {
		return nil, errno
	}

	if entries, found := globalDirCache.Get(d.physicalPath); found {
		return &nfsDirStream{entries: entries}, 0
	}
	// Snapshot the generation before the walk: a concurrent invalidate must not be
	// overwritten by this listing landing after it.
	generation := globalDirCache.Generation(d.physicalPath)

	entries, err := os.ReadDir(d.physicalPath)
	if err != nil {
		logger.Printf("READDIR DIR ERROR: %v", err)
		return nil, vfs.ToErrno(err)
	}

	result := make([]fuse.DirEntry, 0, len(entries))
	for i, e := range entries {
		fullPath := filepath.Join(d.physicalPath, e.Name())
		if e.IsDir() {
			ino := getDirInodeFromMap(fullPath)
			result = append(result, fuse.DirEntry{
				Name: e.Name(),
				Mode: syscall.S_IFDIR,
				Ino:  ino,
				Off:  uint64(i + 1),
			})
		} else if classifyVFSPath(fullPath).Stub {
			ino := getFileInodeFromMap(fullPath)
			result = append(result, fuse.DirEntry{
				Name: e.Name(),
				Mode: syscall.S_IFREG,
				Ino:  ino,
				Off:  uint64(i + 1),
			})
		}
	}

	globalDirCache.PutIfGeneration(d.physicalPath, result, generation)

	return &nfsDirStream{entries: result}, 0
}

func (d *VirtualDirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	fullPath := filepath.Join(d.physicalPath, name)

	// Only Readdir used to check readiness, so a stat during the unready window
	// fell through to ENOENT below - semantic absence rather than refusal, which
	// a non-zero NegativeTimeoutSeconds then let the kernel cache past
	// publication. Waiting here removes the false negative at its source, so the
	// correctness no longer depends on that timeout being zero.
	//
	// Directories inside an audio section wait too. The caller is about to read
	// the directory anyway, so exempting them would only move the wait one call
	// later while adding a branch.
	if errno := audioWaitServable(ctx, d.physicalPath); errno != 0 {
		return nil, errno
	}

	if classifyVFSPath(fullPath).Stub {
		meta, err := stubMeta(fullPath)
		if err == nil {
			addMetaToInodeMap(fullPath, meta)
			ino := getFileInodeFromMap(fullPath)
			node := &VirtualMkvNode{vMeta: meta}
			stable := fs.StableAttr{
				Mode: syscall.S_IFREG,
				Ino:  ino,
				Gen:  1,
			}
			child := d.NewInode(ctx, node, stable)
			fillAttrFromMetadata(meta, &out.Attr)
			out.Ino = ino
			return child, 0
		}
	}

	// Fallback for directories
	st := syscall.Stat_t{}
	if err := syscall.Stat(fullPath, &st); err != nil {
		return nil, syscall.ENOENT
	}

	if (st.Mode & syscall.S_IFMT) == syscall.S_IFDIR {
		node := &VirtualDirNode{physicalPath: fullPath}
		dirIno := getDirInodeFromMap(fullPath)
		stable := fs.StableAttr{
			Mode: syscall.S_IFDIR,
			Ino:  dirIno,
			Gen:  1,
		}
		child := d.NewInode(ctx, node, stable)
		fillAttrFromStat(&st, &out.Attr)
		out.Ino = dirIno
		return child, 0
	}

	return nil, syscall.ENOENT
}

// Getattr returns directory attributes
func (d *VirtualDirNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	st := syscall.Stat_t{}
	if err := syscall.Stat(d.physicalPath, &st); err != nil {
		logger.Printf("GETATTR DIR ERROR: %v", err)
		return vfs.ToErrno(err)
	}

	fillAttrFromStat(&st, &out.Attr)

	// Use full-path hash to avoid inode collisions (e.g. Season.01 dirs).
	out.Ino = getDirInodeFromMap(d.physicalPath)

	// Override ONLY Mode and Size to ensure directory permissions and Samba compliance
	out.Mode = syscall.S_IFDIR | 0755
	// Projected audio directories are read-only to their clients (spec 4); the API
	// owns every mutation, so 0555 is the honest answer.
	if section, rel, ok := audioSectionRel(d.physicalPath); ok {
		out.Mode = syscall.S_IFDIR | 0555
		// Spec 7: an audio directory's mtime moves when its committed child set does,
		// and at no other time. Deriving it from the registry rows also keeps a
		// rolled-back staging file from leaving a spurious timestamp behind.
		if mtime, found := globalAudioNamespace.DirMtime(section, rel); found {
			ts := sanitizeTime(mtime)
			out.Mtime, out.Atime, out.Ctime = ts, ts, ts
		}
	}
	out.Size = 4096

	return 0
}

// audioSectionRel splits a physical path into its audio section and the section-relative
// directory path. ok is false outside music/ and audiobooks/.
func audioSectionRel(fullPath string) (library.Section, string, bool) {
	rel, err := filepath.Rel(physicalSourcePath, fullPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	section := library.Section(parts[0])
	if !library.IsAudioSection(section) {
		return "", "", false
	}
	if len(parts) == 1 {
		return section, "", true
	}
	return section, strings.Join(parts[1:], "/"), true
}

// forceCloseVirtualFile terminates the active pump and closes all open handles for a
// virtual .mkv path. Without this, smbd D-states on a file with an active blocking read.
// Shared by the FUSE Unlink handler and invalidateSyncRemovedPath.
func forceCloseVirtualFile(fullPath string) {
	// V-OpenTracker: attendi che i Read() in volo completino prima di cancellare
	// la pump, per evitare nil-deref su nativeReader durante cancel concorrente.
	if globalOpenTracker.IsPathOpen(fullPath) {
		for i := 0; i < 3; i++ {
			if !globalOpenTracker.IsPathOpen(fullPath) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if val, ok := activePumps.Load(fullPath); ok {
		ps := val.(*NativePumpState)
		if ps.cancel != nil {
			ps.cancel()
		}
		activePumps.Delete(fullPath)
		logger.Printf("UNLINK: force-terminated active pump for %s", filepath.Base(fullPath))
	}
	// Close all handles referencing this file
	activeHandles.Range(func(key, value interface{}) bool {
		h := key.(*MkvHandle)
		if h.path == fullPath {
			if h.nativeReader != nil {
				h.nativeReader.Close()
			}
			activeHandles.Delete(h)
			logger.Printf("UNLINK: force-closed handle for %s", filepath.Base(fullPath))
		}
		return true
	})
}

// invalidateSyncRemovedPath drops FUSE-layer state for a stub removed out-of-band by the
// sync engines: their os.Remove on the physical path bypasses the Unlink handler, so
// without this the virtual file keeps being listed and served for hours (dircache), and
// a Plex scan in that window registers the replaced file as a ghost duplicate version
// that later fails to play once the cache finally expires.
func invalidateSyncRemovedPath(path string) {
	if strings.HasSuffix(path, ".mkv") {
		forceCloseVirtualFile(path)
		registry.RemoveFromRegistry(path)
	}
	// The directory listing is not the only way back to a removed stub: a lookup by
	// exact path is served from the metadata cache, which holds entries for 24h.
	if metaCache != nil {
		metaCache.Delete(path)
	}
	// The file's own directory and every ancestor up to the source root: adding the
	// first track of an album changes the listing of each directory on the way up, and
	// a Readdir of any of them inside the TTL would otherwise miss it. Deleting the
	// path itself covers removed directories too (empty season/show cleanup).
	if physicalSourcePath == "" {
		globalDirCache.Delete(filepath.Dir(path))
	} else {
		dir := filepath.Dir(path)
		for strings.HasPrefix(dir, physicalSourcePath) {
			globalDirCache.Delete(dir)
			if dir == physicalSourcePath {
				break
			}
			next := filepath.Dir(dir)
			if next == dir {
				break
			}
			dir = next
		}
	}
	globalDirCache.Delete(path)
	notifyKernelEntry(path)
}

// fuseMaxBackground is fuse_max_background, or master_concurrency_limit when unset.
func fuseMaxBackground() int {
	if n := gc().FuseMaxBackground; n > 0 {
		return n
	}
	return gc().ConcurrencyLimit
}

// kernelRoot is the mounted root inode, for kernel cache invalidation; nil until mounted.
var kernelRoot atomic.Pointer[fs.Inode]

// notifyKernelEntry drops the kernel's dentry for a physical path, so entry TTLs longer than a
// second never show a removed stub. Listings are not kernel-cached, only the dircache above.
func notifyKernelEntry(path string) {
	root := kernelRoot.Load()
	if root == nil || physicalSourcePath == "" {
		return
	}
	rel, err := filepath.Rel(physicalSourcePath, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return
	}
	safeGo(func() { vfs.NotifyEntryPath(root, filepath.ToSlash(rel)) })
}

// Unlink handles file deletion and triggers torrent auto-remove (FASE 4.2)
func (d *VirtualDirNode) Unlink(ctx context.Context, name string) syscall.Errno {
	logger.Printf("=== UNLINK === dir=%s file=%s", d.physicalPath, name)

	// Only handle .mkv files
	if !strings.HasSuffix(name, ".mkv") {
		logger.Printf("UNLINK: not an mkv file, skipping auto-remove")
		return syscall.EPERM // Not permitted for non-mkv files
	}

	fullPath := filepath.Join(d.physicalPath, name)
	if err := library.UnlinkAllowed(fullPath); err != nil {
		logger.Printf("UNLINK refused: %v", err)
		return syscall.EPERM
	}

	forceCloseVirtualFile(fullPath)
	tailFillTargets.Delete(fullPath)

	// Blacklist first, the torrent last: a crash in between leaves at most an orphan
	// torrent, never a stub without one that the next sync would rehydrate.
	hash, blacklisted, err := globalTorrentRemover.IdentifyAndBlacklist(fullPath)
	if err != nil {
		logger.Printf("UNLINK ERROR: cannot identify the torrent: %v", err)
	} else if !blacklisted {
		logger.Printf("UNLINK: hash not blacklisted (see the warning above)")
	}

	// Delete physical .mkv file
	if err := library.DeleteStub(fullPath); err != nil {
		logger.Printf("UNLINK ERROR: failed to delete file: %v", err)
		return vfs.ToErrno(err)
	}

	registry.RemoveFromRegistry(fullPath)
	if hash != "" {
		released, err := globalTorrentRemover.ReleaseHash(hash)
		switch {
		case err != nil:
			logger.Printf("UNLINK ERROR: torrent %s not removed: %v", hash[:8], err)
		case released:
			logger.Printf("UNLINK: torrent successfully removed from Tentacle")
		default:
			logger.Printf("UNLINK: torrent %s kept, still referenced", hash[:8])
		}
	}
	globalDirCache.Delete(d.physicalPath)
	// Same reason as the sync removal path: a lookup by exact path is answered from
	// the metadata cache, which would keep serving this file for the whole TTL.
	if metaCache != nil {
		metaCache.Delete(fullPath)
	}

	logger.Printf("UNLINK COMPLETE: file deleted successfully")
	return 0
}
