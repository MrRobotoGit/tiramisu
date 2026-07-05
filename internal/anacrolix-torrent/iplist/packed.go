//go:build !wasm
// +build !wasm

package iplist

import (
	"encoding/binary"
	"io"
)

// The packed format is an 8 byte integer of the number of ranges. Then 20
// bytes per range, consisting of 4 byte packed IP being the lower bound IP of
// the range, then 4 bytes of the upper, inclusive bound, 8 bytes for the
// offset of the description from the end of the packed ranges, and 4 bytes
// for the length of the description. After these packed ranges, are the
// concatenated descriptions.

const (
	packedRangesOffset = 8
	packedRangeLen     = 44
)

func (ipl *IPList) WritePacked(w io.Writer) (err error) {
	descOffsets := make(map[string]int64, len(ipl.ranges))
	descs := make([]string, 0, len(ipl.ranges))
	var nextOffset int64
	// This is a little monadic, no?
	write := func(b []byte, expectedLen int) {
		if err != nil {
			return
		}
		var n int
		n, err = w.Write(b)
		if err != nil {
			return
		}
		if n != expectedLen {
			panic(n)
		}
	}
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(len(ipl.ranges)))
	write(b[:], 8)
	for _, r := range ipl.ranges {
		write(r.First.To16(), 16)
		write(r.Last.To16(), 16)
		descOff, ok := descOffsets[r.Description]
		if !ok {
			descOff = nextOffset
			descOffsets[r.Description] = descOff
			descs = append(descs, r.Description)
			nextOffset += int64(len(r.Description))
		}
		binary.LittleEndian.PutUint64(b[:], uint64(descOff))
		write(b[:], 8)
		binary.LittleEndian.PutUint32(b[:], uint32(len(r.Description)))
		write(b[:4], 4)
	}
	for _, d := range descs {
		write([]byte(d), len(d))
	}
	return
}

type PackedIPList []byte
