package app

import (
	"context"
	_ "net/http/pprof"
	"syscall"
	"tiramisu/internal/vfs"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Compile-time interface checks for MkvHandle
var _ fs.FileReader = (*MkvHandle)(nil)
var _ fs.FileReleaser = (*MkvHandle)(nil)

// cueHandle serves one cue track of a single-file image: the generated header from
// memory, then the image's frames through an ordinary MkvHandle whose file ends
// where the track ends. MkvHandle itself is untouched.
type cueHandle struct {
	inner  *MkvHandle
	header []byte
	base   int64 // where the track's frames start in the torrent file
	length int64 // image bytes the track spans
}

var _ fs.FileReader = (*cueHandle)(nil)
var _ fs.FileReleaser = (*cueHandle)(nil)

// openCueTrack opens the image through the regular path, sized to end at the end of
// the track, and wraps it.
func (n *VirtualMkvNode) openCueTrack(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	length := n.vMeta.Size - int64(len(n.vMeta.SegmentHeader))
	if length <= 0 {
		return nil, 0, syscall.EIO
	}
	inner := *n.vMeta
	inner.Size = n.vMeta.SegmentOffset + length
	inner.SegmentHeader = nil
	fh, fuseFlags, errno := (&VirtualMkvNode{vMeta: &inner, wake: n.wake}).Open(ctx, flags)
	if errno != 0 {
		return nil, 0, errno
	}
	h, ok := fh.(*MkvHandle)
	if !ok {
		return nil, 0, syscall.EIO
	}
	return &cueHandle{inner: h, header: n.vMeta.SegmentHeader, base: n.vMeta.SegmentOffset, length: length}, fuseFlags, 0
}

func (h *cueHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	hdrFrom, hdrTo, innerOff, innerN := vfs.SegmentRead(len(h.header), h.base, h.length, off, len(dest))
	n := copy(dest, h.header[hdrFrom:hdrTo])
	if innerN > 0 {
		want := dest[n : n+innerN]
		res, errno := h.inner.Read(ctx, want, innerOff)
		if errno != 0 {
			// Never the header alone: a short read away from EOF is EOF to the
			// kernel, and a cold torrent would truncate the track for the player.
			return nil, errno
		}
		data, _ := res.Bytes(want)
		got := copy(dest[n:], data)
		res.Done()
		if got < innerN {
			return nil, syscall.EIO
		}
		n += got
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (h *cueHandle) Release(ctx context.Context) syscall.Errno {
	return h.inner.Release(ctx)
}
