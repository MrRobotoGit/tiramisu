// Package streamer turns piecestore data into ordered byte streams and schedules the engine:
// piece priorities, playout deadlines, eviction and stranded partials, one scheduler per
// torrent. It reaches the engine only through Control and never imports it.
package streamer
