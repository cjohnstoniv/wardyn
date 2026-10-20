// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// browser is a cookie jar in front of the authenticator's handlers: each request carries the cookies a
// browser would send to that path, and each response's Set-Cookie (clears included) is applied.
type browser struct {
	jar    *cookiejar.Jar
	origin string
}

func (b *browser) serve(h func(http.ResponseWriter, *http.Request), method, path, query string) *httptest.ResponseRecorder {
	u, _ := url.Parse(b.origin + path)
	u.RawQuery = query
	r := httptest.NewRequest(method, u.String(), nil)
	for _, c := range b.jar.Cookies(u) {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h(w, r)
	b.jar.SetCookies(u, w.Result().Cookies())
	return w
}

func (b *browser) cookie(path, name string) string {
	u, _ := url.Parse(b.origin + path)
	for _, c := range b.jar.Cookies(u) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// beginLogin starts a sign-in and returns the state the identity provider would send back and the
// nonce its ID token must carry.
func (b *browser) beginLogin(t *testing.T, auth *writoidc.Authenticator, base, prefix string) (state, nonce string) {
	t.Helper()
	w := b.serve(auth.LoginHandler, http.MethodGet, base+"/auth/login", "")
	loc, err := url.Parse(w.Header().Get("Location"))
	if w.Code != http.StatusFound || err != nil {
		t.Fatalf("login = %d %q", w.Code, w.Header().Get("Location"))
	}
	state, nonce = loc.Query().Get("state"), b.cookie(base+"/auth/callback", prefix+"wardyn_oidc_nonce")
	if state == "" || nonce == "" {
		t.Fatalf("login set no state or nonce")
	}
	return state, nonce
}

// TestSignOutEndsALoginInFlight: sign-out clears the sign-in state, nonce, PKCE and widened-login
// cookies, so a login started before it (a popup still at the identity provider) cannot complete
// after it, under both cookie postures and a base path. A fresh login afterwards still signs in.
func TestSignOutEndsALoginInFlight(t *testing.T) {
	for _, secure := range []bool{true, false} {
		for _, base := range []string{"", "/wardyn"} {
			t.Run("secure="+map[bool]string{true: "true", false: "false"}[secure]+"/base="+base, func(t *testing.T) {
				env := newIdPEnv(t)
				auth := env.newSecureAuth(t, base, secure)
				jar, _ := cookiejar.New(nil)
				b := &browser{jar: jar, origin: "https://console.example.com"}
				prefix := ""
				if secure {
					prefix = "__Host-"
				}
				cookiePath := writoidc.CookiePath(secure, base)
				state, nonce := b.beginLogin(t, auth, base, prefix)
				// A widened login leaves its marker beside the other three.
				u, _ := url.Parse(b.origin + cookiePath)
				jar.SetCookies(u, []*http.Cookie{{Name: prefix + "wardyn_oidc_widened", Value: "1", Path: cookiePath, Secure: secure}})

				b.serve(func(w http.ResponseWriter, r *http.Request) { _, _ = auth.SignOut(w, r) }, http.MethodPost, base+"/api/v1/auth/logout", "")
				for _, name := range []string{"wardyn_oidc_state", "wardyn_oidc_nonce", "wardyn_oidc_pkce", "wardyn_oidc_widened"} {
					if v := b.cookie(base+"/auth/callback", prefix+name); v != "" {
						t.Errorf("after sign-out the browser still holds %s%s", prefix, name)
					}
				}

				env.buildIDToken(t, "sub-late", "late@example.com", nonce, time.Now().Add(time.Hour))
				cb := b.serve(auth.CallbackHandler, http.MethodGet, base+"/auth/callback", "state="+state+"&code=testcode")
				if loc := cb.Header().Get("Location"); cb.Code != http.StatusFound || !strings.Contains(loc, "auth_error=sign_in_expired") {
					t.Fatalf("callback after sign-out = %d %q, want the sign_in_expired redirect", cb.Code, loc)
				}
				if v := b.cookie(base+"/", prefix+"wardyn_session"); v != "" {
					t.Fatal("a callback after sign-out created a session")
				}

				state, nonce = b.beginLogin(t, auth, base, prefix)
				env.buildIDToken(t, "sub-fresh", "fresh@example.com", nonce, time.Now().Add(time.Hour))
				cb = b.serve(auth.CallbackHandler, http.MethodGet, base+"/auth/callback", "state="+state+"&code=testcode")
				if cb.Code != http.StatusFound || b.cookie(base+"/", prefix+"wardyn_session") == "" {
					t.Fatalf("fresh login after sign-out = %d %q, want 302 and a session", cb.Code, cb.Body.String())
				}
			})
		}
	}
}
