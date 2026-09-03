// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// fakeSessionRevocations is an in-memory oidc.SessionRevocations double for
// handleRevokeSessions's tests.
type fakeSessionRevocations struct {
	revokedSubs []string
	revokedAll  int
}

func (f *fakeSessionRevocations) IsSessionRevoked(context.Context, string, string, time.Time) (bool, error) {
	return false, nil
}
func (f *fakeSessionRevocations) RevokeSub(_ context.Context, sub string) error {
	f.revokedSubs = append(f.revokedSubs, sub)
	return nil
}
func (f *fakeSessionRevocations) RevokeAll(context.Context) error {
	f.revokedAll++
	return nil
}

var _ oidc.SessionRevocations = (*fakeSessionRevocations)(nil)

// sessionsTestServer builds a Server with OIDC + a fake SessionRevocations
// store wired, so POST /api/v1/sessions/revoke mounts (routes.go gates it on
// cfg.SessionRevocations != nil).
func sessionsTestServer(t *testing.T) (*Server, *fakeSessionRevocations) {
	srv, fake, _ := sessionsTestServerWithTokens(t, nil)
	return srv, fake
}

// sessionsTestServerWithTokens is sessionsTestServer with a store that
// records API-token revocations — handleRevokeSessions now revokes the
// target's tokens too, so even the plain tests need a store whose token
// methods are implemented (rbacStore's embedded nil store.Store would panic).
func sessionsTestServerWithTokens(t *testing.T, toks []types.APIToken) (*Server, *fakeSessionRevocations, *sessionTokenStore) {
	t.Helper()
	h := newHarness(t)
	st := &sessionTokenStore{toks: toks}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	fake := &fakeSessionRevocations{}
	cfg.SessionRevocations = fake
	return New(cfg), fake, st
}

// sessionTokenStore: rbacStore plus in-memory API-token list/revoke.
type sessionTokenStore struct {
	rbacStore
	toks    []types.APIToken
	revoked []uuid.UUID
}

func (s *sessionTokenStore) ListAPITokens(context.Context) ([]types.APIToken, error) {
	return s.toks, nil
}
func (s *sessionTokenStore) ListAPITokensByPrincipal(_ context.Context, p string) ([]types.APIToken, error) {
	var out []types.APIToken
	for _, t := range s.toks {
		if t.Principal == p {
			out = append(out, t)
		}
	}
	return out, nil
}
func (s *sessionTokenStore) RevokeAPIToken(_ context.Context, id uuid.UUID, _ string, now time.Time) (types.APIToken, error) {
	s.revoked = append(s.revoked, id)
	return types.APIToken{ID: id, RevokedAt: &now}, nil
}

// W-3: "revoke a human now" must cover their wdn_ tokens — apiTokenAuth never
// consults the session cutoff, so an unrevoked PAT would keep authenticating
// as the revoked human indefinitely.
func TestRevokeSessions_AlsoRevokesTokens(t *testing.T) {
	gone := time.Now().UTC()
	a1, a2, b1, ar := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	srv, _, st := sessionsTestServerWithTokens(t, []types.APIToken{
		{ID: a1, Principal: "sub-alice"},
		{ID: a2, Principal: "sub-alice"},
		{ID: b1, Principal: "sub-bob"},
		{ID: ar, Principal: "sub-alice", RevokedAt: &gone}, // already revoked: untouched
	})
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", admin, `{"sub":"sub-alice"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	if len(st.revoked) != 2 || !((st.revoked[0] == a1 && st.revoked[1] == a2) || (st.revoked[0] == a2 && st.revoked[1] == a1)) {
		t.Errorf("revoked = %v, want exactly alice's two live tokens {%s %s}", st.revoked, a1, a2)
	}
	for _, id := range st.revoked {
		if id == b1 || id == ar {
			t.Errorf("revoked %s — bob's token / an already-revoked token must be untouched", id)
		}
	}
}

func TestRevokeSessions_AdminRevokesSub(t *testing.T) {
	srv, fake := sessionsTestServer(t)
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", admin, `{"sub":"alice@corp.example"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusNoContent, w.Body.String())
	}
	if len(fake.revokedSubs) != 1 || fake.revokedSubs[0] != "alice@corp.example" {
		t.Errorf("revokedSubs = %v, want [alice@corp.example]", fake.revokedSubs)
	}
}

func TestRevokeSessions_AdminRevokesAll(t *testing.T) {
	gone := time.Now().UTC()
	x1, x2, xr := uuid.New(), uuid.New(), uuid.New()
	srv, fake, st := sessionsTestServerWithTokens(t, []types.APIToken{
		{ID: x1, Principal: "sub-alice"},
		{ID: x2, Principal: "sub-bob"},
		{ID: xr, Principal: "sub-alice", RevokedAt: &gone},
	})
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", admin, `{"all":true}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusNoContent, w.Body.String())
	}
	if fake.revokedAll != 1 {
		t.Errorf("revokedAll = %d, want 1", fake.revokedAll)
	}
	// The all arm is deployment-wide for tokens too — every LIVE token goes,
	// whoever holds it (the calling admin's own included); already-revoked
	// rows are untouched.
	if len(st.revoked) != 2 {
		t.Errorf("revoked = %v, want the two live tokens {%s %s}", st.revoked, x1, x2)
	}
	for _, id := range st.revoked {
		if id == xr {
			t.Error("re-revoked an already-revoked token")
		}
	}
}

func TestRevokeSessions_MemberForbidden(t *testing.T) {
	srv, fake := sessionsTestServer(t)
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", member, `{"sub":"alice@corp.example"}`)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if len(fake.revokedSubs) != 0 {
		t.Errorf("a member's request must never reach RevokeSub, got %v", fake.revokedSubs)
	}
}

func TestRevokeSessions_Unauthenticated(t *testing.T) {
	srv, _ := sessionsTestServer(t)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", nil, `{"sub":"alice@corp.example"}`)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestRevokeSessions_BothSubAndAllRejected(t *testing.T) {
	srv, fake := sessionsTestServer(t)
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", admin, `{"sub":"alice@corp.example","all":true}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if len(fake.revokedSubs) != 0 || fake.revokedAll != 0 {
		t.Error("an ambiguous body must not revoke anything")
	}
}

func TestRevokeSessions_NeitherSubNorAllRejected(t *testing.T) {
	srv, fake := sessionsTestServer(t)
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", admin, `{}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if len(fake.revokedSubs) != 0 || fake.revokedAll != 0 {
		t.Error("an empty body must not revoke anything")
	}
}

// TestRevokeSessions_NotMountedWithoutStore: with no SessionRevocations
// wired (mirrors an OIDC-off or misconfigured deployment), the route must not
// exist at all — a 404, not a panic on a nil store.
func TestRevokeSessions_NotMountedWithoutStore(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, rbacStore{})
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg) // SessionRevocations left nil

	w := do(t, srv, http.MethodPost, "/api/v1/sessions/revoke", adminToken, `{"all":true}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (route must not mount without a store)", w.Code, http.StatusNotFound)
	}
}

func TestRevokeSessions_AuditEmitted(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, &sessionTokenStore{})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.SessionRevocations = &fakeSessionRevocations{}
	srv := New(cfg)
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", admin, `{"sub":"alice@corp.example"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
	var found bool
	for _, ev := range h.audit.events {
		if ev.Action == "session.revoke" {
			found = true
		}
	}
	if !found {
		t.Error("no session.revoke audit event recorded")
	}
}

// ─── the SEC-over-SUPER direction (Requirement 3) ────────────────────────────

// TestSecurityAdminRevokesSuperAdmin pins the answer to "may a security_admin
// revoke a SUPER admin's sessions and tokens". YES — deliberately, and this
// test is what makes that a decision rather than an accident.
//
// It was previously unpinned in BOTH directions: TestSecurityAdminRouteTier
// (authz_test.go) probes this route with bodyFor("POST") == "{}", which 400s in
// handleRevokeSessions' default arm before any target is named, so it proves
// only that the router gate admits a security_admin. Every test in this file
// used an ADMIN caller. Nothing anywhere named a super admin as the TARGET, so
// adding a target-role guard would have reddened nothing.
//
// The reasoning is on handleRevokeSessions; the short form is that revocation
// only ever SUBTRACTS reach, incident response is this tier's job, and it is
// not a lockout — which the last two subtests pin, because they are what bound
// the blast radius.
func TestSecurityAdminRevokesSuperAdmin(t *testing.T) {
	const superSub = "sub-the-deployer"
	secAdmin := func(t *testing.T) *http.Cookie {
		return ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)
	}

	t.Run("by sub: the super admin's sessions AND tokens go", func(t *testing.T) {
		superTok, secTok := uuid.New(), uuid.New()
		srv, fake, st := sessionsTestServerWithTokens(t, []types.APIToken{
			{ID: superTok, Principal: superSub},
			{ID: secTok, Principal: secAdminSub},
		})
		w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", secAdmin(t),
			`{"sub":"`+superSub+`"}`)
		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204 — a security admin must be able to cut a compromised super-admin session; "+
				"if this is now a 403, the tier boundary changed and OPERATIONS.md's security-admin section says otherwise; body=%s",
				w.Code, w.Body.String())
		}
		if len(fake.revokedSubs) != 1 || fake.revokedSubs[0] != superSub {
			t.Errorf("revokedSubs = %v, want [%s]", fake.revokedSubs, superSub)
		}
		// The token half is the part that does NOT self-heal, so it is the part
		// worth naming: a wdn_ bearer authenticates as the human and never
		// consults the session cutoff.
		if len(st.revoked) != 1 || st.revoked[0] != superTok {
			t.Errorf("revoked tokens = %v, want exactly the super admin's %s (and not the caller's own %s)",
				st.revoked, superTok, secTok)
		}
	})

	t.Run("all: deployment-wide, super admin included", func(t *testing.T) {
		superTok, memberTok := uuid.New(), uuid.New()
		srv, fake, st := sessionsTestServerWithTokens(t, []types.APIToken{
			{ID: superTok, Principal: superSub},
			{ID: memberTok, Principal: "sub-member"},
		})
		w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", secAdmin(t), `{"all":true}`)
		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
		}
		if fake.revokedAll != 1 {
			t.Errorf("revokedAll = %d, want 1", fake.revokedAll)
		}
		if len(st.revoked) != 2 {
			t.Errorf("revoked = %v, want every live token in the deployment (%s, %s) — "+
				"this arm destroys CI/automation credentials too, which is why it is documented as an incident lever",
				st.revoked, superTok, memberTok)
		}
	})

	// THE BOUND, and the reason the direction above is acceptable: adminAuth
	// (http.go) never consults SessionRevocations, so the admin bearer — the
	// break-glass OPERATIONS.md promises — still authenticates after a
	// deployment-wide revoke. Without this, a security admin could revoke their
	// way into a position no super admin could undo.
	t.Run("the admin bearer break-glass survives a revoke-all", func(t *testing.T) {
		srv, _, _ := sessionsTestServerWithTokens(t, nil)
		if w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", secAdmin(t), `{"all":true}`); w.Code != http.StatusNoContent {
			t.Fatalf("revoke-all: status = %d; body=%s", w.Code, w.Body.String())
		}
		w := do(t, srv, http.MethodPost, "/api/v1/sessions/revoke", adminToken, `{"all":true}`)
		if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
			t.Fatalf("the admin bearer was refused (%d) after a revoke-all: the break-glass is gone and a security admin "+
				"can hold the deployment; body=%s", w.Code, w.Body.String())
		}
	})

	// The tier stops where the design says it stops: a MEMBER is still refused,
	// so this test cannot be read as "the route is simply open".
	t.Run("a member is still refused", func(t *testing.T) {
		srv, fake, _ := sessionsTestServerWithTokens(t, nil)
		member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember)
		if w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", member, `{"sub":"`+superSub+`"}`); w.Code != http.StatusForbidden {
			t.Errorf("member: status = %d, want 403", w.Code)
		}
		if len(fake.revokedSubs) != 0 {
			t.Errorf("a member's request reached RevokeSub: %v", fake.revokedSubs)
		}
	})
}

// ─── an email names the same human as their sub (F002) ───────────────────────

// TestRevokeSessions_EmailFormRevokesTheSameHuman: "revoke a human now" is the
// time-critical half of incident response, and it used to be keyed on the OIDC
// sub ALONE while both the CLI flag help and OPERATIONS.md advertised
// "sub/email". On any IdP where the two differ — Entra, whose sub is an opaque
// per-app identifier, the shape the SSO work targets — naming the email stamped
// a cutoff that matched nobody and swept no tokens, and the responder's only
// feedback was 204 plus an append-only outcome=success row.
//
// Both halves of one revoke have to agree about who was named, so both are
// asserted here: the cutoff key AND the token sweep.
//
// Counterfactual: drop the email fallback in revokeAPITokensFor and the token
// assertion fails while the cutoff one still passes — which is exactly how this
// shipped half-working.
func TestRevokeSessions_EmailFormRevokesTheSameHuman(t *testing.T) {
	const (
		aliceSub   = "sub-alice-opaque-entra-identifier"
		aliceMail  = "alice@corp.example"
		bystanderS = "sub-bob"
	)
	gone := time.Now().UTC()

	for _, tc := range []struct {
		name   string
		target string
	}{
		{"named by sub", aliceSub},
		{"named by email", aliceMail},
		// An admin types an address; the IdP's casing is not their problem.
		{"named by email, different case", "Alice@Corp.Example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a1, a2, ar, b1 := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			srv, fake, st := sessionsTestServerWithTokens(t, []types.APIToken{
				{ID: a1, Principal: aliceSub, Email: aliceMail},
				{ID: a2, Principal: aliceSub, Email: aliceMail},
				{ID: ar, Principal: aliceSub, Email: aliceMail, RevokedAt: &gone},
				{ID: b1, Principal: bystanderS, Email: "bob@corp.example"},
			})
			admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

			w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", admin, `{"sub":"`+tc.target+`"}`)
			if w.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
			}
			// Half 1: the cutoff is stamped under whatever was named —
			// IsSessionRevoked is what matches it back to the session.
			if len(fake.revokedSubs) != 1 || fake.revokedSubs[0] != tc.target {
				t.Errorf("revokedSubs = %v, want [%s]", fake.revokedSubs, tc.target)
			}
			// Half 2: the tokens. This is the half that does NOT self-heal —
			// a wdn_ bearer never consults the session cutoff and api_tokens
			// has no expiry, so a missed sweep leaves a live credential forever.
			if len(st.revoked) != 2 {
				t.Fatalf("revoked tokens = %v, want alice's two LIVE tokens (%s, %s) — "+
					"a revoke naming %q swept nothing, so her wdn_ tokens keep authenticating as her",
					st.revoked, a1, a2, tc.target)
			}
			for _, id := range st.revoked {
				if id == b1 {
					t.Errorf("revoked the bystander's token %s", id)
				}
				if id == ar {
					t.Errorf("re-revoked an already-revoked token %s", id)
				}
			}
		})
	}
}

// TestRevokeSessions_UnmatchedTargetSweepsNobodyElse is the bound on the email
// fallback: it must widen a revoke to the SAME human's other identity, never to
// anyone else. A target matching no principal and no email revokes zero tokens
// — not "all of them" via some empty-means-everyone slip, which is exactly the
// convention revokeAPITokensFor uses one branch away.
func TestRevokeSessions_UnmatchedTargetSweepsNobodyElse(t *testing.T) {
	x1, x2 := uuid.New(), uuid.New()
	srv, fake, st := sessionsTestServerWithTokens(t, []types.APIToken{
		{ID: x1, Principal: "sub-alice", Email: "alice@corp.example"},
		{ID: x2, Principal: "sub-bob", Email: "bob@corp.example"},
	})
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", admin, `{"sub":"nobody@corp.example"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if len(st.revoked) != 0 {
		t.Fatalf("revoked = %v, want none — an unmatched target must not sweep the deployment", st.revoked)
	}
	if len(fake.revokedSubs) != 1 || fake.revokedSubs[0] != "nobody@corp.example" {
		t.Errorf("revokedSubs = %v, want the cutoff still stamped (a sub with no live session is indistinguishable from a typo here)", fake.revokedSubs)
	}
}
