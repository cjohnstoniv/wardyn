// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestSelfLapse_ListensOnlyOnceOpen: a self-lapsing proxy binds its listener only on its first
// renew, so a k8s runner's TCP readiness probe reads its pod Ready only once it serves. Closed, the
// proxy accepts no connection, so no agent can start and a short run's recording upload is never
// refused there; once open, the upload reaches the control plane; lapsed, the listener stays bound
// and refuses. Lapsed before its first renew, it never listens, and Shutdown still ends it. A proxy
// with no gate (Docker, server) listens at once.
func TestSelfLapse_ListensOnlyOnceOpen(t *testing.T) {
	var landed atomic.Int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		landed.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer cp.Close()
	serve := func(gate *lapseGate) (*Server, string, chan error) {
		p, _ := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cp), nil, nil)
		p.lapse = gate
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		s := &Server{proxy: p, http: &http.Server{Addr: addr, Handler: p}}
		served := make(chan error, 1)
		go func() { served <- s.ListenAndServe() }()
		t.Cleanup(func() { _ = s.http.Shutdown(context.Background()) })
		return s, addr, served
	}
	listening := func(addr string, within time.Duration) bool {
		for end := time.Now().Add(within); ; time.Sleep(10 * time.Millisecond) {
			if c, err := net.Dial("tcp", addr); err == nil {
				_ = c.Close()
				return true
			}
			if time.Now().After(end) {
				return false
			}
		}
	}
	upload := func(p *Proxy, addr string) int {
		req, err := http.NewRequest(http.MethodPut, "http://"+addr+routeRecordings+p.runID.String(), strings.NewReader("{\"version\": 2}\n"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("upload: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if _, addr, _ := serve(nil); !listening(addr, 5*time.Second) {
		t.Fatal("ungated proxy never listened")
	}

	clock := &fakeBoot{}
	s, addr, _ := serve(newLapseGate(clock, testLapseTTL))
	p := s.proxy
	if listening(addr, 300*time.Millisecond) || landed.Load() != 0 {
		t.Fatalf("closed proxy accepted a connection (%d forwarded); a probe would read it Ready before it serves", landed.Load())
	}
	if !p.lapse.renewed(clock.Now(), func() {}) {
		t.Fatal("first renew did not apply")
	}
	if !listening(addr, 5*time.Second) {
		t.Fatal("open proxy never listened")
	}
	if got := upload(p, addr); got != http.StatusNoContent || landed.Load() != 1 {
		t.Fatalf("open proxy: upload %d, %d forwarded; want 204, one", got, landed.Load())
	}
	clock.advance(testLapseTTL)
	if got := upload(p, addr); got != http.StatusServiceUnavailable || landed.Load() != 1 {
		t.Fatalf("lapsed proxy: upload %d, %d forwarded; want 503, still one", got, landed.Load())
	}

	s, addr, served := serve(newLapseGate(clock, testLapseTTL))
	s.proxy.lapse.lapse(LapseRenewRefused)
	if listening(addr, 300*time.Millisecond) {
		t.Fatal("a proxy lapsed before its first renew listened")
	}
	// ListenAndServe registers its Shutdown hook on its own goroutine, so Shutdown is repeated
	// until it has been seen.
	for end := time.Now().Add(5 * time.Second); ; {
		_ = s.http.Shutdown(context.Background())
		select {
		case err := <-served:
			if err != nil || listening(addr, 0) {
				t.Fatalf("a proxy shut down before its first renew: ListenAndServe %v, listening %v", err, listening(addr, 0))
			}
			return
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(end) {
			t.Fatal("a proxy shut down before its first renew never returned from ListenAndServe")
		}
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

type farClock struct{}

func (farClock) Now() time.Duration { return math.MaxInt64 }

// TestSelfLapse_FarFutureClockSaturates (L1): an unreadable clock's far-future
// answer saturates the deadline instead of wrapping, and lapses at once.
func TestSelfLapse_FarFutureClockSaturates(t *testing.T) {
	g := newLapseGate(farClock{}, selfLapseTTL)
	if g.deadline != math.MaxInt64 {
		t.Fatalf("deadline %d, want saturated", int64(g.deadline))
	}
	if g.serving() || g.reason != LapseDeadline {
		t.Fatalf("serving on a far-future clock (reason %q)", g.reason)
	}
}
