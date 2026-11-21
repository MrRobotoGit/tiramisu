package bt

import (
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"tiramisu/internal/piecestore"
)

// Storage is the engine's storage, backed by a piecestore.Store.
type Storage struct {
	store *piecestore.Store
}

var _ storage.ClientImplCloser = (*Storage)(nil)

func NewStorage(store *piecestore.Store) *Storage { return &Storage{store: store} }

// OpenTorrent runs under the client lock: it must not call back into the engine.
func (s *Storage) OpenTorrent(info *metainfo.Info, infoHash metainfo.Hash) (storage.TorrentImpl, error) {
	ti, _ := s.open(info, infoHash)
	return ti, nil
}

func (s *Storage) open(info *metainfo.Info, infoHash metainfo.Hash) (storage.TorrentImpl, *piecestore.TorrentStore) {
	ts := s.store.Open(infoHash.HexString(), info.PieceLength, info.TotalLength())
	capacity := func() (int64, bool) { return s.store.Capacity(), true }
	return storage.TorrentImpl{
		Piece: func(p metainfo.Piece) storage.PieceImpl { return newPiece(ts, ts.Piece(p.Index())) },
		Close: func() error {
			s.store.Remove(ts)
			return nil
		},
		Capacity: &capacity,
	}, ts
}

// Close is a no-op: torrents close their own stores.
func (s *Storage) Close() error { return nil }

// forTorrent is the storage of one added torrent: it hands the store it opens to onOpen,
// under the client lock, so the registry never looks a store up by hash.
type forTorrent struct {
	s      *Storage
	onOpen func(*piecestore.TorrentStore)
}

func (f forTorrent) OpenTorrent(info *metainfo.Info, infoHash metainfo.Hash) (storage.TorrentImpl, error) {
	ti, ts := f.s.open(info, infoHash)
	if f.onOpen != nil {
		f.onOpen(ts)
	}
	return ti, nil
}
