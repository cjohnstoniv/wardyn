// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
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
	pg store.PG
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
	st := issuerAttachStore{authorityUnits: &authorityUnits{touchCountingStore: &touchCountingStore{authzStore: ast}}, PortalGrantStore: e.st, pg: e.st}
	cfg := baseTestConfig(newHarness(t), st)
	rig := &issuerAttachRig{env: e, runner: &holderTestRunner{}, person: principal,
		audit: &issuerAttachAudit{held: make(chan time.Time, 1), detached: make(chan time.Time, 4)},
		via:   types.DelegationVia{Delegate: d.ID, Grant: g.ID}}
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

func (rig *issuerAttachRig) suspend(t *testing.T) time.Time {
	t.Helper()
	if _, err := rig.env.st.Pool.Exec(context.Background(), `UPDATE principal_identities SET deactivated_at = now() WHERE principal = $1`, rig.person); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	return time.Now()
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
// suspend ends the socket within one re-check. One of the renewal gate's measures: a websocket with a client
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
	go func() {
		chunk := []byte(strings.Repeat("o", 32<<10))
		for {
			if _, err := sess.w.Write(chunk); err != nil {
				return
			}
		}
	}()
	time.Sleep(2 * relayEvery) // the client's buffers fill and the server's write blocks
	at := rig.suspend(t)
	select {
	case got := <-rig.audit.detached:
		cutBy(t, "websocket, client not reading, suspend", at, got, relayEvery+relayBound+relaySlack)
	case <-time.After(20 * time.Second):
		t.Fatal("the terminal outlived a suspend with its client not reading")
	}
}
