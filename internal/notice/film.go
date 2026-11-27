package notice

import (
	"encoding/binary"
	"math"
)

type Colour struct {
	Matrix, Range, Transfer, Primaries uint64 // Matroska/ISO 23091-2 codes; 0 = absent
	MaxCLL, MaxFALL                    uint64
	Mastering                          []byte // raw MasteringMetadata body, for the format key
}

type DVConfig struct {
	Profile, Level int
	BLCompat       int // dv_bl_signal_compatibility_id
	Raw            []byte
}

type Track struct {
	Number          uint64
	Type            uint64 // 1 video, 2 audio, 17 subtitle
	Default         bool
	CodecID         string
	CodecPrivate    []byte
	DefaultDuration uint64 // ns per frame, 0 = absent
	Width, Height   uint64
	Colour          Colour
	DV              *DVConfig
	SampleRate      float64
	Channels        uint64
	AudioBitDepth   uint64
}

type Film struct {
	SegData        int64 // absolute offset of the Segment's data
	TimestampScale uint64
	Duration       float64 // in TimestampScale units
	Tracks         []Track
	FirstCluster   int64
	CuesPos        int64   // absolute, 0 = unknown
	TailElems      []int64 // absolute starts of Cues/Tags/Chapters/Attachments from SeekHead
	HasSeekHead    bool
	SeekHeads      []int64 // absolute positions of further SeekHeads (mkvmerge puts one at the end)
}

func (f *Film) Video() *Track {
	for i := range f.Tracks {
		if f.Tracks[i].Type == 1 {
			return &f.Tracks[i]
		}
	}
	return nil
}

func (f *Film) Audio() []Track {
	var a []Track
	for _, t := range f.Tracks {
		if t.Type == 2 {
			a = append(a, t)
		}
	}
	return a
}

// NALLength is the NAL size-prefix length declared in avcC/hvcC (0 if unknown).
func (t *Track) NALLength() int {
	p := t.CodecPrivate
	switch t.CodecID {
	case "V_MPEG4/ISO/AVC":
		if len(p) > 4 {
			return int(p[4]&3) + 1
		}
	case "V_MPEGH/ISO/HEVC":
		if len(p) > 21 {
			return int(p[21]&3) + 1
		}
	}
	return 0
}

func (t *Track) BitDepth() int {
	p := t.CodecPrivate
	if t.CodecID == "V_MPEGH/ISO/HEVC" && len(p) > 17 {
		return int(p[17]&7) + 8
	}
	return 8
}

// IsMatroska reports an EBML header at the start of b.
func IsMatroska(b []byte) bool { return len(b) >= 4 && beUint(b[:4]) == idEBML }

// IsMP4 reports an ftyp box at the start of b.
func IsMP4(b []byte) bool { return len(b) >= 8 && string(b[4:8]) == "ftyp" }

// ParseHead reads EBML header, SeekHead, Info and Tracks from the first bytes of a film.
func ParseHead(head []byte) (*Film, error) {
	if len(head) >= 4 && !IsMatroska(head) {
		return nil, ErrUnsupported // not Matroska (an MP4 named .mkv)
	}
	f, err := parseHead(head)
	if err == nil && f.Video() == nil {
		return nil, ErrStructure
	}
	return f, err
}

// parseHead is ParseHead without the video requirement (the encoder's audio-only clips).
func parseHead(head []byte) (*Film, error) {
	id, size, n, ok := readHeader(head)
	if !ok || id != idEBML || size == unknownSize {
		return nil, ErrStructure
	}
	off := int64(n) + int64(size)
	if off >= int64(len(head)) {
		return nil, ErrStructure
	}
	id, _, n, ok = readHeader(head[off:])
	if !ok || id != idSegment {
		return nil, ErrStructure
	}
	f := &Film{SegData: off + int64(n), TimestampScale: 1_000_000}
	gotTracks := false
	for p := f.SegData; p < int64(len(head)); {
		id, size, n, ok := readHeader(head[p:])
		if !ok {
			break
		}
		if id == idCluster {
			f.FirstCluster = p
			break
		}
		end := p + int64(n) + int64(size)
		if size == unknownSize || end > int64(len(head)) {
			return nil, ErrStructure
		}
		body := head[p+int64(n) : end]
		switch id {
		case idSeekHead:
			f.HasSeekHead = true
			f.parseSeekHead(body)
		case idInfo:
			f.parseInfo(body)
		case idTracks:
			if err := f.parseTracks(body); err != nil {
				return nil, err
			}
			gotTracks = true
		}
		p = end
	}
	if !gotTracks || f.FirstCluster == 0 {
		return nil, ErrStructure
	}
	return f, nil
}

// children iterates the direct children of a master body; stops on malformed data.
func children(body []byte, fn func(id uint64, b []byte)) {
	for len(body) > 0 {
		id, size, n, ok := readHeader(body)
		if !ok || size == unknownSize || uint64(len(body)-n) < size {
			return
		}
		fn(id, body[n:n+int(size)])
		body = body[n+int(size):]
	}
}

// ParseSeekHeadAt reads a further SeekHead found at b[0] (from the tail) into f.
func (f *Film) ParseSeekHeadAt(b []byte) error {
	id, size, n, ok := readHeader(b)
	if !ok || id != idSeekHead || size == unknownSize || uint64(len(b)-n) < size {
		return ErrStructure
	}
	f.parseSeekHead(b[n : n+int(size)])
	return nil
}

func (f *Film) parseSeekHead(b []byte) {
	children(b, func(id uint64, s []byte) {
		if id != idSeek {
			return
		}
		var target, pos uint64
		children(s, func(id uint64, v []byte) {
			switch id {
			case idSeekID:
				target = beUint(v)
			case idSeekPosition:
				pos = beUint(v)
			}
		})
		abs := f.SegData + int64(pos)
		switch target {
		case idCues:
			f.CuesPos = abs
			f.TailElems = append(f.TailElems, abs)
		case idTags, idChapters, idAttachments:
			f.TailElems = append(f.TailElems, abs)
		case idSeekHead:
			f.SeekHeads = append(f.SeekHeads, abs)
		}
	})
}

func (f *Film) parseInfo(b []byte) {
	children(b, func(id uint64, v []byte) {
		switch id {
		case idTimestampScale:
			if s := beUint(v); s > 0 {
				f.TimestampScale = s
			}
		case idDuration:
			f.Duration = beFloat(v)
		}
	})
}

func beFloat(v []byte) float64 {
	switch len(v) {
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(v)))
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(v))
	}
	return 0
}

func (f *Film) parseTracks(b []byte) error {
	children(b, func(id uint64, e []byte) {
		if id != idTrackEntry {
			return
		}
		t := Track{Default: true}
		children(e, func(id uint64, v []byte) {
			switch id {
			case idTrackNumber:
				t.Number = beUint(v)
			case idTrackType:
				t.Type = beUint(v)
			case idFlagDefault:
				t.Default = beUint(v) == 1
			case idCodecID:
				t.CodecID = string(v)
			case idCodecPrivate:
				t.CodecPrivate = append([]byte(nil), v...)
			case idDefaultDuration:
				t.DefaultDuration = beUint(v)
			case idVideo:
				parseVideo(&t, v)
			case idAudio:
				children(v, func(id uint64, a []byte) {
					switch id {
					case idSamplingFreq:
						t.SampleRate = beFloat(a)
					case idChannels:
						t.Channels = beUint(a)
					case idBitDepth:
						t.AudioBitDepth = beUint(a)
					}
				})
			case idBlockAddMapping:
				parseDV(&t, v)
			}
		})
		if t.Channels == 0 && t.Type == 2 {
			t.Channels = 1
		}
		f.Tracks = append(f.Tracks, t)
	})
	if len(f.Tracks) == 0 {
		return ErrStructure
	}
	return nil
}

func parseVideo(t *Track, v []byte) {
	children(v, func(id uint64, x []byte) {
		switch id {
		case idPixelWidth:
			t.Width = beUint(x)
		case idPixelHeight:
			t.Height = beUint(x)
		case idColour:
			children(x, func(id uint64, c []byte) {
				switch id {
				case idMatrixCoeffs:
					t.Colour.Matrix = beUint(c)
				case idRange:
					t.Colour.Range = beUint(c)
				case idTransfer:
					t.Colour.Transfer = beUint(c)
				case idPrimaries:
					t.Colour.Primaries = beUint(c)
				case idMaxCLL:
					t.Colour.MaxCLL = beUint(c)
				case idMaxFALL:
					t.Colour.MaxFALL = beUint(c)
				case idMastering:
					t.Colour.Mastering = append([]byte(nil), c...)
				}
			})
		}
	})
}

// parseDV reads a dvcC/dvvC BlockAdditionMapping (Dolby Vision configuration record).
func parseDV(t *Track, v []byte) {
	var typ uint64
	var extra []byte
	children(v, func(id uint64, x []byte) {
		switch id {
		case idBlockAddIDType:
			typ = beUint(x)
		case idBlockAddExtra:
			extra = x
		}
	})
	const dvcC, dvvC = 0x64766343, 0x64767643
	if (typ != dvcC && typ != dvvC) || len(extra) < 5 {
		return
	}
	t.DV = &DVConfig{
		Profile:  int(extra[2] >> 1),
		Level:    int((extra[2]&1)<<5 | extra[3]>>3),
		BLCompat: int(extra[4] >> 4),
		Raw:      append([]byte(nil), extra...),
	}
}
