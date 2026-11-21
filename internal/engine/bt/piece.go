package bt

import (
	"errors"

	"github.com/anacrolix/torrent/storage"

	"tiramisu/internal/piecestore"
)

var errNoPiece = errors.New("bt: piece index out of range")

type piece struct {
	ts *piecestore.TorrentStore
	p  *piecestore.Piece
}

func newPiece(ts *piecestore.TorrentStore, p *piecestore.Piece) storage.PieceImpl {
	if ts == nil || p == nil {
		return noPiece{}
	}
	return piece{ts: ts, p: p}
}

func (x piece) ReadAt(b []byte, off int64) (int, error)  { return x.p.ReadAt(b, off) }
func (x piece) WriteAt(b []byte, off int64) (int, error) { return x.p.WriteAt(b, off) }
func (x piece) MarkComplete() error                      { return x.p.MarkComplete() }

// MarkNotComplete may arrive with the client lock held; the store never calls the engine.
func (x piece) MarkNotComplete() error {
	x.ts.MarkNotComplete(x.p.Index())
	return nil
}

func (x piece) Completion() storage.Completion {
	return storage.Completion{Complete: x.p.Complete(), Ok: true}
}

type noPiece struct{}

func (noPiece) ReadAt([]byte, int64) (int, error)  { return 0, errNoPiece }
func (noPiece) WriteAt([]byte, int64) (int, error) { return 0, errNoPiece }
func (noPiece) MarkComplete() error                { return errNoPiece }
func (noPiece) MarkNotComplete() error             { return errNoPiece }
func (noPiece) Completion() storage.Completion     { return storage.Completion{Ok: true} }
