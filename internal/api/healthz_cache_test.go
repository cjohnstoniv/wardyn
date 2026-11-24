// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// capsCountingRunner counts Capabilities calls and, when block is set, makes each wait for it to close or for its
// own context to end.
type capsCountingRunner struct {
	k8sRunner
	calls atomic.Int32
	mu    sync.Mutex
	block chan struct{}
}

func (r *capsCountingRunner) setBlock(c chan struct{}) {
	r.mu.Lock()
	r.block = c
	r.mu.Unlock()
}

func (r *capsCountingRunner) Capabilities(ctx context.Context) (runner.Capabilities, error) {
	r.calls.Add(1)
	r.mu.Lock()
	block := r.block
	r.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return runner.Capabilities{}, ctx.Err()
		}
	}
	return r.k8sRunner.Capabilities(ctx)
}

// healthzClock is a settable clock.
type healthzClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *healthzClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *healthzClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// healthzCacheServer is a server whose runner counts Capabilities calls, on a settable clock.
func healthzCacheServer(t *testing.T, ttl time.Duration) (*Server, *capsCountingRunner, *healthzClock) {
	t.Helper()
	h := newHarness(t)
	rn := &capsCountingRunner{}
	clock := &healthzClock{t: time.Now()}
	cfg := h.srv.cfg
	cfg.Runner = rn
	cfg.HealthzProbeTTL = ttl
	cfg.Now = clock.Now
	return New(cfg), rn, clock
}

func getHealthz(t testing.TB, ctx context.Context, srv *Server) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	panicFails(t, srv.Handler()).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil).WithContext(ctx))
	return w
}

// Within the TTL a second request reuses the probe, so the runner is asked once; at the TTL it is asked again.
func TestHealthz_ProbeIsReadOncePerTTL(t *testing.T) {
	srv, rn, clock := healthzCacheServer(t, 2*time.Second)
	first := getHealthz(t, context.Background(), srv)
	clock.advance(1900 * time.Millisecond)
	second := getHealthz(t, context.Background(), srv)
	if got := rn.calls.Load(); got != 1 {
		t.Fatalf("Capabilities calls after two requests 1.9 s apart = %d, want 1", got)
	}
	classes := func(w *httptest.ResponseRecorder) string {
		var body map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return string(body["confinement_classes"])
	}
	if a, b := classes(first), classes(second); a == "" || a != b {
		t.Errorf("confinement_classes differ between the two requests: %s then %s", a, b)
	}
	clock.advance(100 * time.Millisecond)
	getHealthz(t, context.Background(), srv)
	if got := rn.calls.Load(); got != 2 {
		t.Errorf("Capabilities calls after a request 2 s after the first = %d, want 2", got)
	}
}

// Eight requests that arrive while the probe is being read share that one read.
func TestHealthz_ProbeReadIsSingleFlight(t *testing.T) {
	srv, rn, _ := healthzCacheServer(t, 2*time.Second)
	block := make(chan struct{})
	rn.setBlock(block)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = getHealthz(t, context.Background(), srv).Code
		}()
	}
	waitFor(t, "the first Capabilities call", func() bool { return rn.calls.Load() >= 1 })
	time.Sleep(200 * time.Millisecond)
	close(block)
	wg.Wait()
	if got := rn.calls.Load(); got != 1 {
		t.Errorf("Capabilities calls for eight concurrent requests = %d, want 1", got)
	}
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d: status %d, want 200", i, c)
		}
	}
}

// A read slower than the TTL still serves the requests that waited for it: the entry's start is older than the TTL
// by the time they see it, and without this every queued request would read in turn.
func TestHealthz_AReadSlowerThanTheTTLServesItsWaiters(t *testing.T) {
	srv, rn, clock := healthzCacheServer(t, 2*time.Second)
	block := make(chan struct{})
	rn.setBlock(block)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	serve := func(i int) {
		defer wg.Done()
		codes[i] = getHealthz(t, context.Background(), srv).Code
	}
	wg.Add(1)
	go serve(0)
	waitFor(t, "the first Capabilities call", func() bool { return rn.calls.Load() == 1 })
	for i := 1; i < len(codes); i++ {
		wg.Add(1)
		go serve(i)
	}
	time.Sleep(300 * time.Millisecond)
	clock.advance(3 * time.Second)
	close(block)
	wg.Wait()
	if got := rn.calls.Load(); got != 1 {
		t.Fatalf("Capabilities calls for eight requests behind one slow read = %d, want 1", got)
	}
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d: status %d, want 200", i, c)
		}
	}
	rn.setBlock(nil)
	getHealthz(t, context.Background(), srv)
	if got := rn.calls.Load(); got != 2 {
		t.Errorf("Capabilities calls after a ninth request = %d, want 2 (the entry's start is 3 s old)", got)
	}
}

// A probe is never served past its TTL: with the store or the runner hung, the next read hangs and the request
// ends with its own context, so the probe fails as it would with no cache. A read cut short is not kept.
func TestHealthz_HungProbeIsNeverServedPastTheTTL(t *testing.T) {
	srv, rn, clock := healthzCacheServer(t, 2*time.Second)
	getHealthz(t, context.Background(), srv)
	clock.advance(3 * time.Second)
	rn.setBlock(make(chan struct{})) // never released
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	getHealthz(t, ctx, srv)
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Errorf("the request returned after %v, before its context ended: the old probe was served", elapsed)
	}
	if got := rn.calls.Load(); got != 2 {
		t.Fatalf("Capabilities calls = %d, want 2 (the expired probe is read again)", got)
	}
	rn.setBlock(nil)
	getHealthz(t, context.Background(), srv)
	if got := rn.calls.Load(); got != 3 {
		t.Errorf("Capabilities calls after the hang cleared = %d, want 3 (the cut-short read was not kept)", got)
	}
}

// Two callers share one probe, and the anonymous one still gets the redacted body.
func TestHealthz_SharedProbeKeepsTheAnonymousRedaction(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	rose := now.Add(-5 * time.Minute)
	st := &nodeSensorStore{sensors: []store.GroundtruthSensor{
		{Node: "node-a", LastBeat: now.Add(-20 * time.Second), ObservedTotal: 6, ObservedByKind: healthyKinds, EvidenceAt: &rose},
		{Node: "node-b", LastBeat: now.Add(-10 * time.Second), ObservedTotal: 3, ObservedByKind: healthyKinds, EvidenceAt: &rose},
	}}
	cfg := baseTestConfig(h, st)
	cfg.Now = func() time.Time { return now }
	cfg.Runner = healthzSubstrateRunner{}
	cfg.Components = map[string]ComponentInfo{"recording": {Selected: "pg"}}
	cfg.HealthzProbeTTL = 2 * time.Second
	srv := New(cfg)

	decode := func(w *httptest.ResponseRecorder) map[string]json.RawMessage {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d %s, want 200", w.Code, w.Body)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	sensors := func(body map[string]json.RawMessage) bool {
		var gt map[string]json.RawMessage
		if err := json.Unmarshal(body["ebpf_groundtruth"], &gt); err != nil {
			t.Fatalf("ebpf_groundtruth: %v", err)
		}
		_, ok := gt["sensors"]
		return ok
	}
	signed := decode(do(t, srv, http.MethodGet, "/healthz", adminToken, ""))
	if _, ok := signed["confinement_substrates"]; !ok || !sensors(signed) {
		t.Fatalf("the signed-in body lacks confinement_substrates or ebpf_groundtruth.sensors: %v", signed)
	}
	anon := decode(do(t, srv, http.MethodGet, "/healthz", "", ""))
	if _, ok := anon["confinement_substrates"]; ok {
		t.Error("the anonymous body names confinement_substrates")
	}
	if _, ok := anon["components"]; ok {
		t.Error("the anonymous body names components")
	}
	if sensors(anon) {
		t.Error("the anonymous body lists the node sensors")
	}
}

// With no TTL set, which is every test's default, each request reads the probe.
func TestHealthz_ZeroTTLReadsEveryRequest(t *testing.T) {
	srv, rn, _ := healthzCacheServer(t, 0)
	getHealthz(t, context.Background(), srv)
	getHealthz(t, context.Background(), srv)
	if got := rn.calls.Load(); got != 2 {
		t.Errorf("Capabilities calls with no TTL = %d, want 2", got)
	}
}
