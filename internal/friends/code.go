// Package friends pairs trusted Tiramisu nodes: invite codes, pair keys, control messages, state.
package friends

import (
	"encoding/base32"
	"encoding/binary"
	"errors"
	"net/netip"
	"strings"
)

const codePrefix = "TMF1-"

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// Invite is what a code carries: the one-time secret and the inviter's public address.
type Invite struct {
	Secret [16]byte
	Addr   netip.AddrPort
}

func validAddr(a netip.AddrPort) bool {
	return a.Port() != 0 && a.Addr().Is4() && !a.Addr().IsUnspecified()
}

// EncodeCode renders an invite as TMF1-XXXX-... (base32, IPv4 only).
func EncodeCode(inv Invite) (string, error) {
	if !validAddr(inv.Addr) {
		return "", errors.New("friends: invite address must be a dialable IPv4")
	}
	var raw [22]byte
	copy(raw[:16], inv.Secret[:])
	ip := inv.Addr.Addr().As4()
	copy(raw[16:20], ip[:])
	binary.BigEndian.PutUint16(raw[20:], inv.Addr.Port())
	s := b32.EncodeToString(raw[:])
	var b strings.Builder
	b.WriteString(codePrefix)
	for i := 0; i < len(s); i += 4 {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(s[i:min(i+4, len(s))])
	}
	return b.String(), nil
}

// DecodeCode parses a code, tolerating case, whitespace and dashes.
func DecodeCode(code string) (Invite, error) {
	c := strings.ToUpper(strings.Join(strings.Fields(code), ""))
	if !strings.HasPrefix(c, codePrefix) {
		return Invite{}, errors.New("friends: not an invitation code")
	}
	raw, err := b32.DecodeString(strings.ReplaceAll(c[len(codePrefix):], "-", ""))
	if err != nil || len(raw) != 22 {
		return Invite{}, errors.New("friends: malformed invitation code")
	}
	var inv Invite
	copy(inv.Secret[:], raw[:16])
	inv.Addr = netip.AddrPortFrom(netip.AddrFrom4([4]byte(raw[16:20])), binary.BigEndian.Uint16(raw[20:]))
	if !validAddr(inv.Addr) {
		return Invite{}, errors.New("friends: invitation code has an undialable address")
	}
	return inv, nil
}
