// Package flacsplit cuts a track out of a single-file FLAC image without decoding:
// the track is a range of whole frames of the image behind a generated header.
package flacsplit

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MaxHeaderSize bounds a generated header: tags are caller data.
const MaxHeaderSize = 64 << 10

// window is how much of the image one probe reads. It must hold two whole frames at
// the largest block size a lossless rip uses.
const window = 256 << 10

// maxProbes bounds the reads spent finding one boundary.
const maxProbes = 8

var (
	ErrNotFLAC          = errors.New("flacsplit: not a FLAC stream")
	ErrBoundaryNotFound = errors.New("flacsplit: frame boundary not found")
	ErrHeaderTooLarge   = errors.New("flacsplit: generated header too large")
)

// StreamInfo is the image's STREAMINFO block, raw and decoded.
type StreamInfo struct {
	Raw           [34]byte
	MinBlock      int
	MaxBlock      int
	Rate          int
	Channels      int
	BitsPerSample int
	TotalSamples  int64
}

// Image is an opened FLAC image: its STREAMINFO and where its frames start.
type Image struct {
	Info       StreamInfo
	AudioStart int64
	Size       int64
	r          io.ReaderAt
}

// Tag is one Vorbis comment, written in the order given.
type Tag struct{ Key, Value string }

// Open reads the metadata blocks of a FLAC image of the given size.
func Open(r io.ReaderAt, size int64) (*Image, error) {
	head := make([]byte, 4)
	if _, err := r.ReadAt(head, 0); err != nil {
		return nil, fmt.Errorf("flacsplit: read marker: %w", err)
	}
	if string(head) != "fLaC" {
		return nil, ErrNotFLAC
	}
	im := &Image{Size: size, r: r}
	pos := int64(4)
	sawInfo := false
	for {
		hdr := make([]byte, 4)
		if _, err := r.ReadAt(hdr, pos); err != nil {
			return nil, fmt.Errorf("flacsplit: read block header: %w", err)
		}
		last := hdr[0]&0x80 != 0
		length := int64(hdr[1])<<16 | int64(hdr[2])<<8 | int64(hdr[3])
		if hdr[0]&0x7f == 0 {
			if length != 34 {
				return nil, fmt.Errorf("%w: STREAMINFO of %d bytes", ErrNotFLAC, length)
			}
			if _, err := r.ReadAt(im.Info.Raw[:], pos+4); err != nil {
				return nil, fmt.Errorf("flacsplit: read STREAMINFO: %w", err)
			}
			im.Info.decode()
			sawInfo = true
		}
		pos += 4 + length
		if last {
			break
		}
		if pos >= size {
			return nil, fmt.Errorf("%w: metadata runs past the end", ErrNotFLAC)
		}
	}
	if !sawInfo || im.Info.Rate == 0 || im.Info.TotalSamples == 0 || im.Info.MinBlock < 16 {
		return nil, fmt.Errorf("%w: unusable STREAMINFO", ErrNotFLAC)
	}
	im.AudioStart = pos
	return im, nil
}

func (s *StreamInfo) decode() {
	b := s.Raw[:]
	s.MinBlock = int(binary.BigEndian.Uint16(b[0:2]))
	s.MaxBlock = int(binary.BigEndian.Uint16(b[2:4]))
	v := binary.BigEndian.Uint64(b[10:18])
	s.Rate = int(v >> 44)
	s.Channels = int((v>>41)&0x7) + 1
	s.BitsPerSample = int((v>>36)&0x1f) + 1
	s.TotalSamples = int64(v & 0xfffffffff)
}

// frame is one validated frame header.
type frame struct {
	sample int64 // first sample
	block  int   // samples in the frame
}

// parseFrame reads a frame header at b[i:]. A header must pass its CRC-8 and agree
// with STREAMINFO: a random byte run in the audio passes the CRC alone one time in
// 256.
func (im *Image) parseFrame(b []byte, i int) (frame, bool) {
	if i+16 > len(b) || b[i] != 0xFF || b[i+1]&0xFE != 0xF8 {
		return frame{}, false
	}
	variable := b[i+1]&1 == 1
	bsCode, srCode := int(b[i+2]>>4), int(b[i+2]&0xF)
	chCode, ssCode := int(b[i+3]>>4), int((b[i+3]>>1)&7)
	if b[i+3]&1 != 0 || bsCode == 0 || srCode == 0xF || chCode > 10 || ssCode == 3 {
		return frame{}, false
	}
	j := i + 4
	number, n, ok := readUTF8Number(b[j:])
	if !ok {
		return frame{}, false
	}
	j += n
	block := 0
	switch {
	case bsCode == 1:
		block = 192
	case bsCode >= 2 && bsCode <= 5:
		block = 576 << (bsCode - 2)
	case bsCode == 6:
		if j+1 > len(b) {
			return frame{}, false
		}
		block = int(b[j]) + 1
		j++
	case bsCode == 7:
		if j+2 > len(b) {
			return frame{}, false
		}
		block = int(binary.BigEndian.Uint16(b[j:j+2])) + 1
		j += 2
	default:
		block = 256 << (bsCode - 8)
	}
	rate := 0
	switch srCode {
	case 0:
		rate = im.Info.Rate
	case 1:
		rate = 88200
	case 2:
		rate = 176400
	case 3:
		rate = 192000
	case 4:
		rate = 8000
	case 5:
		rate = 16000
	case 6:
		rate = 22050
	case 7:
		rate = 24000
	case 8:
		rate = 32000
	case 9:
		rate = 44100
	case 10:
		rate = 48000
	case 11:
		rate = 96000
	case 12:
		rate = int(b[j]) * 1000
		j++
	case 13:
		rate = int(binary.BigEndian.Uint16(b[j : j+2]))
		j += 2
	case 14:
		rate = int(binary.BigEndian.Uint16(b[j:j+2])) * 10
		j += 2
	}
	if j >= len(b) || crc8(b[i:j]) != b[j] {
		return frame{}, false
	}
	if rate != im.Info.Rate {
		return frame{}, false
	}
	channels := chCode + 1
	if chCode >= 8 {
		channels = 2
	}
	if channels != im.Info.Channels {
		return frame{}, false
	}
	if bps := [8]int{0, 8, 12, 0, 16, 20, 24, 32}[ssCode]; bps != 0 && bps != im.Info.BitsPerSample {
		return frame{}, false
	}
	if block > im.Info.MaxBlock && im.Info.MaxBlock > 0 {
		return frame{}, false
	}
	if variable {
		return frame{sample: number, block: block}, true
	}
	// Fixed-blocksize streams number frames; only the last frame may be shorter.
	return frame{sample: number * int64(im.Info.MinBlock), block: block}, true
}

// readUTF8Number decodes FLAC's extended UTF-8 frame/sample number.
func readUTF8Number(b []byte) (int64, int, bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	first := b[0]
	var extra int
	var v int64
	switch {
	case first&0x80 == 0:
		return int64(first), 1, true
	case first&0xE0 == 0xC0:
		extra, v = 1, int64(first&0x1F)
	case first&0xF0 == 0xE0:
		extra, v = 2, int64(first&0x0F)
	case first&0xF8 == 0xF0:
		extra, v = 3, int64(first&0x07)
	case first&0xFC == 0xF8:
		extra, v = 4, int64(first&0x03)
	case first&0xFE == 0xFC:
		extra, v = 5, int64(first&0x01)
	case first == 0xFE:
		extra, v = 6, 0
	default:
		return 0, 0, false
	}
	if len(b) < 1+extra {
		return 0, 0, false
	}
	for k := 1; k <= extra; k++ {
		if b[k]&0xC0 != 0x80 {
			return 0, 0, false
		}
		v = v<<6 | int64(b[k]&0x3F)
	}
	return v, 1 + extra, true
}

func crc8(b []byte) byte {
	var c byte
	for _, x := range b {
		c ^= x
		for i := 0; i < 8; i++ {
			if c&0x80 != 0 {
				c = c<<1 ^ 0x07
			} else {
				c <<= 1
			}
		}
	}
	return c
}

// maxFrameScan bounds how far past a frame its successor is looked for: larger than
// any frame a lossless rip produces.
const maxFrameScan = 128 << 10

// successor finds the frame that continues the one at b[i:]: the next header carrying
// exactly sample+block. Headers with any other number are skipped, not trusted: a byte
// run inside the audio can pass for a header, CRC included.
func (im *Image) successor(b []byte, i int, f frame) (int, frame, bool) {
	want := f.sample + int64(f.block)
	end := min(len(b)-16, i+maxFrameScan)
	for k := i + 16; k < end; k++ {
		if next, ok := im.parseFrame(b, k); ok && next.sample == want {
			return k, next, true
		}
	}
	return 0, frame{}, false
}

// confirmed reports whether a header at b[i:] is a real frame: its successor is in
// the window, or it is the last frame and ends the stream.
func (im *Image) confirmed(b []byte, i int, f frame) bool {
	if f.sample+int64(f.block) >= im.Info.TotalSamples {
		return true
	}
	_, _, ok := im.successor(b, i, f)
	return ok
}

// Boundary returns the byte offset of the first frame starting at or after sample,
// and that frame's first sample. A sample at or past the end answers the end of the
// image and the total sample count.
func (im *Image) Boundary(sample int64) (int64, int64, error) {
	if sample <= 0 {
		return im.AudioStart, 0, nil
	}
	if sample >= im.Info.TotalSamples {
		return im.Size, im.Info.TotalSamples, nil
	}
	audioBytes := float64(im.Size - im.AudioStart)
	perSample := audioBytes / float64(im.Info.TotalSamples)
	est := im.AudioStart + int64(float64(sample)*perSample)
	// lo holds a frame before the target, hi one at or after it: every probe narrows
	// the bracket, so the search converges even where the estimate misleads.
	lo, hi := im.AudioStart, im.Size
	buf := make([]byte, window)
	for probe := 0; probe < maxProbes; probe++ {
		start := est - window/2
		if start < im.AudioStart {
			start = im.AudioStart
		}
		if start > im.Size-16 {
			start = im.Size - window
		}
		// An image smaller than the window: never aim before its first frame.
		if start < im.AudioStart {
			start = im.AudioStart
		}
		n, err := im.r.ReadAt(buf, start)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, 0, fmt.Errorf("flacsplit: read at %d: %w", start, err)
		}
		b := buf[:n]
		// The last confirmed frame before the target anchors the answer: the chain of
		// frames is followed from it, so the frame returned is the first at or after
		// the target, never one past a frame whose confirmation a decoy spoiled.
		anchorOff, firstGEOff := -1, -1
		var anchor, firstGE frame
		for i := 0; i < len(b)-16; i++ {
			f, ok := im.parseFrame(b, i)
			if !ok || !im.confirmed(b, i, f) {
				continue
			}
			if f.sample < sample {
				anchor, anchorOff = f, i
				continue
			}
			firstGE, firstGEOff = f, i
			break
		}
		if anchorOff < 0 && firstGEOff >= 0 && start == im.AudioStart {
			return start + int64(firstGEOff), firstGE.sample, nil
		}
		switch {
		case anchorOff >= 0:
			cur, curOff := anchor, anchorOff
			for {
				if cur.sample+int64(cur.block) >= im.Info.TotalSamples {
					// The target falls inside the last frame: the track runs to the end.
					return im.Size, im.Info.TotalSamples, nil
				}
				nextOff, next, ok := im.successor(b, curOff, cur)
				if !ok {
					break
				}
				if next.sample >= sample {
					return start + int64(nextOff), next.sample, nil
				}
				cur, curOff = next, nextOff
			}
			// The chain left the window before the target: re-aim past its end.
			lo = max(lo, start+int64(curOff))
			est = start + int64(curOff) + int64(float64(sample-cur.sample)*perSample)
		case firstGEOff >= 0:
			hi = min(hi, start+int64(firstGEOff))
			est = start + int64(firstGEOff) - int64(float64(firstGE.sample-sample)*perSample)
		default:
			est = lo + (hi-lo)/2
		}
		if est <= lo || est >= hi {
			est = lo + (hi-lo)/2
		}
	}
	return 0, 0, fmt.Errorf("%w: sample %d", ErrBoundaryNotFound, sample)
}

// Header builds the metadata a track needs in front of its frames: the marker, a
// STREAMINFO carrying the track's sample count (frame sizes and MD5 left unknown, as
// the format allows) and a Vorbis comment with the tags.
func (im *Image) Header(totalSamples int64, tags []Tag) ([]byte, error) {
	info := im.Info.Raw
	for i := 4; i < 10; i++ {
		info[i] = 0
	}
	v := binary.BigEndian.Uint64(info[10:18])
	v = v&^0xfffffffff | uint64(totalSamples)&0xfffffffff
	binary.BigEndian.PutUint64(info[10:18], v)
	for i := 18; i < 34; i++ {
		info[i] = 0
	}

	var comment bytes.Buffer
	le := func(n int) { _ = binary.Write(&comment, binary.LittleEndian, uint32(n)) }
	vendor := "tiramisu"
	le(len(vendor))
	comment.WriteString(vendor)
	le(len(tags))
	for _, t := range tags {
		kv := t.Key + "=" + t.Value
		le(len(kv))
		comment.WriteString(kv)
	}

	var out bytes.Buffer
	out.WriteString("fLaC")
	out.Write([]byte{0x00, 0, 0, 34})
	out.Write(info[:])
	cl := comment.Len()
	out.Write([]byte{0x80 | 4, byte(cl >> 16), byte(cl >> 8), byte(cl)})
	out.Write(comment.Bytes())
	if out.Len() > MaxHeaderSize {
		return nil, fmt.Errorf("%w: %d bytes", ErrHeaderTooLarge, out.Len())
	}
	return out.Bytes(), nil
}
