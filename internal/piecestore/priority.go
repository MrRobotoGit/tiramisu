package piecestore

// Priority is a piece's rank in the engine's request order (storage.md §4.2), lowest first.
// The values follow the engine's own levels; the adapter converts them one to one.
type Priority int8

const (
	PriorityNone Priority = iota // not wanted
	PriorityNormal
	PriorityHigh
	PriorityReadahead
	PriorityNext
	PriorityNow
)
