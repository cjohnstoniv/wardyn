// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// wsConn carries one frame per binary message over coder/websocket, as the
// runner link does. That library closes the whole socket when the ctx of an
// in-flight write ends; Loopback does not, so only this Conn shows the harm.
type wsConn struct{ c *websocket.Conn }

func (w wsConn) ReadFrame(ctx context.Context) (Frame, error) {
	_, b, err := w.c.Read(ctx)
	if err != nil {
		return Frame{}, err
	}
	return Decode(b)
}

func (w wsConn) WriteFrame(ctx context.Context, f Frame) error {
	b, err := f.Encode(nil)
	if err != nil {
		return err
	}
	return w.c.Write(ctx, websocket.MessageBinary, b)
}

func (w wsConn) Close() error { return w.c.CloseNow() }

// gatedConn is the runner's TCP socket. While held, a write blocks the way it
// does on a saturated uplink, until released or the socket closes.
type gatedConn struct {
	net.Conn
	mu        sync.Mutex
	gate      chan struct{}
	blocked   chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

func (c *gatedConn) hold() {
	c.mu.Lock()
	c.gate, c.blocked = make(chan struct{}), make(chan struct{})
	c.mu.Unlock()
}

func (c *gatedConn) release() {
	c.mu.Lock()
	close(c.gate)
	c.gate = nil
	c.mu.Unlock()
}

func (c *gatedConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	gate := c.gate
	if gate != nil {
		select {
		case <-c.blocked:
		default:
			close(c.blocked)
		}
	}
	c.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-c.closed:
			return 0, net.ErrClosed
		}
	}
	return c.Conn.Write(b)
}

func (c *gatedConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// newWSPair runs an org peer and a runner peer over a real WebSocket whose
// runner-side socket the test can hold.
func newWSPair(t *testing.T, runCfg PeerConfig) (org, run *Peer, sock *gatedConn) {
	t.Helper()
	orgPeer := make(chan *Peer, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(1 << 20)
		p := NewPeer(wsConn{c}, PeerConfig{Org: true})
		orgPeer <- p
		_ = p.Run(context.Background())
	}))
	t.Cleanup(srv.Close)
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		sock = &gatedConn{Conn: c, closed: make(chan struct{})}
		return sock, nil
	}}
	c, _, err := websocket.Dial(ctxT(t), "ws"+strings.TrimPrefix(srv.URL, "http"), &websocket.DialOptions{HTTPClient: &http.Client{Transport: tr}})
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(1 << 20)
	runCfg.Org = false
	run = NewPeer(wsConn{c}, runCfg)
	go run.Run(context.Background())
	org = <-orgPeer
	t.Cleanup(func() { run.Close(); org.Close() })
	return org, run, sock
}

// R2-0015: resetting one stream while its DATA or CLOSE is blocked in the shared
// connection's write must not close the connection, or every other stream on it.
func TestStreamResetDuringABlockedWriteKeepsTheConnection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(run *Stream) error
		reset func(org, run *Stream)
	}{
		{"remote reset during DATA", func(run *Stream) error {
			_, err := run.Write([]byte("tail -f output"))
			return err
		}, func(org, run *Stream) { _ = org.Close() }},
		{"local reset during CLOSE", func(run *Stream) error { return run.CloseWrite() },
			func(org, run *Stream) { go run.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accepted := make(chan *Stream, 2)
			org, run, sock := newWSPair(t, PeerConfig{OnOpen: func(s *Stream, o Open) error { accepted <- s; return nil }})
			orgA, err := org.Open(ctxT(t), Open{Kind: KindPTY})
			if err != nil {
				t.Fatal(err)
			}
			runA := <-accepted
			orgB, err := org.Open(ctxT(t), Open{Kind: KindPTY})
			if err != nil {
				t.Fatal(err)
			}
			runB := <-accepted

			sock.hold()
			done := make(chan struct{})
			go func() { _ = tc.write(runA); close(done) }()
			select {
			case <-sock.blocked:
			case <-time.After(3 * time.Second):
				t.Fatal("the write never reached the socket")
			}
			tc.reset(orgA, runA)
			select {
			case <-runA.Done(): // abort has cancelled the stream's context
			case <-time.After(3 * time.Second):
				t.Fatal("the reset never reached the stream")
			}
			select { // a write whose ctx ended closes the socket at once
			case <-sock.closed:
			case <-time.After(500 * time.Millisecond):
			}
			sock.release()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("the blocked write never finished")
			}

			go func() { _, _ = runB.Write([]byte("still here")) }()
			got := make(chan string, 1)
			go func() {
				b := make([]byte, len("still here"))
				if _, err := io.ReadFull(orgB, b); err != nil {
					got <- err.Error()
					return
				}
				got <- string(b)
			}()
			select {
			case s := <-got:
				if s != "still here" {
					t.Fatalf("the other stream died with the reset one: %s", s)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the other stream stopped")
			}
			select {
			case <-sock.closed:
				t.Fatal("resetting one stream closed the shared connection")
			default:
			}
			if !org.Online() || !run.Online() {
				t.Fatal("resetting one stream took the session offline")
			}
		})
	}
}
