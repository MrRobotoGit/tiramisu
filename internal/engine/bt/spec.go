package bt

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

// Spec describes a torrent to add; field names follow the stored spec JSON.
type Spec struct {
	InfoHash    string // 40 lowercase hex
	Trackers    [][]string
	DisplayName string
	PeerAddrs   []string
	InfoBytes   []byte
	Webseeds    []string
	DhtNodes    []string
	Sources     []string
}

var ErrBadLink = errors.New("bt: unparseable link")

// ParseMagnet reads a magnet URI.
func ParseMagnet(uri string) (Spec, error) {
	ts, err := torrent.TorrentSpecFromMagnetUri(uri)
	if err != nil {
		return Spec{}, fmt.Errorf("%w: %v", ErrBadLink, err)
	}
	return fromLib(ts), nil
}

// ParseTorrentFile reads .torrent bytes; the info must decode.
func ParseTorrentFile(b []byte) (Spec, error) {
	mi, err := metainfo.Load(bytes.NewReader(b))
	if err != nil {
		return Spec{}, fmt.Errorf("%w: %v", ErrBadLink, err)
	}
	ts, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		return Spec{}, fmt.Errorf("%w: %v", ErrBadLink, err)
	}
	return fromLib(ts), nil
}

// InfoName decodes the name of an info dictionary; "" when it does not decode.
func InfoName(infoBytes []byte) string {
	mi := metainfo.MetaInfo{InfoBytes: infoBytes}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return ""
	}
	return info.BestName()
}

// InfoPrivate reports the private flag of an info dictionary; false when it does not decode.
func InfoPrivate(infoBytes []byte) bool {
	if len(infoBytes) == 0 {
		return false
	}
	mi := metainfo.MetaInfo{InfoBytes: infoBytes}
	info, err := mi.UnmarshalInfo()
	return err == nil && info.Private != nil && *info.Private
}

func fromLib(ts *torrent.TorrentSpec) Spec {
	var trackers [][]string
	for _, tier := range ts.Trackers {
		if len(tier) > 0 {
			trackers = append(trackers, tier)
		}
	}
	return Spec{
		InfoHash: ts.InfoHash.HexString(), Trackers: trackers, DisplayName: ts.DisplayName,
		PeerAddrs: ts.PeerAddrs, InfoBytes: ts.InfoBytes, Webseeds: ts.Webseeds,
		DhtNodes: ts.DhtNodes, Sources: ts.Sources,
	}
}

func (s Spec) lib() (*torrent.TorrentSpec, error) {
	var h metainfo.Hash
	if err := h.FromHexString(s.InfoHash); err != nil {
		return nil, fmt.Errorf("bt: bad infohash %q: %w", s.InfoHash, err)
	}
	return &torrent.TorrentSpec{
		InfoHash: h, Trackers: s.Trackers, DisplayName: s.DisplayName, PeerAddrs: s.PeerAddrs,
		InfoBytes: s.InfoBytes, Webseeds: s.Webseeds, DhtNodes: s.DhtNodes, Sources: s.Sources,
	}, nil
}
