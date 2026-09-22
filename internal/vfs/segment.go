package vfs

import "fmt"

// SegmentInodeHash is the hash a file's inode derives from. The cue tracks of one
// image share its torrent file, so the track joins the key and every track gets its
// own inode; a whole file (track 0) keeps the plain hash, video included.
func SegmentInodeHash(hash string, cueTrack int) string {
	if cueTrack <= 0 {
		return hash
	}
	return fmt.Sprintf("%s#c%d", hash, cueTrack)
}

// SegmentRead splits a read of a cue track into its two sources: the part of the
// generated header it covers, and the part of the torrent file behind it. length is
// the image bytes the track spans, base where they start in the torrent file.
func SegmentRead(headerLen int, base, length, off int64, n int) (hdrFrom, hdrTo int, innerOff int64, innerN int) {
	total := int64(headerLen) + length
	if off < 0 || off >= total || n <= 0 {
		return 0, 0, 0, 0
	}
	end := off + int64(n)
	if end > total {
		end = total
	}
	h := int64(headerLen)
	if off < h {
		hdrFrom = int(off)
		hdrTo = int(min(end, h))
	}
	if end > h {
		start := max(off, h)
		innerOff = base + start - h
		innerN = int(end - start)
	}
	return hdrFrom, hdrTo, innerOff, innerN
}
