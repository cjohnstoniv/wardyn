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

func (f *fakeSessionRevocations) IsSessionRevoked(context.Context, string, time.Time) (bool, error) {
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
		{ID: a1, Principal: "alice@corp.example"},
		{ID: a2, Principal: "alice@corp.example"},
		{ID: b1, Principal: "bob@corp.example"},
		{ID: ar, Principal: "alice@corp.example", RevokedAt: &gone}, // already revoked: untouched
	})
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", admin, `{"sub":"alice@corp.example"}`)
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
	srv, fake := sessionsTestServer(t)
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke", admin, `{"all":true}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusNoContent, w.Body.String())
	}
	if fake.revokedAll != 1 {
		t.Errorf("revokedAll = %d, want 1", fake.revokedAll)
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
