package app

import (
	"fmt"
	"log"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"tiramisu/internal/library"
	"tiramisu/internal/vfs"
)

var globalInodeMap *vfs.InodeMap

func InitGlobalInodeMap(stateDir string, logger *log.Logger) error {
	savePath := vfs.GetDefaultInodeMapPath(stateDir)
	dir := filepath.Dir(savePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create inode map directory: %w", err)
	}
	globalInodeMap = vfs.NewInodeMap(savePath, &vfsLogger{logger})
	return nil
}

// finishGlobalInodeMapInit loads the map and starts the saver. Split from
// InitGlobalInodeMap so the load runs once, after StateDB had a chance to attach:
// loading JSON first would report an empty map on every migrated install.
func finishGlobalInodeMapInit(logger *log.Logger) {
	if globalInodeMap == nil {
		return
	}
	if !globalInodeMap.HasDB() {
		if err := globalInodeMap.LoadFromDisk(); err != nil {
			logger.Printf("WARNING: Failed to load inode map: %v (falling back to filename hash)", err)
		}
		files, dirs, _, _ := GetInodeMapStats()
		logger.Printf("InodeMap: Initialized with %d files, %d dirs from %s", files, dirs, vfs.GetDefaultInodeMapPath(GetStateDir()))
	}
	globalInodeMap.StartBackgroundSaver()
}

func ShutdownGlobalInodeMap() {
	if globalInodeMap != nil {
		globalInodeMap.Stop()
	}
}

func getFileInodeFromMap(fullPath string) uint64 {
	if globalInodeMap == nil {
		return hashFilenameToInode(filepath.Base(fullPath)) & vfs.InodeFileMask
	}
	if inode := globalInodeMap.GetFileInode(fullPath); inode != 0 {
		return inode
	}
	if inode := globalInodeMap.GetFileInodeByName(filepath.Base(fullPath)); inode != 0 {
		return inode
	}
	return hashFilenameToInode(filepath.Base(fullPath)) & vfs.InodeFileMask
}

// publishAudioProjectionsLive wires a committed audio batch into the live VFS. Every
// inode is registered before the namespace gains the paths, exactly as startup
// reconciliation does, so a Readdir that lands before the first Lookup emits the
// content-derived inode instead of a basename-derived fallback (or, through the basename
// map, the inode of a different projection that happens to share the basename). The batch
// lands in one namespace update, so no reader observes half an album.
func publishAudioProjectionsLive(ps []library.AudioProjection) {
	for _, p := range ps {
		if globalInodeMap != nil {
			full := filepath.Join(physicalSourcePath, string(p.Section), filepath.FromSlash(p.VirtualPath))
			globalInodeMap.AddFile(full, vfs.SegmentInodeHash(p.Hash, p.CueTrack), p.FileIndex)
		}
	}
	if globalAudioNamespace != nil {
		globalAudioNamespace.AddProjections(ps)
	}
}

func getDirInodeFromMap(relativePath string) uint64 {
	if globalInodeMap == nil {
		return vfs.GenerateDirInode(relativePath)
	}
	return globalInodeMap.GetDirInode(relativePath)
}

func addFileToInodeMap(fullPath, url string) uint64 {
	if globalInodeMap == nil {
		return 0
	}
	hash, index := vfs.ExtractHashAndIndex(url)
	if hash == "" {
		return 0
	}
	return globalInodeMap.AddFile(fullPath, hash, index)
}

// addMetaToInodeMap registers a virtual file's inode. A cue track joins its track to
// the key, since the tracks of an image share one torrent file; every other file,
// video included, goes through addFileToInodeMap unchanged.
func addMetaToInodeMap(fullPath string, meta *vfs.Metadata) uint64 {
	if meta.SegmentTrack <= 0 {
		return addFileToInodeMap(fullPath, meta.URL)
	}
	if globalInodeMap == nil {
		return 0
	}
	hash, index := vfs.ExtractHashAndIndex(meta.URL)
	if hash == "" {
		return 0
	}
	return globalInodeMap.AddFile(fullPath, vfs.SegmentInodeHash(hash, meta.SegmentTrack), index)
}

func GetInodeMapStats() (files, dirs, hits, misses int64) {
	if globalInodeMap == nil {
		return 0, 0, 0, 0
	}
	return globalInodeMap.Stats()
}

// vfsLogger adapts *log.Logger to the vfs.Logger interface.
type vfsLogger struct{ logger *log.Logger }

func (l *vfsLogger) Printf(format string, args ...interface{}) {
	l.logger.Printf(format, args...)
}
