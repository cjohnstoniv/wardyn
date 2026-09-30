// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	gooidc "github.com/coreos/go-oidc/v3/oidc"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// newAuthRequireEmailVerified is newAuth with Config.RequireEmailVerified set
// and no domain allowlist — the knob alone, which is what the tests pin.
func (e *idpEnv) newAuthRequireEmailVerified(t *testing.T, require bool) *writoidc.Authenticator {
	t.Helper()
	rt := &rewriteTokenRT{
		base:          http.DefaultTransport,
		originalToken: e.httpSrv.URL + "/token",
		replacedToken: e.tokenSrv.URL + "/",
	}
	ctx := gooidc.ClientContext(context.Background(), &http.Client{Transport: rt})
	auth, err := writoidc.New(ctx, writoidc.Config{
		IssuerURL:            e.httpSrv.URL,
		ClientID:             e.clientID,
		ClientSecret:         "secret",
		RedirectURL:          "http://localhost/auth/callback",
		RequireEmailVerified: require,
	}, testHMACKey)
	if err != nil {
		t.Fatalf("writoidc.New: %v", err)
	}
	return auth
}

// TestRequireEmailVerified pins WARDYN_OIDC_REQUIRE_EMAIL_VERIFIED: off (the
// default) never looks at the claim; on, an absent claim is UNVERIFIED and
// refused (email_verified_absent), false is refused (email_unverified), and
// true signs in — with no domain allowlist configured.
func TestRequireEmailVerified(t *testing.T) {
	cases := []struct {
		name      string
		require   bool
		claim     interface{} // nil = key present as null; omit = key absent
		omit      bool
		wantError string // "" = signs in
	}{
		{name: "off, absent signs in", require: false, omit: true},
		{name: "off, false signs in", require: false, claim: false},
		{name: "off, true signs in", require: false, claim: true},
		{name: "on, absent refused", require: true, omit: true, wantError: "email_verified_absent"},
		{name: "on, null refused", require: true, claim: nil, wantError: "email_verified_absent"},
		{name: "on, false refused", require: true, claim: false, wantError: "email_unverified"},
		{name: "on, true signs in", require: true, claim: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newIdPEnv(t)
			auth := env.newAuthRequireEmailVerified(t, c.require)
			if c.omit {
				env.buildIDTokenOmitting(t, "sub-rev", "alice@anywhere.example", "email_verified")
			} else {
				env.buildIDTokenRawClaim(t, "sub-rev", "alice@anywhere.example", "email_verified", c.claim)
			}
			var denied []string
			w, sess := doCallbackVia(t, auth, auth.CallbackHandlerWithDenials(nil, func(_ *http.Request, reason string) {
				denied = append(denied, reason)
			}))
			if w.Code != http.StatusFound {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusFound, w.Body.String())
			}
			loc := w.Result().Header.Get("Location")
			if c.wantError == "" {
				if strings.Contains(loc, "auth_error=") || sess.Sub != "sub-rev" {
					t.Errorf("want a session for sub-rev; Location=%q sess.Sub=%q", loc, sess.Sub)
				}
				return
			}
			if !strings.Contains(loc, "auth_error="+c.wantError) {
				t.Errorf("Location = %q, want auth_error=%s", loc, c.wantError)
			}
			if sess.Sub != "" {
				t.Errorf("sess.Sub = %q, want no session", sess.Sub)
			}
			// Audited the way the domain-allowlist refusal is: no onDenied
			// event, a server-side log line naming the env var (below).
			if len(denied) != 0 {
				t.Errorf("onDenied reasons = %v, want none (the domain refusal raises none)", denied)
			}
			assertSessionCookieCleared(t, w)
		})
	}
}

// TestRequireEmailVerifiedAbsentIsLogged: the absent-claim refusal is logged
// server-side naming WARDYN_OIDC_REQUIRE_EMAIL_VERIFIED, as the domain path
// names WARDYN_OIDC_EMAIL_DOMAINS.
func TestRequireEmailVerifiedAbsentIsLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	env := newIdPEnv(t)
	auth := env.newAuthRequireEmailVerified(t, true)
	env.buildIDTokenOmitting(t, "sub-log", "alice@anywhere.example", "email_verified")
	doCallback(t, auth)
	if out := buf.String(); !strings.Contains(out, "WARDYN_OIDC_REQUIRE_EMAIL_VERIFIED") || !strings.Contains(out, "no email_verified claim") {
		t.Errorf("log = %q, want the absent-claim denial naming WARDYN_OIDC_REQUIRE_EMAIL_VERIFIED", out)
	}
}

// TestRequireEmailVerifiedFalseIsLogged: the email_verified=false refusal is
// logged too, naming the setting that enforced the gate — with and without a
// domain allowlist — so ENV.md's "logged server-side" holds for both denials.
func TestRequireEmailVerifiedFalseIsLogged(t *testing.T) {
	cases := []struct {
		name    string
		domains []string
		wantEnv string
	}{
		{"knob only", nil, "WARDYN_OIDC_REQUIRE_EMAIL_VERIFIED"},
		{"domains", []string{"corp.example"}, "WARDYN_OIDC_EMAIL_DOMAINS"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			env := newIdPEnv(t)
			auth := env.newAuthRequireEmailVerified(t, true)
			if c.domains != nil {
				auth = env.newAuth(t, c.domains)
			}
			env.buildIDTokenRawClaim(t, "sub-false-log", "alice@corp.example", "email_verified", false)
			doCallback(t, auth)
			if out := buf.String(); !strings.Contains(out, "email_verified=false") || !strings.Contains(out, c.wantEnv) {
				t.Errorf("log = %q, want the false-claim denial naming %s", out, c.wantEnv)
			}
		})
	}
}
