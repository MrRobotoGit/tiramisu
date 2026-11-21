package engines

import (
	"context"
	"errors"
	"time"

	"tiramisu/internal/engine"
)

// EngineBackend is the in-process Backend over the engine.
func EngineBackend(e engine.API) Backend { return engineBackend{e} }

type engineBackend struct{ e engine.API }

func (b engineBackend) Add(_ context.Context, link, title string) (string, bool, error) {
	t, err := b.e.Add(link, engine.Meta{Title: title})
	if err != nil {
		return "", false, err
	}
	return t.Hash(), true, nil
}

func (b engineBackend) AddTorrentFile(_ context.Context, data []byte, title string) (string, error) {
	t, err := b.e.AddTorrentFile(data, engine.Meta{Title: title})
	if err != nil {
		return "", err
	}
	return t.Hash(), nil
}

// hydratePoll is how often Info looks again for a torrent not in memory yet (an add racing
// a reconnect); the old API was polled the same way.
var hydratePoll = 250 * time.Millisecond

func (b engineBackend) Info(ctx context.Context, hash string) (*TorrentStats, error) {
	for {
		files, err := b.e.WaitFiles(ctx, hash)
		if err == nil {
			return b.stats(hash, files), nil
		}
		if !errors.Is(err, engine.ErrNotHydrated) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(hydratePoll):
		}
	}
}

func (b engineBackend) stats(hash string, files []engine.File) *TorrentStats {
	st := &TorrentStats{Hash: hash}
	if t := b.e.Get(hash); t != nil {
		st.Hash, st.Title, st.Length, st.ActivePeers = t.Hash(), t.Title(), t.Size(), t.ActivePeers()
	}
	for _, f := range files {
		st.FileStats = append(st.FileStats, FileStat{ID: f.ID, Path: f.Path, Length: f.Length})
	}
	return st
}

func (b engineBackend) Remove(_ context.Context, hash string) error { return b.e.Remove(hash) }

func (b engineBackend) Refs(context.Context) ([]TorrentRef, error) {
	refs, err := b.e.ListRefs()
	if err != nil {
		return nil, err
	}
	out := make([]TorrentRef, len(refs))
	for i, r := range refs {
		out[i] = TorrentRef{Hash: r.Hash, Title: r.Title}
	}
	return out, nil
}

func (b engineBackend) Hydrated(context.Context) ([]string, error) {
	ts := b.e.ListHydrated()
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Hash()
	}
	return out, nil
}
