package notice

import (
	"encoding/binary"
	"math"
)

// Synthetic is a whole notice file for a film of which no byte reached the player: a Matroska
// head with one H.264 SDR video track and one AAC stereo track, then notice Clusters on a 10 s
// Cue grid up to the file's size. It needs nothing of the film.
type Synthetic struct {
	head []byte
	l    *Layout
	size int64
}

const (
	syntheticDur   = 2 * 3600 * 1000 // ms
	syntheticStep  = 10_000          // ms per Cue interval
	minIntervalLen = 256 << 10       // bytes per interval: an IDR, the P-frames and the silence fit
)

// syntheticFormat is the bank clip every player decodes.
var syntheticFormat = Format{Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8, FrameDur: defaultFrameDur, NALLength: 4}

func el(id uint64, body ...[]byte) []byte {
	n := 0
	for _, b := range body {
		n += len(b)
	}
	out := appendElementHeader(nil, id, uint64(n), 0)
	for _, b := range body {
		out = append(out, b...)
	}
	return out
}

func uintEl(id, v uint64) []byte {
	w := 1
	for v>>(8*w) != 0 {
		w++
	}
	return appendUint(nil, id, v, w)
}

func floatEl(id uint64, v float64) []byte {
	b := binary.BigEndian.AppendUint64(nil, math.Float64bits(v))
	return el(id, b)
}

// Matroska IDs used only here.
const (
	idEBMLVersion = 0x4286
	idEBMLReadVer = 0x42F7
	idEBMLMaxID   = 0x42F2
	idEBMLMaxSize = 0x42F3
	idDocType     = 0x4282
	idDocTypeVer  = 0x4287
	idDocTypeRead = 0x4285
	idMuxingApp   = 0x4D80
	idWritingApp  = 0x5741
	idTrackUID    = 0x73C5
	idFlagLacing  = 0x9C
)

// NewSynthetic plans the notice file of size bytes.
func NewSynthetic(size int64) (*Synthetic, error) {
	b, err := loadBank()
	if err != nil {
		return nil, err
	}
	v := b.video(kindOf(syntheticFormat))
	a := b.audioKind(AudioKind{Codec: "aac", Rate: 48000, Channels: 2})
	if v == nil || a == nil {
		return nil, ErrUnsupported
	}
	x := syntheticFormat
	x.Audio = []AudioFormat{{Codec: "aac", Rate: 48000, Channels: 2, ASC: a.Private}}
	c, err := ClipFor(x)
	if err != nil {
		return nil, err
	}
	ebml := el(idEBML, uintEl(idEBMLVersion, 1), uintEl(idEBMLReadVer, 1), uintEl(idEBMLMaxID, 4),
		uintEl(idEBMLMaxSize, 8), el(idDocType, []byte("matroska")), uintEl(idDocTypeVer, 4), uintEl(idDocTypeRead, 2))
	infoOf := func(dur float64) []byte {
		return el(idInfo, uintEl(idTimestampScale, 1_000_000), floatEl(idDuration, dur),
			el(idMuxingApp, []byte("tiramisu")), el(idWritingApp, []byte("tiramisu")))
	}
	info := infoOf(syntheticDur)
	tracks := el(idTracks,
		el(idTrackEntry, uintEl(idTrackNumber, 1), uintEl(idTrackUID, 1), uintEl(idTrackType, 1), uintEl(idFlagLacing, 0),
			el(idCodecID, []byte("V_MPEG4/ISO/AVC")), el(idCodecPrivate, v.Private), uintEl(idDefaultDuration, uint64(x.FrameDur)),
			el(idVideo, uintEl(idPixelWidth, uint64(v.Width)), uintEl(idPixelHeight, uint64(v.Height)))),
		el(idTrackEntry, uintEl(idTrackNumber, 2), uintEl(idTrackUID, 2), uintEl(idTrackType, 2), uintEl(idFlagLacing, 0),
			el(idCodecID, []byte("A_AAC")), el(idCodecPrivate, a.Private),
			el(idAudio, floatEl(idSamplingFreq, 48000), uintEl(idChannels, 2))))
	// SeekHead positions are relative to the Segment data; fixed 4-byte positions keep its size known.
	seek := func(id uint64, pos int) []byte {
		return el(idSeek, el(idSeekID, idBytes(id)), appendUint(nil, idSeekPosition, uint64(pos), 4))
	}
	shLen := len(el(idSeekHead, seek(idInfo, 0), seek(idTracks, 0)))
	sh := el(idSeekHead, seek(idInfo, shLen), seek(idTracks, shLen+len(info)))
	segStart := int64(len(ebml)) + 4 + 8
	head := append(append([]byte(nil), ebml...), idBytes(idSegment)...)
	if size <= segStart {
		return nil, ErrTooSmall
	}
	head = appendSize(head, uint64(size-segStart), 8)
	h := int64(len(head) + len(sh) + len(info) + len(tracks))
	n := min(int64(syntheticDur/syntheticStep), (size-h)/minIntervalLen)
	if n < 1 {
		return nil, ErrTooSmall
	}
	// The Duration is the content's: n intervals (the same 8-byte float, so h does not move).
	head = append(append(append(head, sh...), infoOf(float64(n*syntheticStep))...), tracks...)
	f, err := ParseHead(append(append([]byte(nil), head...), 0x1F, 0x43, 0xB6, 0x75, 0x80))
	if err != nil {
		return nil, err
	}
	step := (size - h) / n
	cues := make([]Cue, n)
	for k := range cues {
		cues[k] = Cue{Pos: h + int64(k)*step, Time: uint64(k) * syntheticStep}
	}
	l, err := NewLayout(f, cues, nil, h, size, step/(syntheticStep/1000), x, c)
	if err != nil {
		return nil, err
	}
	return &Synthetic{head: head, l: l, size: size}, nil
}

// ReadAt fills p with the file's bytes at off; reads past the end are cut.
func (s *Synthetic) ReadAt(p []byte, off int64) (int, error) {
	if off >= s.size {
		return 0, nil
	}
	p = p[:min(int64(len(p)), s.size-off)]
	n := 0
	if h := int64(len(s.head)); off < h {
		n = copy(p, s.head[off:])
	}
	if n == len(p) {
		return n, nil
	}
	k, err := s.l.ReadAt(p[n:], off+int64(n))
	return n + k, err
}
