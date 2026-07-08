// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestSourceIPNotForgeableViaXForwardedFor is the regression for the HIGH
// finding: the router must NOT install chi middleware.RealIP, which would
// overwrite r.RemoteAddr from the client-supplied X-Forwarded-For / X-Real-IP
// headers with no trusted-proxy allowlist. Because r.RemoteAddr is persisted as
// the append-only audit source_ip (handlePostDecision / handleGroundtruthEvents),
// trusting those headers lets any caller reaching the internal endpoints FORGE
// the source_ip in the audit log.
//
// This exercises the real TCP path (httptest.Server, not ServeHTTP on a
// synthetic request) so r.RemoteAddr is the genuine TCP peer. We send a spoofed
// X-Forwarded-For: 1.2.3.4 and assert the recorded SourceIP is the loopback peer
// (127.0.0.1 / ::1), NOT the forged 1.2.3.4.
func TestSourceIPNotForgeableViaXForwardedFor(t *testing.T) {
	h := newHarness(t)

	// A real listening server so RemoteAddr is the actual TCP peer.
	ts := httptest.NewServer(h.srv.Handler())
	defer ts.Close()

	runID := uuid.New()
	tok := h.mintRunToken(t, runID)
	body := `{"request":{"host":"evil.example.com","method":"CONNECT"},"decision":"deny","rule_source":"policy"}`

	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, ts.URL+"/api/v1/internal/decisions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	// The spoof: a client-supplied X-Forwarded-For naming an attacker IP. With
	// middleware.RealIP installed this would become r.RemoteAddr and be persisted
	// as source_ip. Without it, r.RemoteAddr stays the real loopback peer.
	const spoof = "1.2.3.4"
	req.Header.Set("X-Forwarded-For", spoof)
	req.Header.Set("X-Real-IP", spoof)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("decision status = %d, want 202", resp.StatusCode)
	}

	// Find the recorded egress.deny event and inspect its SourceIP.
	var found bool
	for _, ev := range h.audit.events {
		if ev.Action != "egress.deny" {
			continue
		}
		found = true
		host, _, err := net.SplitHostPort(ev.SourceIP)
		if err != nil {
			// RemoteAddr should be "host:port"; a bare value means the header was
			// trusted and copied verbatim (the bug).
			host = ev.SourceIP
		}
		if host == spoof {
			t.Fatalf("source_ip = %q was forged from X-Forwarded-For; "+
				"middleware.RealIP must not be installed (it must be the TCP peer)", ev.SourceIP)
		}
		if !isLoopback(host) {
			t.Fatalf("source_ip host = %q, want the loopback TCP peer (127.0.0.1/::1)", host)
		}
	}
	if !found {
		t.Fatal("no egress.deny audit event recorded")
	}
}

// isLoopback reports whether host is a loopback address (the expected real peer
// for an httptest.Server bound to localhost).
func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
