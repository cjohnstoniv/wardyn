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
		probe, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := probe.Addr().String()
		_ = probe.Close()
		cfg, err := LoadConfigBytes(fmt.Appendf(nil, `{"run_id":%q,"control_plane_url":%q,"run_token":"t","listen":%q,"policy":{"allowed_domains":["example.com"]}%s}`,
			uuid.New(), cp.URL, addr, c.peers))
		if err != nil {
			t.Fatalf("peers %q: config: %v", c.peers, err)
		}
		srv, err := NewServer(context.Background(), cfg, cp.Client(), &bytes.Buffer{})
		if err != nil {
			t.Fatalf("peers %q: NewServer: %v", c.peers, err)
		}
		go func() { _ = srv.ListenAndServe() }()
		var conn net.Conn
		for deadline := time.Now().Add(5 * time.Second); conn == nil; {
			if conn, err = net.Dial("tcp", addr); err != nil {
				if time.Now().After(deadline) {
					t.Fatalf("peers %q: the proxy never listened: %v", c.peers, err)
				}
				time.Sleep(10 * time.Millisecond)
			}
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
