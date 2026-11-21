package engine

// State of a torrent; numbers and strings are in the status JSON (engine.md §1.5).
type State int32

const (
	StateAdded State = iota
	StateGettingInfo
	StatePreload // never entered: engine preload is gone
	StateWorking
	StateClosed
	StateInDB
)

var stateStrings = [...]string{"Torrent added", "Torrent getting info", "Torrent preload", "Torrent working", "Torrent closed", "Torrent in db"}

func (s State) String() string {
	if s < 0 || int(s) >= len(stateStrings) {
		return "unknown"
	}
	return stateStrings[s]
}
