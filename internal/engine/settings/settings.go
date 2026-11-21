package settings

import (
	"reflect"
	"sort"
)

// Settings are the keys Tentacle interprets. JSON keys are the Go field names, as today.
type Settings struct {
	CacheSize                int64 // bytes per torrent
	ReaderReadAHead          int   // percent
	UseDisk                  bool  // SSD warmup on (read once at start)
	TorrentsSavePath         string
	ForceEncrypt             bool
	RetrackersMode           int
	TorrentDisconnectTimeout int // seconds
	EnableDebug              bool
	EnableIPv6               bool
	DisableTCP               bool
	DisableUTP               bool
	DisableDHT               bool
	DisablePEX               bool
	DisableUpload            bool
	DownloadRateLimit        int // kB/s
	UploadRateLimit          int // kB/s
	ConnectionsLimit         int
	AggressivePeerManagement bool
	PeersListenPort          int
	DisableUPNP              bool // library UPnP off; NAT-PMP on turns it off too
	ResponsiveMode           bool
	AdaptiveShield           bool
}

const (
	defaultCacheSize  = 128 << 20
	defaultConns      = 25
	defaultTimeout    = 15
	clampTimeout      = 30 // the save clamp for <= 0 differs from the default, as today
	minReadAhead      = 5
	maxReadAhead      = 100
	defaultReadAhead  = 75
	defaultRetrackers = 1
)

// Defaults is the document written when none exists.
func Defaults() Settings {
	return Settings{
		CacheSize: defaultCacheSize, ReaderReadAHead: defaultReadAhead, RetrackersMode: defaultRetrackers,
		TorrentDisconnectTimeout: defaultTimeout, EnableIPv6: true, DisableUTP: true,
		ConnectionsLimit: defaultConns, AggressivePeerManagement: true, ResponsiveMode: true, AdaptiveShield: true,
	}
}

// clamped applies the save rules of engine.md §3.3.
func (s Settings) clamped() Settings {
	if s.CacheSize <= 0 {
		s.CacheSize = defaultCacheSize
	}
	s.ReaderReadAHead = min(max(s.ReaderReadAHead, minReadAhead), maxReadAhead)
	if s.ConnectionsLimit <= 0 {
		s.ConnectionsLimit = defaultConns
	}
	if s.TorrentDisconnectTimeout <= 0 {
		s.TorrentDisconnectTimeout = clampTimeout
	}
	if s.TorrentsSavePath == "" {
		s.UseDisk = false
	}
	return s
}

// loaded applies the only clamp of a load.
func (s Settings) loaded() Settings {
	s.ReaderReadAHead = max(s.ReaderReadAHead, minReadAhead)
	return s
}

// Kind says what a change of a key needs before it takes effect.
type Kind int

const (
	KindLive      Kind = iota + 1 // read on use: stored and applied, torrents untouched
	KindReconnect                 // a client option: rebuild the client, playback stops
	KindStore                     // the piece store budget: rebuilt by the reconnect
	KindRestart                   // read once at start (SSD warmup)
)

var kinds = map[string]Kind{
	"CacheSize": KindStore, "ReaderReadAHead": KindLive, "UseDisk": KindRestart, "TorrentsSavePath": KindRestart,
	"ForceEncrypt": KindReconnect, "RetrackersMode": KindLive, "TorrentDisconnectTimeout": KindLive,
	"EnableDebug": KindReconnect, "EnableIPv6": KindReconnect, "DisableTCP": KindReconnect,
	"DisableUTP": KindReconnect, "DisableDHT": KindReconnect, "DisablePEX": KindReconnect,
	"DisableUpload": KindReconnect, "DownloadRateLimit": KindReconnect, "UploadRateLimit": KindReconnect,
	"ConnectionsLimit": KindReconnect, "AggressivePeerManagement": KindReconnect, "PeersListenPort": KindReconnect, "DisableUPNP": KindReconnect,
	"ResponsiveMode": KindLive, "AdaptiveShield": KindLive,
}

// KindOf classifies a key; ok is false for keys Tentacle does not interpret.
func KindOf(key string) (Kind, bool) {
	k, ok := kinds[key]
	return k, ok
}

// Keys lists the interpreted keys in field order.
func Keys() []string {
	t := reflect.TypeFor[Settings]()
	out := make([]string, t.NumField())
	for i := range out {
		out[i] = t.Field(i).Name
	}
	return out
}

// changed lists the keys whose values differ, sorted.
func changed(a, b Settings) []string {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	var out []string
	for i, k := range Keys() {
		if va.Field(i).Interface() != vb.Field(i).Interface() {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
