package settings

import (
	"encoding/json"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"tiramisu/internal/metadb"
)

// KeepVersions is how many Engine settings versions the history keeps.
const KeepVersions = 50

var (
	portManaged atomic.Bool // NAT-PMP owns PeersListenPort
	versionMu   sync.Mutex
)

// Headers the :9080 proxy sets on /settings with who saved, for the history.
const (
	HeaderSource = "X-Tiramisu-Source"
	HeaderActor  = "X-Tiramisu-Actor"
)

// ActorFrom reads who saved from the proxy headers: a direct call is "api". Forging them
// on loopback only mislabels a history row.
func ActorFrom(source, actor string) (string, string) {
	switch source {
	case "ui", "api", "internal":
	default:
		source = "api"
	}
	if r := []rune(actor); len(r) > 64 {
		actor = string(r[:64])
	}
	return source, actor
}

// SetPortManagedExternally tells the history that NAT-PMP sets the peer port: it is then
// left out of comparisons and never restored, since an old port is not forwarded.
func SetPortManagedExternally(on bool) { portManaged.Store(on) }

// HistoryIgnoredKeys are the keys that are not manual settings: not compared, not restored.
func HistoryIgnoredKeys() []string {
	// The backend choice and the SSL fields (set from the command line, regenerated at start).
	k := []string{"StoreSettingsInJson", "SslPort", "SslCert", "SslKey"}
	if portManaged.Load() {
		k = append(k, "PeersListenPort")
	}
	return k
}

// CurrentJSON is the settings in use as a document, for the history.
func CurrentJSON() []byte {
	btsetsMu.RLock()
	defer btsetsMu.RUnlock()
	buf, _ := json.Marshal(BTsets)
	return buf
}

// KeepIgnored copies the keys HistoryIgnoredKeys lists from the settings in use into s,
// so that restoring an old version never brings them back.
func KeepIgnored(s *BTSets) {
	btsetsMu.RLock()
	defer btsetsMu.RUnlock()
	if BTsets == nil {
		return
	}
	s.StoreSettingsInJson = BTsets.StoreSettingsInJson
	s.SslPort, s.SslCert, s.SslKey = BTsets.SslPort, BTsets.SslCert, BTsets.SslKey
	if portManaged.Load() {
		s.PeersListenPort = BTsets.PeersListenPort
	}
}

// RecordVersion saves the settings in use as a version when they differ from the newest one.
// Only manual saves call it: NAT-PMP and start-up defaults would bury them.
func RecordVersion(source, actor, note string) (int64, bool, error) {
	return record(source, actor, note, false)
}

// RecordBaseline saves the settings in use once, when the history is still empty.
func RecordBaseline() {
	record("default", "", "baseline", true)
}

func record(source, actor, note string, onlyIfEmpty bool) (int64, bool, error) {
	if ReadOnly || stateDB == nil {
		return 0, false, nil
	}
	btsetsMu.RLock()
	var buf []byte
	var err error
	if BTsets != nil {
		buf, err = json.Marshal(BTsets)
	}
	btsetsMu.RUnlock()
	if buf == nil || err != nil {
		return 0, false, err
	}

	versionMu.Lock()
	defer versionMu.Unlock()
	latest, ok, err := stateDB.LatestGoStormSettingsVersion()
	if err != nil {
		return 0, false, err
	}
	if ok && (onlyIfEmpty || SameSettings(latest.JSON, buf)) {
		return 0, false, nil
	}
	id, err := stateDB.InsertGoStormSettingsVersion(metadb.ConfigVersion{
		JSON: buf, CreatedAt: time.Now().Unix(), Source: source, Actor: actor, Note: note,
	}, KeepVersions)
	return id, err == nil, err
}

// SameSettings compares two settings documents without the keys HistoryIgnoredKeys lists.
func SameSettings(a, b []byte) bool {
	var ma, mb map[string]any
	if json.Unmarshal(a, &ma) != nil || json.Unmarshal(b, &mb) != nil {
		return false
	}
	for _, k := range HistoryIgnoredKeys() {
		delete(ma, k)
		delete(mb, k)
	}
	return reflect.DeepEqual(ma, mb)
}
