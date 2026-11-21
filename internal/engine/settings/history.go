package settings

import (
	"encoding/json"
	"reflect"
	"strings"
	"time"

	"tiramisu/internal/metadb"
)

// KeepVersions is how many settings versions the history keeps.
const KeepVersions = 50

// NormalizeActor keeps the sources a caller may claim (ui, api, internal; anything else is
// api), drops control characters from the actor and cuts it at 64 runes.
func NormalizeActor(source, actor string) (string, string) {
	switch source {
	case "ui", "api", "internal":
	default:
		source = "api"
	}
	r := []rune(strings.Map(func(c rune) rune {
		if c < 0x20 || c == 0x7f {
			return -1
		}
		return c
	}, actor))
	if len(r) > 64 {
		r = r[:64]
	}
	actor = string(r)
	return source, actor
}

// ignoredKeys are not manual settings: never compared, never restored.
func ignoredKeys(portManaged bool) []string {
	k := []string{"StoreSettingsInJson", "SslPort", "SslCert", "SslKey"}
	if portManaged {
		k = append(k, "PeersListenPort")
	}
	return k
}

// sameIgnoring compares two documents without the ignored keys.
func sameIgnoring(a, b []byte, ignored []string) bool {
	var ma, mb map[string]any
	if json.Unmarshal(a, &ma) != nil || json.Unmarshal(b, &mb) != nil {
		return false
	}
	for _, k := range ignored {
		delete(ma, k)
		delete(mb, k)
	}
	return reflect.DeepEqual(ma, mb)
}

type history struct {
	db  *metadb.DB
	now func() time.Time
}

// record appends doc unless it equals the newest version (or, for the baseline, unless the
// table has any row). Insert and prune share one transaction.
func (h history) record(doc []byte, source, actor, note string, ignored []string, onlyIfEmpty bool) (int64, bool, error) {
	latest, ok, err := h.db.LatestGoStormSettingsVersion()
	if err != nil {
		return 0, false, err
	}
	if ok && (onlyIfEmpty || sameIgnoring(latest.JSON, doc, ignored)) {
		return 0, false, nil
	}
	id, err := h.db.InsertGoStormSettingsVersion(metadb.ConfigVersion{
		JSON: doc, CreatedAt: h.now().Unix(), Source: source, Actor: actor, Note: note,
	}, KeepVersions)
	return id, err == nil, err
}
