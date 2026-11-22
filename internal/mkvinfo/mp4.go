package mkvinfo

import (
	"bytes"
	"encoding/binary"
	"time"
)

// maxFilmSeconds bounds a plausible duration; beyond it the mvhd is unknown or not real.
const maxFilmSeconds = 48 * 3600

// MP4Duration returns the movie duration from the mvhd box of an MP4's moov, found anywhere in b:
// the head of a fast-start file, or the tail of one written moov-last. b need not start on a box
// boundary; a "moov" whose box does not fit in b is skipped.
func MP4Duration(b []byte) (time.Duration, bool) {
	for i := 4; i < len(b); {
		j := bytes.Index(b[i:], []byte("moov"))
		if j < 0 {
			return 0, false
		}
		at := i + j
		i = at + 4
		size, hdr := uint64(binary.BigEndian.Uint32(b[at-4:at])), 8
		if size == 1 {
			if at+12 > len(b) {
				continue
			}
			size, hdr = binary.BigEndian.Uint64(b[at+4:at+12]), 16
		}
		start := at - 4
		if size < uint64(hdr) || size > uint64(len(b)-start) {
			continue
		}
		if d, ok := mvhdDuration(b[start+hdr : start+int(size)]); ok {
			return d, true
		}
	}
	return 0, false
}

// mvhdDuration walks moov's children for mvhd: duration in timescale units per second.
func mvhdDuration(p []byte) (time.Duration, bool) {
	for len(p) >= 8 {
		size, hdr := uint64(binary.BigEndian.Uint32(p)), 8
		typ := string(p[4:8])
		if size == 1 {
			if len(p) < 16 {
				return 0, false
			}
			size, hdr = binary.BigEndian.Uint64(p[8:16]), 16
		} else if size == 0 {
			size = uint64(len(p))
		}
		if size < uint64(hdr) || size > uint64(len(p)) {
			return 0, false
		}
		if typ == "mvhd" {
			body := p[hdr:size]
			var scale, dur uint64
			switch {
			case len(body) >= 20 && body[0] == 0:
				scale, dur = uint64(binary.BigEndian.Uint32(body[12:16])), uint64(binary.BigEndian.Uint32(body[16:20]))
				if dur == 0xFFFFFFFF {
					return 0, false // unknown duration
				}
			case len(body) >= 32 && body[0] == 1:
				scale, dur = uint64(binary.BigEndian.Uint32(body[20:24])), binary.BigEndian.Uint64(body[24:32])
			default:
				return 0, false
			}
			if scale == 0 || dur == 0 {
				return 0, false // fragmented MP4 (duration in the fragments) or a bogus box
			}
			sec := float64(dur) / float64(scale)
			if sec > maxFilmSeconds {
				return 0, false // unknown duration (all ones) or not a real mvhd
			}
			return time.Duration(sec * float64(time.Second)), true
		}
		p = p[size:]
	}
	return 0, false
}
