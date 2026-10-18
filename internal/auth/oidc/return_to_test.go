// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// /auth/login?return_to= is remembered only for a console path; everything that could name another
// origin or split a header is dropped, and the login still starts.
func TestLoginReturnToIsDefaultDeny(t *testing.T) {
	long := "/" + strings.Repeat("a", 600)
	// The dot-segment cases are the ones a browser resolves onto a refused route; under a base path
	// they climb out of it and back in, so they are tried with and without one.
	for _, base := range []string{"", "/wardyn"} {
		_, auth := lifeAuth(t, func(c *writoidc.Config) { c.BasePath = base })
		for _, bad := range []string{"", "https://evil.example/", "//evil.example", "/\\evil.example", "/runs\\x", "evil", "/runs\r\nSet-Cookie: x=1", "/runs\tx",
			"/auth/login", "/api/v1/runs", "/api", "/x/../api/v1/runs", "/runs/../auth/login", "/x/%2e%2e/api/v1/runs",
			"/../wardyn/api/v1/runs", "/%2e%2e/wardyn/api/v1/runs", "/../wardyn/auth/login", "/%2e%2e/wardyn/auth/login", "/runs/./x", long} {
			r := httptest.NewRequest(http.MethodGet, "/auth/login?return_to="+url.QueryEscape(bad), nil)
			w := httptest.NewRecorder()
			auth.LoginHandler(w, r)
			if w.Code != http.StatusFound || cookieMap(w.Result().Cookies())["wardyn_oidc_return"] != nil {
				t.Errorf("base %q, return_to %q: %d, cookie %v; want a login with nothing remembered", base, bad, w.Code, cookieMap(w.Result().Cookies())["wardyn_oidc_return"])
			}
		}
	}
}

// An approved sign-in lands on the remembered page; a login without one clears an earlier attempt's,
// and a planted cookie that fails the check lands on the root.
func TestCallbackReturnsToTheRequestedPage(t *testing.T) {
	env, auth := lifeAuth(t, nil)
	env.buildIDTokenRawClaim(t, "sub-life", "life@example.com", "preferred_username", "life@corp.example")

	r := httptest.NewRequest(http.MethodGet, "/auth/login?return_to="+url.QueryEscape("/runs/abc?tab=logs"), nil)
	w := httptest.NewRecorder()
	auth.LoginHandler(w, r)
	remembered := cookieMap(w.Result().Cookies())["wardyn_oidc_return"]
	if remembered == nil || !remembered.HttpOnly {
		t.Fatalf("login remembered %v, want an HttpOnly return cookie", remembered)
	}

	callbackWith := func(c *http.Cookie) *httptest.ResponseRecorder {
		w, _ := doCallbackVia(t, auth, func(w http.ResponseWriter, r *http.Request) {
			if c != nil {
				r.AddCookie(c)
			}
			auth.CallbackHandler(w, r)
		})
		return w
	}
	got := callbackWith(remembered)
	if loc := got.Result().Header.Get("Location"); loc != "/runs/abc?tab=logs" {
		t.Errorf("approved sign-in went to %q, want the requested page", loc)
	}
	if c := cookieMap(got.Result().Cookies())["wardyn_oidc_return"]; c == nil || c.MaxAge >= 0 {
		t.Errorf("the return cookie was not spent: %v", c)
	}
	if loc := callbackWith(nil).Result().Header.Get("Location"); loc != "/" {
		t.Errorf("sign-in with no remembered page went to %q, want /", loc)
	}
	planted := &http.Cookie{Name: "wardyn_oidc_return", Value: base64.RawURLEncoding.EncodeToString([]byte("//evil.example"))}
	if loc := callbackWith(planted).Result().Header.Get("Location"); loc != "/" {
		t.Errorf("a planted off-origin return went to %q, want /", loc)
	}

	// A later login with no return_to must not inherit the earlier one.
	r = httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	r.AddCookie(remembered)
	w = httptest.NewRecorder()
	auth.LoginHandler(w, r)
	if c := cookieMap(w.Result().Cookies())["wardyn_oidc_return"]; c == nil || c.MaxAge >= 0 {
		t.Errorf("a login without return_to left the earlier one: %v", c)
	}
}
