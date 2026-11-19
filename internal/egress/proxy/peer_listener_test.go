// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

// With allowed_peers set, the proxy serves only callers inside those networks:
// any other connection is closed before a byte is answered. Unset, every
// caller is served as before.
func TestProxyServesOnlyAllowedPeers(t *testing.T) {
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer cp.Close()
	for _, c := range []struct {
		peers  string
		served bool
	}{{`,"allowed_peers":["10.200.0.0/24"]`, false}, {`,"allowed_peers":["127.0.0.0/8"]`, true}, {``, true}} {
		// Port 0: the kernel picks the port at the bind, so no other process can hold it first.
		cfg, err := LoadConfigBytes(fmt.Appendf(nil, `{"run_id":%q,"control_plane_url":%q,"run_token":"t","listen":"127.0.0.1:0","policy":{"allowed_domains":["example.com"]}%s}`,
			uuid.New(), cp.URL, c.peers))
		if err != nil {
			t.Fatalf("peers %q: config: %v", c.peers, err)
		}
		srv, err := NewServer(context.Background(), cfg, cp.Client(), &bytes.Buffer{})
		if err != nil {
			t.Fatalf("peers %q: NewServer: %v", c.peers, err)
		}
		listening := make(chan struct{})
		srv.OnListening(func() { close(listening) })
		served := make(chan error, 1)
		go func() { served <- srv.ListenAndServe() }()
		select {
		case <-listening:
		case err := <-served:
			t.Fatalf("peers %q: the proxy never listened: %v", c.peers, err)
		case <-time.After(5 * time.Second):
			t.Fatalf("peers %q: the proxy never listened", c.peers)
		}
		conn, err := net.Dial("tcp", srv.BoundAddr().String())
		if err != nil {
			t.Fatalf("peers %q: dial the proxy: %v", c.peers, err)
		}
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.WriteString(conn, "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n")
		n, _ := conn.Read(make([]byte, 1))
		_ = conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
		if served := n > 0; served != c.served {
			t.Errorf("peers %q: a loopback caller served = %v, want %v", c.peers, served, c.served)
		}
	}
}
