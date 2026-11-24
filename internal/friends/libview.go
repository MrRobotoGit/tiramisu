package friends

import (
	"path"
	"strconv"
	"strings"
	"time"

	"tiramisu/internal/library"
	"tiramisu/internal/metadb"
)

// friendLibView is a friend's library role as GET /api/friends shows it. Side is the friend's:
// "master" sends us its library, "slave" gets ours.
type friendLibView struct {
	Side       string `json:"side"`
	State      string `json:"state"` // proposed | offered | active | released | refused
	Since      int64  `json:"since,omitempty"`
	Proposed   int64  `json:"proposed,omitempty"`
	Expires    int64  `json:"expires,omitempty"`
	Refused    string `json:"refused,omitempty"` // has_master | has_slaves
	Entries    int64  `json:"entries"`
	Waiting    int64  `json:"waiting"`
	AlignedAt  int64  `json:"aligned_at,omitempty"`
	LastChange int64  `json:"last_change,omitempty"`
}

type aligningView struct {
	Done   int64   `json:"done"`
	Total  int64   `json:"total"`
	Block  int     `json:"block"`
	Blocks int     `json:"blocks"`
	Rate   float64 `json:"rate"`
}

type recentView struct {
	Sign       string `json:"sign"` // + or −
	Title      string `json:"title"`
	Resolution string `json:"resolution,omitempty"`
	What       string `json:"what"` // added | removed | replaced by <resolution>
	At         int64  `json:"at"`
}

type slaveView struct {
	Name         string `json:"name"`
	State        string `json:"state"` // aligning | in_sync | behind | offline
	Entries      int64  `json:"entries"`
	Total        int64  `json:"total"`
	Waiting      int64  `json:"waiting"`
	LastSent     int64  `json:"last_sent,omitempty"`
	OfflineSince int64  `json:"offline_since,omitempty"`
}

// libraryView is this node's shared library: as slave (master, mirror) or as master (slaves).
// Entries is the library's size on both; films, episodes and failed are a slave's only.
type libraryView struct {
	Master   string        `json:"master,omitempty"`
	Entries  int64         `json:"entries"`
	Films    *int64        `json:"films,omitempty"`
	Episodes *int64        `json:"episodes,omitempty"`
	Failed   *int64        `json:"failed,omitempty"`
	Aligning *aligningView `json:"aligning,omitempty"`
	Recent   []recentView  `json:"recent,omitempty"`
	Slaves   []slaveView   `json:"slaves,omitempty"`
}

// recentRows is how many mirror rows the view reads for "recent" (the Control Panel shows a few).
const recentRows = 20

// pullProgress is the first alignment as the slave sees it, in memory.
type pullProgress struct {
	blocks      int
	sessStart   time.Time
	sessEntries int
	entries     int   // applied by this process's sessions: entries per block for the estimate
	left        int64 // after the last block, while a session runs
	running     bool
}

// libraryViews builds the per-friend and the node's library views; nil when there is nothing to show.
func (m *Manager) libraryViews() (map[string]*friendLibView, *libraryView) {
	w := m.lib.Load()
	if w == nil {
		return nil, nil
	}
	snap := m.roles.Load()
	m.libMu.Lock()
	refused := make(map[string]string, len(m.refused))
	for k, v := range m.refused {
		refused[k] = v
	}
	m.libMu.Unlock()
	per := map[string]*friendLibView{}
	for name, why := range refused {
		per[name] = &friendLibView{Side: sideSlave, State: "refused", Refused: why}
	}
	var root *libraryView
	for name, r := range snap.rows {
		v := &friendLibView{Side: otherSide(r.Side), State: r.State, Since: r.Since, AlignedAt: r.AlignedAt}
		if r.State == stProposed || r.State == stOffered {
			v.Proposed, v.Expires = r.UpdatedAt, r.ExpiresAt
		}
		per[name] = v
		if r.State != stActive {
			continue
		}
		if r.Side == sideSlave { // our master
			root = m.slaveRoot(r, v)
			continue
		}
		if root == nil {
			root = &libraryView{}
			if _, total, _, err := w.src.Counts(0); err == nil {
				root.Entries = total // what the slaves get
			}
		}
		root.Slaves = append(root.Slaves, m.slaveRow(w.src, r, v))
	}
	return per, root
}

func otherSide(ours string) string {
	if ours == sideMaster {
		return sideSlave
	}
	return sideMaster
}

// slaveRoot is the node's view of the library its master sends.
func (m *Manager) slaveRoot(r metadb.FriendLibrary, fv *friendLibView) *libraryView {
	v := &libraryView{Master: r.Name}
	if films, eps, failed, err := m.store.MirrorCounts(); err == nil {
		v.Films, v.Episodes, v.Failed, v.Entries = &films, &eps, &failed, films+eps
	}
	fv.Entries = v.Entries
	m.mu.Lock()
	var p *pullProgress
	if cur := m.progress[r.Name]; cur != nil {
		cp := *cur // a copy: pull keeps writing the original under mu
		p = &cp
	}
	m.mu.Unlock()
	left := r.LeftCount // the snapshot is reloaded when a session ends
	if p != nil && p.running {
		left = p.left
	}
	if left > 0 {
		fv.Waiting = left
	}
	rows, _ := m.store.ListMirrorRecent(recentRows)
	v.Recent = recentChanges(rows)
	if len(rows) > 0 {
		fv.LastChange = rows[0].At
	}
	if r.AlignedAt == 0 && left != 0 {
		a := &aligningView{Done: v.Entries, Total: v.Entries + max(left, 0)}
		if p != nil {
			a.Block, a.Blocks = p.blocks, p.blocks
			if p.blocks > 0 && left > 0 {
				per := max(int64(p.entries)/int64(p.blocks), 1)
				a.Blocks += int((left + per - 1) / per)
			}
			if d := m.now().Sub(p.sessStart).Seconds(); d > 0 && p.sessEntries > 0 {
				a.Rate = float64(p.sessEntries) / d
			}
		}
		v.Aligning = a
	}
	return v
}

// slaveRow is one slave as its master sees it.
func (m *Manager) slaveRow(src LibrarySource, r metadb.FriendLibrary, fv *friendLibView) slaveView {
	s := slaveView{Name: r.Name, LastSent: r.UpdatedAt}
	if entries, total, waiting, err := src.Counts(r.Cursor); err == nil {
		s.Entries, s.Total, s.Waiting = entries, total, waiting
	}
	m.mu.Lock()
	ns := m.news[r.Name]
	var offline time.Time
	if ns != nil && ns.retry != nil {
		offline = ns.failedSince
	}
	m.mu.Unlock()
	switch {
	case !offline.IsZero():
		s.State, s.OfflineSince = "offline", offline.Unix()
	case r.AlignedAt == 0:
		s.State = "aligning"
	case s.Waiting > 0:
		s.State = "behind"
	default:
		s.State = "in_sync"
	}
	fv.Entries, fv.Waiting, fv.LastChange = s.Entries, s.Waiting, r.UpdatedAt
	return s
}

func titleKey(p string) string {
	t, _ := library.DisplayTitle(path.Base(p))
	return strings.ToLower(t)
}

// recentChanges turns the latest mirror rows into the "recent changes" list; a removal and an
// addition of the same title in one session read "replaced by <resolution>".
func recentChanges(rows []metadb.MirrorRow) []recentView {
	added := map[[2]string]metadb.MirrorRow{}
	for _, r := range rows {
		if r.State == "present" {
			added[[2]string{titleKey(r.Path), sessKey(r.Sess)}] = r
		}
	}
	var out []recentView
	for _, r := range rows {
		title, res := library.DisplayTitle(path.Base(r.Path))
		switch r.State {
		case "present":
			out = append(out, recentView{Sign: "+", Title: title, Resolution: res, What: "added", At: r.At})
		case "removed":
			what := "removed"
			if a, ok := added[[2]string{titleKey(r.Path), sessKey(r.Sess)}]; ok && a.Path != r.Path {
				if _, newRes := library.DisplayTitle(path.Base(a.Path)); newRes != "" {
					what = "replaced by " + newRes
				}
			}
			out = append(out, recentView{Sign: "−", Title: title, Resolution: res, What: what, At: r.At})
		}
	}
	return out
}

func sessKey(s int64) string { return strconv.FormatInt(s, 10) }
