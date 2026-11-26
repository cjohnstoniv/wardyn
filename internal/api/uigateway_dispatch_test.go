// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// plainCookie is a session cookie with no portal on it for principal on run, as a console session's is.
func plainCookie(h *uiHarness, run uuid.UUID, principal string) *http.Cookie {
	now := time.Now()
	return &http.Cookie{Name: uiCookieName, Value: h.srv.encodeUISession(uiSession{
		Run: run, App: "code", Port: uiTestPort, Principal: principal,
		Expires: now.Add(time.Hour).Unix(), IssuedAt: now.Unix(), AuthorizedAt: now.Unix(),
	})}
}

// traced relays one GET for path with c, counting the outgoing request's GotConn and reused connections.
func traced(h *uiHarness, run uuid.UUID, c *http.Cookie, got, reused *atomic.Int32) *httptest.ResponseRecorder {
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) {
		got.Add(1)
		if i.Reused {
			reused.Add(1)
		}
	}})
	req := httptest.NewRequest(http.MethodGet, uiRelayPrefix(run, "code")+"/", nil).WithContext(ctx)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	h.gateway.ServeHTTP(rec, req)
	return rec
}

// slots is the run's held connection slots.
func slots(s *Server, run uuid.UUID) int {
	s.uiConnsMu.Lock()
	defer s.uiConnsMu.Unlock()
	return s.uiConns[run]
}

// slowRunStore delays every GetRun, as a dial's reads before its slot take time.
type slowRunStore struct {
	*issuerUIStore
	d time.Duration
}

func (s slowRunStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	time.Sleep(s.d)
	return s.issuerUIStore.GetRun(ctx, id)
}

// closeHeldRunner is dispatchRunner whose relay execs into ref close slowly, or wait on hold.
type closeHeldRunner struct {
	*dispatchRunner
	ref     string
	delay   time.Duration
	hold    chan struct{}
	entered atomic.Int32
}

func (r *closeHeldRunner) ExecStream(ctx context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	sess, err := r.dispatchRunner.ExecStream(ctx, ref, spec)
	if err != nil || sess.Close == nil || (r.ref != "" && ref != r.ref) {
		return sess, err
	}
	inner := sess.Close
	sess.Close = func() error {
		if r.hold != nil {
			r.entered.Add(1)
			<-r.hold
		}
		time.Sleep(r.delay)
		return inner()
	}
	return sess, nil
}

func TestUIRelayIssuerPortal_FreshDialPerRequest(t *testing.T) {
	rig := newIssuerUIRig(t, okBackend())
	dr := &dispatchRunner{sshFakeRunner: rig.runner}
	rig.srv.cfg.Runner = dr
	c := rig.session(rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(10*time.Minute)))
	var got, reused atomic.Int32
	for i := 0; i < 3; i++ {
		if r := traced(rig.uiHarness, rig.run.ID, c, &got, &reused); r.Code != http.StatusOK {
			t.Fatalf("sequential request %d: %d %s", i, r.Code, r.Body.String())
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := traced(rig.uiHarness, rig.run.ID, c, &got, &reused); r.Code != http.StatusOK {
				t.Errorf("parallel request: %d %s", r.Code, r.Body.String())
			}
		}()
	}
	wg.Wait()
	if n := dr.count("socat"); n != 6 || got.Load() != 6 || reused.Load() != 0 {
		t.Fatalf("six portal requests: %d relay execs, %d GotConn, %d reused; want 6, 6, 0", n, got.Load(), reused.Load())
	}
	waitWithin(t, 5*time.Second, "no portal connection left idle", func() bool { return slots(rig.srv, rig.run.ID) == 0 })

	// A session with no portal on it, on the same run, still reuses the shared pool.
	plain := plainCookie(rig.uiHarness, rig.run.ID, rig.principal)
	var pgot, preused atomic.Int32
	before := dr.count("socat")
	for i := 0; i < 2; i++ {
		if r := traced(rig.uiHarness, rig.run.ID, plain, &pgot, &preused); r.Code != http.StatusOK {
			t.Fatalf("plain request %d: %d %s", i, r.Code, r.Body.String())
		}
	}
	if dials := dr.count("socat") - before; dials != 1 || preused.Load() != 1 {
		t.Fatalf("two plain requests: %d dials, %d reused; want 1, 1", dials, preused.Load())
	}

	t.Run("at the cap", func(t *testing.T) {
		rig := newIssuerUIRig(t, okBackend())
		cr := &closeHeldRunner{dispatchRunner: &dispatchRunner{sshFakeRunner: rig.runner}, delay: 5 * time.Millisecond}
		rig.srv.cfg.Runner = cr
		rig.srv.cfg.Store = slowRunStore{issuerUIStore: rig.is, d: time.Millisecond}
		c := rig.session(rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(10*time.Minute)))
		var maxSlots atomic.Int32
		stop := make(chan struct{})
		go func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if n := int32(slots(rig.srv, rig.run.ID)); n > maxSlots.Load() {
					maxSlots.Store(n)
				}
				time.Sleep(100 * time.Microsecond)
			}
		}()
		var refused atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 40; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := rig.relay(c)
				if r.Code == http.StatusServiceUnavailable {
					refused.Add(1)
				}
				if r.Code != http.StatusOK {
					t.Errorf("request at the cap: %d %s", r.Code, r.Body.String())
				}
			}()
		}
		wg.Wait()
		close(stop)
		if refused.Load() != 0 || maxSlots.Load() > maxUIConnsPerRun {
			t.Fatalf("40 requests: %d slot refusals, at most %d slots held; want 0, <= %d", refused.Load(), maxSlots.Load(), maxUIConnsPerRun)
		}
	})

	t.Run("another run's held idle close", func(t *testing.T) {
		rig := newIssuerUIRig(t, okBackend())
		hold := make(chan struct{})
		cr := &closeHeldRunner{dispatchRunner: &dispatchRunner{sshFakeRunner: rig.runner}, ref: "sandbox-b", hold: hold}
		rig.srv.cfg.Runner = cr
		setUIConnWait(t, 300*time.Millisecond)
		// Run B: one idle shared connection, whose exec close is held.
		runB := types.AgentRun{ID: uuid.New(), CreatedBy: "alice", State: types.RunRunning, SandboxRef: "sandbox-b"}
		rig.store.putRun(runB)
		rig.store.putEffectivePolicy(runB.ID, types.RunPolicySpec{MinConfinementClass: types.CC2,
			UIApps: []types.UIApp{{Name: "code", Port: uiTestPort, Path: "/ide"}}})
		var g, ru atomic.Int32
		if r := traced(rig.uiHarness, runB.ID, plainCookie(rig.uiHarness, runB.ID, "alice"), &g, &ru); r.Code != http.StatusOK {
			t.Fatalf("run B's request: %d %s", r.Code, r.Body.String())
		}
		// Run A's slots are held by connections that do not use the portal session's port.
		for i := 0; i < maxUIConnsPerRun; i++ {
			release, ok := rig.srv.acquireUIConn(rig.run.ID)
			if !ok {
				t.Fatal("could not hold run A's slots")
			}
			t.Cleanup(release)
		}
		c := rig.session(rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(10*time.Minute)))
		startsBefore := rig.srv.uiDispatch.idleCloseStarts.Load()
		type answer struct {
			rec     *httptest.ResponseRecorder
			started time.Time
			took    time.Duration
		}
		answers := make(chan answer, 2)
		for i := 0; i < 2; i++ {
			go func() {
				start := time.Now()
				r := rig.relay(c)
				answers <- answer{r, start, time.Since(start)}
			}()
		}
		waitWithin(t, 5*time.Second, "run B's idle close to be held", func() bool { return cr.entered.Load() >= 1 })
		for i := 0; i < 2; i++ {
			a := <-answers
			if a.rec.Code != http.StatusServiceUnavailable || dispatchReason(a.rec) != reasonUIGatewayConnCapReached {
				t.Fatalf("a portal request at a full run: %d %s, want the connection-cap 503", a.rec.Code, a.rec.Body.String())
			}
			if a.took > uiConnWait()+100*time.Millisecond {
				t.Fatalf("the connection-cap 503 took %s, want within 100ms of the %s wait's end", a.took, uiConnWait())
			}
		}
		if n := rig.srv.uiDispatch.idleCloseStarts.Load() - startsBefore; n != 1 || cr.entered.Load() != 1 {
			t.Fatalf("idle-pool closes started %d, run B's close entered %d; want 1, 1", n, cr.entered.Load())
		}
		close(hold)
		waitWithin(t, 5*time.Second, "run B's close to complete", func() bool { return slots(rig.srv, runB.ID) == 0 })
	})
}
