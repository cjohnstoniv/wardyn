// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net"
	"testing"
	"time"
)

// `wardyn proxy-relay` is an UNAUTHENTICATED TCP relay onto the operator's
// corporate proxy, and neither half of it was covered: relayConn was 0% and the
// listen default was unpinned, so narrowing or widening the exposure was a
// silent edit. Both checks run on loopback with no daemon and no egress.

// The listen default is a SECURITY decision the command's own doc calls out
// ("this exposes the corp proxy to anything that can reach the listen
// address"). 0.0.0.0 is deliberate — a VM-backed Docker host cannot reach the
// host's loopback, which is the whole reason this command exists — so a change
// to it has to be a change somebody makes on purpose.
func TestProxyRelay_ListenDefaults(t *testing.T) {
	cmd := setupProxyRelayCmd()
	for _, tc := range []struct{ flag, want string }{
		{"listen-addr", "0.0.0.0"},
		{"target-host", "127.0.0.1"},
	} {
		f := cmd.Flags().Lookup(tc.flag)
		if f == nil {
			t.Fatalf("proxy-relay has no --%s flag", tc.flag)
		}
		if f.DefValue != tc.want {
			t.Errorf("--%s defaults to %q, want %q — this is the exposure surface of an unauthenticated relay onto the corp proxy; change it deliberately or not at all", tc.flag, f.DefValue, tc.want)
		}
	}
}

// relayConn must pump bytes BOTH ways and close both sides when either
// finishes: a relay that only copies one direction hangs every CONNECT, and one
// that never closes leaks a goroutine and a socket per sandbox connection.
func TestRelayConn_PumpsBothWaysAndClosesOnEitherSide(t *testing.T) {
	// Stand in for the corp proxy: echo a greeting, then uppercase-echo.
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer upstream.Close()
	go func() {
		c, aerr := upstream.Accept()
		if aerr != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		buf := make([]byte, 64)
		n, rerr := c.Read(buf)
		if rerr != nil {
			return
		}
		_, _ = c.Write(buf[:n])
	}()

	// The "client" is one end of a socket pair; relayConn gets the other.
	clientSide, relaySide := net.Pipe()
	defer clientSide.Close()
	done := make(chan struct{})
	go func() { relayConn(relaySide, upstream.Addr().String()); close(done) }()

	_ = clientSide.SetDeadline(time.Now().Add(5 * time.Second))
	const banner = "HTTP/1.1 200 Connection established\r\n\r\n"
	greeting := make([]byte, len(banner))
	if _, err := io.ReadFull(clientSide, greeting); err != nil {
		t.Fatalf("upstream -> client: %v", err)
	}
	if string(greeting) != banner {
		t.Errorf("upstream -> client carried %q, want %q", greeting, banner)
	}
	if _, err := clientSide.Write([]byte("PING")); err != nil {
		t.Fatalf("client -> upstream: %v", err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(clientSide, echo); err != nil {
		t.Fatalf("client -> upstream -> client: %v", err)
	}
	if string(echo) != "PING" {
		t.Errorf("round trip carried %q, want PING", echo)
	}

	// The upstream closing must tear the whole relay down, not leave it pumping.
	clientSide.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("relayConn did not return after the client side closed — every connection leaks a goroutine and two sockets")
	}
}

// A corp proxy that is not listening must not hang the accept loop: relayConn
// closes the client and returns, so the sandbox sees a closed connection and
// wardyn-proxy reports the failure.
func TestRelayConn_UnreachableUpstreamClosesTheClient(t *testing.T) {
	// Bind and immediately release, so the port is almost certainly dead.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	dead := l.Addr().String()
	l.Close()

	clientSide, relaySide := net.Pipe()
	defer clientSide.Close()
	done := make(chan struct{})
	go func() { relayConn(relaySide, dead); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("relayConn hung on an unreachable upstream")
	}
	_ = clientSide.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := clientSide.Read(make([]byte, 1)); err == nil {
		t.Error("the client side stayed open after the upstream dial failed")
	}
}
