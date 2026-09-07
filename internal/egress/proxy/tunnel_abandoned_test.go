// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"io"
	"net"
	"testing"
	"time"
)

// TestTunnelReleasesBothSidesWhenOneDirectionEnds pins F079: an opaque CONNECT
// tunnel whose client side goes away while the upstream stays SILENT must
// release both sockets (and therefore both copy goroutines) instead of pinning
// them until the upstream eventually speaks.
//
// tunnel() used to wg.Wait() for BOTH io.Copy directions before closing
// anything, so an upstream that never sends and never closes — an
// attacker-controlled allowed host, a hung TLS endpoint, a dropped FIN — held
// the hijacked client socket, the upstream socket and 2 goroutines forever:
// the listener's IdleTimeout does not apply to a hijacked connection and the
// opaque lane has no deadline of its own. A process inside the sandbox chooses
// how many of these it opens.
//
// The upstream end here NEVER reads, writes or closes, so on the old shape
// nothing below can complete.
func TestTunnelReleasesBothSidesWhenOneDirectionEnds(t *testing.T) {
	client, clientPeer := net.Pipe()     // the hijacked sandbox connection
	upstream, upstreamPeer := net.Pipe() // the destination

	go tunnel(client, upstream)

	// The sandbox side goes away; the upstream says nothing, ever.
	_ = clientPeer.Close()

	// The upstream socket must be released anyway. A closed pipe end makes the
	// peer's Read return immediately; a still-open tunnel leaves it blocked.
	_ = upstreamPeer.SetReadDeadline(time.Now().Add(5 * time.Second))
	var buf [1]byte
	_, err := upstreamPeer.Read(buf[:])
	if err == nil {
		t.Fatal("upstream peer read returned data on an abandoned tunnel")
	}
	if isTimeout(err) {
		t.Fatalf("upstream socket still open 5s after the client side closed (read err=%v): "+
			"a tunnel whose client is gone must not stay pinned until a silent upstream decides "+
			"to speak — that retains both sockets and both copy goroutines without bound", err)
	}
	if err != io.EOF && !isClosedPipe(err) {
		t.Fatalf("upstream peer read err = %v, want EOF/closed (the tunnel released the socket)", err)
	}
}

// TestTunnelStillRelaysBothDirections is the no-regression half: the
// first-finisher close must not cost the tunnel its ordinary duplex relay.
func TestTunnelStillRelaysBothDirections(t *testing.T) {
	client, clientPeer := net.Pipe()
	upstream, upstreamPeer := net.Pipe()
	go tunnel(client, upstream)

	relay := func(from, to net.Conn, msg string) {
		t.Helper()
		go func() { _, _ = from.Write([]byte(msg)) }()
		_ = to.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(to, buf); err != nil {
			t.Errorf("relay %q: %v", msg, err)
			return
		}
		if string(buf) != msg {
			t.Errorf("relay got %q, want %q", buf, msg)
		}
	}
	relay(clientPeer, upstreamPeer, "sandbox->upstream")
	relay(upstreamPeer, clientPeer, "upstream->sandbox")
	_ = clientPeer.Close()
	_ = upstreamPeer.Close()
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

func isClosedPipe(err error) bool {
	return err != nil && err.Error() == io.ErrClosedPipe.Error()
}
