//go:build notice_bank || notice_matrix

package notice

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"image"
	"image/png"
	"io"
	"math"
	"sync"
)

//go:embed assets/notice.png
var noticePNG []byte

// Frame is a 4:2:0 picture, one sample per uint16.
type Frame struct {
	W, H    int
	Y, U, V []uint16
}

// WriteRaw writes the planes as yuv420p (8-bit) or yuv420p10le (deeper).
func (fr Frame) WriteRaw(w io.Writer, depth int) error {
	bps := 2
	if depth == 8 {
		bps = 1
	}
	buf := make([]byte, 0, len(fr.Y)*bps) // one plane at a time, reused
	for _, pl := range [][]uint16{fr.Y, fr.U, fr.V} {
		buf = buf[:0]
		for _, s := range pl {
			if depth == 8 {
				buf = append(buf, byte(s))
			} else {
				buf = append(buf, byte(s), byte(s>>8))
			}
		}
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
	return nil
}

var (
	srcOnce sync.Once
	srcGray *image.Gray
)

func source() *image.Gray {
	srcOnce.Do(func() {
		img, err := png.Decode(bytes.NewReader(noticePNG))
		if err != nil {
			panic(err)
		}
		b := img.Bounds()
		srcGray = image.NewGray(b)
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				r, g, bl, _ := img.At(x, y).RGBA() // neutral greys: r=g=b
				srcGray.Pix[(y-b.Min.Y)*srcGray.Stride+(x-b.Min.X)] = uint8((r + g + bl) / 3 >> 8)
			}
		}
	})
	return srcGray
}

// Render composes the notice at w×h (contain-fit, centred, black around) in the given transfer.
func Render(w, h, depth int, tr Transfer) Frame {
	src := source()
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	scale := math.Min(float64(w)/float64(sw), float64(h)/float64(sh))
	dw, dh := float64(sw)*scale, float64(sh)*scale
	ox, oy := (float64(w)-dw)/2, (float64(h)-dh)/2
	lut := levels(depth, tr)
	cw, ch := (w+1)/2, (h+1)/2
	fr := Frame{W: w, H: h, Y: make([]uint16, w*h), U: make([]uint16, cw*ch), V: make([]uint16, cw*ch)}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			sx, sy := (float64(x)+.5-ox)/scale-.5, (float64(y)+.5-oy)/scale-.5
			fr.Y[y*w+x] = lut[bilinear(src, sx, sy)]
		}
	}
	mid := uint16(1) << (depth - 1)
	for i := range fr.U {
		fr.U[i], fr.V[i] = mid, mid
	}
	return fr
}

func bilinear(g *image.Gray, x, y float64) uint8 {
	w, h := g.Bounds().Dx(), g.Bounds().Dy()
	if x < 0 || y < 0 || x > float64(w-1) || y > float64(h-1) {
		return 0
	}
	x0, y0 := int(x), int(y)
	x1, y1 := min(x0+1, w-1), min(y0+1, h-1)
	fx, fy := x-float64(x0), y-float64(y0)
	at := func(x, y int) float64 { return float64(g.Pix[y*g.Stride+x]) }
	v := at(x0, y0)*(1-fx)*(1-fy) + at(x1, y0)*fx*(1-fy) + at(x0, y1)*(1-fx)*fy + at(x1, y1)*fx*fy
	return uint8(v + .5)
}

// levels maps an 8-bit sRGB grey to the luma code of the target transfer and range.
func levels(depth int, tr Transfer) [256]uint16 {
	var lut [256]uint16
	maxc := float64(int(1)<<depth - 1)
	lo, span := 16*float64(int(1)<<(depth-8)), 219*float64(int(1)<<(depth-8))
	for i := range lut {
		v := float64(i) / 255
		lin := math.Pow(v, 2.2)
		var e float64
		switch tr {
		case TransferSDR:
			e = v
		case TransferPQ, TransferDV5:
			e = pq(lin * 200)
		case TransferHLG:
			e = hlgOETF(lin * 0.26) // 0.26 → 75% signal, BT.2408 reference white
		}
		if tr == TransferDV5 {
			lut[i] = uint16(math.Round(e * maxc)) // DV profile 5 is full range
		} else {
			lut[i] = uint16(math.Round(lo + e*span))
		}
	}
	return lut
}

func pq(nits float64) float64 {
	const m1, m2, c1, c2, c3 = 0.1593017578125, 78.84375, 0.8359375, 18.8515625, 18.6875
	y := math.Pow(nits/10000, m1)
	return math.Pow((c1+c2*y)/(1+c3*y), m2)
}

func pqCode10(nits float64, full bool) uint16 {
	if full {
		return uint16(math.Round(pq(nits) * 1023))
	}
	return uint16(math.Round(64 + pq(nits)*876))
}

func hlgOETF(e float64) float64 {
	const a, b, c = 0.17883277, 0.28466892, 0.55991073
	if e <= 1.0/12 {
		return math.Sqrt(3 * e)
	}
	return a*math.Log(12*e-b) + c
}

// ImageHash names the notice image in clip format keys: a new image regenerates every clip.
func ImageHash() string {
	h := sha256.Sum256(noticePNG)
	return hex.EncodeToString(h[:6])
}
