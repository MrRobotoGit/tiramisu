package friends

import (
	"context"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

// The secure control channel: after a signed hello carrying a fresh salt from each side, every
// frame is ChaCha20-Poly1305 under a per-direction key, its nonce a counter that must grow by one.
const (
	secHello   = "sec"                 // the hello's message type, in a plain signed frame
	secExt     = 2                     // extension id of encrypted frames (plain ones use 1)
	secInfo    = "tiramisu-control-v2" // HKDF info, followed by the sending side
	secSaltLen = 16
)

var (
	errSecFrame   = errors.New("friends: bad encrypted frame")
	errSecOrder   = errors.New("friends: encrypted frame out of order or replayed")
	errSecRefused = errors.New("friends: plaintext refused from a friend that encrypts")
)

// secureRW carries framed messages encrypted and authenticated on rw.
type secureRW struct {
	rw         io.ReadWriter
	send, recv cipher.AEAD
	sent, got  uint64 // counters of the last frame sent and received
}

// controlKeys derives the key each side sends with: HKDF-SHA256 of the pair key followed by the
// ephemeral X25519 shared secret (forward secrecy), salted with both hellos' salts (dialler's first).
func controlKeys(pair PairKey, shared, saltDialler, saltServer []byte) (inviter, joiner cipher.AEAD, err error) {
	salt := append(append([]byte{}, saltDialler...), saltServer...)
	ikm := append(append([]byte{}, pair[:]...), shared...)
	key := func(side Role) (cipher.AEAD, error) {
		k, err := hkdf.Key(sha256.New, ikm, salt, secInfo+" "+string(rune(side)), chacha20poly1305.KeySize)
		if err != nil {
			return nil, err
		}
		return chacha20poly1305.New(k)
	}
	if inviter, err = key(RoleInviter); err != nil {
		return nil, nil, err
	}
	joiner, err = key(RoleJoiner)
	return inviter, joiner, err
}

// newSecure wraps rw for our side of the pair.
func newSecure(rw io.ReadWriter, pair PairKey, ours Role, shared, saltDialler, saltServer []byte) (*secureRW, error) {
	inv, join, err := controlKeys(pair, shared, saltDialler, saltServer)
	if err != nil {
		return nil, err
	}
	s := &secureRW{rw: rw, send: inv, recv: join}
	if ours == RoleJoiner {
		s.send, s.recv = join, inv
	}
	return s, nil
}

// secHeader is a frame's clear part: length, BEP 10 id 20, extension secExt, counter. It is the
// AEAD's additional data.
func secHeader(n uint64, ctLen int) []byte {
	h := make([]byte, 14)
	binary.BigEndian.PutUint32(h, uint32(2+8+ctLen))
	h[4], h[5] = 20, secExt
	binary.BigEndian.PutUint64(h[6:], n)
	return h
}

func secNonce(n uint64) []byte {
	nonce := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint64(nonce[4:], n)
	return nonce
}

func (s *secureRW) writeFrame(p []byte) error {
	if len(p) > maxFrame {
		return errors.New("friends: message too large")
	}
	s.sent++
	h := secHeader(s.sent, len(p)+s.send.Overhead())
	frame := s.send.Seal(h, secNonce(s.sent), p, h)
	_, err := s.rw.Write(frame)
	return err
}

func (s *secureRW) readFrame() ([]byte, error) {
	var h [14]byte
	if _, err := io.ReadFull(s.rw, h[:6]); err != nil {
		return nil, err // io.EOF at a frame boundary stays io.EOF
	}
	n := binary.BigEndian.Uint32(h[:4])
	if h[4] != 20 || h[5] != secExt || n < 2+8+uint32(s.recv.Overhead()) || n-2-8 > uint32(maxFrame+s.recv.Overhead()) {
		return nil, errSecFrame
	}
	if _, err := io.ReadFull(s.rw, h[6:]); err != nil {
		return nil, unexpected(err)
	}
	ctr := binary.BigEndian.Uint64(h[6:])
	if ctr != s.got+1 {
		return nil, errSecOrder
	}
	ct := make([]byte, n-2-8)
	if _, err := io.ReadFull(s.rw, ct); err != nil {
		return nil, unexpected(err)
	}
	p, err := s.recv.Open(ct[:0], secNonce(ctr), ct, h[:])
	if err != nil {
		return nil, errSecFrame
	}
	s.got = ctr
	return p, nil
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// Read and Write make secureRW an io.ReadWriter; messages go through WriteMsg and ReadMsg only.
func (s *secureRW) Read([]byte) (int, error)  { return 0, errors.New("friends: use ReadMsg") }
func (s *secureRW) Write([]byte) (int, error) { return 0, errors.New("friends: use WriteMsg") }

func (s *secureRW) writeMsg(m Msg) error {
	p, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.writeFrame(p)
}

func (s *secureRW) readMsg() (Msg, error) {
	p, err := s.readFrame()
	if err != nil {
		return Msg{}, err
	}
	var m Msg
	return m, json.Unmarshal(p, &m)
}

func newSalt() []byte { return randomBytes(secSaltLen) }

func decodeSalt(s string) ([]byte, bool) {
	b, err := hex.DecodeString(s)
	return b, err == nil && len(b) == secSaltLen
}

// helloSecure starts the encrypted channel on a fresh connection. old reports an older node: it
// closed cleanly on the hello without a byte of answer. Any other failure is not taken for one.
func (m *Manager) helloSecure(ctx context.Context, f *friendEntry, rw io.ReadWriter) (io.ReadWriter, bool, error) {
	salt := newSalt()
	eph, err := NewEphemeral()
	if err != nil {
		return nil, false, err
	}
	hello := Msg{Type: secHello, Nonce: hex.EncodeToString(salt), Pub: hex.EncodeToString(eph.PublicKey().Bytes())}
	hello.MAC = SignPair(f.pair, f.role, hello, "")
	if err := WriteMsg(rw, hello); err != nil {
		return nil, false, err
	}
	ans, err := ReadMsg(rw)
	if err != nil {
		// A clean end before any byte, while we still wanted the answer: the friend does not know "sec".
		return nil, errors.Is(err, io.EOF) && ctx.Err() == nil, err
	}
	switch {
	case ans.Type == "removed" && CheckPair(f.pair, f.role.Other(), ans, ""):
		m.logf("%q removed us while we were away", f.name)
		m.dropFriend(f, false) // they deleted us and will never dial: no tombstone
		return nil, false, ErrNotFound
	case ans.Type != secHello || !CheckPair(f.pair, f.role.Other(), ans, hello.Nonce):
		return nil, false, ErrUnexpected
	}
	theirs, ok := decodeSalt(ans.Nonce)
	pub, okPub := decodePub(ans.Pub)
	if !ok || !okPub {
		return nil, false, ErrUnexpected
	}
	shared, err := ecdhShared(eph, pub)
	if err != nil {
		return nil, false, ErrUnexpected
	}
	s, err := newSecure(rw, f.pair, f.role, shared, salt, theirs)
	if err != nil {
		return nil, false, err
	}
	m.markSecure(f) // the answer is signed over our fresh salt: no replay can produce it
	return s, false, nil
}

// answerSecure answers a friend's hello and switches the connection to the encrypted channel. The
// friend is recorded as encrypting only once its first encrypted frame decrypts (a hello replays).
func (m *Manager) answerSecure(f *friendEntry, hello Msg, rw io.ReadWriter) (io.ReadWriter, bool) {
	if !CheckPair(f.pair, f.role.Other(), hello, "") {
		return nil, false // before any X25519 work: an unauthenticated peer costs a MAC only
	}
	theirs, ok := decodeSalt(hello.Nonce)
	pub, okPub := decodePub(hello.Pub)
	if !ok || !okPub {
		return nil, false
	}
	eph, err := NewEphemeral()
	if err != nil {
		return nil, false
	}
	shared, err := ecdhShared(eph, pub)
	if err != nil {
		return nil, false
	}
	salt := newSalt()
	ans := Msg{Type: secHello, Nonce: hex.EncodeToString(salt), Pub: hex.EncodeToString(eph.PublicKey().Bytes())}
	ans.MAC = SignPair(f.pair, f.role, ans, hello.Nonce)
	if WriteMsg(rw, ans) != nil {
		return nil, false
	}
	s, err := newSecure(rw, f.pair, f.role, shared, theirs, salt)
	if err != nil {
		return nil, false
	}
	return s, true
}

func ecdhShared(mine *ecdh.PrivateKey, theirs []byte) ([]byte, error) {
	peer, err := ecdh.X25519().NewPublicKey(theirs)
	if err != nil {
		return nil, err
	}
	return mine.ECDH(peer) // an all-zero result is an error
}

// markSecure records, once, that f speaks the encrypted channel: from now on plaintext is refused.
func (m *Manager) markSecure(f *friendEntry) {
	m.mu.Lock() // the flag and its row change together, against ResetEncryption
	if f.secure.Load() {
		m.mu.Unlock()
		return
	}
	f.secure.Store(true)
	delete(m.plainUntil, f.control)
	delete(m.refusedPlain, f.control)
	err := m.store.SetFriendSecure(f.name, f.pair[:])
	m.mu.Unlock()
	if err != nil {
		m.logf("store encryption of %q: %v", f.name, err)
	}
	m.logf("control channel with %q is encrypted", f.name)
}

// refusePlain logs, once per friend until a reset, that plaintext with an encrypting friend was refused.
func (m *Manager) refusePlain(f *friendEntry, line string) {
	m.mu.Lock()
	seen := m.refusedPlain[f.control]
	m.refusedPlain[f.control] = true
	m.mu.Unlock()
	if !seen {
		m.logf("%s", line)
	}
}

// ResetEncryption forgets that friend encrypts, so an older version of it is spoken to in clear again.
func (m *Manager) ResetEncryption(name string) error {
	f := m.friendByName(name)
	if f == nil {
		return ErrNotFound
	}
	m.mu.Lock()
	if !f.secure.Load() {
		m.mu.Unlock()
		return nil // nothing to forget: a no-op, not an error
	}
	err := m.store.ClearFriendSecure(f.name, f.pair[:])
	if err == nil {
		f.secure.Store(false)
		delete(m.refusedPlain, f.control)
		delete(m.plainUntil, f.control)
	}
	m.mu.Unlock()
	if err == nil {
		m.logf("encryption with %q reset: the next contact negotiates again", name)
	}
	return err
}

// plainFriend reports a friend recently found to be an older node: no hello until the backoff ends.
func (m *Manager) plainFriend(f *friendEntry) bool {
	if f.secure.Load() {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.plainUntil[f.control].After(m.now())
}

// notePlain remembers an older friend for maxBackoff, logging only the first time.
func (m *Manager) notePlain(f *friendEntry) {
	m.mu.Lock()
	_, known := m.plainUntil[f.control]
	m.plainUntil[f.control] = m.now().Add(maxBackoff)
	m.mu.Unlock()
	if !known {
		m.logf("%q does not encrypt the control channel yet: plaintext, signed", f.name)
	}
}
