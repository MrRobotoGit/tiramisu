package notice

import "sort"

type Cue struct {
	Time uint64 // TimestampScale units
	Pos  int64  // absolute offset of the Cluster
}

// ParseCues reads a Cues element starting at b[0]: video-track points, absolute positions.
func ParseCues(b []byte, f *Film) ([]Cue, error) {
	id, size, n, ok := readHeader(b)
	if !ok || id != idCues || size == unknownSize || uint64(len(b)-n) < size {
		return nil, ErrStructure
	}
	seen := map[int64]bool{}
	var out []Cue
	children(b[n:n+int(size)], func(id uint64, cp []byte) {
		if id != idCuePoint {
			return
		}
		var tm uint64
		var poss []int64
		video := f.Video().Number
		children(cp, func(id uint64, v []byte) {
			switch id {
			case idCueTime:
				tm = beUint(v)
			case idCueTrackPos:
				var track uint64
				var pos int64 = -1
				children(v, func(id uint64, x []byte) {
					switch id {
					case idCueTrack:
						track = beUint(x)
					case idCueClusterPos:
						pos = f.SegData + int64(beUint(x))
					}
				})
				if track == video && pos >= 0 {
					poss = append(poss, pos)
				}
			}
		})
		for _, p := range poss {
			if !seen[p] {
				seen[p] = true
				out = append(out, Cue{Time: tm, Pos: p})
			}
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Pos < out[j].Pos })
	if len(out) == 0 {
		return nil, ErrStructure
	}
	return out, nil
}
