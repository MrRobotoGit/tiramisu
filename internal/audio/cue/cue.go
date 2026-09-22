// Package cue parses the cue sheets that describe the tracks inside a single-file
// album image.
package cue

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

// FramesPerSecond is the CD frame rate cue times are written in.
const FramesPerSecond = 75

// Track is one TRACK entry. Start is the INDEX 01 position in CD frames.
type Track struct {
	Number    int
	Title     string
	Performer string
	Start     int64
}

// StartSample converts the track start to a sample position at rate.
func (t Track) StartSample(rate int) int64 {
	return t.Start * int64(rate) / FramesPerSecond
}

// File is one FILE block and the tracks inside it.
type File struct {
	Name   string
	Tracks []Track
}

// Sheet is a parsed cue sheet.
type Sheet struct {
	Title     string
	Performer string
	Files     []File
	// Dir is where the sheet sits in its torrent, set by the caller that read it.
	Dir string
}

var ErrInvalid = errors.New("cue: invalid sheet")

// Decode returns the sheet as UTF-8: a BOM is dropped, valid UTF-8 is kept, anything
// else is read as Windows-1251, the encoding most rips in the wild use.
func Decode(data []byte) string {
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	if utf8.Valid(data) {
		return string(data)
	}
	out, err := charmap.Windows1251.NewDecoder().Bytes(data)
	if err != nil {
		return string(data)
	}
	return string(out)
}

// Parse reads a cue sheet. Every track must carry INDEX 01, and starts must grow
// within each file.
func Parse(data []byte) (*Sheet, error) {
	sheet := &Sheet{}
	var file *File
	var track *Track
	for n, raw := range strings.Split(Decode(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" {
			continue
		}
		cmd, args := splitCommand(line)
		switch strings.ToUpper(cmd) {
		case "FILE":
			if len(args) == 0 {
				return nil, fmt.Errorf("%w: line %d: FILE without a name", ErrInvalid, n+1)
			}
			sheet.Files = append(sheet.Files, File{Name: args[0]})
			file, track = &sheet.Files[len(sheet.Files)-1], nil
		case "TRACK":
			if file == nil || len(args) == 0 {
				return nil, fmt.Errorf("%w: line %d: TRACK outside a FILE", ErrInvalid, n+1)
			}
			number, err := strconv.Atoi(args[0])
			if err != nil || number <= 0 {
				return nil, fmt.Errorf("%w: line %d: bad track number %q", ErrInvalid, n+1, args[0])
			}
			file.Tracks = append(file.Tracks, Track{Number: number, Start: -1})
			track = &file.Tracks[len(file.Tracks)-1]
		case "TITLE":
			if len(args) > 0 {
				if track != nil {
					track.Title = args[0]
				} else if file == nil {
					sheet.Title = args[0]
				}
			}
		case "PERFORMER":
			if len(args) > 0 {
				if track != nil {
					track.Performer = args[0]
				} else if file == nil {
					sheet.Performer = args[0]
				}
			}
		case "INDEX":
			if track == nil || len(args) < 2 {
				continue
			}
			if index, err := strconv.Atoi(args[0]); err != nil || index != 1 {
				continue
			}
			frames, err := parseTime(args[1])
			if err != nil {
				return nil, fmt.Errorf("%w: line %d: %v", ErrInvalid, n+1, err)
			}
			track.Start = frames
		}
	}
	if len(sheet.Files) == 0 {
		return nil, fmt.Errorf("%w: no FILE", ErrInvalid)
	}
	for _, f := range sheet.Files {
		for i, t := range f.Tracks {
			if t.Start < 0 {
				return nil, fmt.Errorf("%w: track %d has no INDEX 01", ErrInvalid, t.Number)
			}
			if i > 0 && t.Start <= f.Tracks[i-1].Start {
				return nil, fmt.Errorf("%w: track %d does not start after track %d", ErrInvalid, t.Number, f.Tracks[i-1].Number)
			}
		}
	}
	return sheet, nil
}

// FileFor returns the FILE block naming the given file, compared by base name
// without case, so a sheet written on another machine still matches. A sheet ripped
// to WAV and encoded afterwards still names the .wav: the name without extension
// matches too.
func (s *Sheet) FileFor(name string) (*File, bool) {
	want := baseName(name)
	for _, strip := range []bool{false, true} {
		target := want
		if strip {
			target = stem(want)
		}
		for i := range s.Files {
			got := baseName(s.Files[i].Name)
			if strip {
				got = stem(got)
			}
			if got == target && len(s.Files[i].Tracks) > 0 {
				return &s.Files[i], true
			}
		}
	}
	return nil, false
}

func baseName(name string) string {
	return strings.ToLower(path.Base(strings.ReplaceAll(name, `\`, "/")))
}

func stem(name string) string {
	return strings.TrimSuffix(name, path.Ext(name))
}

// splitCommand splits a cue line into its command and arguments, keeping quoted
// arguments whole.
func splitCommand(line string) (string, []string) {
	var fields []string
	for len(line) > 0 {
		line = strings.TrimLeft(line, " \t")
		if line == "" {
			break
		}
		if line[0] == '"' {
			end := strings.IndexByte(line[1:], '"')
			if end < 0 {
				fields = append(fields, line[1:])
				break
			}
			fields = append(fields, line[1:end+1])
			line = line[end+2:]
			continue
		}
		end := strings.IndexAny(line, " \t")
		if end < 0 {
			fields = append(fields, line)
			break
		}
		fields = append(fields, line[:end])
		line = line[end:]
	}
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], fields[1:]
}

// parseTime reads mm:ss:ff into CD frames.
func parseTime(s string) (int64, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, fmt.Errorf("bad time %q", s)
	}
	var v [3]int64
	for i, p := range parts {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("bad time %q", s)
		}
		v[i] = n
	}
	if v[1] >= 60 || v[2] >= FramesPerSecond {
		return 0, fmt.Errorf("bad time %q", s)
	}
	return (v[0]*60+v[1])*FramesPerSecond + v[2], nil
}
