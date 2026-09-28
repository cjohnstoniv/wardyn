// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// extra_scopes_test.go pins WARDYN_OIDC_EXTRA_SCOPES (#1101): default leaves
// the fixed "openid profile email" request untouched, a scope the provider's
// own discovery document advertises is appended, and one it does not refuses
// BOOT rather than surfacing invalid_scope at every human's login.
package oidc_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// authScopeParam builds an Authenticator against issuer with the given extra
// scopes and returns the "scope" query param LoginHandler's authorization
// redirect actually carries.
func authScopeParam(t *testing.T, issuer string, extraScopes []string) string {
	t.Helper()
	auth, err := writoidc.New(context.Background(), writoidc.Config{
		IssuerURL:   issuer,
		ClientID:    "wardyn-client",
		RedirectURL: "http://wardyn.example/auth/callback",
		ExtraScopes: extraScopes,
	}, testHMACKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := httptest.NewRecorder()
	auth.LoginHandler(w, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	loc, err := url.Parse(w.Result().Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse authorization redirect: %v", err)
	}
	return loc.Query().Get("scope")
}

// TestExtraScopes_DefaultUnchanged: an empty (default) ExtraScopes leaves the
// authorization request byte-identical to before the field existed.
func TestExtraScopes_DefaultUnchanged(t *testing.T) {
	srv := newScopeIdP(t, []string{"openid", "profile", "email", "groups"})
	got := authScopeParam(t, srv.URL, nil)
	if want := "openid profile email"; got != want {
		t.Fatalf("scope = %q, want %q — an unset WARDYN_OIDC_EXTRA_SCOPES must not change the request", got, want)
	}
}

// TestExtraScopes_SupportedIncluded: a configured scope the provider's own
// discovery document advertises is appended to the authorization request.
func TestExtraScopes_SupportedIncluded(t *testing.T) {
	srv := newScopeIdP(t, []string{"openid", "profile", "email", "groups"})
	got := authScopeParam(t, srv.URL, []string{"groups"})
	if want := "openid profile email groups"; got != want {
		t.Fatalf("scope = %q, want %q — a supported configured scope must reach the authorization request", got, want)
	}
}

// TestExtraScopes_UnsupportedRefusesBoot: a configured scope the provider does
// NOT advertise refuses BOOT (New returns an error naming it), rather than
// silently asking and failing every human's login with invalid_scope.
//
// Counterfactual: delete the validateExtraScopes call from New and this test
// goes red — New succeeds, and the unsupported scope reaches the wire instead.
func TestExtraScopes_UnsupportedRefusesBoot(t *testing.T) {
	srv := newScopeIdP(t, []string{"openid", "profile", "email"})
	_, err := writoidc.New(context.Background(), writoidc.Config{
		IssuerURL:   srv.URL,
		ClientID:    "wardyn-client",
		RedirectURL: "http://wardyn.example/auth/callback",
		ExtraScopes: []string{"no-such-scope"},
	}, testHMACKey)
	if err == nil {
		t.Fatal("New succeeded, want a refusal naming the unadvertised scope")
	}
	if !strings.Contains(err.Error(), "no-such-scope") {
		t.Errorf("error = %v, want it to name the offending scope", err)
	}
}

// TestExtraScopes_GroupsConfigured_SilencesBootWarning: configuring `groups`
// via WARDYN_OIDC_EXTRA_SCOPES silences warnUnrequestedGroupsScope even though
// the merged role map is keyed on a value only a claim can answer — the scope
// is now actually being asked for, so there is nothing left to warn about.
func TestExtraScopes_GroupsConfigured_SilencesBootWarning(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	srv := newScopeIdP(t, []string{"openid", "profile", "email", "groups"})
	_, err := writoidc.New(context.Background(), writoidc.Config{
		IssuerURL:   srv.URL,
		ClientID:    "wardyn-client",
		RedirectURL: "http://wardyn.example/auth/callback",
		RoleMap:     map[string]string{"eng-team": writoidc.RoleUser},
		ExtraScopes: []string{"groups"},
	}, testHMACKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if logs := buf.String(); strings.Contains(logs, "advertises a `groups` scope that Wardyn does not request") {
		t.Errorf("boot warning fired with `groups` configured — it is being requested, so nothing is missing\nlogs:\n%s", logs)
	}
}

// TestExtraScopes_NoScopesSupportedWarnsAndAllows: a discovery document with no
// scopes_supported at all (an OPTIONAL field) is not proof a configured scope
// is unsupported, so boot proceeds — the scope is requested unchecked rather
// than refusing every deployment whose IdP simply omits an optional list.
func TestExtraScopes_NoScopesSupportedWarnsAndAllows(t *testing.T) {
	srv := newScopeIdP(t, nil)
	got := authScopeParam(t, srv.URL, []string{"groups"})
	if want := "openid profile email groups"; got != want {
		t.Fatalf("scope = %q, want %q — an absent scopes_supported must not refuse boot or drop the configured scope", got, want)
	}
}
