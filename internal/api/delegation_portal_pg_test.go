// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Hosted-only: the issuer-portal exchange against the real store. The verifier is the in-memory fake, since
// the verifier refuses the loopback issuer a test can serve (its own tests drive the real one); everything
// from the exchange's first store read to the grant row is real.

type exchPG struct {
	*delegationPG
	ver *fakePortalVerifier
}

// storedRecorder writes each event to the shared audit table, as the production chain does, so an owed
// introduction can settle, and keeps a copy in mem for the assertions.
type storedRecorder struct {
	pool *pgxpool.Pool
	mem  *recRecorder
}

func (s storedRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	if err := store.InsertAuditEvent(ctx, s.pool, &ev); err != nil {
		return err
	}
	return s.mem.Record(ctx, ev)
}

func newExchPG(t *testing.T) *exchPG {
	t.Helper()
	e := &exchPG{delegationPG: newDelegationPG(t), ver: &fakePortalVerifier{enforceAge: true}}
	e.h.srv.cfg.PortalIssuers = e.ver
	e.h.srv.cfg.Audit = storedRecorder{pool: e.pool, mem: e.h.audit}
	e.ver.subj = e.person("u1", "")
	return e
}

// person is a verified person of the test issuer, issued 10 s ago.
func (e *exchPG) person(uid, sub string) oidc.PortalSubject {
	return e.personAt(portalTestIssuer, uid, sub)
}

func (e *exchPG) personAt(issuer, uid, sub string) oidc.PortalSubject {
	if sub == "" {
		sub = "sub-of-" + uid
	}
	now := time.Now()
	return oidc.PortalSubject{
		Principal: oidc.PortalPrincipal(issuer, uid), Issuer: issuer, K: oidc.PortalIssuerKey(issuer), PersonID: uid, Sub: sub,
		IssuedAt: now.Add(-10 * time.Second), Expiry: now.Add(time.Hour),
	}
}

// portal binds issuer and registers a portal on it through the store, as a held registration's decision does.
func (e *exchPG) portal(t *testing.T, issuer string, mut func(*types.Delegate)) (types.Delegate, string) {
	t.Helper()
	ctx := context.Background()
	b, err := e.st.BindPortalIssuerQ(ctx, e.pool, issuer, "", "uid", "test")
	if err != nil {
		t.Fatalf("bind %s: %v", issuer, err)
	}
	d := types.Delegate{
		ID: uuid.New(), Name: "portal-" + uuid.NewString(), IdPClientID: uuid.NewString(), RegisteredBy: "test",
		Issuer: issuer, InternalIssuer: b.InternalIssuerNorm, IssuerKey: b.IssuerKey, TokenProfile: oidc.PortalProfileAccessToken,
		Audience: "api://wardyn", RequiredScopes: []string{"wardyn.portal"}, UserType: types.UserTypeStandard,
		PortalManagedAdmission: true, MaxSubjectTokenAgeSeconds: 300, GrantTTLSeconds: 300,
	}
	if mut != nil {
		mut(&d)
	}
	cred := "wdi_" + uuid.NewString()
	sum := sha256.Sum256([]byte(cred))
	d, err = e.st.CreateDelegateQ(ctx, e.pool, d, hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatalf("register a portal on %s: %v", issuer, err)
	}
	return d, cred
}

func (e *exchPG) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// token is a successful exchange's bearer.
func (e *exchPG) token(t *testing.T, d types.Delegate, cred string) string {
	t.Helper()
	tok, _ := portalToken(t, e.exchange(t, d.ID, cred, "subject-token"))
	return tok
}

func TestPortalExchange_EndToEnd_PG(t *testing.T) {
	e := newExchPG(t)
	d, cred := e.portal(t, portalTestIssuer, nil)
	tok := e.token(t, d, cred)
	if n := e.count(t, `SELECT count(*) FROM portal_grants WHERE delegate_id = $1 AND principal = $2`, d.ID, e.ver.subj.Principal); n != 1 {
		t.Fatalf("portal_grants rows for the person = %d, want 1", n)
	}
	if n := e.delegatedTokenCount(t); n != 0 {
		t.Fatalf("delegated_tokens rows = %d, an issuer portal writes only portal_grants", n)
	}
	w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", tok, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), e.ver.subj.Principal) {
		t.Fatalf("GET /me with the grant = %d %s, want the portal person", w.Code, w.Body.String())
	}
	// The audit trail names the exact issuer, the portal and the person.
	var ex *types.AuditEvent
	for _, ev := range e.rows("delegation.exchange") {
		ev := ev
		ex = &ev
	}
	if ex == nil || ex.Actor != delegateActor(d.ID) || ex.Target != e.ver.subj.Principal || ex.Outcome != "success" {
		t.Fatalf("delegation.exchange rows = %+v", e.rows("delegation.exchange"))
	}
	intro := e.rows("person.create")
	if len(intro) != 1 || intro[0].Actor != delegateActor(d.ID) || intro[0].Target != e.ver.subj.Principal || !strings.Contains(string(intro[0].Data), portalTestIssuer) {
		t.Fatalf("person.create rows = %+v, want one under the portal and the real issuer", intro)
	}
	// A portal revoked ends its grants.
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/admin/delegates/"+d.ID.String(), e.admin, ""); w.Code >= 300 {
		t.Fatalf("revoke the portal: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", tok, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("GET /me after the portal was revoked = %d, want 401", w.Code)
	}
}

func TestPortalExchange_IDTokenProfileEndToEnd_PG(t *testing.T) {
	e := newExchPG(t)
	d, cred := e.portal(t, portalTestIssuer, func(d *types.Delegate) {
		d.TokenProfile, d.Audience, d.RequiredScopes, d.EmailDomains = oidc.PortalProfileIDToken, "", nil, []string{"corp.example"}
	})
	e.ver.subj.Email, e.ver.subj.EmailVerified = "pat@corp.example", true
	tok := e.token(t, d, cred)
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", tok, ""); w.Code != http.StatusOK {
		t.Fatalf("GET /me = %d %s", w.Code, w.Body.String())
	}
	var display string
	if err := e.pool.QueryRow(context.Background(), `SELECT email FROM principal_identities WHERE principal = $1`, e.ver.subj.Principal).Scan(&display); err != nil {
		t.Fatalf("read the identity: %v", err)
	}
	// A token past the maximum age is refused, and nothing more is minted.
	n := e.count(t, `SELECT count(*) FROM portal_grants`)
	e.ver.subj.IssuedAt = time.Now().Add(-301 * time.Second)
	w := e.exchange(t, d.ID, cred, "subject-token")
	if w.Code != http.StatusBadRequest || e.count(t, `SELECT count(*) FROM portal_grants`) != n {
		t.Fatalf("a token past the maximum age: %d %s (grants %d), want 400 and none minted", w.Code, w.Body.String(), n)
	}
	_ = display
}

func TestPortalExchange_GroupsAndEmailStoredEmpty_PG(t *testing.T) {
	e := newExchPG(t)
	d, cred := e.portal(t, portalTestIssuer, nil)
	e.ver.subj.Groups, e.ver.subj.Email = []string{"portal-users", "admins"}, "pat@corp.example"
	e.token(t, d, cred)
	var email, groups string
	if err := e.pool.QueryRow(context.Background(), `SELECT email, groups::text FROM principal_groups WHERE principal = $1 AND portal = $2`,
		e.ver.subj.Principal, d.ID.String()).Scan(&email, &groups); err != nil {
		t.Fatalf("read the person's record under the portal: %v", err)
	}
	if email != "" || strings.ReplaceAll(groups, " ", "") != "[]" {
		t.Fatalf("stored email %q groups %s, want both empty", email, groups)
	}
	if n := e.count(t, `SELECT count(*) FROM principal_groups WHERE principal = $1 AND portal = ''`, e.ver.subj.Principal); n != 0 {
		t.Fatalf("the portal person has %d console records", n)
	}
}

func TestPortalExchange_HostileConsoleConfigStillUser_PG(t *testing.T) {
	e := newExchPG(t)
	// The console's setup matches the person on every rule that could grant a role: a default role of admin and
	// a role map naming the person's group and email.
	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL: e.iss, ClientID: "wardyn-client", ClientSecret: "secret", RedirectURL: "http://localhost/auth/callback",
		RoleMap:     map[string]string{"portal-users": oidc.RoleAdmin, "pat@corp.example": oidc.RoleSecurityAdmin},
		DefaultRole: oidc.RoleAdmin,
	}, accessTestHMACKey)
	if err != nil {
		t.Fatal(err)
	}
	e.h.srv.cfg.OIDC = auth
	d, cred := e.portal(t, portalTestIssuer, nil)
	e.ver.subj.Groups, e.ver.subj.Email = []string{"portal-users"}, "pat@corp.example"
	tok := e.token(t, d, cred)
	w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", tok, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"role":"user"`) {
		t.Fatalf("GET /me = %d %s, want the user role", w.Code, w.Body.String())
	}
	for _, p := range []string{"/api/v1/admin/delegates", "/api/v1/admin/user-types"} {
		if w := do(t, e.h.srv, http.MethodGet, p, tok, ""); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "delegation_scope") {
			t.Errorf("GET %s = %d %s, want 403 delegation_scope", p, w.Code, w.Body.String())
		}
	}
}

func TestPortalExchange_IntroductionCrashBetweenCommitAndAudit_PG(t *testing.T) {
	e := newExchPG(t)
	a, credA := e.portal(t, portalTestIssuer, nil)
	b, credB := e.portal(t, portalTestIssuer, nil)
	// The audit write of the introduction fails once, as a crash between the identity commit and its audit.
	chain, _, _ := introChainOn(t, e.h.audit, e.h.srv, "person.create")
	_ = chain
	e.token(t, a, credA)
	if len(e.rows("person.create")) != 0 {
		t.Fatal("the failed write left a person.create row")
	}
	if n := e.count(t, `SELECT count(*) FROM principal_identities WHERE principal = $1 AND introduction_audited_at IS NULL`, e.ver.subj.Principal); n != 1 {
		t.Fatalf("owed introductions = %d, want 1", n)
	}
	e.token(t, b, credB)
	rows := e.rows("person.create")
	if len(rows) != 1 || rows[0].Actor != delegateActor(a.ID) || rows[0].Target != e.ver.subj.Principal || !strings.Contains(string(rows[0].Data), portalTestIssuer) {
		t.Fatalf("person.create after the later exchange = %+v, want the original portal, the real issuer and the person", rows)
	}
	if n := e.count(t, `SELECT count(*) FROM principal_identities WHERE principal = $1 AND introduction_audited_at IS NULL`, e.ver.subj.Principal); n != 0 {
		t.Fatalf("owed introductions after the replay = %d, want 0", n)
	}
}

// introChainOn makes the first audit write of failOnce fail on srv, for the tests that run on a store; the
// others go through the server's recorder, which records into rec.
func introChainOn(t *testing.T, rec *recRecorder, srv *Server, failOnce string) (*failOnceRecorder, *Server, error) {
	t.Helper()
	f := &failOnceRecorder{inner: srv.cfg.Audit, action: failOnce}
	srv.cfg.Audit = f
	return f, srv, nil
}

type failOnceRecorder struct {
	inner  audit.Recorder
	action string
	failed bool
}

func (f *failOnceRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	if ev.Action == f.action && !f.failed {
		f.failed = true
		return errors.New("audit write failed")
	}
	return f.inner.Record(ctx, ev)
}

func TestPortalFreshness_RepeatExchangeNeverPastCutoff_PG(t *testing.T) {
	e := newExchPG(t)
	d, cred := e.portal(t, portalTestIssuer, nil)
	// One last valid token, exchanged again and again near its cutoff: iat + max_age is 40 s away.
	iat := time.Now().Add(-260 * time.Second)
	e.ver.subj.IssuedAt = iat
	cutoff := iat.Add(300 * time.Second)
	for i := 0; i < 3; i++ {
		tok := e.token(t, d, cred)
		var ends time.Time
		if err := e.pool.QueryRow(context.Background(), `SELECT expires_at FROM portal_grants WHERE principal = $1 ORDER BY created_at DESC LIMIT 1`, e.ver.subj.Principal).Scan(&ends); err != nil {
			t.Fatal(err)
		}
		if ends.After(cutoff.Add(time.Millisecond)) {
			t.Fatalf("exchange %d: the grant ends at %v, after iat + max_age %v", i, ends, cutoff)
		}
		_ = tok
	}
	// With under five seconds left the exchange is refused and writes no grant; after the cutoff, too.
	n := e.count(t, `SELECT count(*) FROM portal_grants`)
	e.ver.subj.IssuedAt = time.Now().Add(-297 * time.Second)
	if w := e.exchange(t, d.ID, cred, "subject-token"); w.Code != http.StatusBadRequest {
		t.Fatalf("an exchange with under 5 s left = %d %s, want 400", w.Code, w.Body.String())
	}
	e.ver.subj.IssuedAt = time.Now().Add(-301 * time.Second)
	if w := e.exchange(t, d.ID, cred, "subject-token"); w.Code != http.StatusBadRequest {
		t.Fatalf("an exchange past the cutoff = %d %s, want 400", w.Code, w.Body.String())
	}
	if e.count(t, `SELECT count(*) FROM portal_grants`) != n {
		t.Fatal("a refused exchange wrote a grant")
	}
}

func TestPortalFreshness_TwoPortalsDifferentMaxima_PG(t *testing.T) {
	e := newExchPG(t)
	short, credS := e.portal(t, portalTestIssuer, nil)
	long, credL := e.portal(t, portalTestIssuer, func(d *types.Delegate) { d.MaxSubjectTokenAgeSeconds, d.GrantTTLSeconds = 600, 600 })
	// The same person with a token issued 400 s ago: the 300 s portal refuses, the 600 s portal admits, and the
	// grant of the second ends at its own cutoff, 200 s on.
	e.ver.subj.IssuedAt = time.Now().Add(-400 * time.Second)
	if w := e.exchange(t, short.ID, credS, "subject-token"); w.Code != http.StatusBadRequest {
		t.Fatalf("the 300 s portal at 400 s = %d %s, want 400", w.Code, w.Body.String())
	}
	_, out := portalToken(t, e.exchange(t, long.ID, credL, "subject-token"))
	if in := int(out["expires_in"].(float64)); in < 190 || in > 200 {
		t.Fatalf("the 600 s portal's expires_in = %d, want about 200", in)
	}
}

func TestPortalProfile_SameUidChangedSubSamePerson_PG(t *testing.T) {
	e := newExchPG(t)
	d, cred := e.portal(t, portalTestIssuer, nil)
	e.ver.subj = e.person("uid-1", "login-before")
	e.token(t, d, cred)
	e.ver.subj = e.person("uid-1", "login-after")
	e.token(t, d, cred)
	if n := e.count(t, `SELECT count(*) FROM principal_identities WHERE principal = $1`, e.ver.subj.Principal); n != 1 {
		t.Fatalf("identity rows for one uid under two subs = %d, want 1", n)
	}
	if n := e.count(t, `SELECT count(DISTINCT principal) FROM portal_grants WHERE delegate_id = $1`, d.ID); n != 1 {
		t.Fatalf("grants name %d principals, want one person", n)
	}
	if len(e.rows("person.create")) != 1 {
		t.Fatalf("person.create rows = %d, want one introduction", len(e.rows("person.create")))
	}
}

func TestPortalPrincipal_CaseVariantGrantsRestrictionsErasure_PG(t *testing.T) {
	e := newExchPG(t)
	d, cred := e.portal(t, portalTestIssuer, nil)
	// Two ids that differ only in case are two people: separate principals, identities and grants, and the
	// purge of one leaves the other signed in.
	lo, up := e.person("abc", ""), e.person("ABC", "")
	if lo.Principal == up.Principal {
		t.Fatalf("ids differing in case share the principal %s", lo.Principal)
	}
	e.ver.subj = lo
	tokLo := e.token(t, d, cred)
	e.ver.subj = up
	tokUp := e.token(t, d, cred)
	if n := e.count(t, `SELECT count(*) FROM principal_identities WHERE principal IN ($1, $2)`, lo.Principal, up.Principal); n != 2 {
		t.Fatalf("identity rows = %d, want 2", n)
	}
	var idLo uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM principal_identities WHERE principal = $1`, lo.Principal).Scan(&idLo); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.MarkPortalIdentityPurged(context.Background(), idLo, 0); err != nil {
		t.Fatalf("purge the first person: %v", err)
	}
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", tokUp, ""); w.Code != http.StatusOK {
		t.Errorf("the other person after the purge = %d, want 200", w.Code)
	}
	e.ver.subj = lo
	if w := e.exchange(t, d.ID, cred, "subject-token"); w.Code != http.StatusBadRequest {
		t.Errorf("the purged person's next exchange = %d %s, want 400", w.Code, w.Body.String())
	}
	_ = tokLo
}

func TestPortalPrincipal_SameSubAsConsolePersonDistinct_PG(t *testing.T) {
	e := newExchPG(t)
	d, cred := e.portal(t, portalTestIssuer, nil)
	e.ver.subj = e.person("shared-sub", "shared-sub")
	tok := e.token(t, d, cred)
	if e.ver.subj.Principal == "shared-sub" {
		t.Fatal("a portal person is named like the console person of the same sub")
	}
	if n := e.count(t, `SELECT count(*) FROM principal_identities WHERE principal = 'shared-sub'`); n != 0 {
		t.Fatalf("the exchange wrote %d console identity rows", n)
	}
	w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", tok, "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"principal":"shared-sub"`) {
		t.Fatalf("GET /me = %d %s, want the portal person and not the console one", w.Code, w.Body.String())
	}
}

func TestPortalPrincipal_EmailCollisionNoMergeNoSharedCut_PG(t *testing.T) {
	e := newExchPG(t)
	const other = "https://idp.other.example/oauth2/default"
	a, credA := e.portal(t, portalTestIssuer, nil)
	b, credB := e.portal(t, other, nil)
	pa, pb := e.person("u1", ""), e.personAt(other, "u1", "")
	pa.Email, pb.Email = "pat@corp.example", "pat@corp.example"
	e.ver.subj = pa
	tokA := e.token(t, a, credA)
	e.ver.subj = pb
	tokB := e.token(t, b, credB)
	if pa.Principal == pb.Principal {
		t.Fatal("two issuers' people with one email and one id share a principal")
	}
	if n := e.count(t, `SELECT count(*) FROM principal_identities WHERE principal IN ($1, $2)`, pa.Principal, pb.Principal); n != 2 {
		t.Fatalf("identity rows = %d, want 2", n)
	}
	// A cut of the first person's sessions ends the first person's grant and not the second's.
	revs := e.h.srv.cfg.SessionRevocations.(*memRevocations)
	revs.mu.Lock()
	revs.cut[pa.Principal] = time.Now().Add(time.Minute)
	revs.mu.Unlock()
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", tokA, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("the cut person's grant = %d, want 401", w.Code)
	}
	if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", tokB, ""); w.Code != http.StatusOK {
		t.Errorf("the other person's grant = %d, want 200 (a shared email must not share a cut)", w.Code)
	}
}

// racePortalChange runs change while the exchange's verification is in flight, as a revoke or an admission
// change that committed after the exchange read its portal and before its writes.
func (e *exchPG) racePortalChange(change func()) {
	e.ver.inVerify = func() {
		e.ver.inVerify = nil
		change()
	}
}

// TestPortalExchange_AdmissionOrRevokeRacesWrite_PG covers the order in which the change commits first: the
// exchange's writes then refuse, nothing outlives it. The order in which the grant commits first needs a
// pause between the two commits, which only a store-level hook can make.
func TestPortalExchange_AdmissionOrRevokeRacesWrite_PG(t *testing.T) {
	t.Run("narrowing", func(t *testing.T) {
		e := newExchPG(t)
		d, cred := e.portal(t, portalTestIssuer, func(d *types.Delegate) { d.Group, d.PortalManagedAdmission = "", true })
		e.racePortalChange(func() {
			err := e.st.UpdateDelegateAdmissionQ(context.Background(), e.pool, d.ID,
				types.PortalAdmission{PortalManaged: true}, types.PortalAdmission{Group: "portal-users"})
			if err != nil {
				t.Errorf("narrow the admission: %v", err)
			}
		})
		w := e.exchange(t, d.ID, cred, "subject-token")
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "temporarily_unavailable") {
			t.Fatalf("an exchange over a narrowing = %d %s, want 503", w.Code, w.Body.String())
		}
		if n := e.count(t, `SELECT count(*) FROM portal_grants`) + e.count(t, `SELECT count(*) FROM principal_identities WHERE principal = $1`, e.ver.subj.Principal); n != 0 {
			t.Fatalf("a narrowed portal's exchange wrote %d rows", n)
		}
		// Retried under the new rule, with a person the verifier now admits, it succeeds.
		portalToken(t, e.exchange(t, d.ID, cred, "subject-token"))
	})
	t.Run("revoke before the first identity", func(t *testing.T) {
		e := newExchPG(t)
		d, cred := e.portal(t, portalTestIssuer, nil)
		e.racePortalChange(func() {
			if _, err := e.pool.Exec(context.Background(), `UPDATE delegates SET revoked_at = now() WHERE id = $1`, d.ID); err != nil {
				t.Errorf("revoke: %v", err)
			}
		})
		w := e.exchange(t, d.ID, cred, "subject-token")
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid_client") {
			t.Fatalf("an exchange over a revoke = %d %s, want 401 invalid_client", w.Code, w.Body.String())
		}
		if n := e.count(t, `SELECT count(*) FROM portal_grants`) + e.count(t, `SELECT count(*) FROM principal_identities WHERE principal = $1`, e.ver.subj.Principal); n != 0 {
			t.Fatalf("a revoked portal's exchange wrote %d rows", n)
		}
	})
}

func TestPortalAdmission_TwoPortalsOneIssuerDifferentRules_PG(t *testing.T) {
	e := newExchPG(t)
	rule := func(v string) func(*types.Delegate) {
		return func(d *types.Delegate) { d.RequiredClaim = &types.PortalRequiredClaim{Claim: "tenant", Value: v} }
	}
	a, credA := e.portal(t, portalTestIssuer, rule("a"))
	b, credB := e.portal(t, portalTestIssuer, rule("b"))
	e.ver.claims = map[string]string{"tenant": "a"}
	tokA := e.token(t, a, credA)
	if w := e.exchange(t, b.ID, credB, "subject-token"); w.Code != http.StatusForbidden || oauthCode(w) != "access_denied" {
		t.Fatalf("tenant a at portal b = %d %s, want 403 access_denied", w.Code, w.Body.String())
	}
	e.ver.claims = map[string]string{"tenant": "b"}
	tokB := e.token(t, b, credB)
	if w := e.exchange(t, a.ID, credA, "subject-token"); w.Code != http.StatusForbidden {
		t.Fatalf("tenant b at portal a = %d, want 403", w.Code)
	}
	// One person, two grants, each under the portal that admitted it.
	if n := e.count(t, `SELECT count(DISTINCT delegate_id) FROM portal_grants WHERE principal = $1`, e.ver.subj.Principal); n != 2 {
		t.Fatalf("grants of the person came from %d portals, want 2", n)
	}
	for name, tok := range map[string]string{"a": tokA, "b": tokB} {
		if w := do(t, e.h.srv, http.MethodGet, "/api/v1/me", tok, ""); w.Code != http.StatusOK {
			t.Errorf("portal %s's grant = %d", name, w.Code)
		}
	}
}
