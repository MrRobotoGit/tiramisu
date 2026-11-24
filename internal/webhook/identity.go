// Package webhook reads the identities a Plex or Jellyfin playback webhook carries.
package webhook

import (
	"encoding/json"
	"regexp"
	"strings"
)

// reImdbID matches "imdb://tt1234567" in the Guid array of Plex webhook payloads.
var reImdbID = regexp.MustCompile(`"imdb://(tt\d+)"`)

// MaxBody bounds a webhook post. Plex sends multipart with a thumbnail, so
// the ceiling has to clear a poster, not just the JSON.
const MaxBody = 10 * 1024 * 1024

// reMbid matches "mbid://<uuid>" in Plex webhook payloads for music. A track
// payload can carry several: the recording, its release group and the artist.
var reMbid = regexp.MustCompile(`"mbid://([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})"`)

// reUUID matches a bare MusicBrainz uuid, which is how Jellyfin's ProviderIds
// carry the same identities.
var reUUID = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// IdentityKeys is the identity set a music payload carries, ranked by how
// specific each id is: the item's own recording beats its release group, which
// beats the artist. The ranking is what keeps concurrent sessions of the same
// artist apart when only a coarser id is shared between them.
type IdentityKeys struct {
	recording []string
	release   []string
	artist    []string
}

// Empty reports whether the payload carried no music identity at all.
func (k IdentityKeys) Empty() bool {
	return len(k.recording) == 0 && len(k.release) == 0 && len(k.artist) == 0
}

// Rank reports the most specific group id belongs to: 2 recording, 1 release,
// 0 artist. An unclassified id is deliberately not a match here; the caller puts
// those in the artist group, where they only win when nothing better matches.
func (k IdentityKeys) Rank(id string) (int, bool) {
	for rank, group := range [][]string{k.recording, k.release, k.artist} {
		for _, want := range group {
			if strings.EqualFold(want, id) {
				return 2 - rank, true
			}
		}
	}
	return 0, false
}

// IdentityKeysFromPayload reads the mbid of the item, its parent and its
// grandparent from the payload's JSON shape, because a flat scan cannot tell an
// artist id shared by a whole discography from a recording id unique to one track.
// Ids from fields this does not model stay matchable at the lowest rank.
func IdentityKeysFromPayload(payloadStr string) IdentityKeys {
	keys := IdentityKeys{}
	var raw struct {
		Metadata map[string]interface{} `json:"Metadata"`
	}
	if err := json.Unmarshal([]byte(payloadStr), &raw); err == nil {
		meta := raw.Metadata
		keys.recording = collectMbids(meta["guid"], meta["Guid"])
		keys.release = collectMbids(meta["parentGuid"], meta["ParentGuid"])
		keys.artist = collectMbids(meta["grandparentGuid"], meta["GrandparentGuid"])
		// Jellyfin has no mbid:// scheme: the same identities arrive as bare
		// uuids under ProviderIds.
		if pids, ok := meta["ProviderIds"].(map[string]interface{}); ok {
			keys.recording = append(keys.recording, collectUUIDs(pids["MusicBrainzTrack"])...)
			keys.release = append(keys.release, collectUUIDs(
				pids["MusicBrainzReleaseGroup"], pids["MusicBrainzAlbum"])...)
			keys.artist = append(keys.artist, collectUUIDs(pids["MusicBrainzArtist"])...)
		}
	}
	// Fallback for a payload shape this does not model: keep every mbid matchable,
	// deduplicated into the lowest rank.
	for _, id := range flatMbidIDs(payloadStr) {
		if _, ok := keys.Rank(id); !ok {
			keys.artist = append(keys.artist, id)
		}
	}
	keys.recording = dedupeIDs(keys.recording)
	keys.release = dedupeIDs(keys.release)
	keys.artist = dedupeIDs(keys.artist)
	return keys
}

// collectMbids extracts "mbid://<uuid>" values from any JSON shape a metadata
// field takes (a string or an array of objects with an "id").
func collectMbids(values ...interface{}) []string {
	var ids []string
	for _, value := range values {
		walkJSONStrings(value, func(s string) {
			for _, m := range reMbid.FindAllStringSubmatch(`"`+s+`"`, -1) {
				ids = append(ids, strings.ToLower(m[1]))
			}
		})
	}
	return ids
}

// collectUUIDs extracts bare MusicBrainz uuids from any JSON shape.
func collectUUIDs(values ...interface{}) []string {
	var ids []string
	for _, value := range values {
		walkJSONStrings(value, func(s string) {
			for _, m := range reUUID.FindAllString(s, -1) {
				ids = append(ids, strings.ToLower(m))
			}
		})
	}
	return ids
}

// walkJSONStrings visits every string inside a decoded JSON value.
func walkJSONStrings(value interface{}, visit func(string)) {
	switch t := value.(type) {
	case string:
		visit(t)
	case []interface{}:
		for _, item := range t {
			walkJSONStrings(item, visit)
		}
	case map[string]interface{}:
		for _, item := range t {
			walkJSONStrings(item, visit)
		}
	}
}

// flatMbidIDs is the shape-blind scan, kept as a fallback for payload variants the
// structural reader does not know.
func flatMbidIDs(payloadStr string) []string {
	matches := reMbid.FindAllStringSubmatch(payloadStr, -1)
	if len(matches) == 0 {
		return nil
	}
	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		ids = append(ids, strings.ToLower(m[1]))
	}
	return dedupeIDs(ids)
}

func dedupeIDs(ids []string) []string {
	if len(ids) < 2 {
		return ids
	}
	seen := make(map[string]bool, len(ids))
	out := ids[:0]
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

var reEmptyNumber = regexp.MustCompile(`"(\w+)":\s*,`)

// SanitizeEmptyNumbers turns `"key": ,` (an empty number some servers send) into `"key":0`,
// so the payload decodes as JSON.
func SanitizeEmptyNumbers(payload string) string {
	return reEmptyNumber.ReplaceAllString(payload, `"$1":0`)
}

// ImdbID is the first "imdb://tt…" id anywhere in a payload, "" if none.
func ImdbID(payload string) string {
	if m := reImdbID.FindStringSubmatch(payload); len(m) > 1 {
		return m[1]
	}
	return ""
}
