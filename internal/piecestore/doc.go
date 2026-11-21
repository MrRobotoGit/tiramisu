// Package piecestore keeps torrent piece data in RAM. It must not import the BitTorrent
// engine: the engine reaches it through an adapter, consumers through the streamer.
//
// Invariant: a consumer read never returns a byte that was not written.
package piecestore
