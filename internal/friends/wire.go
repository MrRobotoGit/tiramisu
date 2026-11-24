package friends

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

const maxFrame = 4 << 10

// Msg is one control message; unused fields stay empty.
type Msg struct {
	Type    string `json:"type"`
	Name    string `json:"name,omitempty"`
	Nonce   string `json:"nonce,omitempty"`
	Addr    string `json:"addr,omitempty"`
	Expires int64  `json:"expires,omitempty"`
	Seq     int64  `json:"seq,omitempty"`
	Pub     string `json:"pub,omitempty"`  // ephemeral X25519 public key, hex (join, approved)
	Back    bool   `json:"back,omitempty"` // addr: announce yourself in return
	// Help (watch, help, win, unwatch): the infohash in hex, the watcher's position and rate, the
	// window of pieces and the share i of n.
	Hash  string `json:"hash,omitempty"`
	Pos   int64  `json:"pos,omitempty"`
	Bps   int64  `json:"bps,omitempty"`
	First int    `json:"first,omitempty"`
	Last  int    `json:"last,omitempty"`
	I     int    `json:"i,omitempty"`
	N     int    `json:"n,omitempty"`
	OK    bool   `json:"ok,omitempty"`
	// Library (lib_*): a JSON list of entries or a base64 piece of metainfo, and what remains.
	Body string `json:"body,omitempty"`
	Left int64  `json:"left,omitempty"`
	MAC  string `json:"mac,omitempty"`
}

// WriteMsg frames m like a BEP 10 extended message (id 20, extension 1).
func WriteMsg(w io.Writer, m Msg) error {
	if s, ok := w.(*secureRW); ok {
		return s.writeMsg(m)
	}
	p, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(p) > maxFrame {
		return errors.New("friends: message too large")
	}
	buf := make([]byte, 6+len(p))
	binary.BigEndian.PutUint32(buf, uint32(2+len(p)))
	buf[4], buf[5] = 20, 1
	copy(buf[6:], p)
	_, err = w.Write(buf)
	return err
}

// ReadMsg reads one framed message, rejecting oversized or non-control frames.
func ReadMsg(r io.Reader) (Msg, error) {
	if s, ok := r.(*secureRW); ok {
		return s.readMsg()
	}
	var hdr [6]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Msg{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:4])
	if n < 2 || n-2 > maxFrame {
		return Msg{}, errors.New("friends: bad frame length")
	}
	if hdr[4] != 20 || hdr[5] != 1 {
		return Msg{}, errors.New("friends: not a control frame")
	}
	p := make([]byte, n-2)
	if _, err := io.ReadFull(r, p); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF // a header was read: not a clean end before any byte
		}
		return Msg{}, err
	}
	var m Msg
	return m, json.Unmarshal(p, &m)
}
