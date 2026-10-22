// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

// ServePort serves h over the virtio-serial port at path, one host connection at a time, until ctx
// ends. A virtio-serial port reads end-of-file while no host is connected (runnerd restarted, or has
// not dialled yet), so each host session starts at the first byte after one.
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

// chunk is one read of the port: bytes, end-of-file (no host connected) or a failure.
type chunk struct {
	data  []byte
	epoch uint64 // of data: the listener's epoch when it was read
	eof   bool
	err   error
}

// portListener yields one portConn per host session over the same open port. One goroutine is the
// port's only reader, so bytes never go to a session that has already ended. Writes have no such
// guard in the port: they reach whichever host is connected when they leave, so a session writes
// only until the reader sees its host go (epoch).
type portListener struct {
	ctx    context.Context
	w      io.Writer
	chunks chan chunk
	done   chan struct{} // closed by Close: Accept returns, as http.Server.Close needs
	closer sync.Once

	mu      sync.Mutex
	current *portConn

	wmu   sync.Mutex // held by every session write, and by the reader to end them
	epoch uint64     // hosts the reader has seen go: only it changes epoch, under wmu
}

func newPortListener(ctx context.Context, port io.ReadWriter, idle time.Duration) *portListener {
	l := &portListener{ctx: ctx, w: port, chunks: make(chan chunk), done: make(chan struct{})}
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := port.Read(buf)
			var c chunk
			switch {
			case n > 0:
				c.data, c.epoch = append([]byte(nil), buf[:n]...), l.epoch
			case errors.Is(err, io.EOF):
				l.endWrites()
				c.eof = true
			case err != nil:
				c.err = err
			default:
				continue
			}
			select {
			case l.chunks <- c:
			case <-ctx.Done():
				return
			}
			if c.err != nil {
				return
			}
			if c.eof {
				select {
				case <-time.After(idle):
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return l
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
	if prev != nil {
		select {
		case <-prev.closed:
		case <-l.ctx.Done():
			return nil, net.ErrClosed
		case <-l.done:
			return nil, net.ErrClosed
		}
		// Its last Read has returned and no later one takes a chunk, so sawEOF is final.
		prev.rmu.Lock()
		eof := prev.sawEOF
		prev.rmu.Unlock()
		// A session that ended on anything but the host going away may have left its bytes in the
		// port: drop them until the host disconnects, so the next session starts clean.
		for !eof {
			c, err := l.next()
			if err != nil {
				return nil, err
			}
			eof = c.eof
		}
	}
	for {
		c, err := l.next()
		if err != nil {
			return nil, err
		}
		if len(c.data) > 0 {
			conn := &portConn{l: l, epoch: c.epoch, pending: c.data, closed: make(chan struct{})}
			l.mu.Lock()
			l.current = conn
			l.mu.Unlock()
			return conn, nil
		}
	}
}

func (l *portListener) Close() error {
	l.closer.Do(func() { close(l.done) })
	return nil
}

func (l *portListener) Addr() net.Addr { return portAddr{} }

// portConn is one host session on the port. Close ends the session, never the port.
type portConn struct {
	l      *portListener
	epoch  uint64 // the listener's epoch at the session's first byte
	closed chan struct{}
	once   sync.Once

	rmu     sync.Mutex // held by Read; Accept takes it once the session is closed
	pending []byte
	sawEOF  bool
}

func (c *portConn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	// A closed session, or one past its host's end-of-file, takes no chunk: the next one is the
	// next session's, or the end-of-file Accept drains to.
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	if c.sawEOF {
		return 0, io.EOF
	}
	if len(c.pending) == 0 {
		select {
		case ch := <-c.l.chunks:
			switch {
			case ch.err != nil:
				return 0, ch.err
			case ch.eof:
				c.sawEOF = true
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
