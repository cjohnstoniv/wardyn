// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
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
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, rbacStore{})
	cfg.OIDC = &oidc.Authenticator{}
	fake := &fakeSessionRevocations{}
	cfg.SessionRevocations = fake
	return New(cfg), fake
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
	cfg := baseTestConfig(h, rbacStore{})
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
