// Package notice turns the not-yet-played part of a Matroska film into a same-format MKV that
// shows the stall-notice screen. Pure library: the FUSE read path calls Layout.ReadAt.
package notice

import (
	"errors"
	"math"
)

const (
	idEBML            = 0x1A45DFA3
	idSegment         = 0x18538067
	idSeekHead        = 0x114D9B74
	idSeek            = 0x4DBB
	idSeekID          = 0x53AB
	idSeekPosition    = 0x53AC
	idInfo            = 0x1549A966
	idTimestampScale  = 0x2AD7B1
	idDuration        = 0x4489
	idTracks          = 0x1654AE6B
	idTrackEntry      = 0xAE
	idTrackNumber     = 0xD7
	idTrackType       = 0x83
	idFlagDefault     = 0x88
	idCodecID         = 0x86
	idCodecPrivate    = 0x63A2
	idDefaultDuration = 0x23E383
	idVideo           = 0xE0
	idPixelWidth      = 0xB0
	idPixelHeight     = 0xBA
	idColour          = 0x55B0
	idMatrixCoeffs    = 0x55B1
	idRange           = 0x55B9
	idTransfer        = 0x55BA
	idPrimaries       = 0x55BB
	idMaxCLL          = 0x55BC
	idMaxFALL         = 0x55BD
	idMastering       = 0x55D0
	idAudio           = 0xE1
	idSamplingFreq    = 0xB5
	idChannels        = 0x9F
	idBitDepth        = 0x6264
	idBlockAddMapping = 0x41E4
	idBlockAddIDType  = 0x41E7
	idBlockAddExtra   = 0x41ED
	idCues            = 0x1C53BB6B
	idCuePoint        = 0xBB
	idCueTime         = 0xB3
	idCueTrackPos     = 0xB7
	idCueClusterPos   = 0xF1
	idCluster         = 0x1F43B675
	idTimestamp       = 0xE7
	idSimpleBlock     = 0xA3
	idBlockGroup      = 0xA0
	idBlock           = 0xA1
	idVoid            = 0xEC
	idTags            = 0x1254C367
	idChapters        = 0x1043A770
	idAttachments     = 0x1941A469

	idCueTrack = 0xF7

	unknownSize = math.MaxUint64
)

var (
	ErrStructure   = errors.New("notice: unsupported or truncated MKV structure")
	ErrTooSmall    = errors.New("notice: synthetic content does not fit its byte range")
	ErrUnsupported = errors.New("notice: format not supported")
)

// readHeader decodes an element header: ID (marker bits kept), data size, header length.
func readHeader(p []byte) (id, size uint64, n int, ok bool) {
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
	all := size == uint64(0xFF>>sLen)
	for _, b := range s[1:] {
		size = size<<8 | uint64(b)
		all = all && b == 0xFF
	}
	if all {
		size = unknownSize
	}
	return id, size, idLen + sLen, true
}

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

func idBytes(id uint64) []byte {
	switch {
	case id > 0xFFFFFF:
		return []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	case id > 0xFFFF:
		return []byte{byte(id >> 16), byte(id >> 8), byte(id)}
	case id > 0xFF:
		return []byte{byte(id >> 8), byte(id)}
	}
	return []byte{byte(id)}
}

// sizeLen is the shortest vint length that holds v (all-ones is reserved for "unknown").
func sizeLen(v uint64) int {
	for l := 1; l <= 8; l++ {
		if v < (uint64(1)<<(7*l))-1 {
			return l
		}
	}
	return 8
}

// appendSize writes v as a vint of exactly width bytes (0 = shortest).
func appendSize(b []byte, v uint64, width int) []byte {
	if width == 0 {
		width = sizeLen(v)
	}
	v |= uint64(1) << (7 * width) // marker bit
	for i := width - 1; i >= 0; i-- {
		b = append(b, byte(v>>(8*i)))
	}
	return b
}

func appendElementHeader(b []byte, id, size uint64, width int) []byte {
	return appendSize(append(b, idBytes(id)...), size, width)
}

func appendUint(b []byte, id, v uint64, width int) []byte {
	b = appendElementHeader(b, id, uint64(width), 0)
	for i := width - 1; i >= 0; i-- {
		b = append(b, byte(v>>(8*i)))
	}
	return b
}

// appendVoid writes a Void element of exactly total bytes (total >= 2).
func appendVoid(b []byte, total int) []byte {
	for w := 1; w <= 8; w++ {
		body := total - 1 - w
		if body >= 0 && sizeLen(uint64(body)) <= w {
			b = appendElementHeader(b, idVoid, uint64(body), w)
			return append(b, make([]byte, body)...)
		}
	}
	panic("notice: void too small")
}

func beUint(b []byte) uint64 {
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v
}
