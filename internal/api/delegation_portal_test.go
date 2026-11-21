// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The issuer-portal exchange and delegated lane, in memory: the verifier is a fake that answers what lane 1's
// verifier would (its own tests prove the checks), and the store keeps the three portal tables in maps, with
// the guards the Postgres writes make (a revoked portal, a changed admission revision, a stale token).

const (
	portalTestIssuer = "https://idp.portal.example/oauth2/default"
	portalTestClient = "portal-client"
	portalTestUID    = "00u-person-1"
)

// portalAsk is one call the exchange made to the verifier.
type portalAsk struct {
	reg      oidc.PortalRegistration
	raw      string
	declared string
}

// fakePortalVerifier answers every token with subj or refusal and records what it was asked.
type fakePortalVerifier struct {
	mu      sync.Mutex
	subj    oidc.PortalSubject
	refusal oidc.PortalRefusal
	asks    []portalAsk
	// enforceAge makes the fake apply the registration's maximum token age as the verifier does: a token at or
	// past it is refused stale, and the grant's signed end is the earlier of iat + age and the token's expiry.
	enforceAge bool
	// claims are the signed claims of the token, and the registration's one required claim is matched against them.
	claims map[string]string
	// inVerify runs while a verification is in flight, after the exchange read its portal and before its writes.
	inVerify func()
}

func (f *fakePortalVerifier) VerifyDetail(_ context.Context, reg oidc.PortalRegistration, raw, declared string) (oidc.PortalSubject, oidc.PortalRefusal) {
	if f.inVerify != nil {
		f.inVerify()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asks = append(f.asks, portalAsk{reg: reg, raw: raw, declared: declared})
	if !f.refusal.OK() {
		return oidc.PortalSubject{}, f.refusal
	}
	subj := f.subj
	if reg.RequiredClaim != "" && f.claims[reg.RequiredClaim] != reg.RequiredClaimValue {
		return oidc.PortalSubject{}, oidc.PortalRefusal{Reason: oidc.SubjectTokenClaimMismatch, Class: oidc.PortalRefusedAdmission}
	}
	if f.enforceAge {
		age := reg.MaxSubjectTokenAge
		if age <= 0 {
			age = 300 * time.Second
		}
		end := subj.IssuedAt.Add(age)
		if !time.Now().Before(end) {
			return oidc.PortalSubject{}, oidc.PortalRefusal{Reason: oidc.SubjectTokenStale, Class: oidc.PortalRefusedToken}
		}
		if subj.Expiry.Before(end) {
			end = subj.Expiry
		}
		subj.GrantEnd = end
	}
	return subj, oidc.PortalRefusal{}
}

func (f *fakePortalVerifier) asked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asks)
}

// portalIdent is one identity row of the fake store.
type portalIdent struct {
	id          uuid.UUID
	principal   string
	introBy     uuid.UUID
	realIssuer  string
	key         string
	eventID     uuid.UUID
	eventAt     time.Time
	audited     bool
	deactivated bool
	display     string
}

// portalStore is the issuer-portal surface in memory on top of the matrix server's store.
type portalStore struct {
	*authzStore
	pmu       sync.Mutex
	portals   map[uuid.UUID]types.Delegate
	byCred    map[string]uuid.UUID
	grants    map[string]types.PortalGrant
	idents    map[string]*portalIdent
	userTypes map[string]bool
	stamped   int
	swept     int
	mintErr   error
	// userTypeErr, groupsErr and issueErr fail the user type read, the sign-in record write and the identity write.
	userTypeErr, groupsErr, issueErr error
	// audit is the recorder whose rows stand for the shared audit table.
	audit *recRecorder
}

var (
	_ store.PortalGrantStore    = (*portalStore)(nil)
	_ store.PortalIdentityStore = (*portalStore)(nil)
)

func (p *portalStore) GetUserType(_ context.Context, id string) (types.UserType, error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	if p.userTypeErr != nil {
		return types.UserType{}, p.userTypeErr
	}
	if p.userTypes[id] {
		return types.UserType{ID: id, Name: id}, nil
	}
	return types.UserType{}, store.ErrNotFound
}

// AuditEventsStored answers from the recorder's rows, as the shared audit table, by normalised (time, id).
func (p *portalStore) AuditEventsStored(_ context.Context, want []store.AuditEventKey) (map[store.AuditEventKey]bool, error) {
	norm := func(id uuid.UUID, at time.Time) store.AuditEventKey {
		return store.AuditEventKey{ID: id, At: at.UTC().Truncate(time.Microsecond)}
	}
	asked := map[store.AuditEventKey]bool{}
	for _, k := range want {
		asked[norm(k.ID, k.At)] = true
	}
	out := map[store.AuditEventKey]bool{}
	if p.audit == nil {
		return out, nil
	}
	for _, ev := range p.audit.snapshot() {
		if k := norm(ev.ID, ev.Time); asked[k] {
			out[k] = true
		}
	}
	return out, nil
}

func (p *portalStore) PutPrincipalGroups(context.Context, string, string, string, []string, bool, time.Time) error {
	return p.groupsErr
}

func (p *portalStore) GetIssuerDelegateByRaw(_ context.Context, raw string) (types.Delegate, error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	d, ok := p.portals[p.byCred[memHash(raw)]]
	if !ok || !strings.HasPrefix(raw, store.PortalCredentialPrefix) || d.RevokedAt != nil || d.Issuer == "" {
		return types.Delegate{}, store.ErrNotFound
	}
	return d, nil
}

// lockPortal is the revoked-portal and admission-revision guard both exchange writes take.
func (p *portalStore) lockPortal(id uuid.UUID, rev int64) (types.Delegate, error) {
	d, ok := p.portals[id]
	switch {
	case !ok:
		return types.Delegate{}, store.ErrNotFound
	case d.RevokedAt != nil:
		return types.Delegate{}, store.ErrPortalGone
	case d.AdmissionRev != rev:
		return types.Delegate{}, store.ErrAdmissionChanged
	}
	return d, nil
}

func (p *portalStore) IssuePortalIdentity(_ context.Context, principal, key, display, realIssuer string, portal uuid.UUID, rev int64) (uuid.UUID, bool, error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	if p.issueErr != nil {
		return uuid.Nil, false, p.issueErr
	}
	if _, err := p.lockPortal(portal, rev); err != nil {
		return uuid.Nil, false, err
	}
	if id, ok := p.idents[principal]; ok {
		if id.deactivated {
			return uuid.Nil, false, store.ErrIdentityDeactivated
		}
		return id.id, false, nil
	}
	id := &portalIdent{id: uuid.New(), principal: principal, introBy: portal, realIssuer: realIssuer, key: key,
		eventID: uuid.New(), eventAt: time.Now().UTC().Truncate(time.Microsecond), display: display}
	p.idents[principal] = id
	return id.id, true, nil
}

func (p *portalStore) LookupPortalPerson(context.Context, string, string) (types.PortalPerson, error) {
	return types.PortalPerson{}, store.ErrNotFound
}

func (i *portalIdent) introduction() store.PortalIntroduction {
	return store.PortalIntroduction{IdentityID: i.id, Principal: i.principal, IssuerKey: i.key, PortalID: i.introBy,
		Issuer: i.realIssuer, EventID: i.eventID, EventAt: i.eventAt}
}

// PendingPortalIntroductions is the oldest owed introductions across every identity, at most limit, as the store reads them.
func (p *portalStore) PendingPortalIntroductions(_ context.Context, limit int) ([]store.PortalIntroduction, error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	var out []store.PortalIntroduction
	for _, i := range p.idents {
		if !i.audited {
			out = append(out, i.introduction())
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].EventAt.Before(out[b].EventAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PendingPortalIntroductionFor is one identity's own owed introduction, by its id.
func (p *portalStore) PendingPortalIntroductionFor(_ context.Context, id uuid.UUID) (store.PortalIntroduction, bool, error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	for _, i := range p.idents {
		if i.id == id && !i.audited {
			return i.introduction(), true, nil
		}
	}
	return store.PortalIntroduction{}, false, nil
}

func (p *portalStore) MarkPortalIntroductionAudited(_ context.Context, id uuid.UUID) (bool, error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	for _, i := range p.idents {
		if i.id == id && !i.audited {
			i.audited = true
			return true, nil
		}
	}
	return false, nil
}

func (p *portalStore) MintPortalGrant(_ context.Context, g types.PortalGrant, raw string, signedEnd time.Time, ttl time.Duration, rev int64) (types.PortalGrant, time.Time, error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	if p.mintErr != nil {
		return types.PortalGrant{}, time.Time{}, p.mintErr
	}
	d, err := p.lockPortal(g.DelegateID, rev)
	if err != nil {
		return types.PortalGrant{}, time.Time{}, err
	}
	if !strings.HasPrefix(g.Principal, types.PortalPersonPrefix+d.IssuerKey+":") {
		return types.PortalGrant{}, time.Time{}, store.ErrPortalGone
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	end := now.Add(ttl)
	if signedEnd.Before(end) {
		end = signedEnd
	}
	end = end.Truncate(time.Microsecond)
	if end.Sub(now) < 5*time.Second {
		return types.PortalGrant{}, time.Time{}, store.ErrSubjectTokenStale
	}
	g.CreatedAt, g.ExpiresAt, g.ChainID = now, end, g.ID
	g.Issuer, g.IssuerKey = d.Issuer, d.IssuerKey
	p.grants[memHash(raw)] = g
	return g, now, nil
}

func (p *portalStore) grantLive(g types.PortalGrant, now time.Time) (types.PortalGrant, bool) {
	d, ok := p.portals[g.DelegateID]
	if !ok || d.RevokedAt != nil || !g.ExpiresAt.After(now) {
		return types.PortalGrant{}, false
	}
	g.Issuer, g.IssuerKey = d.Issuer, d.IssuerKey
	return g, true
}

func (p *portalStore) GetPortalGrantByRaw(_ context.Context, raw string, now time.Time) (types.PortalGrant, error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	if g, ok := p.grants[memHash(raw)]; ok {
		if g, ok = p.grantLive(g, now); ok {
			return g, nil
		}
	}
	return types.PortalGrant{}, store.ErrNotFound
}

func (p *portalStore) GetPortalGrantByID(_ context.Context, id uuid.UUID, now time.Time) (types.PortalGrant, error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	for _, g := range p.grants {
		if g.ID == id {
			if g, ok := p.grantLive(g, now); ok {
				return g, nil
			}
		}
	}
	return types.PortalGrant{}, store.ErrNotFound
}

func (p *portalStore) PortalChainLive(context.Context, uuid.UUID, string, uuid.UUID) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

func (p *portalStore) StampPortalUsed(context.Context, uuid.UUID, time.Time) error {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	p.stamped++
	return nil
}

func (p *portalStore) SweepExpiredPortalGrants(context.Context, time.Time) (int64, error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	p.swept++
	return 0, nil
}

func (p *portalStore) revoke(id uuid.UUID) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	d := p.portals[id]
	now := time.Now()
	d.RevokedAt = &now
	p.portals[id] = d
}

// seedGrant stores a live grant row exactly as given, for the reverse check's corrupt-store cases.
func (p *portalStore) seedGrant(g types.PortalGrant) string {
	raw := newBearer(delegatedTokenPrefix)
	if g.ID == uuid.Nil {
		g.ID = uuid.New()
	}
	if g.ExpiresAt.IsZero() {
		g.ExpiresAt = time.Now().Add(5 * time.Minute)
	}
	g.CreatedAt = time.Now()
	p.pmu.Lock()
	defer p.pmu.Unlock()
	p.grants[memHash(raw)] = g
	return raw
}

// exchHarness is the matrix server with an issuer-portal store and a fake verifier behind it.
type exchHarness struct {
	srv *Server
	ps  *portalStore
	ver *fakePortalVerifier
	rec *recRecorder
}

func newExchHarness(t *testing.T, shape ...func(*Config)) *exchHarness {
	t.Helper()
	ps := &portalStore{portals: map[uuid.UUID]types.Delegate{}, byCred: map[string]uuid.UUID{}, grants: map[string]types.PortalGrant{},
		idents: map[string]*portalIdent{}, userTypes: map[string]bool{}}
	ver := &fakePortalVerifier{}
	wire := func(c *Config) {
		ps.authzStore = c.Store.(*authzStore)
		c.Store = ps
		c.PortalIssuers = ver
	}
	srv, _, _, _ := newAuthzMatrixServer(t, append([]func(*Config){wire}, shape...)...)
	h := &exchHarness{srv: srv, ps: ps, ver: ver, rec: srv.cfg.Audit.(*recRecorder)}
	ps.audit = h.rec
	h.ver.subj = h.subject(portalTestUID)
	return h
}

// subject is a verified person of the test issuer, issued 10 s ago, ending at its portal-less cutoff.
func (h *exchHarness) subject(uid string) oidc.PortalSubject {
	now := time.Now()
	return oidc.PortalSubject{
		Principal: oidc.PortalPrincipal(portalTestIssuer, uid), Issuer: portalTestIssuer, K: oidc.PortalIssuerKey(portalTestIssuer),
		PersonID: uid, Sub: "pat@corp.example", IssuedAt: now.Add(-10 * time.Second), Expiry: now.Add(time.Hour),
		GrantEnd: now.Add(290 * time.Second),
	}
}

// seedPortal registers an issuer portal in the store and returns it with its wdi_ credential. mut edits the row first.
func (h *exchHarness) seedPortal(mut func(*types.Delegate)) (types.Delegate, string) {
	d := types.Delegate{
		ID: uuid.New(), Name: "issuer portal", IdPClientID: portalTestClient, Issuer: portalTestIssuer, IssuerKey: oidc.PortalIssuerKey(portalTestIssuer),
		TokenProfile: oidc.PortalProfileAccessToken, Audience: "api://wardyn", RequiredScopes: []string{"wardyn.portal"}, UserType: types.UserTypeStandard,
		MaxSubjectTokenAgeSeconds: 300, GrantTTLSeconds: 300, AdmissionRev: 1, PortalManagedAdmission: true, CreatedAt: time.Now(),
	}
	if mut != nil {
		mut(&d)
	}
	cred := newBearer(store.PortalCredentialPrefix)
	h.ps.pmu.Lock()
	h.ps.portals[d.ID] = d
	h.ps.byCred[memHash(cred)] = d.ID
	h.ps.pmu.Unlock()
	return d, cred
}

// exchange posts the token exchange as portal id with cred; extra overrides or adds form fields.
func (h *exchHarness) exchange(t *testing.T, id uuid.UUID, cred, subject string, extra url.Values) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"grant_type": {grantTypeTokenExchange}, "subject_token": {subject}, "subject_token_type": {tokenTypeAccessToken}}
	for k, v := range extra {
		form[k] = v
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth(id.String(), cred)
	w := httptest.NewRecorder()
	panicFails(t, h.srv.Handler()).ServeHTTP(w, r)
	return w
}

// token is a successful exchange's bearer.
func portalToken(t *testing.T, w *httptest.ResponseRecorder) (string, map[string]any) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("exchange = %d %s, want 200", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	tok, _ := out["access_token"].(string)
	if !strings.HasPrefix(tok, delegatedTokenPrefix) {
		t.Fatalf("exchange response = %s, want a wdg_ bearer", w.Body.String())
	}
	return tok, out
}

// exchangeRows are the delegation.exchange rows written so far.
func (h *exchHarness) exchangeRows() []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range h.rec.snapshot() {
		if ev.Action == "delegation.exchange" {
			out = append(out, ev)
		}
	}
	return out
}

func portalRowData(t *testing.T, ev types.AuditEvent) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// oauthCode is the error code of a refused exchange.
func oauthCode(w *httptest.ResponseRecorder) string {
	var e oauthError
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	return e.Error
}

func TestPortalExchange_ReservedRawSubRefused(t *testing.T) {
	h := newExchHarness(t)
	d, cred := h.seedPortal(nil)
	// The verifier is handed the same predicate every human door asks, and a raw sub or person id it names
	// is refused 400 invalid_grant before anything is stored.
	reserved := PortalReservedCheck("ops-seat")
	for _, p := range append([]string{"delegate:00000000-0000-0000-0000-000000000001", "DELEGATE:person:x", "service:ci", "ops-seat"}, reservedVariants...) {
		if !reserved(p) {
			t.Errorf("the portal verifier's reserved check let %q through", p)
		}
	}
	if reserved("pat@corp.example") || reserved(portalTestUID) {
		t.Error("the portal verifier's reserved check refused an ordinary person")
	}
	h.ver.refusal = oidc.PortalRefusal{Reason: oidc.DenialReservedPrincipal, Class: oidc.PortalRefusedToken}
	w := h.exchange(t, d.ID, cred, "subject-token", nil)
	if w.Code != http.StatusBadRequest || oauthCode(w) != "invalid_grant" {
		t.Fatalf("a reserved sub: %d %s, want 400 invalid_grant", w.Code, w.Body.String())
	}
	if len(h.ps.idents) != 0 || len(h.ps.grants) != 0 {
		t.Fatalf("a refused exchange stored %d identities and %d grants", len(h.ps.idents), len(h.ps.grants))
	}
	if rows := h.exchangeRows(); len(rows) != 1 || rows[0].Outcome != "denied" || portalRowData(t, rows[0])["reason"] != oidc.DenialReservedPrincipal {
		t.Fatalf("delegation.exchange rows = %+v, want one denial naming reserved_principal", rows)
	}
}

func TestPortalExchange_DelegatedTokenAsSubjectRefused(t *testing.T) {
	h := newExchHarness(t)
	d, cred := h.seedPortal(nil)
	// A wdg_ bearer, whichever table holds it, is no subject token: refused before the verifier is asked.
	portalGrant := h.ps.seedGrant(types.PortalGrant{DelegateID: d.ID, Principal: oidc.PortalPrincipal(portalTestIssuer, "x"), UserType: types.UserTypeStandard})
	legacy, _ := seedDelegation(t, h.ps.fakeDelegateStore, "console-person")
	for name, tok := range map[string]string{"portal_grants": portalGrant, "delegated_tokens": legacy, "unknown": delegatedTokenPrefix + "nothing"} {
		w := h.exchange(t, d.ID, cred, tok, nil)
		if w.Code != http.StatusBadRequest || oauthCode(w) != "invalid_grant" {
			t.Errorf("%s as subject: %d %s, want 400 invalid_grant", name, w.Code, w.Body.String())
		}
	}
	if h.ver.asked() != 0 {
		t.Fatalf("the verifier was asked about a wdg_ subject %d times", h.ver.asked())
	}
	for _, ev := range h.exchangeRows() {
		if portalRowData(t, ev)["reason"] != "subject_token_delegated" {
			t.Errorf("denial row = %s, want subject_token_delegated", ev.Data)
		}
	}
}

func TestPortalExchange_ActorTokenRefused(t *testing.T) {
	h := newExchHarness(t)
	d, cred := h.seedPortal(nil)
	w := h.exchange(t, d.ID, cred, "subject-token", url.Values{"actor_token": {"agent-token"}, "actor_token_type": {tokenTypeAccessToken}})
	if w.Code != http.StatusBadRequest || oauthCode(w) != "invalid_request" {
		t.Fatalf("an actor token: %d %s, want 400 invalid_request", w.Code, w.Body.String())
	}
	if h.ver.asked() != 0 || len(h.ps.grants) != 0 {
		t.Fatalf("an actor token reached the verifier (%d) or minted (%d)", h.ver.asked(), len(h.ps.grants))
	}
	if rows := h.exchangeRows(); len(rows) != 1 || portalRowData(t, rows[0])["reason"] != "actor_token_unsupported" {
		t.Fatalf("rows = %+v, want one actor_token_unsupported denial", rows)
	}
	// Without it the same exchange is admitted: the refusal is the actor token's alone.
	if w := h.exchange(t, d.ID, cred, "subject-token", nil); w.Code != http.StatusOK {
		t.Fatalf("control exchange = %d %s", w.Code, w.Body.String())
	}
}

func TestPortalExchange_OutsideScope(t *testing.T) {
	h := newExchHarness(t)
	d, cred := h.seedPortal(func(d *types.Delegate) { d.Group, d.PortalManagedAdmission = "portal-users", false })
	h.ver.refusal = oidc.PortalRefusal{Reason: oidc.PortalOutsideScope, Class: oidc.PortalRefusedAdmission}
	w := h.exchange(t, d.ID, cred, "subject-token", nil)
	if w.Code != http.StatusForbidden || oauthCode(w) != "access_denied" {
		t.Fatalf("outside the group: %d %s, want 403 access_denied", w.Code, w.Body.String())
	}
	if got := h.ver.asks[0].reg.Group; got != "portal-users" {
		t.Errorf("the verifier was handed group %q, want the portal's own", got)
	}
	if len(h.ps.idents) != 0 || len(h.ps.grants) != 0 {
		t.Fatal("a refused person was introduced or granted")
	}
	if rows := h.exchangeRows(); len(rows) != 1 || portalRowData(t, rows[0])["reason"] != oidc.PortalOutsideScope {
		t.Fatalf("rows = %+v, want one outside_scope denial", rows)
	}
}

func TestPortalExchange_EmailDomainGate(t *testing.T) {
	h := newExchHarness(t)
	d, cred := h.seedPortal(func(d *types.Delegate) {
		d.TokenProfile, d.Audience, d.RequiredScopes, d.EmailDomains = oidc.PortalProfileIDToken, "", nil, []string{"corp.example"}
	})
	for _, reason := range []string{oidc.DenialEmailVerifiedAbsent, oidc.DenialEmailUnverified, oidc.DenialEmailDomain} {
		h.ver.refusal = oidc.PortalRefusal{Reason: reason, Class: oidc.PortalRefusedAdmission}
		w := h.exchange(t, d.ID, cred, "subject-token", url.Values{"subject_token_type": {tokenTypeIDToken}})
		if w.Code != http.StatusForbidden || oauthCode(w) != "access_denied" {
			t.Errorf("%s: %d %s, want 403 access_denied", reason, w.Code, w.Body.String())
		}
	}
	if len(h.ps.grants) != 0 {
		t.Fatal("a refused email minted a grant")
	}
	// The list reaches the verifier, which owns the check; an admitted ID-token person is introduced under
	// their email for display only.
	h.ver.refusal = oidc.PortalRefusal{}
	h.ver.subj.Email, h.ver.subj.EmailVerified = "pat@corp.example", true
	if w := h.exchange(t, d.ID, cred, "subject-token", url.Values{"subject_token_type": {tokenTypeIDToken}}); w.Code != http.StatusOK {
		t.Fatalf("admitted: %d %s", w.Code, w.Body.String())
	}
	if got := h.ver.asks[len(h.ver.asks)-1].reg.EmailDomains; len(got) != 1 || got[0] != "corp.example" {
		t.Errorf("the verifier was handed email domains %v", got)
	}
	if h.ps.idents[h.ver.subj.Principal].display != "pat@corp.example" {
		t.Errorf("display value = %q", h.ps.idents[h.ver.subj.Principal].display)
	}
}

func TestPortalExchange_UserTypeDeleted(t *testing.T) {
	h := newExchHarness(t)
	d, cred := h.seedPortal(func(d *types.Delegate) { d.UserType = "contractor" })
	w := h.exchange(t, d.ID, cred, "subject-token", nil)
	if w.Code != http.StatusBadRequest || oauthCode(w) != "invalid_grant" {
		t.Fatalf("a deleted user type: %d %s, want 400 invalid_grant", w.Code, w.Body.String())
	}
	if len(h.ps.idents) != 0 || len(h.ps.grants) != 0 {
		t.Fatal("a portal whose user type is gone introduced or granted")
	}
	if rows := h.exchangeRows(); len(rows) != 1 || portalRowData(t, rows[0])["reason"] != "user_type_unknown" {
		t.Fatalf("rows = %+v, want one user_type_unknown denial", rows)
	}
	h.ps.userTypes["contractor"] = true
	tok, _ := portalToken(t, h.exchange(t, d.ID, cred, "subject-token", nil))
	if w := do(t, h.srv, http.MethodGet, "/api/v1/me", tok, ""); w.Code != http.StatusOK {
		t.Fatalf("GET /me with the grant = %d %s", w.Code, w.Body.String())
	}
}

func TestPortalExchange_AuditRows(t *testing.T) {
	h := newExchHarness(t)
	d, cred := h.seedPortal(func(d *types.Delegate) {
		d.RequiredClaim = &types.PortalRequiredClaim{Claim: "tenant", Value: "secret-environment-name"}
	})
	h.ver.subj.Email = "pat@corp.example"
	k := oidc.PortalIssuerKey(portalTestIssuer)
	// A denial: reason, issuer, k and profile, and the claim's name, never a value.
	h.ver.refusal = oidc.PortalRefusal{Reason: oidc.SubjectTokenClaimMismatch, Class: oidc.PortalRefusedAdmission}
	h.exchange(t, d.ID, cred, "the-subject-token", nil)
	admission := map[string]any{"group": false, "claim": "tenant", "portal_managed": true}
	want := map[string]any{
		"reason": oidc.SubjectTokenClaimMismatch, "issuer": portalTestIssuer, "k": k, "token_profile": oidc.PortalProfileAccessToken, "claim": "tenant",
		"max_subject_token_age_seconds": float64(300), "grant_ttl_seconds": float64(300), "admission": admission,
	}
	if rows := h.exchangeRows(); len(rows) != 1 || rows[0].Outcome != "denied" || rows[0].Actor != delegateActor(d.ID) || rows[0].Target != "" || !portalSameJSON(portalRowData(t, rows[0]), want) {
		t.Fatalf("denial rows = %+v, want exactly %v", rows, want)
	}
	// The success: the grant, its end, the role, the freshness settings and the names of the admission rule.
	h.ver.refusal = oidc.PortalRefusal{}
	_, out := portalToken(t, h.exchange(t, d.ID, cred, "the-subject-token", nil))
	rows := h.exchangeRows()
	if len(rows) != 2 || rows[1].Outcome != "success" || rows[1].Target != h.ver.subj.Principal {
		t.Fatalf("success rows = %+v", rows)
	}
	got := portalRowData(t, rows[1])
	for _, k := range []string{"grant", "expires_at"} {
		if _, ok := got[k]; !ok {
			t.Errorf("the success row has no %s: %s", k, rows[1].Data)
		}
		delete(got, k)
	}
	want = map[string]any{
		"user_type": types.UserTypeStandard, "role": "user", "issuer": portalTestIssuer, "k": k, "token_profile": oidc.PortalProfileAccessToken,
		"max_subject_token_age_seconds": float64(300), "grant_ttl_seconds": float64(300),
		"admission": admission,
	}
	if !portalSameJSON(got, want) {
		t.Fatalf("success row data = %v, want %v", got, want)
	}
	// No row carries a token, a claim value or an email.
	for _, ev := range h.rec.snapshot() {
		for _, bad := range []string{"the-subject-token", "secret-environment-name", "pat@corp.example", out["access_token"].(string)} {
			if strings.Contains(string(ev.Data), bad) || ev.Target == bad {
				t.Errorf("audit row %s carries %q: %s", ev.Action, bad, ev.Data)
			}
		}
	}
}

func portalSameJSON(a, b map[string]any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestPortalExchange_ExpiresInMatchesGrant(t *testing.T) {
	h := newExchHarness(t)
	d, cred := h.seedPortal(nil)
	check := func(name string, end time.Duration) {
		t.Helper()
		h.ver.subj.GrantEnd = time.Now().Add(end)
		tok, out := portalToken(t, h.exchange(t, d.ID, cred, "subject-token", nil))
		g, err := h.ps.GetPortalGrantByRaw(context.Background(), tok, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		in := int(out["expires_in"].(float64))
		if want := int(g.ExpiresAt.Sub(g.CreatedAt) / time.Second); in != want || in < 5 {
			t.Errorf("%s: expires_in = %d, the grant lives %d s (and never under 5)", name, in, want)
		}
		if g.ExpiresAt.After(h.ver.subj.GrantEnd.Add(time.Millisecond)) || g.ExpiresAt.After(g.CreatedAt.Add(300*time.Second)) {
			t.Errorf("%s: the grant ends at %v, past its signed end or its own ttl", name, g.ExpiresAt)
		}
	}
	check("cutoff far", time.Hour)       // the mint's own ttl comes first: 300
	check("cutoff near", 42*time.Second) // the token's cutoff comes first: about 42
	// Under five seconds of life is refused and writes nothing.
	n := len(h.ps.grants)
	h.ver.subj.GrantEnd = time.Now().Add(2 * time.Second)
	w := h.exchange(t, d.ID, cred, "subject-token", nil)
	if w.Code != http.StatusBadRequest || oauthCode(w) != "invalid_grant" || len(h.ps.grants) != n {
		t.Fatalf("a token with 2 s left: %d %s (grants %d -> %d), want 400 and no grant", w.Code, w.Body.String(), n, len(h.ps.grants))
	}
}

func TestPortalMaxAgeMode_EndsByIatPlusMaxAge(t *testing.T) {
	h := newExchHarness(t)
	d, cred := h.seedPortal(nil)
	// A token issued 250 s ago with the default 300 s maximum age: its grant ends at iat + 300, 50 s from now,
	// not 300 s from now, and the bearer stops working there.
	iat := time.Now().Add(-250 * time.Second)
	h.ver.subj.IssuedAt, h.ver.subj.GrantEnd = iat, iat.Add(300*time.Second)
	tok, out := portalToken(t, h.exchange(t, d.ID, cred, "subject-token", nil))
	if in := int(out["expires_in"].(float64)); in < 45 || in > 50 {
		t.Fatalf("expires_in = %d, want about 50", in)
	}
	g, _ := h.ps.GetPortalGrantByRaw(context.Background(), tok, time.Now())
	if g.ExpiresAt.After(iat.Add(300 * time.Second).Add(time.Millisecond)) {
		t.Fatalf("the grant ends at %v, after iat + max_age %v", g.ExpiresAt, iat.Add(300*time.Second))
	}
	if w := do(t, h.srv, http.MethodGet, "/api/v1/me", tok, ""); w.Code != http.StatusOK {
		t.Fatalf("before the cutoff: %d, want 200", w.Code)
	}
	h.srv.cfg.Now = func() time.Time { return time.Now().Add(55 * time.Second) }
	if w := do(t, h.srv, http.MethodGet, "/api/v1/me", tok, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("after iat + max_age: %d %s, want the single 401", w.Code, w.Body.String())
	}
}

func TestPortalExchange_IssuerOutageColdAndWarm(t *testing.T) {
	h := newExchHarness(t)
	d, cred := h.seedPortal(nil)
	for _, c := range []struct {
		name  string
		cause string
		retry time.Duration
		want  string
	}{
		{"cold: discovery down at the first request", "issuer_unreachable", 0, ""},
		{"warm: a never-seen kid inside the cooldown", "keys_cooldown", 20 * time.Second, "20"},
		{"warm: the key set is down", "keys_unreachable", 0, ""},
		{"warm: a refused address", "address_refused", 0, ""},
	} {
		h.ver.refusal = oidc.PortalRefusal{Reason: oidc.PortalIssuerUnavailable, Cause: c.cause, Class: oidc.PortalRefusedUnavailable, RetryAfter: c.retry}
		w := h.exchange(t, d.ID, cred, "subject-token", nil)
		if w.Code != http.StatusServiceUnavailable || oauthCode(w) != "temporarily_unavailable" || w.Header().Get("Retry-After") != c.want {
			t.Errorf("%s: %d %s Retry-After=%q, want 503 temporarily_unavailable with %q", c.name, w.Code, w.Body.String(), w.Header().Get("Retry-After"), c.want)
		}
	}
	rows := h.exchangeRows()
	if len(rows) != 4 || portalRowData(t, rows[1])["cause"] != "keys_cooldown" || portalRowData(t, rows[1])["reason"] != oidc.PortalIssuerUnavailable {
		t.Fatalf("outage rows = %+v, want four, each naming reason and coarse cause", rows)
	}
	if len(h.ps.idents) != 0 || len(h.ps.grants) != 0 {
		t.Fatal("an outage stored an identity or a grant")
	}
	// Recovery needs nothing from the person: the next exchange is admitted.
	h.ver.refusal = oidc.PortalRefusal{}
	portalToken(t, h.exchange(t, d.ID, cred, "subject-token", nil))
}

func TestPortalExchange_ProxiedRefreshOutageStatusAndAudit(t *testing.T) {
	h := newExchHarness(t)
	d, cred := h.seedPortal(nil)
	causes := []struct {
		cause string
		retry time.Duration
		want  string
	}{
		{"unreachable", 30 * time.Second, "30"},
		{"keys_unreachable", 0, ""},
		{"issuer_unreachable", 20 * time.Second, "20"},
	}
	for _, c := range causes {
		h.ver.refusal = oidc.PortalRefusal{Reason: oidc.PortalIssuerUnavailable, Cause: c.cause, Class: oidc.PortalRefusedUnavailable, RetryAfter: c.retry}
		w := h.exchange(t, d.ID, cred, "subject-token", nil)
		if w.Code != http.StatusServiceUnavailable || oauthCode(w) != "temporarily_unavailable" || w.Header().Get("Retry-After") != c.want {
			t.Errorf("%s: %d %s Retry-After=%q, want 503 temporarily_unavailable with %q", c.cause, w.Code, w.Body.String(), w.Header().Get("Retry-After"), c.want)
		}
	}
	// The audit rows are checked on their own: each denied exchange names the reason and its own cause.
	rows := h.exchangeRows()
	if len(rows) != len(causes) {
		t.Fatalf("%d exchange rows, want %d", len(rows), len(causes))
	}
	for i, c := range causes {
		data := portalRowData(t, rows[i])
		if data["reason"] != oidc.PortalIssuerUnavailable || data["cause"] != c.cause {
			t.Errorf("row %d = %v, want reason %s and cause %s", i, data, oidc.PortalIssuerUnavailable, c.cause)
		}
	}
}

func TestPortalRefreshFailures_VerifierSourceExposed(t *testing.T) {
	counter := &PortalRefreshFailures{}
	issuer := "https://login.corp.example/oauth2/a"
	counter.Record(oidc.PortalRefreshFailure{Issuer: issuer, Cause: "address_refused", Source: "verifier"})
	var b strings.Builder
	counter.write(&b)
	key := oidc.PortalIssuerKey(issuer)
	if !strings.Contains(b.String(), `wardyn_portal_issuer_refresh_failures_total{issuer_key="`+key+`",portal="",source="verifier",cause="address_refused"} 1`) {
		t.Fatalf("a verifier refusal is not exposed with source=\"verifier\":\n%s", b.String())
	}
	help, _, _ := strings.Cut(b.String(), "\n")
	for _, source := range []string{"scheduled", "unknown_kid", "verifier"} {
		if !strings.HasPrefix(help, "# HELP ") || !strings.Contains(help, source) {
			t.Errorf("the HELP line %q does not name the source %s", help, source)
		}
	}
}

func TestPortalAdmission_RefusalStatusesDistinct(t *testing.T) {
	h := newExchHarness(t)
	d, cred := h.seedPortal(nil)
	for _, c := range []struct {
		name   string
		reason string
		class  oidc.PortalRefusalClass
		status int
		code   string
	}{
		{"a stale token", oidc.SubjectTokenStale, oidc.PortalRefusedToken, 400, "invalid_grant"},
		{"a claim mismatch", oidc.SubjectTokenClaimMismatch, oidc.PortalRefusedAdmission, 403, "access_denied"},
		{"a key outage", oidc.PortalIssuerUnavailable, oidc.PortalRefusedUnavailable, 503, "temporarily_unavailable"},
		{"a declared type that does not match", oidc.SubjectTokenTypeMismatch, oidc.PortalRefusedRequest, 400, "invalid_request"},
		{"a registration that cannot be verified against", oidc.PortalRegistrationInvalid, oidc.PortalRefusedRequest, 400, "invalid_request"},
	} {
		h.ver.refusal = oidc.PortalRefusal{Reason: c.reason, Class: c.class}
		w := h.exchange(t, d.ID, cred, "subject-token", nil)
		if w.Code != c.status || oauthCode(w) != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.name, w.Code, w.Body.String(), c.status, c.code)
		}
	}
	if len(h.ps.grants) != 0 {
		t.Fatal("a refusal minted")
	}
}

func TestPortalAdmission_NoGroupNeedsPortalManagedFlag(t *testing.T) {
	// The registration half (422 delegate_group_or_portal_managed) is the registration lane's; the exchange
	// half is here: a portal-managed portal with no group admits a verified person on the token checks alone,
	// the verifier is handed no group, and a token the verifier refuses (another client id) is 400.
	h := newExchHarness(t)
	d, cred := h.seedPortal(func(d *types.Delegate) { d.Group, d.PortalManagedAdmission = "", true })
	portalToken(t, h.exchange(t, d.ID, cred, "subject-token", nil))
	if got := h.ver.asks[0].reg; got.Group != "" || got.RequiredClaim != "" {
		t.Errorf("a no-group portal handed the verifier %+v", got)
	}
	h.ver.refusal = oidc.PortalRefusal{Reason: oidc.SubjectTokenCIDMismatch, Class: oidc.PortalRefusedToken}
	if w := h.exchange(t, d.ID, cred, "subject-token", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("another cid: %d, want 400", w.Code)
	}
}

func TestPortalExchange_RefreshFailureCountsMetric(t *testing.T) {
	counter := &PortalRefreshFailures{}
	opts, err := portalVerifierOptionsForTest(counter)
	if err != nil {
		t.Fatal(err)
	}
	p, err := oidc.NewPortalIssuers(opts)
	if err != nil {
		t.Fatal(err)
	}
	// A registration whose issuer is a loopback address: the verifier refuses to dial it, which is a failed
	// refresh. The token only has to parse as an RS256 JWS; no key is ever looked at.
	reg := oidc.PortalRegistration{Issuer: "https://127.0.0.1:1", TokenProfile: oidc.PortalProfileAccessToken, ClientID: "c", Audience: "a", RequiredScopes: []string{"s"}}
	jwt := "eyJhbGciOiJSUzI1NiIsImtpZCI6ImsifQ.e30.c2ln"
	_, refusal := p.VerifyDetail(context.Background(), reg, jwt, oidc.PortalTokenTypeAccess)
	if refusal.Class != oidc.PortalRefusedUnavailable {
		t.Fatalf("an unreachable issuer: %+v, want an unavailable refusal", refusal)
	}
	var b strings.Builder
	counter.write(&b)
	key := oidc.PortalIssuerKey(reg.Issuer)
	if !strings.Contains(b.String(), `wardyn_portal_issuer_refresh_failures_total{issuer_key="`+key+`",portal="",source="scheduled",cause="`) {
		t.Fatalf("a failed refresh left the metric without a series for the issuer:\n%s", b.String())
	}
	// And it renders on the scrape.
	h := newExchHarness(t, func(c *Config) { c.PortalRefreshFailures = counter })
	if !strings.Contains(string(h.srv.MetricsText(context.Background())), "wardyn_portal_issuer_refresh_failures_total{issuer_key=") {
		t.Fatal("/metrics does not render the portal refresh-failure counter")
	}
}

// portalVerifierOptionsForTest are the production options with the counter wired as cmd/wardynd wires it.
func portalVerifierOptionsForTest(counter *PortalRefreshFailures) (oidc.PortalIssuersOptions, error) {
	return oidc.PortalIssuersOptions{Reserved: PortalReservedCheck(""), OnRefreshFailure: counter.Record}, nil
}
