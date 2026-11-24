package engine

import (
	"cmp"
	"encoding/hex"
	"slices"

	"tiramisu/internal/friends"
)

// peerRates samples the per-peer rates, naming the friends on proven links.
func (t *Torrent) peerRates() []PeerRate {
	var name func([20]byte) string
	if m := t.e.friends; m != nil && m.FriendCount() > 0 {
		if ih, err := hex.DecodeString(t.hash); err == nil && len(ih) == 20 {
			name = func(id [20]byte) string { return m.FriendName([20]byte(ih), id) }
		}
	}
	out := t.rates.sample(t.bt.PeerSnapshots(), t.e.now(), t.e.peerInfo(), name)
	if name != nil {
		helpers, helped := t.e.helpsOn(t.hash)
		for i := range out {
			switch f := out[i].Friend; {
			case f == "":
			case helpers[f]:
				out[i].FriendRole = "helping_you"
			case helped[f]:
				out[i].FriendRole = "you_help"
			}
		}
	}
	return out
}

// helpsOn lists the friends helping our playback of hash and those whose playback of it we help.
func (e *Engine) helpsOn(hash string) (helpers, helped map[string]bool) {
	helpers, helped = map[string]bool{}, map[string]bool{}
	e.friendsPlayMu.Lock()
	ws := e.watching[hash]
	e.friendsPlayMu.Unlock()
	if ws != nil {
		for _, n := range ws.helping() {
			helpers[n] = true
		}
	}
	e.helps.mu.Lock()
	for k, hp := range e.helps.byKey {
		if k.hash != hash {
			continue
		}
		hp.mu.Lock()
		if !hp.ended {
			helped[k.friend] = true
		}
		hp.mu.Unlock()
	}
	e.helps.mu.Unlock()
	return helpers, helped
}

// friendActivity reports, by friend, the proven links and the helps running now in both
// directions; a help we give runs at low priority while one of our own playbacks is open.
func (e *Engine) friendActivity() map[string]*friends.Activity {
	out := map[string]*friends.Activity{}
	of := func(name string) *friends.Activity {
		a := out[name]
		if a == nil {
			a = &friends.Activity{}
			out[name] = a
		}
		return a
	}
	title := func(hash string) string {
		if t := e.Get(hash); t != nil {
			return t.Meta().Title
		}
		return ""
	}
	for _, t := range e.ListHydrated() {
		if t.bt == nil {
			continue
		}
		for _, pr := range t.peerRates() {
			if pr.Friend != "" {
				a := of(pr.Friend)
				a.Links = append(a.Links, friends.Link{Hash: t.hash, Title: t.Meta().Title, DownKBps: pr.KBps, UpKBps: pr.UpKBps})
			}
		}
	}
	e.friendsPlayMu.Lock()
	ownPlaying := len(e.friendsPlaying) > 0
	type watched struct {
		hash    string
		helpers []string
	}
	var ws []watched
	for hash, w := range e.watching {
		ws = append(ws, watched{hash, w.helping()})
	}
	e.friendsPlayMu.Unlock()
	for _, w := range ws {
		for _, name := range w.helpers {
			a := of(name)
			a.HelpingYou = append(a.HelpingYou, friends.Help{Hash: w.hash, Title: title(w.hash)})
		}
	}
	e.helps.mu.Lock()
	for _, hp := range e.helps.byKey {
		hp.mu.Lock()
		ended := hp.ended
		hp.mu.Unlock()
		if !ended {
			a := of(hp.friend)
			a.YouHelp = append(a.YouHelp, friends.Help{Hash: hp.hash, Title: hp.title, LowPriority: ownPlaying})
		}
	}
	for name, r := range e.helps.refused {
		of(name).NotHelping = &r.Refusal
	}
	e.helps.mu.Unlock()
	byHash := func(a, b friends.Help) int { return cmp.Compare(a.Hash, b.Hash) }
	for _, a := range out { // stable order between polls
		slices.SortFunc(a.Links, func(x, y friends.Link) int { return cmp.Compare(x.Hash, y.Hash) })
		slices.SortFunc(a.HelpingYou, byHash)
		slices.SortFunc(a.YouHelp, byHash)
	}
	return out
}
