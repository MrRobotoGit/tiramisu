package friends

import (
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strconv"
)

const peerIDPrefix = "-qB5030-" // same client the swarm already sees (bt.Bep20Prefix)
const b32Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

// PairKey is the secret shared by two paired nodes.
type PairKey [32]byte

// InviteHash is the fake infohash of the invite channel.
func InviteHash(secret [16]byte) [20]byte {
	return sha1.Sum(append([]byte("tiramisu-invite"), secret[:]...))
}

// ControlHash is the fake infohash of a pair's control channel.
func ControlHash(pair PairKey) [20]byte {
	return sha1.Sum(append([]byte("tiramisu-control"), pair[:]...))
}

// NewEphemeral returns a fresh X25519 key for one invite exchange.
func NewEphemeral() (*ecdh.PrivateKey, error) {
	return ecdh.X25519().GenerateKey(rand.Reader)
}

// DerivePairKey computes the pair key from the invite secret, both nonces, both ephemeral public
// keys and their X25519 shared secret; role is our side. An all-zero shared secret is an error.
func DerivePairKey(invite, nonceInviter, nonceJoiner [16]byte, mine *ecdh.PrivateKey, role Role, theirPub []byte) (PairKey, error) {
	peer, err := ecdh.X25519().NewPublicKey(theirPub)
	if err != nil {
		return PairKey{}, err
	}
	shared, err := mine.ECDH(peer)
	if err != nil {
		return PairKey{}, err
	}
	pubI, pubJ := mine.PublicKey().Bytes(), peer.Bytes()
	if role == RoleJoiner {
		pubI, pubJ = pubJ, pubI
	}
	m := hmac.New(sha256.New, invite[:])
	for _, f := range [][]byte{[]byte("tiramisu-pair-v2"), nonceInviter[:], nonceJoiner[:], pubI, pubJ, shared} {
		m.Write(f) // every field has a fixed length
	}
	var k PairKey
	copy(k[:], m.Sum(nil))
	return k, nil
}

// parsePub parses a hex X25519 public key without checking the point.
func parsePub(s string) ([]byte, bool) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil, false
	}
	return b, true
}

// usablePub refuses points that give no shared secret; it costs a key generation and a DH.
func usablePub(b []byte) bool {
	probe, err := NewEphemeral()
	if err != nil {
		return false
	}
	_, err = DerivePairKey([16]byte{}, [16]byte{}, [16]byte{}, probe, RoleInviter, b)
	return err == nil
}

// decodePub parses a hex X25519 public key, refusing points that give no shared secret.
func decodePub(s string) ([]byte, bool) {
	b, ok := parsePub(s)
	return b, ok && usablePub(b)
}

// Role is the side of the pairing that presents an ID, not the direction of the connection.
type Role byte

const (
	RoleInviter Role = 'V'
	RoleJoiner  Role = 'J'
)

// Other returns the opposite side.
func (r Role) Other() Role {
	if r == RoleInviter {
		return RoleJoiner
	}
	return RoleInviter
}

// PeerID is the per-torrent peer ID presented by the given side of the pair.
func PeerID(pair PairKey, ih [20]byte, role Role) (id [20]byte) {
	m := hmac.New(sha256.New, pair[:])
	m.Write(ih[:])
	m.Write([]byte{byte(role)})
	sum := m.Sum(nil)
	copy(id[:], peerIDPrefix)
	for i := 0; i < 12; i++ {
		id[8+i] = b32Alphabet[sum[i]&31]
	}
	return id
}

// VerifyPeerID checks that id is what the given side of the pair presents for ih.
func VerifyPeerID(pair PairKey, ih [20]byte, role Role, id [20]byte) bool {
	want := PeerID(pair, ih, role)
	return hmac.Equal(want[:], id[:])
}

// ConnProof binds one connection to the pair: HMAC(pair, "tiramisu-conn" ‖ s ‖ ih ‖ from ‖
// nonce of the receiver ‖ nonce of from), every field fixed-length. s is the 96-byte MSE secret;
// any other length gives nil.
func ConnProof(pair PairKey, s []byte, ih [20]byte, from Role, nonceTo, nonceFrom [16]byte) []byte {
	if len(s) != 96 {
		return nil
	}
	m := hmac.New(sha256.New, pair[:])
	for _, f := range [][]byte{[]byte("tiramisu-conn"), s, ih[:], {byte(from)}, nonceTo[:], nonceFrom[:]} {
		m.Write(f)
	}
	return m.Sum(nil)
}

// transcript is the MAC input: label, sender role, then type, name, nonce, addr, otherNonce,
// seq (decimal string), pub and, only when set, "back" and the labelled help and library fields, each prefixed
// with its length as uint16 big endian. MAC is excluded. It returns nil when a field does not fit.
func transcript(label string, from Role, m Msg, otherNonce string) []byte {
	var b []byte
	fields := []string{label, string(rune(from)), m.Type, m.Name, m.Nonce, m.Addr, otherNonce, strconv.FormatInt(m.Seq, 10), m.Pub}
	if m.Back {
		fields = append(fields, "back")
	}
	fields = appendExtraFields(fields, m)
	for _, f := range fields {
		if len(f) > 0xFFFF {
			return nil
		}
		b = binary.BigEndian.AppendUint16(b, uint16(len(f)))
		b = append(b, f...)
	}
	return b
}

func sign(key []byte, label string, from Role, m Msg, otherNonce string) string {
	t := transcript(label, from, m, otherNonce)
	if t == nil {
		return ""
	}
	h := hmac.New(sha256.New, key)
	h.Write(t)
	return hex.EncodeToString(h.Sum(nil))
}

func check(key []byte, label string, from Role, m Msg, otherNonce string) bool {
	want := sign(key, label, from, m, otherNonce)
	return want != "" && hmac.Equal([]byte(want), []byte(m.MAC))
}

// SignInvite MACs m, sent by the given side, with the invite secret.
func SignInvite(secret [16]byte, from Role, m Msg, otherNonce string) string {
	return sign(secret[:], "tiramisu-invite-mac", from, m, otherNonce)
}

// CheckInvite verifies m.MAC against the invite secret and the sender's side.
func CheckInvite(secret [16]byte, from Role, m Msg, otherNonce string) bool {
	return check(secret[:], "tiramisu-invite-mac", from, m, otherNonce)
}

// SignPair MACs m, sent by the given side, with the pair key.
func SignPair(pair PairKey, from Role, m Msg, otherNonce string) string {
	return sign(pair[:], "tiramisu-pair-mac", from, m, otherNonce)
}

// CheckPair verifies m.MAC against the pair key and the sender's side.
func CheckPair(pair PairKey, from Role, m Msg, otherNonce string) bool {
	return check(pair[:], "tiramisu-pair-mac", from, m, otherNonce)
}

// appendExtraFields adds the help and library fields that are set, labelled: messages without them
// keep their MAC.
func appendExtraFields(fields []string, m Msg) []string {
	if m.Hash != "" {
		fields = append(fields, "hash="+m.Hash)
	}
	for _, f := range []struct {
		label string
		v     int64
	}{{"pos=", m.Pos}, {"bps=", m.Bps}, {"first=", int64(m.First)}, {"last=", int64(m.Last)}, {"i=", int64(m.I)}, {"n=", int64(m.N)}} {
		if f.v != 0 {
			fields = append(fields, f.label+strconv.FormatInt(f.v, 10))
		}
	}
	if m.OK {
		fields = append(fields, "ok")
	}
	if m.Body != "" {
		fields = append(fields, "body="+m.Body)
	}
	if m.Left != 0 {
		fields = append(fields, "left="+strconv.FormatInt(m.Left, 10))
	}
	return fields
}
