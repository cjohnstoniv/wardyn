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
	// attempts counts EVERY /token/renew request, refused ones included — the
	// renewer's give-up and backoff rules are about how often it asks, so the ask
	// is what has to be observable.
	attempts int
	at       []time.Time // when each attempt landed, for the backoff shape
}

func (c *renewCP) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/internal/token/renew", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.attempts++
		c.at = append(c.at, time.Now())
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

// renewAttempts is how many times the loop has asked; gaps returns the interval
// between consecutive asks, which is what "backs off" means.
func (c *renewCP) renewAttempts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts
}

func (c *renewCP) gaps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []time.Duration
	for i := 1; i < len(c.at); i++ {
		out = append(out, c.at[i].Sub(c.at[i-1]))
	}
	return out
}

func (c *renewCP) bearers() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen...)
}

func itoa(i int) string { return string(rune('0' + i)) }

// TestRenewerRotatesTokenUsedByControlPlaneCalls is the proxy-side
// counterfactual. It proves the renewed token actually REACHES the callers: the
// decision sink must present the renewed bearer, not the startup one it was
// constructed with. Before the change the sink captured the token string at
// startup, so it presented the stale token forever and 401'd once the TTL lapsed.
func TestRenewerRotatesTokenUsedByControlPlaneCalls(t *testing.T) {
	// ticket: U070
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

// TestRenewerKeepsOldTokenWhenControlPlaneRefuses proves the loop is
// dumb and safe: when the control plane REFUSES a renew (a revoked or terminal
// run gets 403), the renewer must not clobber the source with garbage or wedge —
// it keeps the existing token. Authority lives on the control plane, never in
// this loop. Since B5 the loop also STOPS on that 403 (the refusal is permanent
// and the control plane has said so post-authentication), which the sibling test
// below owns; what this one still pins is that the token is left intact.
func TestRenewerKeepsOldTokenWhenControlPlaneRefuses(t *testing.T) {
	// ticket: U070
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

// ─── B5: the renew loop is what flooded the audit trail ──────────────────────
//
// The field report: 999 of the last 1000 audit rows were `auth.failed` from this
// loop, once a minute, forever, against a run the control plane would never renew
// again — and every real security event had aged out of the console's 1000-row
// window mid-investigation into who had admin. The loop retried ANY failure every
// 60s with no give-up, and said so out loud in its own comment.

// TestRenewer_PermanentRefusalStopsAtOnce: a post-auth 403 is
// handleInternalTokenRenew's OWN answer — the run is gone or terminal — so the
// loop asks exactly once and exits. One request, not one a minute forever.
func TestRenewer_PermanentRefusalStopsAtOnce(t *testing.T) {
	// ticket: B5
	for _, status := range []int{http.StatusForbidden} {
		cp := &renewCP{ttl: time.Hour, renewErr: status}
		srv := httptest.NewServer(cp.handler())
		defer srv.Close()

		ts := newTokenSource("startup-token")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			runTokenRenewerTuned(ctx, ts, srv.URL, srv.Client(), renewerTuning{
				firstRetry: 10 * time.Millisecond, maxInterval: 20 * time.Millisecond,
				giveUpAfter: time.Hour, now: time.Now,
			})
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("status %d: the loop is still running; a post-auth 403 is permanent and must end it", status)
		}
		if got := cp.renewAttempts(); got != 1 {
			t.Errorf("status %d: renew attempts = %d, want exactly 1", status, got)
		}
		if got := ts.Get(); got != "startup-token" {
			t.Errorf("status %d: token = %q, want the original left intact", status, got)
		}
	}
}

// TestRenewer_UnauthorizedBacksOffThenGivesUp is the case a first draft would
// have got wrong in the dangerous direction. A 401 is NOT proof of revocation:
// the embedded provider treats ANY RevocationStore error as revoked, so a
// Postgres blip answers 401 exactly like a real revocation, and giving up on the
// first one would brick every healthy long run's /internal/* calls the moment a
// read flickered. So the loop keeps trying with GROWING gaps — and stops once the
// last token it actually held would have expired anyway.
func TestRenewer_UnauthorizedBacksOffThenGivesUp(t *testing.T) {
	// ticket: B5
	cp := &renewCP{ttl: time.Hour, renewErr: http.StatusUnauthorized}
	srv := httptest.NewServer(cp.handler())
	defer srv.Close()

	ts := newTokenSource("startup-token")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runTokenRenewerTuned(ctx, ts, srv.URL, srv.Client(), renewerTuning{
			firstRetry: 20 * time.Millisecond, maxInterval: 200 * time.Millisecond,
			giveUpAfter: 400 * time.Millisecond, now: time.Now,
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop never gave up; past the last good token's lifetime there is nothing left to renew")
	}
	attempts := cp.renewAttempts()
	if attempts < 3 {
		t.Fatalf("renew attempts = %d, want several — a 401 must be retried, not treated as proof of revocation", attempts)
	}
	gaps := cp.gaps()
	if len(gaps) < 2 {
		t.Fatalf("only %d gaps observed; cannot see the backoff", len(gaps))
	}
	// The shape, not the exact numbers (a loaded test box adds jitter): the last
	// gap must be meaningfully wider than the first.
	if gaps[len(gaps)-1] <= gaps[0] {
		t.Errorf("gaps = %v; the retry interval must GROW, or a refused renew is still a fixed 1/min drip", gaps)
	}
	if got := ts.Get(); got != "startup-token" {
		t.Errorf("token = %q, want the original left intact", got)
	}
}

// TestRenewer_TransientFailuresAlsoBackOff: a 5xx is the blip case, and it gets
// the same growing gaps — the point of the backoff is the AUDIT VOLUME at the
// other end, which does not care which failure class caused it.
func TestRenewer_TransientFailuresAlsoBackOff(t *testing.T) {
	// ticket: B5
	cp := &renewCP{ttl: time.Hour, renewErr: http.StatusServiceUnavailable}
	srv := httptest.NewServer(cp.handler())
	defer srv.Close()

	ts := newTokenSource("startup-token")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runTokenRenewerTuned(ctx, ts, srv.URL, srv.Client(), renewerTuning{
			firstRetry: 20 * time.Millisecond, maxInterval: 200 * time.Millisecond,
			giveUpAfter: 400 * time.Millisecond, now: time.Now,
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop never gave up on a persistently 503ing control plane")
	}
	gaps := cp.gaps()
	if len(gaps) < 2 || gaps[len(gaps)-1] <= gaps[0] {
		t.Errorf("gaps = %v; a 503 must back off exactly like a 401", gaps)
	}
}

// TestRenewer_SuccessfulRenewResetsTheGiveUpHorizon: the horizon is measured
// from the last token the loop actually HELD, not from startup — otherwise a run
// longer than one token lifetime would give up while perfectly healthy.
func TestRenewer_SuccessfulRenewResetsTheGiveUpHorizon(t *testing.T) {
	// ticket: B5
	cp := &renewCP{ttl: 80 * time.Millisecond} // half-life 40ms => renews promptly
	srv := httptest.NewServer(cp.handler())
	defer srv.Close()

	ts := newTokenSource("startup-token")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runTokenRenewerTuned(ctx, ts, srv.URL, srv.Client(), renewerTuning{
			firstRetry: 10 * time.Millisecond, maxInterval: 40 * time.Millisecond,
			// Shorter than the run: only a horizon that RESETS on success survives.
			giveUpAfter: 60 * time.Millisecond, now: time.Now,
		})
	}()
	time.Sleep(400 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("the loop gave up on a HEALTHY run: the give-up horizon must reset on every successful renew")
	default:
	}
	cancel()
	<-done
	if cp.renewAttempts() < 3 {
		t.Errorf("renew attempts = %d over 400ms of healthy renewing, want several", cp.renewAttempts())
	}
}

// TestServerStartsRenewerAndShutdownStopsIt drives the REAL lifecycle
// the sidecar uses: NewServer, then ListenAndServe on one goroutine and Shutdown
// from another (exactly cmd/wardyn-proxy's shape). It proves the renewer actually
// runs for a Server built the production way — not just when a test calls
// runTokenRenewer directly — and that Shutdown stops it rather than leaking it.
// Under -race this also pins the renewer's fields as set-once-in-NewServer:
// starting it from ListenAndServe would race the read in Shutdown.
func TestServerStartsRenewerAndShutdownStopsIt(t *testing.T) {
	// ticket: U070
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

// TestRenewTokenParsesFreshTokenAndExpiry covers the wire contract in
// isolation: the fields the control plane returns are the fields the loop reads.
func TestRenewTokenParsesFreshTokenAndExpiry(t *testing.T) {
	// ticket: U070
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
