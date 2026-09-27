// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

// reservedVariants are subjects that must never act as a person: the exact
// reserved names, their case, whitespace and Unicode-fold variants (U+212A
// KELVIN SIGN lowercases to k), a local seat other than the configured one,
// and a device.
var reservedVariants = []string{
	"admin-token", "Admin-Token", " ADMIN-TOKEN ", "admin-toKen",
	"local:operator", "LOCAL:alice", "ops-seat", "Ops-Seat",
	"device:laptop", "Device:0f0e",
}

// TestIsReservedPrincipal pins the set, its folds, and that a person's
// subject — including ones that merely contain a reserved name — is not in it.
func TestIsReservedPrincipal(t *testing.T) {
	srv := &Server{cfg: Config{LocalOperator: "ops-seat"}}
	for _, p := range reservedVariants {
		if !srv.isReservedPrincipal(p) {
			t.Errorf("isReservedPrincipal(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"", "alice@example.com", "sub-123", "admin-token-2", "x-admin-token",
		"devices:laptop", "localhost", "ops-seat-2"} {
		if srv.isReservedPrincipal(p) {
			t.Errorf("isReservedPrincipal(%q) = true, want false", p)
		}
	}
	if (&Server{}).isReservedPrincipal("ops-seat") {
		t.Error("an unconfigured local operator reserved a name")
	}
}

// authFailReasons returns the reason of every auth.fail row by actor.
func authFailReasons(events []types.AuditEvent, actor string) []string {
	var out []string
	for _, ev := range events {
		if ev.Action != "auth.fail" || ev.Actor != actor {
			continue
		}
		var d struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		out = append(out, d.Reason)
	}
	return out
}

// TestReservedSessionRefusedAtRequestTime: a session cookie minted before the
// callback refused reserved subjects — admin role included — authenticates
// nothing, and each refusal is an auth.fail row; a person's session still
// works.
func TestReservedSessionRefusedAtRequestTime(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, rbacStore{})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.LocalOperator = "ops-seat"
	srv := New(cfg)
	for _, sub := range reservedVariants {
		w := doSSO(t, srv, http.MethodGet, "/api/v1/me", ssoSession(t, sub, "x@corp.example", oidc.RoleAdmin), "")
		if w.Code != http.StatusUnauthorized {
			t.Errorf("session sub %q: GET /me = %d, want 401", sub, w.Code)
		}
	}
	if got := authFailReasons(h.audit.snapshot(), adminAuthActor); len(got) == 0 || got[0] != authFailedReservedPrincipal {
		t.Errorf("auth.fail reasons = %v, want %s rows", got, authFailedReservedPrincipal)
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/me", ssoSession(t, "sub-person", "p@corp.example", oidc.RoleUser), ""); w.Code != http.StatusOK {
		t.Errorf("control: a person's session = %d, want 200", w.Code)
	}
}

// TestReservedTokenRefusedAtRequestTime: a wdn_ token row whose principal is
// reserved (minted before the callback refused one) is refused with a 401 and
// an auth.fail row, not replayed as that identity; a person's token works.
func TestReservedTokenRefusedAtRequestTime(t *testing.T) {
	srv, st, h := apiTokenTestServer(t)
	seed := func(principal, raw string) {
		t.Helper()
		if _, err := st.CreateAPIToken(context.Background(), types.APIToken{
			ID: uuid.New(), Principal: principal, Role: oidc.RoleAdmin, GroupsTruncated: new(bool),
		}, raw); err != nil {
			t.Fatalf("seed token: %v", err)
		}
	}
	seed("Admin-Token", apiTokenPrefix+"impostor")
	seed(tokenMemberSub, apiTokenPrefix+"person")

	w := do(t, srv, http.MethodGet, "/api/v1/me", apiTokenPrefix+"impostor", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("reserved token: GET /me = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	if got := authFailReasons(h.audit.snapshot(), adminAuthActor); !slices.Equal(got, []string{authFailedReservedPrincipal}) {
		t.Errorf("auth.fail reasons = %v, want [%s]", got, authFailedReservedPrincipal)
	}
	if w := do(t, srv, http.MethodGet, "/api/v1/me", apiTokenPrefix+"person", ""); w.Code != http.StatusOK {
		t.Errorf("control: a person's token = %d, want 200", w.Code)
	}
}

// TestSignInCallbackRouteRefusesReservedSubject drives the mounted
// /auth/callback against a fake identity provider whose subject is a case
// variant of the admin token: the login is refused with the generic
// auth_error, no session is issued, and the refusal is an auth.fail row from
// the callback's boundary. Proves the route passes isReservedPrincipal.
func TestSignInCallbackRouteRefusesReservedSubject(t *testing.T) {
	fake := entrafake.New()
	t.Cleanup(fake.Close)
	redirect := "http://console.example.invalid/auth/callback"
	fake.SetRedirectURI(redirect)
	fake.SetSubject("ADMIN-TOKEN")
	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL: fake.Issuer(), ClientID: fake.ClientID(), RedirectURL: redirect, DefaultRole: oidc.RoleUser,
	}, testLoginHMACKey)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}
	h := newHarness(t)
	cfg := baseTestConfig(h, rbacStore{})
	cfg.OIDC = auth
	handler := New(cfg).Handler()

	lw := httptest.NewRecorder()
	handler.ServeHTTP(lw, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if lw.Code != http.StatusFound {
		t.Fatalf("/auth/login = %d", lw.Code)
	}
	q := follow(t, lw.Header().Get("Location"))
	cb := httptest.NewRequest(http.MethodGet, "/auth/callback?"+url.Values{"state": {q.Get("state")}, "code": {q.Get("code")}}.Encode(), nil)
	for _, c := range lw.Result().Cookies() {
		cb.AddCookie(c)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, cb)

	loc, _ := url.Parse(w.Header().Get("Location"))
	if w.Code != http.StatusFound || loc == nil || loc.Query().Get("auth_error") != "sign_in_refused" {
		t.Fatalf("callback = %d Location %q, want 302 with auth_error=sign_in_refused", w.Code, w.Header().Get("Location"))
	}
	if sessionIssued(w) {
		t.Error("a session was issued for a reserved subject")
	}
	if got := authFailReasons(h.audit.snapshot(), oidcCallbackActor); !slices.Equal(got, []string{authFailedReservedPrincipal}) {
		t.Errorf("auth.fail reasons = %v, want [%s]", got, authFailedReservedPrincipal)
	}
}
