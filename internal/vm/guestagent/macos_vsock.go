// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// A macOS guest has no virtio-serial port. The agent dials the host instead (a guest can open a vsock
// stream to the host, not the reverse) and keeps one idle connection open on each port. The host's
// helper pairs a connection with runnerd's when runnerd dials, and closes both ends together. Each
// paired connection carries one session in the same records as the Linux port (record.go), opened by
// the host's hello, so the client and the HTTP/2 layer above are unchanged.

// DialFunc opens one connection to the host.
type DialFunc func(ctx context.Context) (net.Conn, error)

// redialWait is how long a failed or ended connection waits before the next dial.
const redialWait = time.Second

// ServeVsock serves h over connections dial makes, one session at a time, until ctx ends. When a
// session ends, from either side, it dials again.
func ServeVsock(ctx context.Context, dial DialFunc, h http.Handler) error {
	return serveVsock(ctx, dial, h, redialWait)
}

func serveVsock(ctx context.Context, dial DialFunc, h http.Handler, wait time.Duration) error {
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	for ctx.Err() == nil {
		conn, err := dial(ctx)
		if err != nil {
			slog.Debug("guest agent: dial the host", "err", err)
			sleep(ctx, wait)
			continue
		}
		serveSession(ctx, conn, h, &protocols, nil)
		sleep(ctx, wait)
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// serveSession waits on conn for the host's hello (the connection is idle until the helper pairs it),
// answers it, and serves h to that session until it ends. onHello, when set, is called once the hello
// is answered.
func serveSession(ctx context.Context, conn net.Conn, h http.Handler, protocols *http.Protocols, onHello func()) {
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	rc := newRecordConn(conn)
	hello, err := readHostHello(rc)
	if err != nil {
		_ = conn.Close()
		return
	}
	hello[0] = helloVersion // the echo: the host's nonce, this agent's version
	if _, err := conn.Write(appendRecord([]byte{0}, recHello, hello)); err != nil {
		_ = conn.Close()
		return
	}
	if onHello != nil {
		onHello()
	}
	l := newOneConn(rc)
	srv := &http.Server{Handler: h, Protocols: protocols, ReadHeaderTimeout: time.Minute}
	go func() {
		<-l.closed
		_ = srv.Close()
	}()
	_ = srv.Serve(l)
}

// readHostHello reads records until the first hello and returns its body. The records before it are
// dropped: nothing but a host's hello starts a session.
func readHostHello(rc *recordConn) ([]byte, error) {
	for {
		rec, err := rc.r.ReadSlice(0)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // longer than any record
		}
		if err != nil {
			return nil, err
		}
		if typ, body, ok := decodeRecord(rec[:len(rec)-1]); ok && typ == recHello && len(body) == helloLen {
			return body, nil
		}
	}
}

// oneConn is a listener that yields one connection and then waits for it to close.
type oneConn struct {
	conn   *trackedConn
	once   sync.Once
	closed chan struct{}
	taken  bool
	mu     sync.Mutex
}

func newOneConn(c net.Conn) *oneConn {
	l := &oneConn{closed: make(chan struct{})}
	l.conn = &trackedConn{Conn: c, l: l}
	return l
}

func (l *oneConn) Accept() (net.Conn, error) {
	l.mu.Lock()
	first := !l.taken
	l.taken = true
	l.mu.Unlock()
	if first {
		return l.conn, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *oneConn) Close() error   { l.end(); return nil }
func (l *oneConn) Addr() net.Addr { return vsockAddr{} }
func (l *oneConn) end()           { l.once.Do(func() { close(l.closed) }) }

// trackedConn tells its listener when the session's connection closes.
type trackedConn struct {
	net.Conn
	l *oneConn
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.l.end()
	return err
}

// SpliceDesktop carries the desktop tunnel: it keeps one idle connection to the host open, and when the
// host writes DesktopOpenByte (a viewer has connected) it dials local and copies both ways until either
// side ends, then starts over. track registers the open tunnel (Desktop.Track): it reports false for a
// run whose desktop is off, which closes the connection, and a Disable closes a tunnel already open.
// local is the guest's own Screen Sharing and nothing the host sends
// changes where it dials; a first byte that is not the open byte closes the connection.
func SpliceDesktop(ctx context.Context, host, local DialFunc, track func(io.Closer) (func(), bool)) error {
	return spliceDesktop(ctx, host, local, track, redialWait)
}

func spliceDesktop(ctx context.Context, host, local DialFunc, track func(io.Closer) (func(), bool), wait time.Duration) error {
	for ctx.Err() == nil {
		hc, err := host(ctx)
		if err != nil {
			slog.Debug("guest agent: dial the host (desktop)", "err", err)
			sleep(ctx, wait)
			continue
		}
		spliceOne(ctx, hc, local, track)
		sleep(ctx, wait)
	}
	return nil
}

func spliceOne(ctx context.Context, hc net.Conn, local DialFunc, track func(io.Closer) (func(), bool)) {
	defer hc.Close()
	stop := context.AfterFunc(ctx, func() { _ = hc.Close() })
	defer stop()
	var b [1]byte
	if _, err := io.ReadFull(hc, b[:]); err != nil || b[0] != DesktopOpenByte {
		return
	}
	release, ok := track(hc)
	if !ok {
		return
	}
	release() // only to check: the tunnel is tracked below, once both ends are open
	lc, err := local(ctx)
	if err != nil {
		slog.Warn("guest agent: reach Screen Sharing", "err", err)
		return
	}
	defer lc.Close()
	release, ok = track(pair{hc, lc})
	if !ok {
		return
	}
	defer release()
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(lc, hc)
	go cp(hc, lc)
	<-done // either side ending ends the pairing; the deferred closes release the other copy
}

type vsockAddr struct{}

func (vsockAddr) Network() string { return "vsock" }
func (vsockAddr) String() string  { return "vsock" }

// pair closes both ends of a tunnel.
type pair struct{ a, b net.Conn }

func (p pair) Close() error {
	_ = p.a.Close()
	return p.b.Close()
}
