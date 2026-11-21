// Package bridge is the FUSE side's door to Tentacle, the torrent engine: the pump's
// NativeReader, FetchAhead, FetchBlockBlocking/PrefetchBlock, Wake and housekeeping.
// Bytes travel through a synchronous io.Pipe fed by an ordered streamer stream, so the
// consumer's pace drives the stream (design.md §1.5).
package bridge
