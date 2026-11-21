package bt

import (
	"github.com/anacrolix/torrent"
)

// Torrent is one torrent of a Client.
type Torrent struct {
	t *torrent.Torrent
	c *Client
}

// File is a file of the torrent in the engine's (info) order.
type File struct {
	Path   string // torrent name + path, as the library reports it
	Length int64
	Offset int64
}

func (t *Torrent) InfoHash() string             { return t.t.InfoHash().HexString() }
func (t *Torrent) GotInfo() <-chan struct{}     { return t.t.GotInfo() }
func (t *Torrent) Closed() <-chan struct{}      { return t.t.Closed() }
func (t *Torrent) Client() *Client              { return t.c }
func (t *Torrent) AddTrackers(tiers [][]string) { t.t.AddTrackers(tiers) }
func (t *Torrent) SetMaxConns(n int)            { t.t.SetMaxEstablishedConns(n) }
func (t *Torrent) Drop()                        { _ = t.c.drop(t.t) }
func (t *Torrent) HasInfo() bool                { return t.t.Info() != nil }

// Name is the info name, or the display name before info.
func (t *Torrent) Name() string { return t.t.Name() }

// Length is 0 before info.
func (t *Torrent) Length() int64 {
	if !t.HasInfo() {
		return 0
	}
	return t.t.Length()
}

// InfoBytes is nil before info.
func (t *Torrent) InfoBytes() []byte {
	if !t.HasInfo() {
		return nil
	}
	return t.t.Metainfo().InfoBytes
}

// Private reports the info's private flag.
func (t *Torrent) Private() bool {
	info := t.t.Info()
	return info != nil && info.Private != nil && *info.Private
}

// Files lists the files in engine order; nil before info.
func (t *Torrent) Files() []File {
	if !t.HasInfo() {
		return nil
	}
	fs := t.t.Files()
	out := make([]File, len(fs))
	for i, f := range fs {
		out[i] = File{Path: f.Path(), Length: f.Length(), Offset: f.Offset()}
	}
	return out
}

// PeerAddrs lists the remote addresses of the connected peers.
func (t *Torrent) PeerAddrs() []string {
	var out []string
	for _, p := range t.t.PeerSnapshots() {
		if p.Addr != "" {
			out = append(out, p.Addr)
		}
	}
	return out
}

// UpdateCompletion makes the engine re-read a piece's completion: call it after an eviction.
func (t *Torrent) UpdateCompletion(piece int) {
	if t.HasInfo() && piece >= 0 && piece < t.t.NumPieces() {
		t.t.Piece(piece).UpdateCompletion()
	}
}
