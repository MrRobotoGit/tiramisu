package notice

import (
	"bytes"
	"cmp"
	_ "embed"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"slices"
	"sync"
)

// The bank holds the notice pre-encoded once per video kind and silent audio frames per audio
// configuration (assets/bank.bin, made by `go test -tags notice_bank -run TestGenBank`). Decoders
// accept an in-band SPS at another resolution or level, so one clip per kind serves every film.

//go:embed assets/bank.bin
var bankBin []byte

// VideoKind names one clip of the bank; Height is 720, 1080 or 2160.
type VideoKind struct {
	Codec    string
	Depth    int
	Transfer Transfer
	Height   int
	Width    int // 0: a size class (1280/1920/3840 wide); else an exact coded size
}

// AudioKind names one silent frame of the bank.
type AudioKind struct {
	Codec          string
	Rate, Channels int
	BitDepth       int // FLAC only
}

type bankVideo struct {
	Kind          VideoKind
	Width, Height int
	Private       []byte   // avcC / hvcC
	Frames        [][]byte // one GOP from an IDR, 4-byte NAL lengths
}

type bankAudio struct {
	Kind      AudioKind
	Private   []byte
	BlockSize int // FLAC max block size
	Frame     []byte
	FrameDur  float64 // ns
}

type bankData struct {
	Video []bankVideo
	Audio []bankAudio
}

var (
	bankOnce sync.Once
	bank     bankData
	bankErr  error
)

func loadBank() (*bankData, error) {
	bankOnce.Do(func() {
		bankErr = gob.NewDecoder(bytes.NewReader(bankBin)).Decode(&bank)
	})
	return &bank, bankErr
}

// BankOK loads the bank and checks it holds every kind.
func BankOK() error {
	b, err := loadBank()
	if err != nil {
		return err
	}
	for _, k := range bankKinds() {
		if b.video(k) == nil {
			return fmt.Errorf("notice: bank lacks %+v", k)
		}
	}
	return nil
}

// bankKinds is what the bank must hold (the generator encodes exactly these).
func bankKinds() []VideoKind {
	var ks []VideoKind
	for _, h := range []int{720, 1080, 2160} {
		ks = append(ks, VideoKind{Codec: "h264", Depth: 8, Height: h}, VideoKind{Codec: "hevc", Depth: 8, Height: h})
		for _, tr := range []Transfer{TransferSDR, TransferPQ, TransferHLG, TransferDV5} {
			ks = append(ks, VideoKind{Codec: "hevc", Depth: 10, Transfer: tr, Height: h})
		}
	}
	return append(ks, bankExact...)
}

// bankExact are clips coded at a film's exact size: a player that ignores the SPS cropping
// (Plex for iOS) shows a size-class clip off centre. The sizes are the library's (coded sizes of
// the SSD heads on the Pi, `-tags notice_scan`, 2026-11-27) that no size class already has.
var bankExact = exactKinds(map[VideoKind][]string{
	{Codec: "h264", Depth: 8}: {"1792x1088", "1920x1040", "1920x800", "1920x816", "1920x880", "1920x960"},
	{Codec: "hevc", Depth: 8}: {"3840x1600", "3840x1920"},
	{Codec: "hevc", Depth: 10}: {"1920x1040", "1920x800", "1920x808", "1920x816", "1920x960", "3584x2144",
		"3840x1608", "3840x1920", "3840x2080"},
	{Codec: "hevc", Depth: 10, Transfer: TransferPQ}: {"3832x1600", "3832x1736", "3832x1912", "3832x1920", "3840x1600",
		"3840x1608", "3840x1752", "3840x1808", "3840x1920", "3840x2080"},
	{Codec: "hevc", Depth: 10, Transfer: TransferDV5}: {"3840x1608"},
})

func exactKinds(m map[VideoKind][]string) []VideoKind {
	var ks []VideoKind
	for k, sizes := range m {
		for _, sz := range sizes {
			if _, err := fmt.Sscanf(sz, "%dx%d", &k.Width, &k.Height); err == nil {
				ks = append(ks, k)
			}
		}
	}
	slices.SortFunc(ks, func(a, b VideoKind) int {
		return cmp.Or(cmp.Compare(a.Codec, b.Codec), cmp.Compare(a.Depth, b.Depth), cmp.Compare(a.Transfer, b.Transfer),
			cmp.Compare(a.Width, b.Width), cmp.Compare(a.Height, b.Height))
	})
	return ks
}

// codedSize is the film's coded picture: macroblocks of 16 (H.264), coding blocks of 8 (HEVC).
func codedSize(x Format) (w, h int) {
	a := 16
	if x.Codec == "hevc" {
		a = 8
	}
	return (x.Width + a - 1) / a * a, (x.Height + a - 1) / a * a
}

func bankAudioKinds() []AudioKind {
	var ks []AudioKind
	for _, r := range []int{48000, 44100} {
		for _, ch := range []int{1, 2, 6, 8} {
			ks = append(ks, AudioKind{"aac", r, ch, 0})
		}
		for _, ch := range []int{1, 2, 6} {
			ks = append(ks, AudioKind{"ac3", r, ch, 0})
		}
		for _, ch := range []int{1, 2, 6, 8} {
			ks = append(ks, AudioKind{"eac3", r, ch, 0}, AudioKind{"flac", r, ch, 16}, AudioKind{"flac", r, ch, 24})
		}
	}
	ks = append(ks, AudioKind{"dts", 48000, 2, 0}, AudioKind{"dts", 48000, 6, 0},
		AudioKind{"opus", 48000, 2, 0}, AudioKind{"opus", 48000, 6, 0})
	return ks
}

// kindOf is the bank clip for a film's format.
func kindOf(x Format) VideoKind {
	// The smallest clip that holds the film: its SPS is then cropped to the film's size.
	k := VideoKind{Codec: x.Codec, Depth: x.BitDepth, Transfer: x.Transfer, Height: 2160}
	switch {
	case x.Width <= 1280 && x.Height <= 720:
		k.Height = 720
	case x.Width <= 1920 && x.Height <= 1088:
		k.Height = 1080
	}
	if k.Codec == "h264" {
		k.Depth, k.Transfer = 8, TransferSDR
	}
	return k
}

func (b *bankData) video(k VideoKind) *bankVideo {
	for i := range b.Video {
		if b.Video[i].Kind == k {
			return &b.Video[i]
		}
	}
	return nil
}

func (b *bankData) audioKind(k AudioKind) *bankAudio {
	for i := range b.Audio {
		if b.Audio[i].Kind == k {
			return &b.Audio[i]
		}
	}
	return nil
}

// audio is the bank frame that can play under the film's track, nil when none matches.
func (b *bankData) audio(a AudioFormat) *bankAudio {
	for i := range b.Audio {
		e := &b.Audio[i]
		k := e.Kind
		if k.Codec != a.Codec || k.Rate != a.Rate || k.Channels != a.Channels {
			continue
		}
		switch a.Codec {
		case "aac":
			if !sameAAC(a.ASC, e.Private) {
				continue
			}
		case "flac":
			if k.BitDepth != a.BitDepth || a.BlockSize != 0 && e.BlockSize > a.BlockSize {
				continue
			}
		case "opus":
			if !bytes.Equal(a.OpusMap, opusMap(e.Private)) {
				continue
			}
		}
		return e
	}
	return nil
}

// ClipFor builds the clip for a film's format from the bank: the GOP cut to ten seconds at the
// film's frame duration, NAL lengths converted, the DV RPU appended, and one silent frame per
// audio track the bank can play (nil leaves the track without blocks: TrueHD, HE-AAC…).
func ClipFor(x Format) (*Clip, error) {
	b, err := loadBank()
	if err != nil {
		return nil, fmt.Errorf("notice: bank: %w", err)
	}
	k := kindOf(x)
	k.Width, k.Height = codedSize(x)
	v := b.video(k) // a clip at the film's exact coded size, else its size class
	if v == nil {
		v = b.video(kindOf(x))
	}
	if v == nil {
		return nil, ErrUnsupported
	}
	n := int((gopNs + x.FrameDur/2) / x.FrameDur)
	n = max(min(n, len(v.Frames)), 1)
	c := &Clip{FrameDur: x.FrameDur, Video: make([][]byte, 0, n)}
	for _, fr := range v.Frames[:n] {
		out, err := convertNAL(cropFrame(fr, x.Codec, x.Width, x.Height), 4, x.NALLength)
		if err != nil {
			return nil, err
		}
		if x.DVProfile != 0 {
			out = AppendRPU(out, x.NALLength, RPUFor(x.DVProfile))
		}
		c.Video = append(c.Video, out)
	}
	for _, a := range x.Audio {
		var ac AudioClip
		if e := b.audio(a); e != nil {
			ac = AudioClip{FrameDur: e.FrameDur, Frame: e.Frame}
		}
		c.Audio = append(c.Audio, ac)
	}
	return c, nil
}

// gopNs is the clip's GOP: one IDR every ten seconds.
const gopNs = 10_000_000_000

// sameAAC: the film's AudioSpecificConfig and the bank's describe the same LC stream. ffmpeg
// appends the explicit "no SBR" extension (56 E5 00) that most films leave out; any other
// extension (HE-AAC, PS) must match byte for byte.
func sameAAC(film, bank []byte) bool {
	if len(film) < 2 || len(bank) < 2 || !bytes.Equal(film[:2], bank[:2]) {
		return false
	}
	return len(film) == 2 || bytes.Equal(film, bank)
}

// cropFrame rewrites the SPS of an access unit (4-byte NAL lengths) so the visible picture is
// the film's w×h, centred in the clip: a player keeps the film's geometry for the notice.
func cropFrame(fr []byte, codec string, w, h int) []byte {
	var out []byte
	changed := false
	for p := 0; p+4 <= len(fr); {
		n := int(binary.BigEndian.Uint32(fr[p:]))
		if p+4+n > len(fr) || n == 0 {
			return fr
		}
		nal := fr[p+4 : p+4+n]
		if c, ok := CropSPS(codec, nal, w, h); ok && (codec == "h264" && nal[0]&0x1F == 7 || codec == "hevc" && nal[0]>>1&0x3F == 33) {
			nal, changed = c, true
		}
		out = binary.BigEndian.AppendUint32(out, uint32(len(nal)))
		out = append(out, nal...)
		p += 4 + n
	}
	if !changed {
		return fr
	}
	return out
}
