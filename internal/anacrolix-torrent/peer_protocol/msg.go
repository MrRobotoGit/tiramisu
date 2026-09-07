package peer_protocol

import (
	"bufio"
	"bytes"
	"encoding"
	"encoding/binary"
	"fmt"
	"io"
)

// This is a lazy union representing all the possible fields for messages. Go doesn't have ADTs, and
// I didn't choose to use type-assertions.
type Message struct {
	Keepalive            bool
	Type                 MessageType
	Index, Begin, Length Integer
	Piece                []byte
	Bitfield             []bool
	ExtendedID           ExtensionNumber
	ExtendedPayload      []byte
	Port                 uint16
}

var _ interface {
	encoding.BinaryUnmarshaler
	encoding.BinaryMarshaler
} = (*Message)(nil)

func MakeCancelMessage(piece, offset, length Integer) Message {
	return Message{
		Type:   Cancel,
		Index:  piece,
		Begin:  offset,
		Length: length,
	}
}

func (msg Message) RequestSpec() (ret RequestSpec) {
	return RequestSpec{
		msg.Index,
		msg.Begin,
		func() Integer {
			if msg.Type == Piece {
				return Integer(len(msg.Piece))
			} else {
				return msg.Length
			}
		}(),
	}
}

func (msg Message) MustMarshalBinary() []byte {
	b, err := msg.MarshalBinary()
	if err != nil {
		panic(err)
	}
	return b
}

func (msg Message) MarshalBinary() (data []byte, err error) {
	var buf bytes.Buffer
	err = msg.MarshalTo(&buf)
	if err != nil {
		return
	}
	data = make([]byte, 4+buf.Len())
	binary.BigEndian.PutUint32(data, uint32(buf.Len()))
	if buf.Len() != copy(data[4:], buf.Bytes()) {
		panic("bad copy")
	}
	return
}

// V239-Optimization: Write directly to writer to avoid allocs
//
// Fixed-width fields go through binary.BigEndian.Put* into a stack array rather than
// binary.Write: that takes an interface{}, so every 1-4 byte field was boxed onto the heap.
// The write sequence is unchanged, so the bytes on the wire are identical.
func (msg Message) MarshalTo(w io.Writer) (err error) {
	if !msg.Keepalive {
		var b [4]byte
		b[0] = byte(msg.Type)
		if _, err = w.Write(b[:1]); err != nil {
			return
		}
		switch msg.Type {
		case Choke, Unchoke, Interested, NotInterested, HaveAll, HaveNone:
		case Have, AllowedFast, Suggest:
			binary.BigEndian.PutUint32(b[:], uint32(msg.Index))
			_, err = w.Write(b[:])
		case Request, Cancel, Reject:
			for _, i := range [3]Integer{msg.Index, msg.Begin, msg.Length} {
				binary.BigEndian.PutUint32(b[:], uint32(i))
				if _, err = w.Write(b[:]); err != nil {
					break
				}
			}
		case Bitfield:
			_, err = w.Write(marshalBitfield(msg.Bitfield))
		case Piece:
			for _, i := range [2]Integer{msg.Index, msg.Begin} {
				binary.BigEndian.PutUint32(b[:], uint32(i))
				if _, err = w.Write(b[:]); err != nil {
					return
				}
			}
			var n int
			// The original shadowed err here, so a failed payload write was reported as
			// success and the peer was left believing it had the piece.
			n, err = w.Write(msg.Piece)
			if err != nil {
				break
			}
			if n != len(msg.Piece) {
				panic(n)
			}
		case Extended:
			b[0] = byte(msg.ExtendedID)
			if _, err = w.Write(b[:1]); err != nil {
				return
			}
			_, err = w.Write(msg.ExtendedPayload)
		case Port:
			binary.BigEndian.PutUint16(b[:], msg.Port)
			_, err = w.Write(b[:2])
		default:
			err = fmt.Errorf("unknown message type: %v", msg.Type)
		}
	}
	return
}

func marshalBitfield(bf []bool) (b []byte) {
	b = make([]byte, (len(bf)+7)/8)
	for i, have := range bf {
		if !have {
			continue
		}
		c := b[i/8]
		c |= 1 << uint(7-i%8)
		b[i/8] = c
	}
	return
}

func (me *Message) UnmarshalBinary(b []byte) error {
	d := Decoder{
		R: bufio.NewReader(bytes.NewReader(b)),
	}
	err := d.Decode(me)
	if err != nil {
		return err
	}
	if d.R.Buffered() != 0 {
		return fmt.Errorf("%d trailing bytes", d.R.Buffered())
	}
	return nil
}
