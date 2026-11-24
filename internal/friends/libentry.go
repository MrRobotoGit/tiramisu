package friends

import (
	"encoding/json"
	"math"
	"path"
	"strings"
)

// LibEntry is one stub of the master's library, as the slave needs it to write the same file.
type LibEntry struct {
	Path  string `json:"p"`           // relative to the media root: movies/… or tv/…
	Hash  string `json:"h"`           // 40 hex
	Index int    `json:"i"`           // file ID in the stream URL
	Size  int64  `json:"s"`           // file length
	IMDB  string `json:"m,omitempty"` // movie stubs only
	Key   string `json:"k,omitempty"` // TV registry key
	Score int    `json:"q,omitempty"` // TV registry quality score
	Show  string `json:"x,omitempty"` // TV registry show IMDb id
	Info  bool   `json:"n,omitempty"` // the master holds the metainfo
	Del   bool   `json:"d,omitempty"`
}

// Valid reports whether e is an entry a slave may apply: a clean .mkv path under movies/ or tv/.
// The name is opaque: it is mirrored as is, even when it does not carry the hash.
func (e LibEntry) Valid() bool { return e.valid() }

func (e LibEntry) valid() bool {
	if !isHex40(e.Hash) || strings.Contains(e.Path, "\\") || path.Clean(e.Path) != e.Path || !IsStubName(e.Path) {
		return false
	}
	if !strings.HasPrefix(e.Path, "movies/") && !strings.HasPrefix(e.Path, "tv/") {
		return false
	}
	return e.Del || (e.Index >= 1 && e.Size > 0)
}

// IsStubName reports whether p names a stub: a non-empty name ending in .mkv, in any case.
func IsStubName(p string) bool {
	base := path.Base(p)
	return len(base) > len(".mkv") && strings.EqualFold(path.Ext(base), ".mkv")
}

func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// blockMsg is the largest lib_block that could carry es: every numeric field at its widest.
func blockMsg(es []LibEntry) Msg {
	body, _ := json.Marshal(es)
	return Msg{Type: "lib_block", Nonce: strings.Repeat("0", 32), Seq: math.MaxInt64, Left: math.MinInt64,
		Body: string(body), MAC: strings.Repeat("0", 64)}
}

// frameFits reports whether m serializes within frame bytes.
func frameFits(m Msg, frame int) bool {
	p, err := json.Marshal(m)
	return err == nil && len(p) <= frame
}
