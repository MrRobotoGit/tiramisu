package engine

import (
	"context"
	"errors"
	"strings"
	"time"

	"tiramisu/internal/engine/bt"
)

var (
	ErrBadLink      = bt.ErrBadLink
	ErrNotHydrated  = errors.New("engine: torrent not hydrated")
	ErrTorrentGone  = errors.New("engine: torrent closed before its metadata arrived")
	ErrMetadataWait = errors.New("engine: metadata timeout")
)

// Add adds a magnet link or a bare 40-hex infohash. The torrent is saved to the DB when its
// metadata arrives; one whose metadata never arrives leaves no row.
func (e *Engine) Add(link string, meta Meta) (*Torrent, error) {
	link = strings.TrimSpace(link)
	var spec bt.Spec
	if h, err := ParseHash(link); err == nil {
		spec.InfoHash = h
	} else if strings.HasPrefix(strings.ToLower(link), "magnet:") {
		if spec, err = bt.ParseMagnet(link); err != nil {
			e.deps.logf("[Tentacle] add: %v (%s)", err, Redact(link))
			return nil, err
		}
	} else {
		return nil, ErrBadLink
	}
	return e.addSpec(spec, meta)
}

// AddTorrentFile adds .torrent bytes; the metadata is known at once, so it is saved at once.
func (e *Engine) AddTorrentFile(data []byte, meta Meta) (*Torrent, error) {
	spec, err := bt.ParseTorrentFile(data)
	if err != nil {
		return nil, err
	}
	return e.addSpec(spec, meta)
}

func (e *Engine) addSpec(spec bt.Spec, meta Meta) (*Torrent, error) {
	cfg := e.config()
	row := e.row(spec.InfoHash)
	var stored storedSpec
	if row != nil {
		// DB injection (V255): cached metainfo and peers, stored trackers as one more tier.
		stored = decodeSpec(row.Spec)
		if len(spec.InfoBytes) == 0 {
			spec.InfoBytes = row.InfoBytes
		}
		if len(spec.PeerAddrs) == 0 {
			spec.PeerAddrs = stored.PeerAddrs
		}
		spec.Trackers = mergeStoredTiers(spec.Trackers, stored.Trackers)
	}
	extra := readTrackersFile(e.deps.StateDir)
	if bt.InfoPrivate(spec.InfoBytes) {
		// A private torrent announces only to its own trackers (BEP 27), whatever the mode.
		spec.Trackers = withoutTrackers(spec.Trackers, e.trackers(), BuiltinTrackers, extra)
	} else {
		spec.Trackers = applyRetrackers(cfg.RetrackersMode, spec.Trackers, e.trackers(), extra)
	}

	e.addMu.Lock()
	defer e.addMu.Unlock()
	c := e.currentClient()
	if c == nil {
		return nil, ErrNotConnected
	}
	e.dropServed(spec.InfoHash) // hydration takes over from a friends-only torrent
	if t := e.lookup(spec.InfoHash); t != nil {
		if _, _, err := addHydrating(c, spec, func() (*bt.Torrent, bool, error) { return c.Add(spec, nil) }); err != nil {
			return nil, err
		}
		t.extend(min(cfg.timeout(), 60*time.Second))
		t.fillMeta(meta)
		return t, nil
	}
	t := &Torrent{e: e, hash: spec.InfoHash, closed: make(chan struct{}), working: make(chan struct{}), tiers: spec.Trackers, peers: spec.PeerAddrs}
	t.state.Store(int32(StateGettingInfo)) // before the registry: no later write may undo a close
	t.deadline.Store(e.now().Add(min(cfg.timeout(), 60*time.Second)).UnixNano())
	t.added = e.now().Unix()
	cands := []Meta{meta}
	if row != nil {
		t.added, t.size, t.stored = row.Timestamp, row.Size, len(row.InfoBytes) > 0
		cands = append(cands, Meta{Title: row.Title, Category: row.Category, Poster: row.Poster, Data: row.Data})
	}
	t.fillMeta(cands...)
	lt, _, err := addHydrating(c, spec, func() (*bt.Torrent, bool, error) { return c.Add(spec, t.setStore) })
	if err != nil {
		return nil, err
	}
	t.bt = lt
	e.mu.Lock()
	if e.client != c {
		e.mu.Unlock()
		lt.Drop()
		return nil, ErrNotConnected
	}
	e.torrents[t.hash] = t
	e.mu.Unlock()
	if e.friends != nil {
		e.friendsColdIntentTake(t)
	}
	go e.watchInfo(t, spec.DisplayName)
	return t, nil
}

// watchInfo waits for the metadata, never past infoWait; until then the torrent never expires.
func (e *Engine) watchInfo(t *Torrent, displayName string) {
	timer := time.NewTimer(e.infoWait(e.config()))
	defer timer.Stop()
	select {
	case <-t.bt.GotInfo():
	case <-t.bt.Closed():
		e.close(t)
		return
	case <-t.closed:
		return
	case <-timer.C:
		e.deps.logf("[Tentacle] %s: no metadata, closing", t.hash)
		e.close(t)
		return
	}
	t.life.Lock()
	defer t.life.Unlock()
	if ts := t.Store(); ts == nil || ts.Closed() {
		// Dropped while waiting: the store is gone, the torrent is unavailable (fe760a8).
		e.closeLocked(t)
		return
	}
	t.fillMeta(Meta{Title: t.bt.Name()}, Meta{Title: displayName})
	t.Files()
	if !t.transition(StateGettingInfo, StateWorking) {
		return // closed meanwhile
	}
	close(t.working)
	if e.friendsWanted(t.hash) { // played before the info, or hydrated again during a playback
		e.goFriends(func() { e.dialFriends(t, dialPlay) })
	}
	if t.friendsCold.Load() { // opened cold before the info
		e.friendsColdDial(t)
	}
	t.extend(e.config().timeout())
	t.mu.Lock()
	if t.bt.Private() { // a magnet learns it only now: the saved row keeps no public tracker
		t.tiers = withoutTrackers(t.tiers, e.trackers(), BuiltinTrackers, readTrackersFile(e.deps.StateDir))
	}
	stored := t.stored
	t.mu.Unlock()
	if !stored {
		e.saveLocked(t)
	}
}

// WaitFiles waits until a hydrated torrent is working (metadata in, store open) and returns
// its files.
func (e *Engine) WaitFiles(ctx context.Context, hash string) ([]File, error) {
	h, err := ParseHash(hash)
	if err != nil {
		return nil, err
	}
	t := e.Get(h)
	if t == nil {
		return nil, ErrNotHydrated
	}
	select {
	case <-t.working: // info arrived and its store checked
		if fs := t.Files(); fs != nil {
			return fs, nil
		}
		return nil, ErrTorrentGone
	case <-t.closed:
		return nil, ErrTorrentGone
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
