// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"
)

// ServePort serves h over the virtio-serial port at path, one host session at a time, until ctx
// ends. A virtio-serial port reads end-of-file while no host is connected (runnerd restarted, or has
// not dialled yet), but a host that redials at once can leave none between two sessions, so each
// session starts at its host's hello (sessionMagic), wherever it falls in the stream.
func ServePort(ctx context.Context, path string, h http.Handler) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	l := newPortListener(ctx, f, 200*time.Millisecond)
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: h, Protocols: &protocols, ReadHeaderTimeout: time.Minute}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
		_ = f.Close()
	}()
	err = srv.Serve(l)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// chunk is one event of the port's byte stream, in order: bytes, a host's hello (start), end-of-file
// (no host connected) or a failure.
type chunk struct {
	data  []byte
	start bool   // a host's hello: the bytes after it are the next session's
	epoch uint64 // of a start: the listener's epoch once the writes before the hello had ended
	eof   bool
	err   error
}

// portListener yields one portConn per host session over the same open port. One goroutine is the
// port's only reader, so bytes never go to a session that has already ended. Writes have no such
// guard in the port: they reach whichever host is connected when they leave, so a session writes
// only until the reader sees its host go or the next host's hello (epoch).
type portListener struct {
	ctx    context.Context
	w      io.Writer
	chunks chan chunk
	done   chan struct{} // closed by Close: Accept returns, as http.Server.Close needs
	closer sync.Once

	mu      sync.Mutex
	current *portConn

	wmu   sync.Mutex // held by every session write, and by the reader to end them
	epoch uint64     // sessions the reader has ended: only it changes epoch, under wmu
}

func newPortListener(ctx context.Context, port io.ReadWriter, idle time.Duration) *portListener {
	l := &portListener{ctx: ctx, w: port, chunks: make(chan chunk), done: make(chan struct{})}
	go l.read(port, idle)
	return l
}

// read is the port's only reader.
func (l *portListener) read(port io.Reader, idle time.Duration) {
	buf := make([]byte, 32<<10)
	var s helloScanner
	for {
		n, err := port.Read(buf)
		switch {
		case n > 0:
			if !l.scan(&s, buf[:n]) {
				return
			}
		case errors.Is(err, io.EOF):
			s = helloScanner{} // a hello its host left in the middle of is dropped
			l.endWrites()
			if !l.send(chunk{eof: true}) {
				return
			}
			select {
			case <-time.After(idle):
			case <-l.ctx.Done():
				return
			}
		case err != nil:
			l.send(chunk{err: err})
			return
		}
	}
}

func (l *portListener) send(c chunk) bool {
	select {
	case l.chunks <- c:
		return true
	case <-l.ctx.Done():
		return false
	}
}

// helloScanner finds hosts' hellos in the port's bytes. A hello can straddle two reads, so the last
// bytes passed on are kept to match against. They are passed on at once all the same: a session
// whose bytes were held back here could wait on them for good, its host waiting on its answer.
type helloScanner struct {
	tail  []byte // the last magicLen-1 bytes passed on
	hello []byte // a hello whose magic has been read, while the rest of it is
}

// scan passes b on as session bytes and hellos; false once the listener has ended.
func (l *portListener) scan(s *helloScanner, b []byte) bool {
	for len(b) > 0 {
		if s.hello != nil {
			k := min(helloLen-len(s.hello), len(b))
			s.hello, b = append(s.hello, b[:k]...), b[k:]
			if len(s.hello) < helloLen {
				return true
			}
			hello := s.hello
			s.hello = nil
			if !l.startSession(hello) {
				return false
			}
			continue
		}
		t := len(s.tail)
		w := slices.Concat(s.tail, b)
		i := bytes.Index(w, sessionMagic)
		if i < 0 {
			s.tail = bytes.Clone(w[max(0, len(w)-(magicLen-1)):])
			return l.send(chunk{data: w[t:]})
		}
		if i > t && !l.send(chunk{data: w[t:i]}) {
			return false
		}
		s.tail, s.hello = nil, append(make([]byte, 0, helloLen), sessionMagic...)
		b = w[i+magicLen:]
	}
	return true
}

// startSession ends the writes of the session before a host's hello, then answers the hello and
// hands the next session its start. No session writes in between: the ended one is refused, and
// the next starts at the chunk sent here. An echo the port holds for want of a host goes to the
// next one, which drops it: its nonce is not that host's.
func (l *portListener) startSession(hello []byte) bool {
	l.endWrites()
	hello[magicLen] = helloVersion // the echo: the host's nonce, this agent's version
	_, _ = l.w.Write(hello)        // if the port has failed, the reader's next read says so
	return l.send(chunk{start: true, epoch: l.epoch})
}

// endWrites ends the writes of the session whose host has gone, before the reader looks for the
// next host. The kernel holds a write made while no host is connected and hands it to the next
// one, so a past deadline ends any such write first; taking wmu then waits out one in flight.
func (l *portListener) endWrites() {
	d, _ := l.w.(interface{ SetWriteDeadline(time.Time) error })
	if d != nil {
		_ = d.SetWriteDeadline(time.Unix(1, 0))
	}
	l.wmu.Lock()
	l.epoch++
	if d != nil {
		_ = d.SetWriteDeadline(time.Time{})
	}
	l.wmu.Unlock()
}

func (l *portListener) next() (chunk, error) {
	select {
	case c := <-l.chunks:
		return c, c.err
	case <-l.ctx.Done():
		return chunk{}, net.ErrClosed
	case <-l.done:
		return chunk{}, net.ErrClosed
	}
}

func (l *portListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	prev := l.current
	l.mu.Unlock()
	var c chunk
	if prev != nil {
		select {
		case <-prev.closed:
		case <-l.ctx.Done():
			return nil, net.ErrClosed
		case <-l.done:
			return nil, net.ErrClosed
		}
		// Its last Read has returned and no later one takes a chunk, so end is final.
		prev.rmu.Lock()
		c = prev.end
		prev.rmu.Unlock()
	}
	// A session starts at its host's hello: what comes before one (bytes a session left in the
	// port, or those of a host that gave up) is dropped.
	for !c.start {
		var err error
		if c, err = l.next(); err != nil {
			return nil, err
		}
	}
	conn := &portConn{l: l, epoch: c.epoch, closed: make(chan struct{})}
	l.mu.Lock()
	l.current = conn
	l.mu.Unlock()
	return conn, nil
}

func (l *portListener) Close() error {
	l.closer.Do(func() { close(l.done) })
	return nil
}

func (l *portListener) Addr() net.Addr { return portAddr{} }

// portConn is one host session on the port. Close ends the session, never the port.
type portConn struct {
	l      *portListener
	epoch  uint64 // the listener's epoch at its host's hello
	closed chan struct{}
	once   sync.Once

	rmu     sync.Mutex // held by Read; Accept takes it once the session is closed
	pending []byte
	end     chunk // the end-of-file or next hello that ended the session, once Read took it
}

func (c *portConn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	// A closed session, or one past its end, takes no chunk: the next one is the next session's,
	// or one Accept drops on its way to the next hello.
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	if c.end.eof || c.end.start {
		return 0, io.EOF
	}
	if len(c.pending) == 0 {
		select {
		case ch := <-c.l.chunks:
			switch {
			case ch.err != nil:
				return 0, ch.err
			case ch.eof, ch.start:
				c.end = ch
				return 0, io.EOF
			}
			c.pending = ch.data
		case <-c.closed:
			return 0, net.ErrClosed
		case <-c.l.ctx.Done():
			return 0, net.ErrClosed
		}
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *portConn) Write(p []byte) (int, error) {
	c.l.wmu.Lock()
	defer c.l.wmu.Unlock()
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	if c.epoch != c.l.epoch {
		return 0, net.ErrClosed // its host has gone: the bytes would reach the next one
	}
	return c.l.w.Write(p)
}

func (c *portConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *portConn) LocalAddr() net.Addr              { return portAddr{} }
func (c *portConn) RemoteAddr() net.Addr             { return portAddr{} }
func (c *portConn) SetDeadline(time.Time) error      { return nil }
func (c *portConn) SetReadDeadline(time.Time) error  { return nil }
func (c *portConn) SetWriteDeadline(time.Time) error { return nil }

type portAddr struct{}

func (portAddr) Network() string { return "virtio-serial" }
func (portAddr) String() string  { return PortName }
