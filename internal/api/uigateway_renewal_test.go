// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// issuerUIStore is the UI harness's store with one issuer portal on top: its grants, the people's identity
// state and the unit tree its pin is read from, all on the harness clock.
type issuerUIStore struct {
	*uiMemStore
	clock   func() time.Time
	mu      sync.Mutex
	portal  types.Delegate
	revoked bool
	renewal bool
	grants  []types.PortalGrant
	blocked map[string]bool
	units   []types.GovernanceUnit
	reads   int // grant and chain reads

	hang           chan struct{}                          // while set, a chain read waits for it to close
	hangIdentity   chan struct{}                          // while set, an identity read waits for it to close
	runHook        func(ctx context.Context, n int) error // when set, answers the n-th run read (1 is the first): an error, or nil to read
	runReads       int
	policyHook     func(ctx context.Context, n int) error // when set, answers the n-th audit (policy) read after the entry's run read
	policyReads    int
	afterChainRead func()                    // runs once, after the next chain read answers
	stallTouch     func(ctx context.Context) // replaces the idle clock's write when set
}

var (
	_ store.PortalGrantStore = (*issuerUIStore)(nil)
	_ store.DelegateReader   = (*issuerUIStore)(nil)
	_ identityBlocker        = (*issuerUIStore)(nil)
)

const issuerUIKey = "abcdefghijklmnopqrstuvwxyz"

func (s *issuerUIStore) GetDelegateByID(_ context.Context, id uuid.UUID) (types.Delegate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.portal.ID {
		return types.Delegate{}, store.ErrNotFound
	}
	return s.portal, nil
}

func (s *issuerUIStore) GetPortalGrantByID(_ context.Context, id uuid.UUID, now time.Time) (types.PortalGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	for _, g := range s.grants {
		if g.ID == id && !s.revoked && g.ExpiresAt.After(now) {
			return g, nil
		}
	}
	return types.PortalGrant{}, store.ErrNotFound
}

func (s *issuerUIStore) PortalChainLive(ctx context.Context, d uuid.UUID, principal string, chain uuid.UUID) (time.Time, bool, error) {
	s.mu.Lock()
	hang := s.hang
	s.mu.Unlock()
	if hang != nil {
		select {
		case <-hang:
		case <-ctx.Done():
			return time.Time{}, false, ctx.Err()
		}
	}
	defer s.afterRead()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if d != s.portal.ID || s.revoked {
		return time.Time{}, false, nil
	}
	var end time.Time
	for _, g := range s.grants {
		if g.DelegateID == d && g.Principal == principal && g.ChainID == chain && g.ExpiresAt.After(s.clock()) && g.ExpiresAt.After(end) {
			end = g.ExpiresAt
		}
	}
	return end, s.renewal, nil
}

func (s *issuerUIStore) GetIssuerDelegateByRaw(context.Context, string) (types.Delegate, error) {
	return types.Delegate{}, store.ErrNotFound
}

func (s *issuerUIStore) MintPortalGrant(context.Context, types.PortalGrant, string, time.Time, time.Duration, int64) (types.PortalGrant, time.Time, error) {
	return types.PortalGrant{}, time.Time{}, store.ErrNotFound
}

func (s *issuerUIStore) GetPortalGrantByRaw(context.Context, string, time.Time) (types.PortalGrant, error) {
	return types.PortalGrant{}, store.ErrNotFound
}

func (s *issuerUIStore) StampPortalUsed(context.Context, uuid.UUID, time.Time) error { return nil }

func (s *issuerUIStore) SweepExpiredPortalGrants(context.Context, time.Time) (int64, error) {
	return 0, nil
}

func (s *issuerUIStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	s.runReads++
	n, hook := s.runReads, s.runHook
	s.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, n); err != nil {
			return types.AgentRun{}, err
		}
	}
	return s.uiMemStore.GetRun(ctx, id)
}

// QueryAuditEvents is the run's policy rows. Reads from the entry's own run read on (the second run read; the
// first is redemption's) are counted and answered through policyHook when it is set.
func (s *issuerUIStore) QueryAuditEvents(ctx context.Context, runID uuid.UUID, limit int) ([]types.AuditEvent, error) {
	s.mu.Lock()
	hook := s.policyHook
	if hook != nil && s.runReads >= 2 {
		s.policyReads++
	} else {
		hook = nil
	}
	n := s.policyReads
	s.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, n); err != nil {
			return nil, err
		}
	}
	return s.uiMemStore.QueryAuditEvents(ctx, runID, limit)
}

func (s *issuerUIStore) IdentityBlocked(ctx context.Context, principal string, _ int64) (bool, error) {
	s.mu.Lock()
	hang := s.hangIdentity
	s.mu.Unlock()
	if hang != nil {
		select {
		case <-hang:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blocked[principal], nil
}

func (s *issuerUIStore) ListGovernanceUnits(context.Context) ([]types.GovernanceUnit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]types.GovernanceUnit(nil), s.units...), nil
}

// afterRead runs the one-shot afterChainRead hook, outside the store's lock.
func (s *issuerUIStore) afterRead() {
	s.mu.Lock()
	f := s.afterChainRead
	s.afterChainRead = nil
	s.mu.Unlock()
	if f != nil {
		f()
	}
}

// hold makes every chain read wait until ch closes or its context ends: a store that has stopped answering.
func (s *issuerUIStore) hold(ch chan struct{}) { s.mu.Lock(); s.hang = ch; s.mu.Unlock() }

func (s *issuerUIStore) TouchRun(ctx context.Context, id uuid.UUID) error {
	s.mu.Lock()
	f := s.stallTouch
	s.mu.Unlock()
	if f != nil {
		f(ctx)
		return nil
	}
	return s.uiMemStore.TouchRun(ctx, id)
}

func (s *issuerUIStore) readCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.reads }

// grant adds one grant of the portal's person, of chain, ending at end, and returns it.
func (s *issuerUIStore) grant(principal string, chain uuid.UUID, end time.Time) types.PortalGrant {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := types.PortalGrant{ID: uuid.New(), DelegateID: s.portal.ID, Principal: principal, UserType: types.UserTypeStandard,
		ChainID: chain, CreatedAt: s.clock(), ExpiresAt: end, Issuer: s.portal.Issuer, IssuerKey: issuerUIKey}
	if g.ChainID == uuid.Nil {
		g.ChainID = g.ID
	}
	s.grants = append(s.grants, g)
	return g
}

// issuerUIRig is a UI harness whose run belongs to an issuer portal's person, pinned to the Default unit.
type issuerUIRig struct {
	*uiHarness
	is        *issuerUIStore
	principal string
}

func newIssuerUIRig(t *testing.T, backend http.Handler) *issuerUIRig {
	t.Helper()
	h := newUIHarness(t, backend)
	pin := store.DefaultUnitID
	is := &issuerUIStore{uiMemStore: h.store, clock: h.clock.now, blocked: map[string]bool{},
		portal: types.Delegate{ID: uuid.New(), Name: "issuer portal", Issuer: "https://idp.example.com", IssuerKey: issuerUIKey, RunInUnder: &pin},
		units:  []types.GovernanceUnit{{ID: store.DefaultUnitID, Name: "Default"}}}
	h.srv.cfg.Store = is
	principal := types.PortalPrincipal(issuerUIKey, "person-1")
	h.owner = principal
	h.run.CreatedBy = principal
	h.store.putRun(h.run)
	return &issuerUIRig{uiHarness: h, is: is, principal: principal}
}

// ticket mints a ticket under grant g, as the portal's delegated mint does.
func (rig *issuerUIRig) ticket(g types.PortalGrant) string {
	rig.t.Helper()
	tok, err := mintAttachTicket(audit.WithDelegation(context.Background(), types.DelegationVia{Delegate: g.DelegateID, Grant: g.ID}),
		rig.store, rig.run.ID, types.ActorHuman, rig.principal, oidc.RoleUser, oidc.Issue{At: rig.clock.now()})
	if err != nil {
		rig.t.Fatalf("mint ticket: %v", err)
	}
	return tok
}

// session enters the app under grant g and returns its cookie, failing unless the enter was admitted.
func (rig *issuerUIRig) session(g types.PortalGrant) *http.Cookie {
	rig.t.Helper()
	rec := rig.enterWith(rig.ticket(g))
	cs := setUICookies(rec)
	if rec.Code != http.StatusFound || len(cs) != 1 {
		rig.t.Fatalf("enter under grant %s: %d %s", g.ID, rec.Code, rec.Body.String())
	}
	return cs[0]
}

func (rig *issuerUIRig) relay(c *http.Cookie) *httptest.ResponseRecorder {
	return uiGet(rig.uiHarness, uiRelayPrefix(rig.run.ID, "code")+"/", c)
}

// TestUIRenewal_NeverDebounced: every request of an issuer portal's session is checked afresh, renewal on or
// off, so a revoke between two requests a second apart refuses the second; a console portal's session keeps
// the pass cache.
func TestUIRenewal_NeverDebounced(t *testing.T) {
	for _, renewal := range []bool{false, true} {
		rig := newIssuerUIRig(t, closingBackend("app"))
		rig.is.renewal = renewal
		c := rig.session(rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(10*time.Minute)))
		for i := 0; i < 3; i++ {
			before := rig.is.readCount()
			if r := rig.relay(c); r.Code != http.StatusOK {
				t.Fatalf("renewal=%v: request %d: %d %s", renewal, i, r.Code, r.Body.String())
			}
			if rig.is.readCount() == before {
				t.Fatalf("renewal=%v: request %d was answered from the pass cache", renewal, i)
			}
			rig.clock.advance(time.Second)
		}
		rig.is.mu.Lock()
		rig.is.revoked = true
		rig.is.mu.Unlock()
		if r := rig.relay(c); r.Code != http.StatusForbidden {
			t.Fatalf("renewal=%v: the request a second after a portal revoke: %d, want 403", renewal, r.Code)
		}
	}
	// A console portal's session keeps the pass cache; an issuer portal's never takes it.
	h := newUIHarness(t, okBackend())
	now := h.clock.now()
	via := &types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()}
	console := uiSession{Run: h.run.ID, App: "code", Port: uiTestPort, Principal: "alice", IssuedAt: now.Unix(), AuthorizedAt: now.Unix(), Via: via}
	issuer := console
	issuer.Principal = types.PortalPrincipal(issuerUIKey, "person-1")
	for _, sess := range []uiSession{console, issuer} {
		h.srv.markUIReasserted(sess, now, h.srv.uiInbound.drops.Load())
	}
	if h.srv.uiReassertDue(console, now.Add(time.Second)) {
		t.Error("a console portal's session lost its pass cache")
	}
	if !h.srv.uiReassertDue(issuer, now.Add(time.Second)) {
		t.Error("an issuer portal's session was answered from the pass cache")
	}
}

// TestUIReassert_KeyIncludesAuthority: two cookies of one person on one app, minted in the same second, that
// differ only in their portal, grant, authority time or revocation sequence never share a pass.
func TestUIReassert_KeyIncludesAuthority(t *testing.T) {
	h := newUIHarness(t, okBackend())
	now := h.clock.now()
	base := uiSession{Run: h.run.ID, App: "code", Port: uiTestPort, Principal: "alice", Role: oidc.RoleUser,
		IssuedAt: now.Unix(), AuthorizedAt: now.Unix(), Via: &types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()}}
	variants := map[string]func(*uiSession){
		"portal":         func(s *uiSession) { s.Via = &types.DelegationVia{Delegate: uuid.New(), Grant: s.Via.Grant} },
		"grant":          func(s *uiSession) { s.Via = &types.DelegationVia{Delegate: s.Via.Delegate, Grant: uuid.New()} },
		"no portal":      func(s *uiSession) { s.Via = nil },
		"authority time": func(s *uiSession) { s.AuthorizedAt-- },
		"authority ns":   func(s *uiSession) { s.AuthorizedNs = 7 },
		"revocation seq": func(s *uiSession) { s.RevSeq = 3 },
	}
	h.srv.markUIReasserted(base, now, h.srv.uiInbound.drops.Load())
	if h.srv.uiReassertDue(base, now) {
		t.Fatal("the marked session itself is due: the pass was not recorded")
	}
	for name, mut := range variants {
		other := base
		mut(&other)
		if !h.srv.uiReassertDue(other, now) {
			t.Errorf("%s: a cookie differing only in its %s rode the other's pass", name, name)
		}
	}
	if uiReassertKey(base) != uiReassertKey(uiSession{Run: base.Run, App: base.App, Principal: base.Principal, IssuedAt: base.IssuedAt}) {
		t.Error("uiReassertKey changed: a Closed desktop is keyed on it across replicas")
	}
}

// renewingRig is an issuer UI rig whose portal renews sessions, with one session open under grant a.
func renewingRig(t *testing.T) (*issuerUIRig, types.PortalGrant, *http.Cookie) {
	t.Helper()
	rig := newIssuerUIRig(t, closingBackend("app"))
	rig.is.renewal = true
	a := rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(5*time.Minute))
	return rig, a, rig.session(a)
}

// TestUIRenewal_ContinuingGrantsKeepServing: while the portal keeps minting the person grants of the same
// chain, each before the last one ends, the session keeps serving past every single grant's end, and its
// cookie expires at redemption plus the session TTL, not with the entry grant.
func TestUIRenewal_ContinuingGrantsKeepServing(t *testing.T) {
	rig, a, c := renewingRig(t)
	if want := rig.clock.now().Add(rig.srv.uiSessionTTL() - time.Minute); c.Expires.Before(want) {
		t.Fatalf("a renewable cookie expires %v, capped by its entry grant (%v)", c.Expires, a.ExpiresAt)
	}
	for i := 0; i < 4; i++ {
		rig.is.grant(rig.principal, a.ChainID, rig.clock.now().Add(5*time.Minute))
		rig.clock.advance(4 * time.Minute)
		if r := rig.relay(c); r.Code != http.StatusOK {
			t.Fatalf("renewal %d, %v past the entry grant's end: %d %s", i, rig.clock.now().Sub(a.ExpiresAt), r.Code, r.Body.String())
		}
	}
}

// TestUIRenewal_GapDoesNotReviveCookie: once no grant of the chain is live, the cookie is refused, and a
// later exchange, which starts a new chain, never revives it.
func TestUIRenewal_GapDoesNotReviveCookie(t *testing.T) {
	rig, _, c := renewingRig(t)
	rig.clock.advance(6 * time.Minute)
	if r := rig.relay(c); r.Code != http.StatusForbidden {
		t.Fatalf("after the chain's last grant ended: %d, want 403", r.Code)
	}
	fresh := rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(5*time.Minute))
	if r := rig.relay(c); r.Code != http.StatusForbidden {
		t.Fatalf("a new chain's grant (%s) revived the old cookie: %d", fresh.ChainID, r.Code)
	}
	if r := rig.relay(rig.session(fresh)); r.Code != http.StatusOK {
		t.Fatalf("the new chain's own session: %d", r.Code)
	}
}

// TestUIRenewal_OtherPortalGrantDoesNotExtendSession: a live grant of the same person and even the same chain
// id through another portal never keeps this portal's session alive.
func TestUIRenewal_OtherPortalGrantDoesNotExtendSession(t *testing.T) {
	rig, a, c := renewingRig(t)
	rig.is.mu.Lock()
	rig.is.grants = append(rig.is.grants, types.PortalGrant{ID: uuid.New(), DelegateID: uuid.New(), Principal: rig.principal,
		ChainID: a.ChainID, ExpiresAt: rig.clock.now().Add(time.Hour), IssuerKey: issuerUIKey})
	rig.is.mu.Unlock()
	rig.clock.advance(6 * time.Minute)
	if r := rig.relay(c); r.Code != http.StatusForbidden {
		t.Fatalf("another portal's grant kept the session: %d, want 403", r.Code)
	}
}

// TestUIRenewal_PortalRevokeRefusesNextRequest: a portal revoke refuses the session's very next request,
// though the portal minted it a successor a moment before.
func TestUIRenewal_PortalRevokeRefusesNextRequest(t *testing.T) {
	rig, a, c := renewingRig(t)
	rig.is.grant(rig.principal, a.ChainID, rig.clock.now().Add(10*time.Minute))
	if r := rig.relay(c); r.Code != http.StatusOK {
		t.Fatalf("before the revoke: %d", r.Code)
	}
	rig.is.mu.Lock()
	rig.is.revoked = true
	rig.is.mu.Unlock()
	if r := rig.relay(c); r.Code != http.StatusForbidden {
		t.Fatalf("the request right after a portal revoke: %d, want 403", r.Code)
	}
}

// TestUIRenewal_PersonRevokeEndsSession: a revoke of the person, or a suspend, ends the renewable session
// at its next request while the portal keeps exchanging.
func TestUIRenewal_PersonRevokeEndsSession(t *testing.T) {
	for _, how := range []string{"revoke", "suspend"} {
		rig, a, c := renewingRig(t)
		rev := &memRevocations{cut: map[string]time.Time{}}
		rig.srv.cfg.SessionRevocations = rev
		rig.is.grant(rig.principal, a.ChainID, rig.clock.now().Add(10*time.Minute))
		if r := rig.relay(c); r.Code != http.StatusOK {
			t.Fatalf("%s: before: %d", how, r.Code)
		}
		if how == "revoke" {
			_ = rev.RevokeSub(context.Background(), rig.principal)
		} else {
			rig.is.mu.Lock()
			rig.is.blocked[rig.principal] = true
			rig.is.mu.Unlock()
		}
		rig.is.grant(rig.principal, a.ChainID, rig.clock.now().Add(15*time.Minute))
		if r := rig.relay(c); r.Code != http.StatusForbidden {
			t.Fatalf("%s: the next request: %d, want 403", how, r.Code)
		}
	}
}

// TestUIRenewal_DesktopAndSupportNeverRenew: a ticket that would renew opens a desktop, or a helper's
// session, with no chain and an expiry capped by the grant.
func TestUIRenewal_DesktopAndSupportNeverRenew(t *testing.T) {
	h := newUIHarness(t, okBackend())
	now := h.clock.now()
	grantEnd := now.Add(5 * time.Minute)
	base := ticketActor{principal: types.PortalPrincipal(issuerUIKey, "person-1"), authorizedAt: now, grantExpires: grantEnd,
		via: &types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()}, renewChain: uuid.New()}
	if sess := h.srv.newUISession(h.run, base, "code", uiTestPort, time.Hour); sess.Chain == uuid.Nil || sess.Expires <= grantEnd.Unix() {
		t.Fatalf("a renewable web app session: chain %s, expires %d; want the chain and the TTL", sess.Chain, sess.Expires)
	}
	for _, app := range []string{uiDesktopAppName, "novnc", desktopSigninApp} {
		if sess := h.srv.newUISession(h.run, base, app, 0, time.Hour); sess.Chain != uuid.Nil || sess.Expires > grantEnd.Unix() {
			t.Errorf("desktop %s: chain %s, expires %d past the grant's %d", app, sess.Chain, sess.Expires, grantEnd.Unix())
		}
	}
	helper := base
	helper.support, helper.supportExpires = uuid.New(), now.Add(time.Hour)
	if sess := h.srv.newUISession(h.run, helper, "code", uiTestPort, time.Hour); sess.Chain != uuid.Nil || sess.Expires > grantEnd.Unix() {
		t.Errorf("a helper's session: chain %s, expires %d past the grant's %d", sess.Chain, sess.Expires, grantEnd.Unix())
	}
}

// TestUIRenewal_OffKeepsGrantCap: with renewal off the cookie is capped by its grant, carries no chain, and
// is refused once that grant ends though a later grant of its chain lives.
func TestUIRenewal_OffKeepsGrantCap(t *testing.T) {
	rig := newIssuerUIRig(t, closingBackend("app"))
	a := rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(5*time.Minute))
	c := rig.session(a)
	if c.Expires.After(a.ExpiresAt) {
		t.Fatalf("renewal off: the cookie expires %v, after its grant's %v", c.Expires, a.ExpiresAt)
	}
	rig.is.grant(rig.principal, a.ChainID, rig.clock.now().Add(time.Hour))
	rig.clock.advance(4 * time.Minute)
	if r := rig.relay(c); r.Code != http.StatusOK {
		t.Fatalf("inside the grant: %d", r.Code)
	}
	rig.clock.advance(2 * time.Minute)
	if r := rig.relay(c); r.Code != http.StatusForbidden {
		t.Fatalf("renewal off, past the entry grant with a successor live: %d, want 403", r.Code)
	}
}

// TestUIRenewal_NeverPastIssuedAtPlusTTL: a chain that keeps renewing never serves the cookie past its
// redemption plus the session TTL.
func TestUIRenewal_NeverPastIssuedAtPlusTTL(t *testing.T) {
	rig := newIssuerUIRig(t, closingBackend("app"))
	rig.is.renewal = true
	rig.srv.cfg.UISessionTTL = 12 * time.Minute
	a := rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(5*time.Minute))
	c := rig.session(a)
	for i := 0; i < 2; i++ {
		rig.is.grant(rig.principal, a.ChainID, rig.clock.now().Add(5*time.Minute))
		rig.clock.advance(4 * time.Minute)
		if r := rig.relay(c); r.Code != http.StatusOK {
			t.Fatalf("renewal %d: %d", i, r.Code)
		}
	}
	rig.is.grant(rig.principal, a.ChainID, rig.clock.now().Add(5*time.Minute))
	rig.clock.advance(4*time.Minute + time.Second) // 12 min past redemption, the chain still live
	if r := rig.relay(c); r.Code != http.StatusForbidden {
		t.Fatalf("past redemption + the session TTL with the chain live: %d, want 403", r.Code)
	}
}

// TestPortalRegistration_RenewalWithheldRefused: with the release switch set to withhold renewal, an issuer
// portal's registration naming ui_session_renewal is refused, and a session through a portal that already
// renews is opened with no chain and ends with its grant.
func TestPortalRegistration_RenewalWithheldRefused(t *testing.T) {
	// A renewable cookie signed while renewal shipped, with a successor live and its entry grant ended.
	rig, a, c := renewingRig(t)
	rig.is.grant(rig.principal, a.ChainID, rig.clock.now().Add(time.Hour))
	rig.clock.advance(6 * time.Minute)
	if r := rig.relay(c); r.Code != http.StatusOK {
		t.Fatalf("renewal shipping, past the entry grant with a successor live: %d %s", r.Code, r.Body.String())
	}

	prev := uiSessionRenewalShips
	uiSessionRenewalShips = false
	t.Cleanup(func() { uiSessionRenewalShips = prev })
	e := newRegEnv(t)
	e.wantRefusal(t, "renewal withheld", regBody(map[string]any{"ui_session_renewal": true}), 422, reasonDelegateRenewalWithheld)
	e.wantCreated(t, "without renewal", regBody(nil))

	// The cookie signed before the switch: served by its entry grant alone, which has ended.
	if r := rig.relay(c); r.Code != http.StatusForbidden {
		t.Fatalf("renewal withheld, an already-signed renewable cookie past its entry grant: %d, want 403", r.Code)
	}
	// A cookie signed after it: capped by its grant.
	fresh := newIssuerUIRig(t, closingBackend("app"))
	fresh.is.renewal = true
	b := fresh.is.grant(fresh.principal, uuid.Nil, fresh.clock.now().Add(5*time.Minute))
	fc := fresh.session(b)
	if fc.Expires.After(b.ExpiresAt) {
		t.Fatalf("renewal withheld: the cookie expires %v, after its grant's %v", fc.Expires, b.ExpiresAt)
	}
	fresh.is.grant(fresh.principal, b.ChainID, fresh.clock.now().Add(time.Hour))
	fresh.clock.advance(6 * time.Minute)
	if r := fresh.relay(fc); r.Code != http.StatusForbidden {
		t.Fatalf("renewal withheld, past the grant with a successor live: %d, want 403", r.Code)
	}
}

// TestUIIssuerPortal_AdmissionReadHeldPastBound: the store holds the opening check's portal read; the request
// is answered 503 at the bound, not held with the store.
func TestUIIssuerPortal_AdmissionReadHeldPastBound(t *testing.T) {
	const bound = 200 * time.Millisecond
	setSupportWatch(t, time.Hour, bound)
	rig, _, c := renewingRig(t)
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	rig.is.hold(hang)
	start := time.Now()
	r := rig.relay(c)
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("an opening check held by the store: %d %s, want 503", r.Code, r.Body.String())
	}
	if d := time.Since(start); d > bound+400*time.Millisecond {
		t.Errorf("answered %v after the request, want at the %v bound", d, bound)
	}
}

// TestUIIssuerPortal_TicketForAMissingRunRefused: a ticket whose run is gone is a bad ticket (403), not an
// outage (503).
func TestUIIssuerPortal_TicketForAMissingRunRefused(t *testing.T) {
	rig := newIssuerUIRig(t, closingBackend("app"))
	tok := rig.ticket(rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(5*time.Minute)))
	rig.store.sshMemStore.mu.Lock()
	delete(rig.store.runs, rig.run.ID)
	rig.store.sshMemStore.mu.Unlock()
	if rec := rig.enterWith(tok); rec.Code != http.StatusForbidden {
		t.Fatalf("a ticket for a missing run: %d %s, want 403", rec.Code, rec.Body.String())
	}
}

// TestUIIssuerPortal_TicketAdmissionReadHeldPastBound: the store holds the redemption's identity read; the enter
// is answered 503 at the bound, not held with the store.
func TestUIIssuerPortal_TicketAdmissionReadHeldPastBound(t *testing.T) {
	const bound = 200 * time.Millisecond
	setSupportWatch(t, time.Hour, bound)
	rig := newIssuerUIRig(t, closingBackend("app"))
	tok := rig.ticket(rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(5*time.Minute)))
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	rig.is.mu.Lock()
	rig.is.hangIdentity = hang
	rig.is.mu.Unlock()
	start := time.Now()
	if rec := rig.enterWith(tok); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a redemption held by the store: %d %s, want 503", rec.Code, rec.Body.String())
	}
	if d := time.Since(start); d > bound+400*time.Millisecond {
		t.Errorf("answered %v after the enter, want at the %v bound", d, bound)
	}
}

// TestUIIssuerPortal_EntryRunReadAnswers: after redemption (its own run read is the first), the entry's run read
// stalls, fails or finds no run. A stall answers the retryable 503 within the opening bound, an outage the same
// 503, and a missing run the wrong-owner 403 a foreign run gets.
func TestUIIssuerPortal_EntryRunReadAnswers(t *testing.T) {
	const bound = 200 * time.Millisecond
	setSupportWatch(t, time.Hour, bound)
	for _, tc := range []struct {
		name   string
		hook   func(ctx context.Context) error
		code   int
		reason string
	}{
		{"stall", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, http.StatusServiceUnavailable, uiDeniedReasonRevocationUnavailable},
		{"outage", func(context.Context) error { return errors.New("store down") }, http.StatusServiceUnavailable, uiDeniedReasonRevocationUnavailable},
		{"missing", func(context.Context) error { return store.ErrNotFound }, http.StatusForbidden, reasonUIGatewayTicketRunMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIssuerUIRig(t, closingBackend("app"))
			tok := rig.ticket(rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(5*time.Minute)))
			var hit bool
			rig.is.mu.Lock()
			rig.is.runReads = 0 // count from the enter on
			rig.is.runHook = func(ctx context.Context, n int) error {
				if n < 2 {
					return nil
				}
				hit = true // read after the enter has returned
				return tc.hook(ctx)
			}
			rig.is.mu.Unlock()
			start := time.Now()
			rec := rig.enterWithin(tok, 5*time.Second)
			if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.reason) {
				t.Fatalf("%s: %d %s, want %d %s", tc.name, rec.Code, rec.Body.String(), tc.code, tc.reason)
			}
			if !hit {
				t.Fatalf("%s: the entry's run read was never asked: the answer came from redemption", tc.name)
			}
			if d := time.Since(start); d > bound+400*time.Millisecond {
				t.Errorf("%s: answered %v after the enter, want within the %v bound", tc.name, d, bound)
			}
		})
	}
}

// dataOutKeptUIStore keeps the run's data_out rule on its row (store.RunDataOuts), so the app door reads the
// rule from the run and the first policy read after the entry's run read is the declared-apps lookup's.
type dataOutKeptUIStore struct{ *issuerUIStore }

func (dataOutKeptUIStore) RaiseRunDataOut(context.Context, uuid.UUID, types.TransferRule) error {
	return nil
}

// TestUIIssuerPortal_EntryPolicyReadAnswers: the entry's run read succeeds, then a policy read stalls or fails:
// the app door's data_out rule (its own 503) or the declared-apps lookup (the retryable 503, never a 500). Each
// answers within the opening bound.
func TestUIIssuerPortal_EntryPolicyReadAnswers(t *testing.T) {
	const bound = 200 * time.Millisecond
	setSupportWatch(t, time.Hour, bound)
	stall := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	for _, tc := range []struct {
		name   string
		kept   bool // the rule is on the run's row
		hook   func(ctx context.Context) error
		reason string
		msg    string // the answering read's own message: the declared-apps lookup's, not a shared door's
	}{
		{"data-out rule stalls", false, stall, reasonDataOutUnreadable, dataOutUnreadableMsg},
		{"declared apps stall", true, stall, uiDeniedReasonRevocationUnavailable, uiSessionUnverifiableMsg},
		{"declared apps fail", true, func(context.Context) error { return errors.New("store down") }, uiDeniedReasonRevocationUnavailable, uiSessionUnverifiableMsg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIssuerUIRig(t, closingBackend("app"))
			if tc.kept {
				allowed := types.TransferAllowed
				rig.run.DataOut = &allowed
				rig.store.putRun(rig.run)
				rig.srv.cfg.Store = dataOutKeptUIStore{rig.is}
			}
			tok := rig.ticket(rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(5*time.Minute)))
			rig.is.mu.Lock()
			rig.is.runReads, rig.is.policyReads = 0, 0 // count from the enter on
			rig.is.policyHook = func(ctx context.Context, n int) error {
				if n > 1 {
					return nil
				}
				return tc.hook(ctx)
			}
			rig.is.mu.Unlock()
			start := time.Now()
			rec := rig.enterWithin(tok, 5*time.Second)
			took := time.Since(start)
			rig.is.mu.Lock()
			policyReads := rig.is.policyReads
			rig.is.mu.Unlock()
			if policyReads == 0 {
				t.Fatalf("%s: no policy read after the entry's run read: %d %s", tc.name, rec.Code, rec.Body.String())
			}
			if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), tc.reason) || !strings.Contains(rec.Body.String(), tc.msg) {
				t.Fatalf("%s: %d %s, want 503 %s: %s", tc.name, rec.Code, rec.Body.String(), tc.reason, tc.msg)
			}
			if took > bound+400*time.Millisecond {
				t.Errorf("%s: answered %v after the enter, want within the %v bound", tc.name, took, bound)
			}
		})
	}
}

// setIssuerAdmit sets the issuer opening reads' bound (issuerAdmitBound) apart from the watch's, for the test.
func setIssuerAdmit(t *testing.T, d time.Duration) {
	t.Helper()
	issuerAdmitWaitNS.Store(int64(d))
	t.Cleanup(func() { issuerAdmitWaitNS.Store(0) })
}

// enterWithin is enterWith on a request whose context ends after d, so an entry held past its bound fails the
// test there, never at the package timeout.
func (rig *issuerUIRig) enterWithin(tok string, d time.Duration) *httptest.ResponseRecorder {
	rig.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	q := url.Values{"run": {rig.run.ID.String()}, "app": {"code"}, "ticket": {tok}}
	req := rig.bound(httptest.NewRequestWithContext(ctx, http.MethodGet, uiEnterPath+"?"+q.Encode(), nil), q)
	rec := httptest.NewRecorder()
	rig.gateway.ServeHTTP(rec, req)
	return rec
}

// TestUIRenewal_ChainReadHeldAtRedemptionAnswers503: a renewing portal's chain read, the last of redemption's,
// hangs. The entry answers the retryable 503 within the opening bound, never a session that quietly does not
// renew.
func TestUIRenewal_ChainReadHeldAtRedemptionAnswers503(t *testing.T) {
	const bound = 200 * time.Millisecond
	setSupportWatch(t, time.Hour, bound)
	rig := newIssuerUIRig(t, closingBackend("app"))
	rig.is.renewal = true
	tok := rig.ticket(rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(5*time.Minute)))
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	rig.is.hold(hang)
	start := time.Now()
	rec := rig.enterWithin(tok, 5*time.Second)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), uiDeniedReasonRevocationUnavailable) {
		t.Fatalf("a held chain read at redemption: %d %s, want 503 %s", rec.Code, rec.Body.String(), uiDeniedReasonRevocationUnavailable)
	}
	if len(setUICookies(rec)) != 0 {
		t.Fatal("a held chain read at redemption set a session cookie")
	}
	if d := time.Since(start); d > bound+400*time.Millisecond {
		t.Errorf("answered %v after the enter, want within the %v bound", d, bound)
	}
}

// TestUIIssuerPortal_OpeningReadsShareOneBound: redemption's identity read takes most of the opening bound and
// the entry's run read then hangs. The entry answers within one bound of its start, not one bound per read.
func TestUIIssuerPortal_OpeningReadsShareOneBound(t *testing.T) {
	const bound, first = 500 * time.Millisecond, 400 * time.Millisecond
	setSupportWatch(t, time.Hour, bound)
	rig := newIssuerUIRig(t, closingBackend("app"))
	tok := rig.ticket(rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(5*time.Minute)))
	slow := make(chan struct{})
	var hit bool
	rig.is.mu.Lock()
	rig.is.hangIdentity = slow
	rig.is.runReads = 0
	rig.is.runHook = func(ctx context.Context, n int) error {
		if n < 2 {
			return nil
		}
		hit = true // read after the enter has returned
		<-ctx.Done()
		return ctx.Err()
	}
	rig.is.mu.Unlock()
	time.AfterFunc(first, func() { close(slow) })
	start := time.Now()
	rec := rig.enterWithin(tok, 5*time.Second)
	took := time.Since(start)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), uiDeniedReasonRevocationUnavailable) {
		t.Fatalf("%d %s, want 503 %s", rec.Code, rec.Body.String(), uiDeniedReasonRevocationUnavailable)
	}
	if !hit {
		t.Fatal("the entry's run read was never asked: the slow identity read ran out the bound alone")
	}
	if took < first || took > bound+200*time.Millisecond {
		t.Fatalf("answered %v after the enter, want after the %v identity read and within the one %v bound", took, first, bound)
	}
}

// issuerDoorStore is the background watch harness's store with its roster read (the second site read; the first
// is the site's own limit) and its policy rows answered by site and audit when set.
type issuerDoorStore struct {
	*desktopSiteStore
	siteReads   atomic.Int32
	site, audit func(ctx context.Context) error
}

func (s *issuerDoorStore) GetSiteConfig(ctx context.Context) (types.SiteConfig, error) {
	if s.siteReads.Add(1) >= 2 && s.site != nil {
		if err := s.site(ctx); err != nil {
			return types.SiteConfig{}, err
		}
	}
	return s.desktopSiteStore.GetSiteConfig(ctx)
}

func (s *issuerDoorStore) QueryAuditEvents(ctx context.Context, runID uuid.UUID, limit int) ([]types.AuditEvent, error) {
	if s.audit != nil {
		if err := s.audit(ctx); err != nil {
			return nil, err
		}
	}
	return s.desktopSiteStore.QueryAuditEvents(ctx, runID, limit)
}

// TestUIIssuerPortal_BackgroundDoorReadsAnswer503: a background Mac run's door, entered with the watch's ticket,
// meets a sign-in roster or an inbound-app policy it cannot read, failed or held past the request's deadline. On
// an issuer portal's opening each is the retryable 503. Any other ticket keeps its answer: the roster refuses as
// sign-ins off (403), and an unreadable policy is no inbound app.
func TestUIIssuerPortal_BackgroundDoorReadsAnswer503(t *testing.T) {
	stall := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	outage := func(context.Context) error { return errors.New("store down") }
	for _, tc := range []struct {
		name        string
		site, audit func(context.Context) error
	}{
		{"roster outage", outage, nil},
		{"roster stall", stall, nil},
		{"inbound policy outage", nil, outage},
		{"inbound policy stall", nil, stall},
	} {
		for _, issuer := range []bool{true, false} {
			name := tc.name + "/other ticket"
			if issuer {
				name = tc.name + "/issuer ticket"
			}
			t.Run(name, func(t *testing.T) {
				h, st := bgWatchHarness(t)
				h.srv.cfg.Store = &issuerDoorStore{desktopSiteStore: st, site: tc.site, audit: tc.audit}
				ta := ticketActor{actorType: types.ActorHuman, principal: h.owner, role: oidc.RoleUser, app: uiDesktopAppName}
				if issuer {
					ta.admitBy = time.Now().Add(200 * time.Millisecond)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond) // the opening's, or the request's end
				defer cancel()
				req := httptest.NewRequestWithContext(ctx, http.MethodGet, uiEnterPath, nil)
				rec := httptest.NewRecorder()
				answered := h.srv.uiBackgroundDoorRefused(rec, req, backgroundOnlyRefusal(h.run, "web application gateway"), h.run, uiDesktopAppName, ta)
				unverifiable := answered && rec.Code == http.StatusServiceUnavailable && strings.Contains(rec.Body.String(), uiDeniedReasonRevocationUnavailable)
				switch {
				case issuer && !unverifiable:
					t.Fatalf("answered %v: %d %s, want the retryable 503", answered, rec.Code, rec.Body.String())
				case !issuer && unverifiable:
					t.Fatalf("another ticket's answer changed to the issuer's 503: %s", rec.Body.String())
				case !issuer && tc.site != nil && (!answered || rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), reasonDesktopSignInsOff)):
					t.Fatalf("another ticket's unreadable roster: %v %d %s, want 403 %s", answered, rec.Code, rec.Body.String(), reasonDesktopSignInsOff)
				case !issuer && tc.audit != nil:
					// An unreadable policy is no inbound app: the answer is the one a readable policy gives the same ticket.
					h2, st2 := bgWatchHarness(t)
					h2.srv.cfg.Store = &issuerDoorStore{desktopSiteStore: st2}
					want := httptest.NewRecorder()
					wantAnswered := h2.srv.uiBackgroundDoorRefused(want, httptest.NewRequest(http.MethodGet, uiEnterPath, nil),
						backgroundOnlyRefusal(h2.run, "web application gateway"), h2.run, uiDesktopAppName, ta)
					if answered != wantAnswered || rec.Code != want.Code || rec.Body.String() != want.Body.String() {
						t.Fatalf("another ticket's unreadable policy: %v %d %q, want the readable policy's %v %d %q",
							answered, rec.Code, rec.Body.String(), wantAnswered, want.Code, want.Body.String())
					}
				}
			})
		}
	}
}

// TestUIRenewal_CookieReadsShareOneBound: a renewing session's relay request reads its run slowly, taking most of
// the opening bound, and then its chain read hangs. The request answers within one bound of its start, not one
// bound per read.
func TestUIRenewal_CookieReadsShareOneBound(t *testing.T) {
	const bound, first = 500 * time.Millisecond, 400 * time.Millisecond
	setSupportWatch(t, time.Hour, bound)
	rig, _, c := renewingRig(t)
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	var slowed bool
	rig.is.mu.Lock()
	rig.is.runReads = 0
	rig.is.runHook = func(ctx context.Context, n int) error {
		if n != 1 {
			return nil
		}
		slowed = true // read after the request has returned
		select {
		case <-time.After(first):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	rig.is.mu.Unlock()
	rig.is.hold(hang)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	rec := rig.uiHarness.relay("/", c, func(r *http.Request) { *r = *r.WithContext(ctx) })
	took := time.Since(start)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d %s, want 503", rec.Code, rec.Body.String())
	}
	if !slowed {
		t.Fatal("the relay's run read was never asked: the slow read did not take part of the bound")
	}
	if took < first || took > bound+200*time.Millisecond {
		t.Fatalf("answered %v after the request, want after the %v run read and within the one %v bound", took, first, bound)
	}
}
