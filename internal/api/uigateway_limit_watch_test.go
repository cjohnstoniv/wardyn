// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The limit watch (uigateway_limit_watch.go): a governance limit set after a request began closes it. The watch
// ticks every limitWatchEvery and bounds a check by limitWatchWait; a close is expected within limitWatchBound.
const (
	limitWatchEvery = 50 * time.Millisecond
	limitWatchWait  = time.Second
	limitWatchBound = 3 * time.Second
)

// limitFaults is a store's run read made to fail or hang on demand, and counted.
type limitFaults struct {
	mode  atomic.Int32 // limitFault*
	calls atomic.Int64
}

const (
	limitFaultNone = iota
	limitFaultErr  // GetRun fails with an error that is not ErrNotFound
	limitFaultHang // GetRun waits for its context, as a wedged read does
)

func (f *limitFaults) before(ctx context.Context) error {
	f.calls.Add(1)
	switch f.mode.Load() {
	case limitFaultErr:
		return errors.New("db down")
	case limitFaultHang:
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

// limitFaultOwner and limitFaultInbound put limitFaults in front of the harness's store, keeping every other method.
type limitFaultOwner struct {
	*uiMemStore
	f *limitFaults
}

func (s *limitFaultOwner) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if err := s.f.before(ctx); err != nil {
		return types.AgentRun{}, err
	}
	return s.uiMemStore.GetRun(ctx, id)
}

type limitFaultInbound struct {
	*uiTokenStore
	f *limitFaults
}

func (s *limitFaultInbound) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if err := s.f.before(ctx); err != nil {
		return types.AgentRun{}, err
	}
	return s.uiTokenStore.GetRun(ctx, id)
}

// limitWatchRig is a gateway whose backend echoes after an upgrade, with the watch's seams set for the test.
type limitWatchRig struct {
	*uiHarness
	faults *limitFaults
}

func newLimitWatchRig(t *testing.T) *limitWatchRig {
	t.Helper()
	setSupportWatch(t, limitWatchEvery, limitWatchWait)
	held := make(chan struct{})
	h := newUIHarness(t, echoAfterUpgradeBackend(t, held))
	t.Cleanup(func() { close(held) }) // after the harness's backend, so it runs before the backend's Close waits on its handlers
	f := &limitFaults{}
	h.srv.cfg.Store = &limitFaultOwner{uiMemStore: h.store, f: f}
	return &limitWatchRig{uiHarness: h, faults: f}
}

// newInboundLimitWatchRig is newLimitWatchRig for an inbound app, entered with a browser session.
func newInboundLimitWatchRig(t *testing.T) *limitWatchRig {
	t.Helper()
	setSupportWatch(t, limitWatchEvery, limitWatchWait)
	held := make(chan struct{})
	h := inboundHarnessOn(t, echoAfterUpgradeBackend(t, held))
	t.Cleanup(func() { close(held) })
	f := &limitFaults{}
	h.srv.cfg.Store = &limitFaultInbound{uiTokenStore: h.srv.cfg.Store.(*uiTokenStore), f: f}
	return &limitWatchRig{uiHarness: h, faults: f}
}

// inboundCookie is the relay cookie of a browser session on an inbound app: the run's owner.
func (h *uiHarness) inboundCookie() *http.Cookie {
	h.t.Helper()
	now := h.clock.now()
	return &http.Cookie{Name: uiCookieName, Value: h.srv.encodeUISession(uiSession{Run: h.run.ID, App: "code", Port: uiTestPort,
		Principal: h.owner, Expires: now.Add(time.Hour).Unix(), IssuedAt: now.Unix(), AuthorizedAt: now.Unix(), Inbound: true})}
}

// setLimits puts the run under a profile with limits, as an administrator's later edit does. It clears any
// profile read fault.
func (h *uiHarness) setLimits(limits types.GovernanceLimits) {
	h.t.Helper()
	if h.run.UnitID == nil {
		id := uuid.New()
		h.run.UnitID = &id
	}
	unit := types.GovernanceUnit{ID: *h.run.UnitID, Name: "walled", Limits: limits}
	h.store.sshMemStore.mu.Lock()
	h.store.profiles, h.store.profilesErr = []types.GovernanceUnit{unit}, nil
	h.store.sshMemStore.mu.Unlock()
	h.store.putRun(h.run)
}

// failProfileRead makes the run's profile read fail (the run must already have a profile).
func (h *uiHarness) failProfileRead(err error) {
	h.store.sshMemStore.mu.Lock()
	h.store.profilesErr = err
	h.store.sshMemStore.mu.Unlock()
}

// upgradeSocket opens a WebSocket to app under cookie and requires the gateway's 101.
func (h *uiHarness) upgradeSocket(app string, cookie *http.Cookie) (net.Conn, *bufio.Reader) {
	h.t.Helper()
	ds := upgradeAppWith(h.t, h, app, cookie)
	if ds.resp.StatusCode != http.StatusSwitchingProtocols {
		h.t.Fatalf("upgrade %s = %d, want 101", app, ds.resp.StatusCode)
	}
	return ds.conn, ds.br
}

// closesWithin reports whether the stream ends (EOF or a reset) before d passes, reading whatever it still carries.
func closesWithin(conn net.Conn, br *bufio.Reader, d time.Duration) bool {
	_ = conn.SetReadDeadline(time.Now().Add(d))
	_, err := io.Copy(io.Discard, br)
	var ne net.Error
	return !(errors.As(err, &ne) && ne.Timeout())
}

// denyRowCount counts the denied rows of action whose data carries reason.
func denyRowCount(h *uiHarness, action, reason string) int {
	n := 0
	for _, ev := range h.audit.auditRows(action, "denied") {
		if strings.Contains(string(ev.Data), `"reason":"`+reason+`"`) {
			n++
		}
	}
	return n
}

// anyDeniedRows is every ui.authorize and inbound.deny denied row the watch could have written.
func anyDeniedRows(h *uiHarness) int {
	return len(h.audit.auditRows("ui.authorize", "denied")) + len(h.audit.auditRows("inbound.deny", "denied"))
}

// waitRows polls until n rows of action carry reason, then gives a stray second row a moment to show, and answers the count.
func waitRows(h *uiHarness, action, reason string, n int) int {
	for deadline := time.Now().Add(limitWatchBound); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if denyRowCount(h, action, reason) >= n {
			break
		}
	}
	time.Sleep(4 * limitWatchEvery)
	return denyRowCount(h, action, reason)
}

// Row 1, S6: a deny_ui_apps set after the 101 closes the socket, with one governance_unit row.
func TestUILimitWatch_DenyUIAppsClosesAnOpenSocket(t *testing.T) {
	rig := newLimitWatchRig(t)
	conn, br := rig.upgradeSocket("code", rig.openSession())
	if !echoes(conn, br) {
		t.Fatal("the socket carried nothing before the limit")
	}
	if n := anyDeniedRows(rig.uiHarness); n != 0 {
		t.Fatalf("%d denied rows before the limit", n)
	}
	rig.setLimits(types.GovernanceLimits{DenyUIApps: true})
	if !closesWithin(conn, br, limitWatchBound) {
		t.Fatal("the socket outlived the deny_ui_apps limit")
	}
	if n := waitRows(rig.uiHarness, "ui.authorize", string(authz.ReasonGovernanceUnit), 1); n != 1 {
		t.Fatalf("ui.authorize denied governance_unit rows = %d, want 1", n)
	}
}

// Row 2: the noVNC desktop under deny_interactive, past the RFB handshake.
func TestUILimitWatch_DenyInteractiveClosesNoVNC(t *testing.T) {
	setSupportWatch(t, limitWatchEvery, limitWatchWait)
	offered, pasted := make(chan string, 1), make(chan []byte, 1)
	h := desktopHarness(t, rfbDesktopBackend(t, offered, pasted, false), nil)
	ds := openDesktop(t, h, "novnc")
	if ds.resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade = %d, want 101", ds.resp.StatusCode)
	}
	h.setLimits(types.GovernanceLimits{DenyInteractive: true})
	if !closesWithin(ds.conn, ds.br, limitWatchBound) {
		t.Fatal("the desktop socket outlived the deny_interactive limit")
	}
	if n := waitRows(h, "ui.authorize", string(authz.ReasonGovernanceUnit), 1); n != 1 {
		t.Fatalf("ui.authorize denied governance_unit rows = %d, want 1", n)
	}
}

// Row 3: deny_interactive binds desktop apps only.
func TestUILimitWatch_DenyInteractiveKeepsAWebApp(t *testing.T) {
	rig := newLimitWatchRig(t)
	conn, br := rig.upgradeSocket("code", rig.openSession())
	rig.setLimits(types.GovernanceLimits{DenyInteractive: true})
	time.Sleep(10 * limitWatchEvery)
	if !echoes(conn, br) {
		t.Fatal("the web app's socket closed under deny_interactive")
	}
	if n := anyDeniedRows(rig.uiHarness); n != 0 {
		t.Fatalf("%d denied rows under deny_interactive on a web app, want 0", n)
	}
}

// Row 6, S2: the door's own exemption holds for the watch.
func TestUILimitWatch_ExemptAdminKeepsItsSocket(t *testing.T) {
	rig := newLimitWatchRig(t)
	op := rig.run
	op.CreatedBy, op.OperatorOwned = "admin-token", true
	rig.store.putRun(op)
	rig.run = op
	rec := rig.enterWith(rig.ticket(rig.run.ID, "admin-token", oidc.RoleAdmin))
	cookies := setUICookies(rec)
	if rec.Code != http.StatusFound || len(cookies) != 1 {
		t.Fatalf("admin enter = %d %s, want a session", rec.Code, rec.Body)
	}
	conn, br := rig.upgradeSocket("code", cookies[0])
	rig.setLimits(types.GovernanceLimits{DenyUIApps: true})
	time.Sleep(10 * limitWatchEvery)
	if !echoes(conn, br) {
		t.Fatal("an exempt admin's socket closed under deny_ui_apps")
	}
	if n := anyDeniedRows(rig.uiHarness); n != 0 {
		t.Fatalf("%d denied rows for an exempt admin, want 0", n)
	}
}

// Rows 7 and S1: a read that fails, or runs out of time, closes nothing and writes nothing, on the session arm
// and the inbound arm alike; once it clears, a limit closes the socket.
func TestUILimitWatch_UnreadKeepsTheSocketAndWritesNothing(t *testing.T) {
	faults := []struct {
		name  string
		apply func(rig *limitWatchRig)
		clear func(rig *limitWatchRig)
	}{
		{"the run read fails",
			func(rig *limitWatchRig) { rig.faults.mode.Store(limitFaultErr) },
			func(rig *limitWatchRig) { rig.faults.mode.Store(limitFaultNone) }},
		{"the profile read fails",
			func(rig *limitWatchRig) { rig.failProfileRead(errors.New("db down")) },
			func(rig *limitWatchRig) {}}, // setLimits clears it
		{"the run read blocks past the wait",
			func(rig *limitWatchRig) { rig.faults.mode.Store(limitFaultHang) },
			func(rig *limitWatchRig) { rig.faults.mode.Store(limitFaultNone) }},
	}
	for _, inbound := range []bool{false, true} {
		for _, fault := range faults {
			name := fault.name
			if inbound {
				name += "/inbound browser session"
			}
			t.Run(name, func(t *testing.T) {
				var rig *limitWatchRig
				var cookie *http.Cookie
				if inbound {
					rig = newInboundLimitWatchRig(t)
					cookie = rig.inboundCookie()
				} else {
					rig = newLimitWatchRig(t)
					cookie = rig.openSession()
				}
				rig.setLimits(types.GovernanceLimits{}) // a profile with no limit, so its read has something to fail
				conn, br := rig.upgradeSocket("code", cookie)
				fault.apply(rig)
				time.Sleep(10 * limitWatchEvery)
				if !echoes(conn, br) {
					t.Fatal("the socket closed on a read that did not answer")
				}
				if n := anyDeniedRows(rig.uiHarness); n != 0 {
					t.Fatalf("%d denied rows on a read that did not answer, want 0", n)
				}
				fault.clear(rig)
				rig.setLimits(types.GovernanceLimits{DenyUIApps: true})
				if !closesWithin(conn, br, limitWatchBound) {
					t.Fatal("the socket outlived the limit once the read answered")
				}
			})
		}
	}
}

// Row 8: a run that is gone ends the stream with no row.
func TestUILimitWatch_GoneRunCloses(t *testing.T) {
	rig := newLimitWatchRig(t)
	conn, br := rig.upgradeSocket("code", rig.openSession())
	rig.store.dropRun(rig.run.ID)
	if !closesWithin(conn, br, limitWatchBound) {
		t.Fatal("the socket outlived its run")
	}
	time.Sleep(4 * limitWatchEvery)
	if n := anyDeniedRows(rig.uiHarness); n != 0 {
		t.Fatalf("%d denied rows for a run that is gone, want 0", n)
	}
}

// Row 12: an inbound app entered with a browser session closes with the inbound.deny row.
func TestUILimitWatch_InboundBrowserSessionCloses(t *testing.T) {
	rig := newInboundLimitWatchRig(t)
	conn, br := rig.upgradeSocket("code", rig.inboundCookie())
	if !echoes(conn, br) {
		t.Fatal("the inbound socket carried nothing before the limit")
	}
	rig.setLimits(types.GovernanceLimits{DenyInteractive: true})
	if !closesWithin(conn, br, limitWatchBound) {
		t.Fatal("the inbound socket outlived the deny_interactive limit")
	}
	if n := waitRows(rig.uiHarness, "inbound.deny", inboundDenyUnitDenies, 1); n != 1 {
		t.Fatalf("inbound.deny unit_denies rows = %d, want 1", n)
	}
	if n := len(rig.audit.auditRows("ui.authorize", "denied")); n != 0 {
		t.Fatalf("%d ui.authorize denied rows beside the inbound.deny", n)
	}
}

// Row 11: a request an inbound token admitted has no limit watch (watchInboundStream holds its stream).
func TestUILimitWatch_TokenRequestHasNoLimitWatch(t *testing.T) {
	h := newUIHarness(t, okBackend())
	tokenCtx := context.WithValue(context.Background(), inboundReadmitCtxKey{}, inboundReadmission{})
	got, stop := h.srv.limitWatchCtx(tokenCtx, uiSession{})
	if got != tokenCtx {
		t.Fatal("a token request got a derived context")
	}
	stop()
	if tokenCtx.Err() != nil {
		t.Fatal("stop of a token request's watch ended its context")
	}
	plain, cancel := context.WithCancel(context.Background())
	defer cancel()
	got, stop = h.srv.limitWatchCtx(plain, uiSession{})
	if got == plain {
		t.Fatal("a request with no token got its parent context, not a cancellable one")
	}
	stop()
	if got.Err() == nil {
		t.Fatal("stop did not end the watch's context")
	}
	if plain.Err() != nil {
		t.Fatal("stop of the watch ended its parent")
	}
}

// Row 9: a helper's open socket keeps supportWatch alone: with the seam interval the store sees about one run
// read per tick, not the two a second watch would add. The issuer portal's half is not asserted here: its
// session never reaches limitWatchCtx, and asserting that without a new test seam is not possible.
func TestUILimitWatch_HelperKeepsItsOwnWatch(t *testing.T) {
	setSupportWatch(t, limitWatchEvery, limitWatchWait)
	rig := newSupportUIRig(t, "web_apps")
	granted := rig.srv.cfg.Store.(*supportGrantedStore)
	counted := &helperRunCountStore{supportGrantedStore: granted}
	rig.srv.cfg.Store = counted
	ended := rig.openStream("socket", rig.helperSession("code"))
	requireStreamOpen(t, ended)
	before := counted.n.Load()
	window := 20 * limitWatchEvery
	time.Sleep(window)
	reads := counted.n.Load() - before
	ticks := int64(window / limitWatchEvery)
	if reads > ticks+ticks/2 {
		t.Fatalf("a helper's open socket made %d run reads in %d ticks: a second watch is reading the run", reads, ticks)
	}
	if reads == 0 {
		t.Fatal("a helper's open socket made no run reads: its own watch did not tick")
	}
}

// helperRunCountStore counts a helper rig's run reads.
type helperRunCountStore struct {
	*supportGrantedStore
	n atomic.Int64
}

func (s *helperRunCountStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.n.Add(1)
	return s.supportGrantedStore.GetRun(ctx, id)
}

// Row 20: a console portal's person is not the issuer, so its session gets the owner's limit watch.
func TestUILimitWatch_NonIssuerPortalSessionCloses(t *testing.T) {
	setSupportWatch(t, limitWatchEvery, limitWatchWait)
	held := make(chan struct{})
	h := newUIHarness(t, echoAfterUpgradeBackend(t, held))
	t.Cleanup(func() { close(held) })
	ds := newFakeDelegateStore()
	h.srv.cfg.Store = &uiDelegateStore{h.store, ds}
	_, via := seedDelegation(t, ds, h.owner)
	rec := h.enterWith(h.portalTicket(via))
	cookies := setUICookies(rec)
	if rec.Code != http.StatusFound || len(cookies) != 1 {
		t.Fatalf("portal enter = %d %s, want a session", rec.Code, rec.Body)
	}
	conn, br := h.upgradeSocket("code", cookies[0])
	if !echoes(conn, br) {
		t.Fatal("the portal's socket carried nothing before the limit")
	}
	h.setLimits(types.GovernanceLimits{DenyUIApps: true})
	if !closesWithin(conn, br, limitWatchBound) {
		t.Fatal("a console portal's socket outlived the deny_ui_apps limit")
	}
	if n := waitRows(h, "ui.authorize", string(authz.ReasonGovernanceUnit), 1); n != 1 {
		t.Fatalf("ui.authorize denied governance_unit rows = %d, want 1", n)
	}
}

// Row 16, S7: a request that finishes at once leaves no watch behind: no run read follows it.
func TestUILimitWatch_EndsWithTheRequest(t *testing.T) {
	const every = 200 * time.Millisecond
	setSupportWatch(t, every, limitWatchWait)
	h := newUIHarness(t, okBackend())
	f := &limitFaults{}
	h.srv.cfg.Store = &limitFaultOwner{uiMemStore: h.store, f: f}
	cookie := h.openSession()
	for range 5 {
		if rec := h.relay("/ide", cookie, nil); rec.Code != http.StatusOK {
			t.Fatalf("plain GET = %d %s, want 200", rec.Code, rec.Body)
		}
	}
	after := f.calls.Load()
	time.Sleep(5 * every)
	if got := f.calls.Load(); got != after {
		t.Fatalf("run reads went from %d to %d after the requests ended: a watch outlived its request", after, got)
	}
}
