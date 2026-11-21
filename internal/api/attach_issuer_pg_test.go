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
	"github.com/cjohnstoniv/wardyn/internal/db"
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
	pg         store.PG
	reads      *runReadHook
	grantHang  *atomic.Bool // while set, a grant read waits for its context to end
	runnerHang *atomic.Bool // while set, a runner read waits for its context to end
}

func (s issuerAttachStore) GetRunner(ctx context.Context, id uuid.UUID) (types.Runner, error) {
	if s.runnerHang != nil && s.runnerHang.Load() {
		<-ctx.Done()
		return types.Runner{}, ctx.Err()
	}
	return types.Runner{}, store.ErrNotFound
}

func (s issuerAttachStore) GetPortalGrantByID(ctx context.Context, id uuid.UUID, now time.Time) (types.PortalGrant, error) {
	if s.grantHang != nil && s.grantHang.Load() {
		<-ctx.Done()
		return types.PortalGrant{}, ctx.Err()
	}
	return s.PortalGrantStore.GetPortalGrantByID(ctx, id, now)
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
	env        *authorityEnv
	srv        *Server
	ts         *httptest.Server
	runner     *holderTestRunner
	audit      *issuerAttachAudit
	run        types.AgentRun
	via        types.DelegationVia
	person     string
	reads      *runReadHook
	grantHang  *atomic.Bool
	runnerHang *atomic.Bool
	ast        *authzStore // the rig's runs
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
	reads, grantHang, runnerHang := &runReadHook{}, &atomic.Bool{}, &atomic.Bool{}
	st := issuerAttachStore{authorityUnits: &authorityUnits{touchCountingStore: &touchCountingStore{authzStore: ast}}, PortalGrantStore: e.st, pg: e.st,
		reads: reads, grantHang: grantHang, runnerHang: runnerHang}
	cfg := baseTestConfig(newHarness(t), st)
	rig := &issuerAttachRig{env: e, runner: &holderTestRunner{}, person: principal,
		audit: &issuerAttachAudit{held: make(chan time.Time, 1), detached: make(chan time.Time, 4)},
		via:   types.DelegationVia{Delegate: d.ID, Grant: g.ID}, reads: reads, grantHang: grantHang, runnerHang: runnerHang, ast: ast}
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
// missing run gets on this route. The body names the terminal's own 503, apart from redemption's, and the hook
// shows the read after redemption is the one answered.
func TestAttachIssuerPortal_OpeningRunReadAnswers_PG(t *testing.T) {
	const bound = time.Second // the Postgres reads before the stalled one fit inside it under -race
	setSupportWatch(t, time.Hour, bound)
	for _, tc := range []struct {
		name string
		hook func(ctx context.Context) error
		code int
		body string
	}{
		{"stall", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, http.StatusServiceUnavailable, attachUnverifiableMsg},
		{"outage", func(context.Context) error { return errors.New("store down") }, http.StatusServiceUnavailable, attachUnverifiableMsg},
		{"missing", func(context.Context) error { return store.ErrNotFound }, http.StatusNotFound, reasonRunNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIssuerAttachRig(t)
			tok, err := mintAttachTicket(audit.WithDelegation(context.Background(), rig.via), rig.srv.cfg.Store, rig.run.ID, types.ActorHuman,
				rig.person, oidc.RoleUser, oidc.Issue{At: time.Now()})
			if err != nil {
				t.Fatalf("mint a ticket: %v", err)
			}
			var hit atomic.Bool
			rig.reads.mu.Lock()
			rig.reads.n = 0 // count from the dial on
			rig.reads.hook = func(ctx context.Context) error { hit.Store(true); return tc.hook(ctx) }
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
			if body, _ := io.ReadAll(resp.Body); !strings.Contains(string(body), tc.body) {
				t.Fatalf("%s: answered %s, want %s", tc.name, body, tc.body)
			}
			if !hit.Load() {
				t.Fatalf("%s: the read after redemption was never asked: the answer came from another read", tc.name)
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
	const bound = time.Second
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
	if body, _ := io.ReadAll(resp.Body); !strings.Contains(string(body), uiDeniedReasonRevocationUnavailable) || !strings.Contains(string(body), attachUnverifiableMsg) {
		t.Fatalf("a held run lock answered %s, want %s: %s", body, uiDeniedReasonRevocationUnavailable, attachUnverifiableMsg)
	}
	if took > bound+400*time.Millisecond {
		t.Fatalf("the open waited %v on the run's lock, want within %v", took, bound+400*time.Millisecond)
	}
}

// runOpLockSeen is a Locker that reports each wait begun on one key, before it waits.
type runOpLockSeen struct {
	db.Locker
	key     db.LockKey
	waiting chan struct{}
}

func (l runOpLockSeen) Lock(ctx context.Context, k db.LockKey, wait time.Duration) (context.Context, func(), error) {
	if k == l.key {
		select {
		case l.waiting <- struct{}{}:
		default:
		}
	}
	return l.Locker.Lock(ctx, k, wait)
}

// TestAttachIssuerPortal_GrantEndCarriedPastSlowSetup_PG: past redemption, the terminal's thaw is seen waiting on
// the run's lock; only then do grant reads start to hang, and the lock frees after part of the opening bound. The
// socket still opens, at the grant end redemption read: setup that spends the bound never turns a live grant
// into an ended one.
func TestAttachIssuerPortal_GrantEndCarriedPastSlowSetup_PG(t *testing.T) {
	const bound = 2 * time.Second
	setSupportWatch(t, time.Hour, bound)
	rig := newIssuerAttachRig(t)
	rig.srv.authorityTest.every = time.Hour // no re-check while the grant reads hang: only the open decides
	_, unlock, err := rig.srv.lockRunOp(context.Background(), rig.run.ID)
	if err != nil {
		t.Fatalf("hold the run's lock: %v", err)
	}
	unlocked := false
	defer func() {
		if !unlocked {
			unlock()
		}
	}()
	waits := runOpLockSeen{Locker: rig.srv.locker(), key: db.NewLockKey(db.RunOpLockClass, rig.run.ID.String()), waiting: make(chan struct{}, 1)}
	rig.srv.locks.override = waits
	tok, err := mintAttachTicket(audit.WithDelegation(context.Background(), rig.via), rig.srv.cfg.Store, rig.run.ID, types.ActorHuman,
		rig.person, oidc.RoleUser, oidc.Issue{At: time.Now()})
	if err != nil {
		t.Fatalf("mint a ticket: %v", err)
	}
	var redeemed atomic.Bool // the terminal's own run read, after redemption's
	rig.reads.mu.Lock()
	rig.reads.n = 0
	rig.reads.hook = func(context.Context) error { redeemed.Store(true); return nil }
	rig.reads.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type dialed struct {
		c    *websocket.Conn
		resp *http.Response
		err  error
	}
	got := make(chan dialed, 1)
	go func() {
		c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(rig.ts.URL, "http")+"/api/v1/runs/"+rig.run.ID.String()+"/attach?ticket="+tok, nil)
		got <- dialed{c, resp, err}
	}()
	select {
	case <-waits.waiting:
	case d := <-got:
		t.Fatalf("the terminal answered before its thaw waited on the run's lock: %v %v", d.resp, d.err)
	case <-time.After(5 * time.Second):
		t.Fatal("the terminal's thaw never waited on the run's lock")
	}
	if !redeemed.Load() {
		t.Fatal("the lock wait came before the terminal's run read: not the stage after redemption")
	}
	rig.grantHang.Store(true) // every later grant read hangs: none may decide the socket's end
	time.Sleep(bound / 3)     // the thaw spends part of the bound waiting
	select {
	case d := <-got:
		t.Fatalf("the terminal answered while the run's lock was held: %v %v", d.resp, d.err)
	default:
	}
	unlock()
	unlocked = true
	select {
	case d := <-got:
		if d.err != nil {
			t.Fatalf("a live grant after a slow setup: %v %v, want the terminal open", d.resp, d.err)
		}
		_ = d.c.CloseNow()
	case <-time.After(5 * time.Second):
		t.Fatal("the terminal never answered")
	}
}

// TestAttachIssuerPortal_LocalRunnerReadHeld_PG: a portal person's own local-placement run asks for transport=local
// and its runner read hangs. The terminal answers the retryable 503 within the opening bound, not the 409 a
// runner that is not theirs gets, and not when the store answers.
func TestAttachIssuerPortal_LocalRunnerReadHeld_PG(t *testing.T) {
	const bound = time.Second
	setSupportWatch(t, time.Hour, bound)
	rig := newIssuerAttachRig(t)
	rig.srv.cfg.AttachLocal = true
	runner := uuid.New()
	rig.run.Placement, rig.run.RunnerID = types.PlacementLocal, &runner
	rig.ast.mu.Lock()
	rig.ast.runs[rig.run.ID] = rig.run
	rig.ast.mu.Unlock()
	rig.runnerHang.Store(true)
	tok, err := mintAttachTicket(audit.WithDelegation(context.Background(), rig.via), rig.srv.cfg.Store, rig.run.ID, types.ActorHuman,
		rig.person, oidc.RoleUser, oidc.Issue{At: time.Now()})
	if err != nil {
		t.Fatalf("mint a ticket: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(rig.ts.URL, "http")+"/api/v1/runs/"+rig.run.ID.String()+"/attach?transport=local&ticket="+tok, nil)
	took := time.Since(start)
	if err == nil {
		_ = c.CloseNow()
		t.Fatal("the terminal opened while its runner could not be read")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a held runner read: %v %v, want the retryable 503", resp, err)
	}
	if body, _ := io.ReadAll(resp.Body); !strings.Contains(string(body), attachUnverifiableMsg) {
		t.Fatalf("a held runner read answered %s, want %s", body, attachUnverifiableMsg)
	}
	if took > bound+400*time.Millisecond {
		t.Fatalf("the open waited %v on the runner read, want within %v", took, bound+400*time.Millisecond)
	}
}
