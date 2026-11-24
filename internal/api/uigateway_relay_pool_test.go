// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// gatedBackend holds every request until release, so n relayed requests in one round need n connections at once.
type gatedBackend struct {
	mu   sync.Mutex
	held int
	gate chan struct{}
}

func newGatedBackend() *gatedBackend { return &gatedBackend{gate: make(chan struct{})} }

func (b *gatedBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	b.held++
	gate := b.gate
	b.mu.Unlock()
	select {
	case <-gate:
	case <-r.Context().Done():
		return
	}
	_, _ = io.WriteString(w, "sandbox app")
}

func (b *gatedBackend) inFlight() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.held
}

// release answers every held request and starts the next round.
func (b *gatedBackend) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	close(b.gate)
	b.gate = make(chan struct{})
	b.held = 0
}

// relayTarget is one run's "code" app and the relay cookie for it.
type relayTarget struct {
	run    uuid.UUID
	cookie *http.Cookie
}

// relayRound sends perRun requests to each target at once, holds them at the backend until every one has arrived
// (so each holds a connection of its own), answers them all and wants every answer 200.
func relayRound(t *testing.T, h *uiHarness, b *gatedBackend, targets []relayTarget, perRun int) {
	t.Helper()
	total := len(targets) * perRun
	codes := make(chan int, total)
	var wg sync.WaitGroup
	for _, tg := range targets {
		for range perRun {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := httptest.NewRequest(http.MethodGet, uiRelayPrefix(tg.run, "code")+"/ide", nil)
				req.AddCookie(tg.cookie)
				rec := httptest.NewRecorder()
				h.gateway.ServeHTTP(rec, req)
				codes <- rec.Code
			}()
		}
	}
	waitFor(t, "every relayed request to reach the backend", func() bool { return b.inFlight() == total })
	b.release()
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusOK {
			t.Fatalf("a relayed request answered %d, want 200", c)
		}
	}
}

// openRunSession is openAppSession("code") for another run on h.
func openRunSession(h *uiHarness, runID uuid.UUID) *http.Cookie {
	h.t.Helper()
	rec := h.enter(url.Values{"run": {runID.String()}, "app": {"code"}, "ticket": {h.ticket(runID, h.owner, oidc.RoleUser)}})
	if rec.Code != http.StatusFound {
		h.t.Fatalf("enter: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == uiCookieName {
			return c
		}
	}
	h.t.Fatal("no relay cookie set")
	return nil
}

// countExecs counts h's launcher probes ("sh") and relay dials ("socat", or a VM's forward) as they start.
func countExecs(h *uiHarness) (probes, dials *atomic.Int32) {
	probes, dials = new(atomic.Int32), new(atomic.Int32)
	inner := h.runner.execFn
	h.runner.execFn = func(spec runner.ExecSpec) (*runner.ExecSession, error) {
		switch {
		case spec.Argv[0] == "sh":
			probes.Add(1)
		case spec.Argv[0] == "socat", len(spec.Argv) > 1 && spec.Argv[1] == "forward":
			dials.Add(1)
		}
		return inner(spec)
	}
	return probes, dials
}

// holdProbes makes every launcher probe on h wait for release before it answers 0 (already listening).
func holdProbes(h *uiHarness) (release func()) {
	gate := make(chan struct{})
	inner := h.runner.execFn
	h.runner.execFn = func(spec runner.ExecSpec) (*runner.ExecSession, error) {
		if spec.Argv[0] == "sh" {
			return &runner.ExecSession{Wait: func() (int, error) { <-gate; return 0, nil }}, nil
		}
		return inner(spec)
	}
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

// Connections that miss the ready answer while the app is being probed wait for that probe: twenty at once run one
// probe, whether the app was never probed or its ready answer has aged out (#4056).
func TestUIGateway_ConcurrentDialsShareOneLauncherProbe(t *testing.T) {
	for name, aged := range map[string]bool{"never probed": false, "past the ready window": true} {
		t.Run(name, func(t *testing.T) {
			h := newUIHarness(t, okBackend())
			release := holdProbes(h)
			t.Cleanup(release)
			probes, _ := countExecs(h)
			sess := uiSession{Run: h.run.ID, App: "code", Port: uiTestPort, Principal: h.owner}
			if aged {
				h.srv.markUIAppReady(h.run.ID.String() + "/code")
				h.clock.advance(uiReadyTTL + time.Second)
			}
			const dials = 20
			errs := make(chan error, dials)
			var started, done sync.WaitGroup
			for range dials {
				started.Add(1)
				done.Add(1)
				go func() {
					defer done.Done()
					started.Done()
					errs <- h.srv.uiEnsureApp(context.Background(), h.run, sess)
				}()
			}
			started.Wait()
			time.Sleep(100 * time.Millisecond) // every connection reaches its probe or its wait
			release()
			done.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("uiEnsureApp: %v", err)
				}
			}
			if got := probes.Load(); got != 1 {
				t.Fatalf("%d concurrent connections ran %d launcher probes, want 1", dials, got)
			}
		})
	}
}

// A connection waiting on another connection's probe leaves at its own bound, with the probe-failed answer, and
// runs no probe of its own.
func TestUIGateway_DialQueuedBehindAProbeLeavesAtItsOwnBound(t *testing.T) {
	h := newUIHarness(t, okBackend())
	release := holdProbes(h)
	t.Cleanup(release)
	probes, _ := countExecs(h)
	sess := uiSession{Run: h.run.ID, App: "code", Port: uiTestPort, Principal: h.owner}
	first := make(chan error, 1)
	go func() { first <- h.srv.uiEnsureApp(context.Background(), h.run, sess) }()
	waitFor(t, "the first connection's probe", func() bool { return probes.Load() == 1 })
	late := time.AfterFunc(2*time.Second, release) // the base runs a second probe; this ends it
	defer late.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	begin := time.Now()
	err := h.srv.uiEnsureApp(ctx, h.run, sess)
	took := time.Since(begin)
	var de *uiDialError
	if !errors.As(err, &de) || de.reason != reasonUIGatewayLauncherProbeFailed || !strings.Contains(de.msg, "did not finish in time") {
		t.Fatalf("the queued connection = %v, want the probe-failed answer saying the other start did not finish in time", err)
	}
	if took > time.Second {
		t.Fatalf("the queued connection took %s, want it to leave at its own 100ms bound", took)
	}
	release()
	if err := <-first; err != nil {
		t.Fatalf("the probing connection: %v", err)
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("launcher probes = %d, want 1", got)
	}
}

// Six runs each holding eight relay connections keep all of them when they go idle: the next round of requests
// dials no new connection (#4056).
func TestUIGateway_IdleConnectionsKeptPerRun(t *testing.T) {
	backend := newGatedBackend()
	h := newUIHarness(t, backend)
	_, dials := countExecs(h)
	targets := []relayTarget{{run: h.run.ID, cookie: h.openSession()}}
	for range 5 {
		run := types.AgentRun{ID: uuid.New(), CreatedBy: h.owner, State: types.RunRunning, SandboxRef: "sandbox-1"}
		h.store.putRun(run)
		h.store.putEffectivePolicy(run.ID, types.RunPolicySpec{
			MinConfinementClass: types.CC2,
			UIApps:              []types.UIApp{{Name: "code", Port: uiTestPort, Path: "/ide"}},
		})
		targets = append(targets, relayTarget{run: run.ID, cookie: openRunSession(h, run.ID)})
	}
	relayRound(t, h, backend, targets, maxUIConnsPerRun)
	time.Sleep(200 * time.Millisecond) // the transport puts each finished connection in its idle pool
	before := dials.Load()
	relayRound(t, h, backend, targets, maxUIConnsPerRun)
	if got := dials.Load() - before; got != 0 {
		t.Fatalf("the second round over %d runs dialled %d new relay connections, want 0 (each run keeps its %d idle)",
			len(targets), got, maxUIConnsPerRun)
	}
}

// A run's pooled relay connections share one keepalive writer: the row is written once per interval, not once
// per connection (#4056).
func TestUIGateway_PooledConnectionsShareOneKeepalive(t *testing.T) {
	backend := newGatedBackend()
	h := newUIHarness(t, backend)
	h.srv.keepaliveEvery = 25 * time.Millisecond
	relayRound(t, h, backend, []relayTarget{{run: h.run.ID, cookie: h.openSession()}}, maxUIConnsPerRun)
	time.Sleep(200 * time.Millisecond) // the eight connections are idle in the pool, each still open
	before, begin := h.store.touchCount(), time.Now()
	time.Sleep(500 * time.Millisecond)
	touches := h.store.touchCount() - before
	ticks := int(time.Since(begin)/h.srv.keepaliveEvery) + 1
	if touches > ticks+ticks/2 {
		t.Fatalf("a run with %d pooled relay connections wrote its row %d times over about %d keepalive ticks, want one per tick",
			maxUIConnsPerRun, touches, ticks)
	}
}

// Holders of one run share the keepalive writer, which runs while any holder lives and stops with the last.
func TestAttachKeepaliveIsOnePerRunAndStopsWithTheLast(t *testing.T) {
	h := newUIHarness(t, okBackend())
	h.srv.keepaliveEvery = 10 * time.Millisecond
	stops := make([]context.CancelFunc, 8)
	var wg sync.WaitGroup
	for i := range stops {
		ctx, cancel := context.WithCancel(context.Background())
		stops[i] = cancel
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.srv.attachKeepalive(ctx, h.run.ID)
		}()
	}
	t.Cleanup(func() {
		for _, stop := range stops {
			stop()
		}
		wg.Wait()
	})
	waitFor(t, "the first keepalive write", func() bool { return h.store.touchCount() > 0 })
	before, begin := h.store.touchCount(), time.Now()
	time.Sleep(300 * time.Millisecond)
	touches := h.store.touchCount() - before
	if ticks := int(time.Since(begin)/h.srv.keepaliveEvery) + 1; touches > ticks+ticks/2 {
		t.Fatalf("8 holders wrote the row %d times over about %d ticks, want one per tick", touches, ticks)
	}
	for _, stop := range stops[:7] {
		stop()
	}
	time.Sleep(50 * time.Millisecond)
	still := h.store.touchCount()
	time.Sleep(100 * time.Millisecond)
	if h.store.touchCount() == still {
		t.Fatal("the keepalive stopped while one holder was still open")
	}
	stops[7]()
	wg.Wait()
	time.Sleep(30 * time.Millisecond) // a write already in flight lands
	after := h.store.touchCount()
	time.Sleep(100 * time.Millisecond)
	if got := h.store.touchCount() - after; got != 0 {
		t.Fatalf("the run was written %d times after its last holder ended, want 0", got)
	}
}

// A desktop stream opens on a run whose eight slots are held by idle relay connections (#4056).
func TestDesktopOpensPastTheRunsIdlePooledConnections(t *testing.T) {
	backend := newGatedBackend()
	h, _ := desktopHarnessOn(t, backend)
	relayRound(t, h, backend, []relayTarget{{run: h.run.ID, cookie: h.openSession()}}, maxUIConnsPerRun)
	time.Sleep(200 * time.Millisecond) // the run's eight connections are idle in the pool, each holding its slot
	srv := httptest.NewServer(h.gateway)
	t.Cleanup(srv.Close)
	c := desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v := dialDesktop(t, ctx, h, srv.URL, c)
	v.handshake() // at the base the socket closes with the connection-cap reason before the greeting
}
