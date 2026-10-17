// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fakeBoot is the self-lapse clock seam's test double: CLOCK_BOOTTIME that the
// test moves, including across a simulated suspend (a jump with no wall time).
type fakeBoot struct {
	mu  sync.Mutex
	now time.Duration
}

func (c *fakeBoot) Now() time.Duration      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeBoot) set(d time.Duration)     { c.mu.Lock(); c.now = d; c.mu.Unlock() }
func (c *fakeBoot) advance(d time.Duration) { c.mu.Lock(); c.now += d; c.mu.Unlock() }

const testLapseTTL = time.Hour

func TestSelfLapse_ClosedBeforeFirstRenew(t *testing.T) {
	clock := &fakeBoot{}
	p, _ := newTestProxy(t, types.RunPolicySpec{AllowAllEgress: true}, "", nil, nil)
	p.lapse = newLapseGate(clock, testLapseTTL)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://example.com/"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed proxy served: status %d", rec.Code)
	}
	if !p.lapse.renewed(clock.Now(), func() {}) || !p.lapse.serving() {
		t.Fatal("first successful renew did not open the proxy")
	}
}

// TestSelfLapse_DeadlineOnBootClockAcrossSuspend: the deadline is the last
// successful renew's SEND time + TTL on the boot clock; a suspend that moves
// the boot clock past it lapses at once, with no wall time passing.
func TestSelfLapse_DeadlineOnBootClockAcrossSuspend(t *testing.T) {
	clock := &fakeBoot{}
	g := newLapseGate(clock, testLapseTTL)
	sent := 5 * time.Minute
	clock.set(sent + 30*time.Second) // the response lands later than the send
	if !g.renewed(sent, func() {}) {
		t.Fatal("renew refused")
	}
	clock.set(sent + testLapseTTL - time.Nanosecond)
	if !g.serving() {
		t.Fatal("lapsed before send+TTL")
	}
	clock.advance(2 * time.Hour) // suspend: boot clock jumps
	if g.serving() {
		t.Fatal("still serving after a suspend past the deadline")
	}
	select {
	case <-g.Done():
	default:
		t.Fatal("lapse did not close Done")
	}
	if g.reason != LapseDeadline {
		t.Fatalf("reason %q", g.reason)
	}
}

// TestSelfLapse_WatchdogLapsesIdleProxy: no traffic still lapses within a tick.
func TestSelfLapse_WatchdogLapsesIdleProxy(t *testing.T) {
	clock := &fakeBoot{}
	g := newLapseGate(clock, testLapseTTL)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go g.watch(ctx, time.Millisecond)
	clock.set(testLapseTTL)
	select {
	case <-g.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog did not lapse an idle proxy at its deadline")
	}
}

// TestSelfLapse_ExpiryRacingRenewResultIsDropped: a successful renew whose
// result is applied after the deadline is dropped, and nothing reopens.
func TestSelfLapse_ExpiryRacingRenewResultIsDropped(t *testing.T) {
	clock := &fakeBoot{}
	g := newLapseGate(clock, testLapseTTL)
	g.renewed(0, func() {})
	sent := testLapseTTL - time.Second // sent before the deadline...
	clock.set(testLapseTTL)            // ...answered at it
	applied := false
	if g.renewed(sent, func() { applied = true }) || applied {
		t.Fatal("renew result applied after the deadline")
	}
	if g.renewed(clock.Now(), func() { applied = true }) || applied || g.serving() {
		t.Fatal("a lapsed proxy reopened")
	}
}

// TestSelfLapse_RenewTimelines pins R3-L1 and the N1 example: renewals failing
// from t=10 with no success lapse at t=60 even if runnerd returns at t=61; a
// renew succeeding at t=56 keeps the run; a crash at L+9 with a return at L+55
// survives.
func TestSelfLapse_RenewTimelines(t *testing.T) {
	m := time.Minute
	clock := &fakeBoot{}
	g := newLapseGate(clock, testLapseTTL)
	g.renewed(0, func() {})
	clock.set(60 * m)
	if g.serving() {
		t.Fatal("run with no success since t=0 still serving at t=60")
	}
	clock.set(61 * m)
	if g.renewed(61*m, func() {}) {
		t.Fatal("runnerd READY at t=61 revived a lapsed run")
	}

	clock = &fakeBoot{}
	g = newLapseGate(clock, testLapseTTL)
	g.renewed(0, func() {})
	clock.set(56 * m)
	if !g.renewed(56*m, func() {}) {
		t.Fatal("renew at t=56 refused")
	}
	clock.set(61 * m)
	if !g.serving() {
		t.Fatal("run renewed at t=56 lapsed at t=61")
	}

	clock = &fakeBoot{}
	g = newLapseGate(clock, testLapseTTL)
	l := 3 * m
	g.renewed(l, func() {})
	clock.set(l + 55*m) // crash at L+9, back at L+55
	if !g.renewed(l+55*m, func() {}) {
		t.Fatal("N1: return at L+55 lost the run")
	}
	clock.set(l + 61*m)
	if !g.serving() {
		t.Fatal("N1: run lapsed after a successful renew at L+55")
	}
}

func lapseTestTuning() lapseRenewTuning {
	return lapseRenewTuning{success: 20 * time.Millisecond, failure: 5 * time.Millisecond, watchEvery: time.Millisecond,
		jitter: func(time.Duration) time.Duration { return 0 }}
}

// TestSelfLapse_PermanentRefusalLapsesAtOnce: a 403 lapses; nothing retries.
func TestSelfLapse_PermanentRefusalLapsesAtOnce(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "run terminal", http.StatusForbidden)
	}))
	defer srv.Close()
	g := newLapseGate(&fakeBoot{}, testLapseTTL)
	done := make(chan struct{})
	go func() {
		runSelfLapseRenewer(t.Context(), newTokenSource("seed"), srv.URL, srv.Client(), g, lapseTestTuning())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("renewer kept running after a permanent refusal")
	}
	if g.serving() || g.reason != LapseRenewRefused || calls.Load() != 1 {
		t.Fatalf("serving=%v reason=%q calls=%d", g.serving(), g.reason, calls.Load())
	}
}

// TestSelfLapse_FailuresRetryUntilDeadlineNeverGiveUp: failures keep retrying
// at the failure cadence (no give-up before the deadline), a later success
// opens the proxy and installs the token.
func TestSelfLapse_FailuresRetryUntilDeadlineNeverGiveUp(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 20 {
			http.Error(w, "blip", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "fresh", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
	}))
	defer srv.Close()
	clock := &fakeBoot{}
	g := newLapseGate(clock, testLapseTTL)
	ts := newTokenSource("seed")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go runSelfLapseRenewer(ctx, ts, srv.URL, srv.Client(), g, lapseTestTuning())
	deadline := time.Now().Add(10 * time.Second)
	for !g.serving() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !g.serving() || ts.Get() != "fresh" {
		t.Fatalf("serving=%v token=%q after %d calls", g.serving(), ts.Get(), calls.Load())
	}
}
