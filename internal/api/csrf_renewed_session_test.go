// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

// TestCSRFGuard_RenewedSessionCookie drives the real router with session renewal on. A same-origin
// read the console marks active renews the session; the CSRF guard accepts the renewed cookie on a
// same-origin write and refuses it from a cross-site page or a sibling host; and a request from
// either of those never earns a renewal, so a forged request cannot extend a session either.
func TestCSRFGuard_RenewedSessionCookie(t *testing.T) {
	fake := entrafake.New()
	t.Cleanup(fake.Close)
	fake.SetRedirectURI(csrfRedirectURL)
	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL: fake.Issuer(), ClientID: fake.ClientID(), RedirectURL: csrfRedirectURL, SessionRenewCap: 8 * time.Hour,
	}, testLoginHMACKey)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}
	h := newHarness(t)
	cfg := baseTestConfig(h, rbacStore{})
	cfg.OIDC = auth
	cfg.OIDCRedirectURL = csrfRedirectURL
	handler := panicFails(t, New(cfg).Handler())
	serve := func(method, path, origin, site string, c *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Host = csrfOIDCHost
		r.RemoteAddr = "127.0.0.1:54321"
		r.Header.Set("Origin", origin)
		r.Header.Set("Sec-Fetch-Site", site)
		r.Header.Set("X-Wardyn-Activity", "1")
		r.AddCookie(c)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	self := "http://" + csrfOIDCHost

	old := keyedSessionCookie(t, 50*time.Minute, 10*time.Minute)
	w := serve(http.MethodGet, "/api/v1/me", self, "same-origin", old)
	renewed := sessionCookieSet(w)
	if w.Code != http.StatusOK || renewed == nil {
		t.Fatalf("active same-origin read: %d, renewed cookie %v", w.Code, renewed)
	}
	if !renewed.HttpOnly || renewed.SameSite != http.SameSiteLaxMode || renewed.Path != "/" {
		t.Errorf("the renewed cookie lost the session cookie's posture: %+v", renewed)
	}

	for _, tc := range []struct{ name, origin, site string }{
		{"cross-site page", "https://evil.example", "cross-site"},
		{"sibling host", "https://sibling.wardyn.example", "same-site"},
	} {
		for _, c := range []*http.Cookie{renewed, old} {
			w := serve(http.MethodPost, "/api/v1/auth/logout", tc.origin, tc.site, c)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), csrfRefusedBody) {
				t.Errorf("%s write: %d %q, want the CSRF 403", tc.name, w.Code, w.Body.String())
			}
			if sessionCookieSet(w) != nil {
				t.Errorf("%s write earned a renewal", tc.name)
			}
		}
		if sessionCookieSet(serve(http.MethodGet, "/api/v1/me", tc.origin, tc.site, old)) != nil {
			t.Errorf("%s read carrying the activity header earned a renewal", tc.name)
		}
	}
	if w := serve(http.MethodPost, "/api/v1/auth/logout", self, "same-origin", renewed); w.Code != http.StatusFound {
		t.Errorf("same-origin write with the renewed cookie: %d %q, want the sign-out's 302", w.Code, w.Body.String())
	}
}

// keyedSessionCookie is a session signed with testLoginHMACKey, issued issuedAgo ago and ending in
// expiresIn: past half its span when expiresIn is the smaller, so console activity renews it.
func keyedSessionCookie(t *testing.T, issuedAgo, expiresIn time.Duration) *http.Cookie {
	t.Helper()
	now := time.Now().UTC()
	payload, err := json.Marshal(oidc.Session{
		V: oidc.SessionCodecVersion, Sub: "sso-human", Email: "human@corp.example", Role: oidc.RoleAdmin,
		UserType: types.UserTypeStandard, IssuedAt: now.Add(-issuedAgo), Expiry: now.Add(expiresIn),
	})
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, testLoginHMACKey)
	mac.Write(payload)
	return &http.Cookie{
		Name:  "wardyn_session",
		Value: base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)),
	}
}

// sessionCookieSet is the session cookie a response sets with a value, or nil.
func sessionCookieSet(w *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == "wardyn_session" && c.Value != "" {
			return c
		}
	}
	return nil
}
