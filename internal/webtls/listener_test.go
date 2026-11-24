package webtls

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

// dialSend writes b and keeps the connection open for a second; errors go to errc.
func dialSend(ln net.Listener, b []byte, errc chan<- error) {
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		errc <- err
		return
	}
	defer c.Close()
	if _, err := c.Write(b); err != nil {
		errc <- err
		return
	}
	time.Sleep(time.Second)
}

func TestSplitRoutesByFirstByte(t *testing.T) {
	ln := listen(t)
	tl, pl := split(ln, 2*time.Second, nolog)
	errc := make(chan error, 2)

	go dialSend(ln, []byte{0x16, 0x03, 0x01}, errc)
	c, err := tl.Accept()
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 3)
	if _, err := io.ReadFull(c, b); err != nil || b[0] != 0x16 {
		t.Fatalf("the byte read for routing must stay in the connection: %v %v", b, err)
	}
	if _, ok := c.(interface{ CloseWrite() error }); !ok {
		t.Fatal("CloseWrite must stay available to net/http")
	}

	go dialSend(ln, []byte("GET / HTTP/1.1\r\n"), errc)
	c, err = pl.Accept()
	if err != nil {
		t.Fatal(err)
	}
	b = make([]byte, 3)
	io.ReadFull(c, b)
	if string(b) != "GET" {
		t.Fatalf("plain: %q", b)
	}
	select {
	case err := <-errc:
		t.Fatal(err)
	default:
	}
}

func TestSplitSlowClientDoesNotBlock(t *testing.T) {
	ln := listen(t)
	_, pl := split(ln, 300*time.Millisecond, nolog)
	mute, err := net.Dial("tcp", ln.Addr().String()) // never writes
	if err != nil {
		t.Fatal(err)
	}
	defer mute.Close()
	errc := make(chan error, 1)
	go dialSend(ln, []byte("G"), errc)
	done := make(chan struct{})
	go func() { pl.Accept(); close(done) }()
	select {
	case <-done:
	case err := <-errc:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("a silent client blocks the others")
	}
	mute.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err = mute.Read(make([]byte, 1))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("the silent client should have been closed by the server after the deadline, got: %v", err)
	}
}

func TestSplitCloseUnblocksAccept(t *testing.T) {
	for _, which := range []string{"ln", "tls", "plain"} {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		tl, pl := split(ln, time.Second, nolog)
		switch which {
		case "ln":
			ln.Close()
		case "tls":
			tl.Close()
		case "plain":
			pl.Close()
		}
		if _, err := tl.Accept(); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("%s: tls Accept after close: %v", which, err)
		}
		if _, err := pl.Accept(); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("%s: plain Accept after close: %v", which, err)
		}
	}
}
