// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// renewCP is a stand-in control plane: it serves /token/renew with an
// incrementing token and records the bearer presented on every request.
type renewCP struct {
	mu       sync.Mutex
	seen     []string // bearer tokens presented to /internal/decisions
	issued   int
	ttl      time.Duration
	renewErr int // when non-zero, /token/renew replies with this status
}

func (c *renewCP) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/internal/token/renew", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.renewErr != 0 {
			w.WriteHeader(c.renewErr)
			return
		}
		c.issued++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "renewed-" + itoa(c.issued),
			"expires_at": time.Now().Add(c.ttl).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/api/v1/internal/decisions", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.seen = append(c.seen, r.Header.Get("Authorization"))
		c.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	})
	return mux
}

func (c *renewCP) bearers() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen...)
}

func itoa(i int) string { return string(rune('0' + i)) }

// TestRenewU070_RenewerRotatesTokenUsedByControlPlaneCalls is the proxy-side
// counterfactual. It proves the renewed token actually REACHES the callers: the
// decision sink must present the renewed bearer, not the startup one it was
// constructed with. Before the change the sink captured the token string at
// startup, so it presented the stale token forever and 401'd once the TTL lapsed.
func TestRenewU070_RenewerRotatesTokenUsedByControlPlaneCalls(t *testing.T) {
	cp := &renewCP{ttl: 2 * time.Second} // half-life 1s => renews promptly
	srv := httptest.NewServer(cp.handler())
	defer srv.Close()

	ts := newTokenSource("startup-token")
	sink := newDecisionSink(srv.URL, ts, 16, srv.Client(), &bytes.Buffer{})
	defer func() { _ = sink.close(context.Background()) }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runTokenRenewer(ctx, ts, srv.URL, srv.Client())

	// Wait for the first renew to land.
	deadline := time.Now().Add(3 * time.Second)
	for ts.Get() == "startup-token" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := ts.Get(); got == "startup-token" {
		t.Fatal("token was never renewed — the renewer did not rotate the source")
	}
	renewed := ts.Get()

	// The sink must now present the RENEWED token, proving the rotation reaches
	// the caller rather than only the tokenSource.
	sink.emit(egress.DecisionLog{Request: egress.Request{Host: "example.com"}, Decision: egress.Allow})
	deadline = time.Now().Add(3 * time.Second)
	for len(cp.bearers()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := cp.bearers()
	if len(got) == 0 {
		t.Fatal("decision sink never reached the control plane")
	}
	if want := "Bearer " + renewed; got[0] != want {
		t.Fatalf("decision sink presented %q, want %q (stale startup token still in use)", got[0], want)
	}
}

// TestRenewU070_RenewerKeepsOldTokenWhenControlPlaneRefuses proves the loop is
// dumb and safe: when the control plane REFUSES a renew (a revoked or terminal
// run gets 403), the renewer must not clobber the source with garbage or wedge —
// it keeps the existing token and retries. Authority lives on the control plane,
// never in this loop.
func TestRenewU070_RenewerKeepsOldTokenWhenControlPlaneRefuses(t *testing.T) {
	cp := &renewCP{ttl: time.Hour, renewErr: http.StatusForbidden}
	srv := httptest.NewServer(cp.handler())
	defer srv.Close()

	ts := newTokenSource("startup-token")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runTokenRenewer(ctx, ts, srv.URL, srv.Client())

	time.Sleep(200 * time.Millisecond)
	if got := ts.Get(); got != "startup-token" {
		t.Fatalf("token = %q after a refused renew, want the original kept intact", got)
	}
}

// TestRenewU070_ServerStartsRenewerAndShutdownStopsIt drives the REAL lifecycle
// the sidecar uses: NewServer, then ListenAndServe on one goroutine and Shutdown
// from another (exactly cmd/wardyn-proxy's shape). It proves the renewer actually
// runs for a Server built the production way — not just when a test calls
// runTokenRenewer directly — and that Shutdown stops it rather than leaking it.
// Under -race this also pins the renewer's fields as set-once-in-NewServer:
// starting it from ListenAndServe would race the read in Shutdown.
func TestRenewU070_ServerStartsRenewerAndShutdownStopsIt(t *testing.T) {
	cp := &renewCP{ttl: 2 * time.Second}
	cpSrv := httptest.NewServer(cp.handler())
	defer cpSrv.Close()

	cfg := &Config{
		RunID:           uuid.New(),
		ControlPlaneURL: cpSrv.URL,
		RunToken:        "startup-token",
		Listen:          "127.0.0.1:0",
		Policy:          types.RunPolicySpec{AllowedDomains: []string{"example.com"}},
	}
	if err := cfg.applyDefaultsAndValidate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	srv, err := NewServer(context.Background(), cfg, cpSrv.Client(), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go func() { _ = srv.ListenAndServe() }() // production shape: serve on its own goroutine

	// The renewer must reach the control plane on its own.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cp.mu.Lock()
		n := cp.issued
		cp.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cp.mu.Lock()
	issued := cp.issued
	cp.mu.Unlock()
	if issued == 0 {
		t.Fatal("NewServer did not start the run-token renewer")
	}

	// Shutdown must stop it (and not hang waiting on a renewer that never ran).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if srv.renewStopped != nil {
		select {
		case <-srv.renewStopped:
		default:
			t.Fatal("renewer still running after Shutdown")
		}
	}
}

// TestRenewU070_RenewTokenParsesFreshTokenAndExpiry covers the wire contract in
// isolation: the fields the control plane returns are the fields the loop reads.
func TestRenewU070_RenewTokenParsesFreshTokenAndExpiry(t *testing.T) {
	cp := &renewCP{ttl: 30 * time.Minute}
	srv := httptest.NewServer(cp.handler())
	defer srv.Close()

	tok, exp, err := renewToken(context.Background(), srv.URL, "old", srv.Client())
	if err != nil {
		t.Fatalf("renewToken: %v", err)
	}
	if tok == "" || tok == "old" {
		t.Fatalf("renewToken token = %q, want a fresh non-empty token", tok)
	}
	if d := time.Until(exp); d <= 0 || d > 31*time.Minute {
		t.Fatalf("renewToken expiry in %s, want ~30m in the future", d)
	}

	// A non-200 must be an error, never a silently-empty token.
	cp.mu.Lock()
	cp.renewErr = http.StatusUnauthorized
	cp.mu.Unlock()
	if _, _, err := renewToken(context.Background(), srv.URL, "old", srv.Client()); err == nil {
		t.Fatal("renewToken accepted a 401 response — a refused renew must be an error")
	}
}
