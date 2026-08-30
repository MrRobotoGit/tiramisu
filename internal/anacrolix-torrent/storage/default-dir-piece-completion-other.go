// sqlite piece completion is not available. Bolt support was removed from this
// fork, so there is no fallback implementation left.
//go:build !cgo || nosqlite
// +build !cgo nosqlite

package storage

import (
	"errors"
)

func NewDefaultPieceCompletionForDir(dir string) (PieceCompletion, error) {
	return nil, errors.New("y ur OS no have features")
}
