//go:build !linux

package vfs

// SetMountReadAhead is a no-op off Linux.
func SetMountReadAhead(dir string, kb int) {}
