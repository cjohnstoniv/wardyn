// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
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

	hang           chan struct{}             // while set, a chain read waits for it to close
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

func (s *issuerUIStore) IdentityBlocked(_ context.Context, principal string, _ int64) (bool, error) {
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
	for _, app := range []string{uiDesktopAppName, "novnc"} {
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
	uiSessionRenewalShips = false
	t.Cleanup(func() { uiSessionRenewalShips = true })
	e := newRegEnv(t)
	e.wantRefusal(t, "renewal withheld", regBody(map[string]any{"ui_session_renewal": true}), 422, reasonDelegateRenewalWithheld)
	e.wantCreated(t, "without renewal", regBody(nil))

	rig := newIssuerUIRig(t, closingBackend("app"))
	rig.is.renewal = true
	a := rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(5*time.Minute))
	c := rig.session(a)
	if c.Expires.After(a.ExpiresAt) {
		t.Fatalf("renewal withheld: the cookie expires %v, after its grant's %v", c.Expires, a.ExpiresAt)
	}
	rig.is.grant(rig.principal, a.ChainID, rig.clock.now().Add(time.Hour))
	rig.clock.advance(6 * time.Minute)
	if r := rig.relay(c); r.Code != http.StatusForbidden {
		t.Fatalf("renewal withheld, past the grant with a successor live: %d, want 403", r.Code)
	}
}
