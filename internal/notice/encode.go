//go:build notice_bank || notice_matrix

package notice

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Encoder turns the rendered notice into frames with ffmpeg. One encode at a time is the
// caller's job (Cache); Nice lowers the priority on the Pi.
type Encoder struct {
	FFmpeg string // "" = "ffmpeg" from PATH
	TmpDir string // "" = os.TempDir()
	Nice   bool
}

const gopSeconds = 10

func (e *Encoder) run(ctx context.Context, args ...string) error {
	bin := e.FFmpeg
	if bin == "" {
		bin = "ffmpeg"
	}
	args = append([]string{"-y", "-v", "error", "-nostdin"}, args...)
	cmd := exec.CommandContext(ctx, bin, args...)
	if e.Nice {
		cmd = exec.CommandContext(ctx, "nice", append([]string{"-n", "19", bin}, args...)...)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// fps is the exact rate for the usual NTSC durations, else 1e9/FrameDur.
func fps(frameDur int64) string {
	for _, r := range []struct {
		num, den int64
	}{{24000, 1001}, {30000, 1001}, {60000, 1001}, {24, 1}, {25, 1}, {30, 1}, {50, 1}, {60, 1}} {
		d := 1_000_000_000 * r.den / r.num
		if frameDur >= d-2 && frameDur <= d+2 {
			return fmt.Sprintf("%d/%d", r.num, r.den)
		}
	}
	return fmt.Sprintf("1000000000/%d", frameDur)
}

// Video encodes one GOP of the notice in the film's format: frames[0] is an IDR, no B-frames.
// fade > 0 opens it with that many frames rising from black.
func (e *Encoder) Video(ctx context.Context, x Format, fade int) ([][]byte, []byte, error) {
	dir, err := os.MkdirTemp(e.TmpDir, "notice-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)
	raw, out := filepath.Join(dir, "frame.yuv"), filepath.Join(dir, "clip.mkv")
	f, err := os.Create(raw)
	if err != nil {
		return nil, nil, err
	}
	for k := 0; k < fade && err == nil; k++ {
		err = RenderLevel(x.Width, x.Height, x.BitDepth, x.Transfer, float64(k)/float64(fade)).WriteRaw(f, x.BitDepth)
	}
	if err == nil {
		err = Render(x.Width, x.Height, x.BitDepth, x.Transfer).WriteRaw(f, x.BitDepth)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, nil, err
	}
	n := int((gopSeconds*1_000_000_000 + x.FrameDur/2) / x.FrameDur)
	pix := "yuv420p"
	if x.BitDepth > 8 {
		pix = "yuv420p10le"
	}
	// The last raw frame (the notice) is held for the rest of the GOP.
	args := []string{"-f", "rawvideo", "-pix_fmt", pix,
		"-s", fmt.Sprintf("%dx%d", x.Width, x.Height), "-r", fps(x.FrameDur), "-i", raw,
		"-vf", "tpad=stop_mode=clone:stop=-1", "-frames:v", fmt.Sprint(n), "-pix_fmt", pix, "-preset", "ultrafast"}
	// Colour goes into the encoder's own VUI: the wrappers do not copy -color_* there reliably.
	gop := fmt.Sprintf("keyint=%d:min-keyint=%d:bframes=0:scenecut=0:open-gop=0:repeat-headers=1", n, n)
	for k, v := range map[string]string{"colorprim": x.Primaries, "transfer": x.TRC, "colormatrix": x.Matrix} {
		if v != "" {
			gop += ":" + k + "=" + v
		}
	}
	if x.FullRange {
		args = append(args, "-color_range", "pc")
	} else {
		args = append(args, "-color_range", "tv")
	}
	switch x.Codec {
	case "h264":
		prof := map[int]string{66: "baseline", 77: "main", 100: "high"}[x.Profile]
		gop += ":range=" + map[bool]string{true: "pc", false: "tv"}[x.FullRange]
		args = append(args, "-c:v", "libx264", "-profile:v", prof, "-level", x.Level, "-x264-params", gop+":sar=1/1")
	case "hevc":
		// The films' geometry, not ultrafast's: CUs down to 8 (16 would code 1080 as 1088), CTUs of 64
		// (32 shows the clip off centre on Plex for iOS) and square pixels declared.
		p := gop + ":log-level=error:min-cu-size=8:ctu=64:sar=1:level-idc=" + x.Level + ":range=" + map[bool]string{true: "full", false: "limited"}[x.FullRange]
		if x.HighTier {
			p += ":high-tier=1"
		}
		if x.Transfer == TransferPQ {
			p += ":hdr10=1"
			if x.MasterDisplay != "" {
				p += ":master-display=" + x.MasterDisplay
			}
			if x.MaxCLL > 0 {
				p += fmt.Sprintf(":max-cll=%d,%d", x.MaxCLL, x.MaxFALL)
			}
		}
		args = append(args, "-c:v", "libx265", "-x265-params", p)
	default:
		return nil, nil, ErrUnsupported
	}
	if err := e.run(ctx, append(args, "-f", "matroska", out)...); err != nil {
		return nil, nil, err
	}
	b, err := os.ReadFile(out)
	if err != nil {
		return nil, nil, err
	}
	cf, err := ParseHead(b)
	if err != nil {
		return nil, nil, err
	}
	var frames [][]byte
	var bad error
	walkBlocks(b[cf.FirstCluster:], cf.Video().Number, func(_ int64, key bool, data []byte) bool {
		if len(frames) == 0 && !key {
			bad = fmt.Errorf("notice: clip does not start with a keyframe")
			return false
		}
		fr, err := convertNAL(data, cf.Video().NALLength(), x.NALLength)
		if err != nil {
			bad = err
			return false
		}
		if x.DVProfile != 0 {
			fr = AppendRPU(fr, x.NALLength, RPUFor(x.DVProfile))
		}
		frames = append(frames, fr)
		return true
	})
	if bad != nil {
		return nil, nil, bad
	}
	if len(frames) != n {
		return nil, nil, fmt.Errorf("notice: clip has %d frames, want %d", len(frames), n)
	}
	return frames, cf.Video().CodecPrivate, nil
}

var audioEncoders = map[string][]string{
	"aac": {"aac"}, "ac3": {"ac3"}, "eac3": {"eac3"}, "dts": {"dca", "-strict", "-2"},
	"flac": {"flac"}, "opus": {"libopus"},
}

// flacArgs: the silent stream's bits per sample must be the film's (no bps switch mid-stream).
var flacArgs = map[int][]string{16: {"-sample_fmt", "s16"}, 24: {"-sample_fmt", "s32", "-bits_per_raw_sample", "24"}}

// sameConfig checks the encoded stream against the film's FLAC STREAMINFO or Opus mapping.
func sameConfig(a AudioFormat, cp []byte) bool {
	switch a.Codec {
	case "flac":
		rate, ch, bps, mb, ok := flacInfo(cp)
		return ok && rate == a.Rate && ch == a.Channels && bps == a.BitDepth && (a.BlockSize == 0 || mb <= a.BlockSize)
	case "opus":
		return len(cp) > 9 && int(cp[9]) == a.Channels && bytes.Equal(opusMap(cp), a.OpusMap)
	}
	return true
}

var layouts = map[int]string{1: "mono", 2: "stereo", 6: "5.1", 8: "7.1"}

// Audio encodes silence in the track's codec and returns one frame to repeat.
func (e *Encoder) Audio(ctx context.Context, a AudioFormat) (AudioClip, []byte, error) {
	enc, ok := audioEncoders[a.Codec]
	layout, lok := layouts[a.Channels]
	if !ok || !lok || a.Rate <= 0 {
		return AudioClip{}, nil, ErrUnsupported
	}
	dir, err := os.MkdirTemp(e.TmpDir, "notice-a-")
	if err != nil {
		return AudioClip{}, nil, err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "a.mkv")
	args := []string{"-f", "lavfi", "-i", fmt.Sprintf("anullsrc=r=%d:cl=%s", a.Rate, layout), "-t", "2", "-c:a", enc[0]}
	args = append(args, enc[1:]...)
	switch a.Codec {
	case "flac":
		fl, ok := flacArgs[a.BitDepth]
		if !ok {
			return AudioClip{}, nil, ErrUnsupported
		}
		args = append(args, fl...)
		if a.BlockSize > 0 {
			args = append(args, "-frame_size", fmt.Sprint(a.BlockSize))
		}
	case "opus":
		if a.OpusMap == nil {
			return AudioClip{}, nil, ErrUnsupported // the film's channel mapping is unknown: cannot be checked
		}
	}
	args = append(args, "-f", "matroska", out)
	if err := e.run(ctx, args...); err != nil {
		return AudioClip{}, nil, err
	}
	b, err := os.ReadFile(out)
	if err != nil {
		return AudioClip{}, nil, err
	}
	cf, err := parseHead(b)
	if err != nil || len(cf.Tracks) != 1 {
		return AudioClip{}, nil, ErrStructure
	}
	tr := cf.Tracks[0]
	if a.ASC != nil && !bytes.Equal(tr.CodecPrivate, a.ASC) {
		return AudioClip{}, nil, ErrUnsupported // the film's AAC configuration cannot be reproduced
	}
	if !sameConfig(a, tr.CodecPrivate) {
		return AudioClip{}, nil, ErrUnsupported
	}
	var ts []int64
	var frames [][]byte
	walkBlocks(b[cf.FirstCluster:], tr.Number, func(t int64, _ bool, data []byte) bool {
		ts, frames = append(ts, t), append(frames, data)
		return true
	})
	if len(frames) < 4 {
		return AudioClip{}, nil, ErrStructure
	}
	// Mean over the whole run: per-block timestamps are rounded to the 1 ms TimestampScale.
	dur := float64(ts[len(ts)-2]-ts[1]) / float64(len(ts)-3) * float64(cf.TimestampScale) // edges: priming, short tail
	return AudioClip{FrameDur: dur, Frame: frames[2]}, tr.CodecPrivate, nil
}
