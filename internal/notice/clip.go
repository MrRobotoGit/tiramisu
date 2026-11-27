package notice

import (
	"encoding/binary"
	"slices"
	"strings"
)

// Clip is one format's notice content, ready to place. Durations are in ns so a cached clip
// serves every film of the format whatever its TimestampScale and track numbers.
type Clip struct {
	FrameDur int64       // ns
	Video    [][]byte    // Video[0] is an IDR; frames in the film's NAL length framing
	Fade     [][]byte    // the first GOP where the notice starts: from black to Video; nil = Video
	Audio    []AudioClip // aligned with Format.Audio; a nil Frame leaves that track without blocks
}

// gop is the fade GOP when asked for and present, else the static one.
func (c *Clip) gop(fade bool) [][]byte {
	if fade && len(c.Fade) > 0 {
		return c.Fade
	}
	return c.Video
}

type AudioClip struct {
	FrameDur float64 // ns
	Frame    []byte  // one silent frame, repeated
}

type Transfer int

const (
	TransferSDR Transfer = iota
	TransferPQ
	TransferHLG
	TransferDV5 // IPTPQc2, full range
)

// Format is everything the encoder must match; its Key names the cache entry.
type Format struct {
	Codec           string // "h264" | "hevc"
	Width, Height   int
	BitDepth        int
	Profile         int    // profile_idc / general_profile_idc
	Level           string // x264/x265 syntax, e.g. "5.1"
	HighTier        bool
	Transfer        Transfer
	Primaries, TRC  string // ffmpeg/x265 names; "" = leave unspecified
	Matrix          string
	FullRange       bool
	MasterDisplay   string // x265 syntax, "" when absent
	MaxCLL, MaxFALL int
	DVProfile       int
	FrameDur        int64 // ns
	NALLength       int
	Audio           []AudioFormat // aligned with Film.Audio()
}

type AudioFormat struct {
	Codec    string // "aac" "ac3" "eac3" "dts" "flac" "opus"; "" = unsupported, track left empty
	Rate     int
	Channels int
	ASC      []byte // AAC AudioSpecificConfig to reproduce
	// FLAC: bits per sample and max block size from STREAMINFO. Opus: OpusHead from the mapping family on.
	BitDepth  int    `json:",omitempty"`
	BlockSize int    `json:",omitempty"`
	OpusMap   []byte `json:",omitempty"`
}

// flacInfo reads STREAMINFO from a FLAC CodecPrivate ("fLaC" + metadata blocks).
func flacInfo(cp []byte) (rate, channels, bps, maxBlock int, ok bool) {
	if len(cp) < 8+18 || string(cp[:4]) != "fLaC" || cp[4]&0x7F != 0 {
		return 0, 0, 0, 0, false
	}
	si := cp[8:]
	maxBlock = int(binary.BigEndian.Uint16(si[2:]))
	rate = int(si[10])<<12 | int(si[11])<<4 | int(si[12]>>4)
	channels = int(si[12]>>1&7) + 1
	bps = int(si[12]&1)<<4 | int(si[13]>>4) + 1
	return rate, channels, bps, maxBlock, true
}

// opusMap is the OpusHead from the channel mapping family on (family, then its table).
func opusMap(cp []byte) []byte {
	if len(cp) < 19 || string(cp[:8]) != "OpusHead" {
		return nil
	}
	return append([]byte(nil), cp[18:]...)
}

var audioCodecs = map[string]string{"A_AC3": "ac3", "A_EAC3": "eac3", "A_DTS": "dts", "A_FLAC": "flac", "A_OPUS": "opus"}

// defaultFrameDur (23.976 fps) stands in for a film whose frame duration cannot be read.
const defaultFrameDur = 41_708_333

// FormatOf derives the clip format from the film's head alone: the colour comes from the SPS VUI
// (CodecPrivate, else the first frame), then the container's Colour element. A video codec other
// than H.264 or HEVC is ErrUnsupported: no clip of the bank can play under its track.
func FormatOf(f *Film, head []byte) (Format, error) {
	v := f.Video()
	if v == nil {
		return Format{}, ErrStructure
	}
	x := Format{Width: int(v.Width), Height: int(v.Height), BitDepth: v.BitDepth(), NALLength: v.NALLength()}
	switch v.CodecID {
	case "V_MPEG4/ISO/AVC":
		x.Codec, x.BitDepth = "h264", 8
	case "V_MPEGH/ISO/HEVC":
		x.Codec = "hevc"
		if x.BitDepth != 8 {
			x.BitDepth = 10
		}
	default:
		return x, ErrUnsupported
	}
	if x.NALLength == 0 {
		x.NALLength = 4
	}
	vui := x.vui(f, v.CodecPrivate, head)
	if !vui.Present {
		vui = VUI{Present: v.Colour.Transfer != 0, Primaries: int(v.Colour.Primaries), TRC: int(v.Colour.Transfer),
			Matrix: int(v.Colour.Matrix)}
	}
	switch {
	case vui.TRC == 16:
		x.Transfer = TransferPQ
	case vui.TRC == 18:
		x.Transfer = TransferHLG
	case !vui.Present && x.BitDepth == 10 && v.Colour.Primaries == 9:
		x.Transfer = TransferPQ // BT.2020 10-bit with no transfer named: HDR10 in practice
	}
	x.FullRange = vui.FullRange
	if v.DV != nil {
		x.DVProfile = v.DV.Profile
		if x.DVProfile == 5 {
			x.Transfer, x.FullRange = TransferDV5, true
		}
	}
	if x.Codec == "h264" || x.BitDepth == 8 {
		x.Transfer = TransferSDR // the bank has no 8-bit HDR clip
	}
	if x.FrameDur = int64(v.DefaultDuration); x.FrameDur == 0 {
		x.FrameDur = FrameDurFromHead(f, head)
	}
	if x.FrameDur < minFrameDurNs || x.FrameDur > maxFrameDurNs {
		x.FrameDur = defaultFrameDur
	}
	for _, a := range f.Audio() {
		af := AudioFormat{Rate: int(a.SampleRate), Channels: int(a.Channels), Codec: audioCodecs[a.CodecID]}
		switch {
		case strings.HasPrefix(a.CodecID, "A_AAC"):
			af.Codec, af.ASC = "aac", a.CodecPrivate
		case af.Codec == "flac":
			af.BitDepth = int(a.AudioBitDepth)
			if _, _, bps, mb, ok := flacInfo(a.CodecPrivate); ok {
				af.BitDepth, af.BlockSize = bps, mb
			}
		case af.Codec == "opus":
			af.OpusMap = opusMap(a.CodecPrivate)
		}
		x.Audio = append(x.Audio, af)
	}
	return x, nil
}

// vui reads the colour of the SPS in the CodecPrivate, else of the first frame in the head.
func (x Format) vui(f *Film, cp, head []byte) VUI {
	sps := spsNAL(x.Codec, cp)
	if sps == nil && f.FirstCluster > 0 && f.FirstCluster < int64(len(head)) {
		walkBlocks(head[f.FirstCluster:], f.Video().Number, func(_ int64, _ bool, fr []byte) bool {
			sps = frameSPS(x.Codec, fr, x.NALLength)
			return false
		})
	}
	if sps == nil {
		return VUI{}
	}
	parse := HEVCVUI
	if x.Codec == "h264" {
		parse = AVCVUI
	}
	v, err := parse(sps)
	if err != nil {
		return VUI{}
	}
	return v
}

// FrameDurFromHead is the median gap between the first video presentation times in the head, in ns.
func FrameDurFromHead(f *Film, head []byte) int64 {
	if f.FirstCluster <= 0 || f.FirstCluster > int64(len(head)) || f.Video() == nil {
		return 0
	}
	var ts []int64
	walkBlocks(head[f.FirstCluster:], f.Video().Number, func(t int64, _ bool, _ []byte) bool {
		ts = append(ts, t)
		return len(ts) < 48
	})
	if len(ts) < 3 {
		return 0
	}
	slices.Sort(ts) // B-frames: decode order is not presentation order
	var d []int64
	for i := 1; i < len(ts); i++ {
		if g := ts[i] - ts[i-1]; g > 0 {
			d = append(d, g)
		}
	}
	if len(d) == 0 {
		return 0
	}
	slices.Sort(d)
	return d[len(d)/2] * int64(f.TimestampScale)
}

// walkBlocks calls fn for each block of track in the Clusters at b[0:], with its absolute
// time (TimestampScale units), keyframe flag and payload; stops when fn returns false or data ends.
func walkBlocks(b []byte, track uint64, fn func(t int64, key bool, frame []byte) bool) {
	for len(b) > 0 {
		id, size, n, ok := readHeader(b)
		if !ok || id != idCluster || size == unknownSize || uint64(len(b)-n) < size {
			return
		}
		var ts int64
		stop := false
		children(b[n:n+int(size)], func(id uint64, c []byte) {
			if stop {
				return
			}
			switch id {
			case idTimestamp:
				ts = int64(beUint(c))
			case idSimpleBlock:
				if tr, rel, flags, data, ok := parseBlock(c); ok && tr == track {
					stop = !fn(ts+rel, flags&0x80 != 0, data)
				}
			case idBlockGroup:
				children(c, func(id uint64, x []byte) {
					if tr, rel, _, data, ok := parseBlock(x); id == idBlock && ok && tr == track && !stop {
						stop = !fn(ts+rel, false, data)
					}
				})
			}
		})
		if stop {
			return
		}
		b = b[n+int(size):]
	}
}

func parseBlock(b []byte) (track uint64, rel int64, flags byte, data []byte, ok bool) {
	l, ok := vintLen(b, 8)
	if !ok || len(b) < l+3 {
		return 0, 0, 0, nil, false
	}
	track = beUint(b[:l]) &^ (uint64(1) << (7 * l))
	rel = int64(int16(binary.BigEndian.Uint16(b[l:])))
	return track, rel, b[l+2], b[l+3:], true
}

// convertNAL rewrites the NAL size prefixes of one frame from `from` to `to` bytes.
func convertNAL(frame []byte, from, to int) ([]byte, error) {
	if from < 1 || from > 4 || to < 1 || to > 4 {
		return nil, ErrStructure
	}
	if from == to {
		return frame, nil
	}
	var out []byte
	for p := 0; p < len(frame); {
		if p+from > len(frame) {
			return nil, ErrStructure
		}
		n := int(beUint(frame[p : p+from]))
		p += from
		if p+n > len(frame) || (to < 4 && n >= 1<<(8*to)) {
			return nil, ErrStructure
		}
		for i := to - 1; i >= 0; i-- {
			out = append(out, byte(n>>(8*i)))
		}
		out = append(out, frame[p:p+n]...)
		p += n
	}
	return out, nil
}
