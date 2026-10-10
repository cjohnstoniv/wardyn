// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// startRecordingUpstream stands in for the opaque TLS origin a CONNECT tunnel
// carries, and reports the first flight it is handed. It records rather than
// echoes: the assertion is what the UPSTREAM received, which is exactly where a
// dropped client flight shows up — an echo would relay the loss back at the
// client and hide it behind a timeout.
func startRecordingUpstream(t *testing.T, want int) (addr string, got <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ch := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, want)
		n, _ := io.ReadFull(c, buf)
		ch <- string(buf[:n])
		_, _ = io.Copy(io.Discard, c) // hold the tunnel open until the client goes away
	}()
	return ln.Addr().String(), ch
}

// TestConnectTunnelForwardsBytesPipelinedBehindTheHeaders: a client that writes
// its CONNECT and the first bytes of the tunnel in ONE write — the shape a TLS
// client that pipelines its ClientHello behind the CONNECT headers produces —
// has those bytes buffered by net/http before handleConnect runs. Hijack()
// hands that buffered reader back, and the tunnel has to read through it:
// reading the raw connection instead silently drops the flight, and the origin
// never sees the client's opening bytes.
func TestConnectTunnelForwardsBytesPipelinedBehindTheHeaders(t *testing.T) {
	const flight = "the client's first flight inside the tunnel, pipelined behind the CONNECT headers"
	upstream, received := startRecordingUpstream(t, len(flight))

	p, _ := newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"tls.test"}}, upstream, nil, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(proxySrv.URL, "http://"))
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	// ONE write: headers and payload in the same segment, so the server's read
	// takes both and the bytes are in the hijack's buffer before the handler runs.
	if _, err := io.WriteString(conn, "CONNECT tls.test:443 HTTP/1.1\r\nHost: tls.test:443\r\n\r\n"+flight); err != nil {
		t.Fatalf("write CONNECT + flight: %v", err)
	}

	select {
	case got := <-received:
		if got != flight {
			t.Fatalf("upstream received %q, want %q", got, flight)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("upstream never received the pipelined flight (%q); the hijack's buffered reader was dropped", flight)
	}
}
