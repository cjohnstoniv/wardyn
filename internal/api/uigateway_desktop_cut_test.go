// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// An issuer portal's desktop stream is cut through the production viewer wrapper (viewerHijack): its deadline
// reaches the connection during the handshake's flush, and after the switch the cut closes the transport under
// the frame-checked reader, never through TLS's own close.

// viewerCut is a stream cut over w wrapped as the desktop wraps it, cut by its watch at cutAt.
func viewerCut(w http.ResponseWriter, r *http.Request, cutAt time.Time) (context.Context, *streamCut, http.ResponseWriter) {
	viewer := &viewerHijack{ResponseWriter: w}
	ctx, cancel := context.WithCancel(r.Context())
	c := newStreamCut(viewer, time.Now().Add(time.Hour), cancel)
	c.watch(ctx, cutAt, time.Hour, time.Second, func(context.Context) error { return errTestRevoked })
	return ctx, c, c.Writer(viewer)
}

// TestUIDesktopIssuerPortal_CutInterruptsHandshakeFlush: the cut lands while the desktop's upgrade flushes its
// 101 to a viewer that is not reading. The flush ends at the cut and the switch is refused.
func TestUIDesktopIssuerPortal_CutInterruptsHandshakeFlush(t *testing.T) {
	conns := make(chan *stallConn, 4)
	result := make(chan error, 1)
	held := make(chan *stallConn, 1)
	srv := cutTestServer(t, false, conns, func(w http.ResponseWriter, r *http.Request) {
		sc := <-conns
		held <- sc
		_, c, cw := viewerCut(w, r, time.Now().Add(200*time.Millisecond))
		defer c.End()
		sc.stall(true)
		cw.WriteHeader(http.StatusSwitchingProtocols)
		_, _, err := cw.(http.Hijacker).Hijack()
		result <- err
	})
	// Registered after the server's Close, so it runs first: a flush the cut never reached is released and the
	// failure below is reported, not the package timeout while Close waits on the handler.
	t.Cleanup(func() {
		select {
		case sc := <-held:
			sc.stall(false)
		default:
		}
	})
	raw := dialUpgrade(t, srv)
	defer raw.Close()
	start := time.Now()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("a desktop upgrade whose flush the cut interrupted switched")
		}
		if d := time.Since(start); d > 600*time.Millisecond {
			t.Errorf("the flush ended %v after the request, want at the cut (200ms)", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the desktop's handshake flush outlived the cut: the deadline never reached the connection")
	}
}

// TestUIDesktopIssuerPortal_CutClosesTransportUnderFrameCheck: a switched desktop stream over TLS whose viewer
// has stopped reading is cut; the socket under the frame-checked reader closes at once, without waiting on a
// close_notify the viewer would never read.
func TestUIDesktopIssuerPortal_CutClosesTransportUnderFrameCheck(t *testing.T) {
	conns := make(chan *stallConn, 4)
	cutAt := make(chan time.Time, 1)
	finished := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		sc := <-conns
		at := time.Now().Add(300 * time.Millisecond)
		ctx, c, cw := viewerCut(w, r, at)
		defer c.End()
		cw.WriteHeader(http.StatusSwitchingProtocols)
		if _, _, err := cw.(http.Hijacker).Hijack(); err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		sc.stall(true) // the viewer stopped reading: a TLS close_notify would wait on its write deadline
		cutAt <- at
		<-ctx.Done()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			sc.mu.Lock()
			closed := sc.closed
			sc.mu.Unlock()
			if closed {
				if d := time.Since(at); d > 400*time.Millisecond {
					t.Errorf("the socket closed %v after the cut: it went through the TLS close", d)
				}
				return
			}
		}
		t.Error("the socket under the frame-checked reader never closed")
	})
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = &stallListener{Listener: srv.Listener, conns: conns}
	srv.TLS = &tls.Config{NextProtos: []string{"http/1.1"}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	conn, err := tls.Dial("tcp", srv.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}}) // #nosec G402 -- the test server's own certificate
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(conn).ReadString('\n'); !strings.Contains(line, "101") {
		t.Fatalf("no 101: %q", line)
	}
	select {
	case <-cutAt:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never switched")
	}
	select {
	case <-finished:
	case <-time.After(15 * time.Second):
		t.Fatal("the handler never finished")
	}
}
