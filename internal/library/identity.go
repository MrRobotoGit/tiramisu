package library

import (
	"net/http"
	"strings"
)

// resolveAudioIdentity reconciles the hash and magnet a request carries. The magnet's
// own BTIH stays authoritative when no hash is supplied, but a hash that disagrees with
// it is refused: the alternative is waking and mutating a torrent the caller never named.
// Comparison uses the canonical spelling so a base32 magnet and its hex hash are one
// torrent, not a mismatch.
func resolveAudioIdentity(reqHash, reqMagnet string) (hash, magnet string, err error) {
	hash = strings.ToLower(strings.TrimSpace(reqHash))
	magnet = strings.TrimSpace(reqMagnet)
	fromMagnet := ""
	if magnet != "" {
		fromMagnet = HashFromMagnet(magnet)
	}
	if fromMagnet != "" && hash != "" && canonicalHashKey(fromMagnet) != canonicalHashKey(hash) {
		return "", "", errf(http.StatusBadRequest,
			"hash_magnet_mismatch: hash %s but magnet carries %s", hash, fromMagnet)
	}
	switch {
	case fromMagnet != "":
		hash = fromMagnet
	case magnet != "" && hash == "":
		return "", "", errf(http.StatusBadRequest, "magnet carries no info hash")
	}
	if hash == "" {
		return "", "", errf(http.StatusBadRequest, "hash or magnet is required")
	}
	if !reInfoHash.MatchString(hash) {
		return "", "", errf(http.StatusBadRequest, "malformed info hash %q", hash)
	}
	return hash, magnet, nil
}
