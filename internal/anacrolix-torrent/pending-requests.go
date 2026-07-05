package torrent

import (
	rbm "github.com/RoaringBitmap/roaring"
	roaring "github.com/RoaringBitmap/roaring/BitSliceIndexing"
)

type pendingRequests struct {
	m *roaring.BSI
}

var allBits rbm.Bitmap

func init() {
	allBits.AddRange(0, rbm.MaxRange)
}
