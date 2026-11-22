// Package mkvinfo reads the duration of a film (Matroska header, or an MP4's mvhd in its head or
// tail), so the stall-risk estimate can measure the buffer in seconds of content (the film's
// bitrate), not in the pump's drain, which follows the player's buffering.
package mkvinfo

import (
	"encoding/binary"
	"math"
	"time"
)

const (
	idEBML          = 0x1A45DFA3
	idSegment       = 0x18538067
	idInfo          = 0x1549A966
	idTimecodeScale = 0x2AD7B1
	idDuration      = 0x4489
	idCluster       = 0x1F43B675
	defaultScale    = 1_000_000 // ns per timecode unit when TimecodeScale is absent
)

// Duration returns the Segment duration found in head, the first bytes of the file. Info sits
// before the first Cluster, so the head warmup always holds it.
func Duration(head []byte) (time.Duration, bool) {
	id, size, n, ok := element(head)
	if !ok || id != idEBML {
		return 0, false
	}
	p := head[n+int(min(size, uint64(len(head)-n))):]
	id, _, n, ok = element(p)
	if !ok || id != idSegment {
		return 0, false
	}
	p = p[n:] // Segment children; its size may be unknown, the head bounds the walk
	for len(p) > 0 {
		id, size, n, ok = element(p)
		if !ok || id == idCluster {
			return 0, false
		}
		end := n + int(min(size, uint64(len(p)-n)))
		if id == idInfo {
			if uint64(end-n) < size {
				return 0, false // Info cut by the end of head
			}
			return infoDuration(p[n:end])
		}
		p = p[end:]
	}
	return 0, false
}

// Bitrate is the file's average bytes per second of content: size over Duration.
func Bitrate(head []byte, size int64) (int64, bool) {
	return BitrateOf(head, nil, size)
}

// BitrateOf is size over the duration read from head (Matroska, or a fast-start MP4) or from
// tail (an MP4 written moov-last). Some ".mkv" files are MP4 inside.
func BitrateOf(head, tail []byte, size int64) (int64, bool) {
	d, ok := Duration(head)
	if !ok {
		d, ok = MP4Duration(head)
	}
	if !ok {
		d, ok = MP4Duration(tail)
	}
	if !ok || size <= 0 || d <= 0 {
		return 0, false
	}
	return int64(float64(size) / d.Seconds()), true
}

func infoDuration(p []byte) (time.Duration, bool) {
	scale := uint64(defaultScale)
	var dur float64
	for len(p) > 0 {
		id, size, n, ok := element(p)
		if !ok || uint64(len(p)-n) < size {
			return 0, false
		}
		body := p[n : n+int(size)]
		switch id {
		case idTimecodeScale:
			if v := beUint(body); v > 0 {
				scale = v
			}
		case idDuration:
			switch len(body) {
			case 4:
				dur = float64(math.Float32frombits(binary.BigEndian.Uint32(body)))
			case 8:
				dur = math.Float64frombits(binary.BigEndian.Uint64(body))
			}
		}
		p = p[n+int(size):]
	}
	ns := dur * float64(scale)
	if !(ns > 0) || ns > float64(math.MaxInt64) {
		return 0, false
	}
	return time.Duration(ns), true
}

// element decodes an EBML element header: ID (marker bits kept), data size, header length.
func element(p []byte) (id uint64, size uint64, n int, ok bool) {
	idLen, ok := vintLen(p, 4)
	if !ok {
		return 0, 0, 0, false
	}
	for _, b := range p[:idLen] {
		id = id<<8 | uint64(b)
	}
	sLen, ok := vintLen(p[idLen:], 8)
	if !ok {
		return 0, 0, 0, false
	}
	s := p[idLen : idLen+sLen]
	size = uint64(s[0] & (0xFF >> sLen))
	unknown := size == uint64(0xFF>>sLen)
	for _, b := range s[1:] {
		size = size<<8 | uint64(b)
		unknown = unknown && b == 0xFF
	}
	if unknown {
		size = math.MaxUint64
	}
	return id, size, idLen + sLen, true
}

// vintLen is the length of the variable-size integer at p[0], at most max bytes.
func vintLen(p []byte, max int) (int, bool) {
	if len(p) == 0 || p[0] == 0 {
		return 0, false
	}
	l := 1
	for mask := byte(0x80); p[0]&mask == 0; mask >>= 1 {
		l++
	}
	if l > max || len(p) < l {
		return 0, false
	}
	return l, true
}

func beUint(b []byte) uint64 {
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v
}
