package friends

// Activity is what a friend and this node exchange right now, as the engine reports it.
type Activity struct {
	Links      []Link   `json:"links,omitempty"`       // proven peer links, with their current rates
	HelpingYou []Help   `json:"helping_you,omitempty"` // the friend helps our playback
	YouHelp    []Help   `json:"you_help,omitempty"`    // we help the friend's playback
	NotHelping *Refusal `json:"not_helping,omitempty"` // the friend's last watch we refused
}

// Link is a proven peer link with the friend on one torrent.
type Link struct {
	Hash     string  `json:"hash"`
	Title    string  `json:"title,omitempty"`
	DownKBps float64 `json:"kbps"`
	UpKBps   float64 `json:"up_kbps"`
}

// Help is one playback helped, in either direction. LowPriority: our own playback runs meanwhile.
type Help struct {
	Hash        string `json:"hash"`
	Title       string `json:"title,omitempty"`
	LowPriority bool   `json:"low_priority,omitempty"`
}

// Refusal is a friend's watch we declined: Reason is off, upload_off, cap, busy or unavailable.
type Refusal struct {
	Hash   string `json:"hash"`
	Title  string `json:"title,omitempty"`
	Reason string `json:"reason"`
	At     int64  `json:"at"`
}

// SetActivity installs the engine's report of what each friend exchanges now, by friend name.
func (m *Manager) SetActivity(f func() map[string]*Activity) { m.activity.Store(&f) }

// FriendName is the friend whose side presents peerID on ih; "" for a stranger.
func (m *Manager) FriendName(ih, peerID [20]byte) string {
	if f := m.snap.Load().friendByID(ih, peerID); f != nil {
		return f.name
	}
	return ""
}
