package vfs

import (
	"strings"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
)

// NotifyEntryPath drops the kernel's cached dentry for rel (slash-separated, relative to root),
// so a path added or removed out of band is seen before the entry TTL expires. Paths the kernel
// never looked up have no inode here and need nothing.
func NotifyEntryPath(root *fs.Inode, rel string) syscall.Errno {
	rel = strings.Trim(rel, "/")
	if root == nil || rel == "" {
		return 0
	}
	parts := strings.Split(rel, "/")
	dir := root
	for _, p := range parts[:len(parts)-1] {
		if dir = dir.GetChild(p); dir == nil {
			return 0
		}
	}
	return dir.NotifyEntry(parts[len(parts)-1])
}
