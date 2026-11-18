package webtls

import (
	"bufio"
	"errors"
	"net"
	"sync"
	"time"
)

const tlsHandshake = 0x16 // first byte of a TLS record carrying a ClientHello

// split serves one port as two listeners. The first byte is read in a goroutine per
// connection, under a deadline, so a client that sends nothing cannot stall Accept.
// The port is one unit: closing either listener closes both, and the caller shuts down
// both servers when one returns.
func split(ln net.Listener, peekTimeout time.Duration, logf func(string, ...any)) (tlsLn, plainLn net.Listener) {
	done := make(chan struct{})
	t := &chanListener{addr: ln.Addr(), ch: make(chan net.Conn), done: done}
	p := &chanListener{addr: ln.Addr(), ch: make(chan net.Conn), done: done}
	var once sync.Once
	stop := func() { once.Do(func() { close(done); ln.Close() }) }
	t.stop, p.stop = stop, stop
	go func() {
		backoff, failures := 5*time.Millisecond, 0
		for {
			c, err := ln.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					stop()
					return
				}
				// EMFILE and the like pass; an error that lasts minutes means a dead port.
				if failures++; failures > 300 {
					logf("[TLS] ERROR accept keeps failing (%v): closing :%s", err, portOf(ln.Addr()))
					stop()
					return
				}
				logf("[TLS] accept: %v", err)
				time.Sleep(backoff)
				backoff = min(backoff*2, time.Second)
				continue
			}
			backoff, failures = 5*time.Millisecond, 0
			go route(c, peekTimeout, t, p)
		}
	}()
	return t, p
}

func route(c net.Conn, timeout time.Duration, t, p *chanListener) {
	br := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(timeout))
	b, err := br.Peek(1)
	c.SetReadDeadline(time.Time{})
	if err != nil {
		c.Close()
		return
	}
	dst := p
	if b[0] == tlsHandshake {
		dst = t
	}
	select {
	case dst.ch <- &peekedConn{Conn: c, r: br}:
	case <-dst.done:
		c.Close()
	}
}

type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// CloseWrite: net/http uses it to close a connection with an unread body (a 403 on a
// POST) without the client seeing a reset instead of the response.
func (c *peekedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func portOf(a net.Addr) string {
	_, p, _ := net.SplitHostPort(a.String())
	return p
}

type chanListener struct {
	addr net.Addr
	ch   chan net.Conn
	done chan struct{}
	stop func()
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error   { l.stop(); return nil }
func (l *chanListener) Addr() net.Addr { return l.addr }
