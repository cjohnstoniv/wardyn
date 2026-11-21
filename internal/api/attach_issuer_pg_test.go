// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An issuer portal's terminal socket on Postgres: its cut is made before the upgrade and its watch runs from
// the ticket's redemption, so an event during the setup or under a client that has stopped reading ends the
// socket within one re-check (scaled to relayEvery + relayBound).

// issuerAttachStore is the attach rig's run and ticket store with the issuer portal's reads on Postgres.
type issuerAttachStore struct {
	*authorityUnits
	store.PortalGrantStore
	pg    store.PG
	reads *runReadHook
}

// runReadHook answers the rig's run reads through hook, when set, from the second after n is reset on (the
// first is redemption's own check).
type runReadHook struct {
	mu   sync.Mutex
	n    int
	hook func(ctx context.Context) error
}

func (s issuerAttachStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if s.reads != nil {
		s.reads.mu.Lock()
		s.reads.n++
		n, hook := s.reads.n, s.reads.hook
		s.reads.mu.Unlock()
		if hook != nil && n >= 2 {
			if err := hook(ctx); err != nil {
				return types.AgentRun{}, err
			}
		}
	}
	return s.authorityUnits.GetRun(ctx, id)
}

func (s issuerAttachStore) GetDelegateByID(ctx context.Context, id uuid.UUID) (types.Delegate, error) {
	return s.pg.GetDelegateByID(ctx, id)
}
func (s issuerAttachStore) IdentityBlocked(ctx context.Context, principal string, epoch int64) (bool, error) {
	return s.pg.IdentityBlocked(ctx, principal, epoch)
}
func (s issuerAttachStore) ListGovernanceUnits(ctx context.Context) ([]types.GovernanceUnit, error) {
	return s.pg.ListGovernanceUnits(ctx)
}
func (s issuerAttachStore) GetGovernanceUnitChain(ctx context.Context, id uuid.UUID) ([]types.GovernanceUnit, error) {
	return s.pg.GetGovernanceUnitChain(ctx, id)
}

// issuerAttachAudit records the rig's audit rows; while hold is set, the first session.attach row waits for
// it to close, and each session.detach row's time is sent on detached.
type issuerAttachAudit struct {
	sshTestRecorder
	mu       sync.Mutex
	hold     chan struct{}
	held     chan time.Time
	detached chan time.Time
}

func (a *issuerAttachAudit) Record(ctx context.Context, ev types.AuditEvent) error {
	a.mu.Lock()
	hold := a.hold
	if ev.Action == "session.attach" {
		a.hold = nil
	}
	a.mu.Unlock()
	if hold != nil && ev.Action == "session.attach" {
		a.held <- time.Now()
		<-hold
	}
	if ev.Action == "session.detach" {
		select {
		case a.detached <- time.Now():
		default:
		}
	}
	return a.sshTestRecorder.Record(ctx, ev)
}

type issuerAttachRig struct {
	env    *authorityEnv
	srv    *Server
	ts     *httptest.Server
	runner *holderTestRunner
	audit  *issuerAttachAudit
	run    types.AgentRun
	via    types.DelegationVia
	person string
	reads  *runReadHook
}

func newIssuerAttachRig(t *testing.T) *issuerAttachRig {
	t.Helper()
	e := newAuthorityEnv(t)
	d := e.portal(t, e.bind(t), &e.plat.ID)
	principal, raw := e.person(t, d, "person-1")
	g, err := e.st.GetPortalGrantByRaw(context.Background(), raw, time.Now())
	if err != nil {
		t.Fatalf("read the seeded grant: %v", err)
	}
	ast := newAuthzStore()
	reads := &runReadHook{}
	st := issuerAttachStore{authorityUnits: &authorityUnits{touchCountingStore: &touchCountingStore{authzStore: ast}}, PortalGrantStore: e.st, pg: e.st, reads: reads}
	cfg := baseTestConfig(newHarness(t), st)
	rig := &issuerAttachRig{env: e, runner: &holderTestRunner{}, person: principal,
		audit: &issuerAttachAudit{held: make(chan time.Time, 1), detached: make(chan time.Time, 4)},
		via:   types.DelegationVia{Delegate: d.ID, Grant: g.ID}, reads: reads}
	cfg.Audit, cfg.Runner, cfg.SessionRevocations = rig.audit, rig.runner, newAttachRevocations()
	rig.srv = New(cfg)
	rig.srv.authorityTest = attachAuthorityTimings{every: relayEvery, lookup: relayBound}
	rig.run = types.AgentRun{ID: uuid.New(), CreatedBy: principal, State: types.RunRunning, SandboxRef: "sbx-1", UnitID: &e.team.ID}
	ast.mu.Lock()
	ast.runs[rig.run.ID] = rig.run
	ast.mu.Unlock()
	rig.ts = httptest.NewServer(panicFails(t, rig.srv.Handler()))
	t.Cleanup(rig.ts.Close)
	return rig
}

// suspend commits the person's suspension and returns the time just before the statement went out: the commit,
// and so any cut it causes, lies after it, while a close before it is no cut by the suspend.
func (rig *issuerAttachRig) suspend(t *testing.T) time.Time {
	t.Helper()
	before := time.Now()
	if _, err := rig.env.st.Pool.Exec(context.Background(), `UPDATE principal_identities SET deactivated_at = now() WHERE principal = $1`, rig.person); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	return before
}

// dial opens the terminal with a ticket the portal minted for the person, and returns the socket unread.
func (rig *issuerAttachRig) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	tok, err := mintAttachTicket(audit.WithDelegation(context.Background(), rig.via), rig.srv.cfg.Store, rig.run.ID, types.ActorHuman,
		rig.person, oidc.RoleUser, oidc.Issue{At: time.Now()})
	if err != nil {
		t.Fatalf("mint a ticket: %v", err)
	}
	url := "ws" + strings.TrimPrefix(rig.ts.URL, "http") + "/api/v1/runs/" + rig.run.ID.String() + "/attach?ticket=" + tok
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial attach: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

// TestAttachIssuerPortal_RevokeDuringSetupCutWithin35s: a suspend commits while the socket's session.attach
// audit write is held, before the first frame; the socket closes within one re-check of the commit.
func TestAttachIssuerPortal_RevokeDuringSetupCutWithin35s(t *testing.T) {
	rig := newIssuerAttachRig(t)
	release := make(chan struct{})
	rig.audit.mu.Lock()
	rig.audit.hold = release
	rig.audit.mu.Unlock()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	c := rig.dial(t)
	<-rig.audit.held
	at := rig.suspend(t)
	closed := make(chan time.Time, 1)
	go func() {
		for {
			if _, _, err := c.Read(context.Background()); err != nil {
				closed <- time.Now()
				return
			}
		}
	}()
	select {
	case got := <-closed:
		cutBy(t, "terminal, suspend during setup", at, got, relayEvery+relayBound+relaySlack)
	case <-time.After(10 * time.Second):
		t.Fatal("the socket outlived a suspend during its setup")
	}
	close(release)
}

// TestAttachIssuerPortal_BlockedWriterCutOnSuspend_PG: the terminal floods output to a client that has
// stopped reading, so the server's websocket write blocks under the unscaled 30 s per-write deadline; a
// suspend ends the socket within one re-check. Just before the suspend the socket is still open and the
// output has stopped moving. One of the renewal gate's measures: a websocket with a client
// that has stopped reading.
func TestAttachIssuerPortal_BlockedWriterCutOnSuspend_PG(t *testing.T) {
	rig := newIssuerAttachRig(t)
	c := rig.dial(t)
	_ = readAttachMode(t, c) // then never read again
	var sess *countingShellSession
	for deadline := time.Now().Add(5 * time.Second); sess == nil; time.Sleep(5 * time.Millisecond) {
		rig.runner.mu.Lock()
		if len(rig.runner.sessions) > 0 {
			sess = rig.runner.sessions[0]
		}
		rig.runner.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("the terminal's exec never opened")
		}
	}
	var wrote atomic.Int64
	go func() {
		chunk := []byte(strings.Repeat("o", 32<<10))
		for {
			n, err := sess.w.Write(chunk)
			wrote.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	time.Sleep(2 * relayEvery) // the client's buffers fill and the server's write blocks
	before := wrote.Load()
	time.Sleep(relayEvery)
	if after := wrote.Load(); before == 0 || after != before {
		t.Fatalf("the terminal's output was not blocked just before the event: %d then %d bytes", before, after)
	}
	select {
	case <-rig.audit.detached:
		t.Fatal("the socket closed before the event: not a cut by it")
	default:
	}
	at := rig.suspend(t)
	select {
	case got := <-rig.audit.detached:
		cutBy(t, "websocket, client not reading, suspend", at, got, relayEvery+relayBound+relaySlack)
	case <-time.After(20 * time.Second):
		t.Fatal("the terminal outlived a suspend with its client not reading")
	}
}

// TestAttachIssuerPortal_OpeningRunReadAnswers_PG: after redemption, the terminal's run read stalls, fails or
// finds no run. A stall and an outage answer the retryable 503 within the opening bound; a missing run the 404 a
// missing run gets on this route.
func TestAttachIssuerPortal_OpeningRunReadAnswers_PG(t *testing.T) {
	const bound = 200 * time.Millisecond
	setSupportWatch(t, time.Hour, bound)
	for _, tc := range []struct {
		name string
		hook func(ctx context.Context) error
		code int
	}{
		{"stall", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, http.StatusServiceUnavailable},
		{"outage", func(context.Context) error { return errors.New("store down") }, http.StatusServiceUnavailable},
		{"missing", func(context.Context) error { return store.ErrNotFound }, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIssuerAttachRig(t)
			tok, err := mintAttachTicket(audit.WithDelegation(context.Background(), rig.via), rig.srv.cfg.Store, rig.run.ID, types.ActorHuman,
				rig.person, oidc.RoleUser, oidc.Issue{At: time.Now()})
			if err != nil {
				t.Fatalf("mint a ticket: %v", err)
			}
			rig.reads.mu.Lock()
			rig.reads.n = 0 // count from the dial on
			rig.reads.hook = tc.hook
			rig.reads.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			start := time.Now()
			c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(rig.ts.URL, "http")+"/api/v1/runs/"+rig.run.ID.String()+"/attach?ticket="+tok, nil)
			if err == nil {
				_ = c.CloseNow()
				t.Fatalf("%s: the terminal opened", tc.name)
			}
			if resp == nil || resp.StatusCode != tc.code {
				t.Fatalf("%s: %v %v, want %d", tc.name, resp, err, tc.code)
			}
			if d := time.Since(start); d > bound+400*time.Millisecond {
				t.Errorf("%s: answered %v after the dial, want within the %v bound", tc.name, d, bound)
			}
		})
	}
}

// TestAttachIssuerPortal_OpeningWaitsOnRunLock_PG: another operation holds the run's lock, so the terminal's
// thaw waits for it. The wait runs under the opening deadline and answers the retryable 503 within the bound,
// not when the lock frees.
func TestAttachIssuerPortal_OpeningWaitsOnRunLock_PG(t *testing.T) {
	const bound = 200 * time.Millisecond
	setSupportWatch(t, time.Hour, bound)
	rig := newIssuerAttachRig(t)
	_, unlock, err := rig.srv.lockRunOp(context.Background(), rig.run.ID)
	if err != nil {
		t.Fatalf("hold the run's lock: %v", err)
	}
	defer unlock()
	tok, err := mintAttachTicket(audit.WithDelegation(context.Background(), rig.via), rig.srv.cfg.Store, rig.run.ID, types.ActorHuman,
		rig.person, oidc.RoleUser, oidc.Issue{At: time.Now()})
	if err != nil {
		t.Fatalf("mint a ticket: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(rig.ts.URL, "http")+"/api/v1/runs/"+rig.run.ID.String()+"/attach?ticket="+tok, nil)
	took := time.Since(start)
	if err == nil {
		_ = c.CloseNow()
		t.Fatal("the terminal opened while the run's lock was held")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a held run lock: %v %v, want the retryable 503", resp, err)
	}
	if body, _ := io.ReadAll(resp.Body); !strings.Contains(string(body), uiDeniedReasonRevocationUnavailable) {
		t.Fatalf("a held run lock answered %s, want %s", body, uiDeniedReasonRevocationUnavailable)
	}
	if took > bound+400*time.Millisecond {
		t.Fatalf("the open waited %v on the run's lock, want within %v", took, bound+400*time.Millisecond)
	}
}
