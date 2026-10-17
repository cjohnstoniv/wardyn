// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

func lifeAuth(t *testing.T, set func(*writoidc.Config)) (*idpEnv, *writoidc.Authenticator) {
	t.Helper()
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)
	if set != nil {
		writoidc.ConfigureSessionLifeForTest(auth, set)
	}
	return env, auth
}

// lifeCookie is a session issued issuedAgo ago that ends in expiresIn, stamped with an authority
// epoch and revocation sequence so a test can see they survive a re-mint.
func lifeCookie(t *testing.T, auth *writoidc.Authenticator, issuedAgo, expiresIn time.Duration, lastActive int64) *http.Cookie {
	t.Helper()
	now := time.Now().UTC()
	c, err := writoidc.EncodeSessionForTest(auth, writoidc.Session{
		Sub: "sub-life", Email: "life@example.com", Role: writoidc.RoleUser, UserType: "standard",
		IssuedAt: now.Add(-issuedAgo), Expiry: now.Add(expiresIn), AuthorityEpoch: 2, RevocationSeq: 3,
		LastActive: lastActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// lifeRequest sends one request through Middleware. site is the browser's Sec-Fetch-Site ("" sends none).
func lifeRequest(auth *writoidc.Authenticator, c *http.Cookie, method, site string) (w *httptest.ResponseRecorder, live bool, reason string) {
	r := httptest.NewRequest(method, "/api/v1/runs", nil)
	r.AddCookie(c)
	if site != "" {
		r.Header.Set("Sec-Fetch-Site", site)
	}
	w = httptest.NewRecorder()
	auth.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		live = writoidc.PrincipalFromContext(r.Context()) != ""
		reason = writoidc.SessionRejectedFromContext(r.Context())
	})).ServeHTTP(w, r)
	return w, live, reason
}

func reminted(t *testing.T, auth *writoidc.Authenticator, w *httptest.ResponseRecorder) (writoidc.Session, bool) {
	t.Helper()
	c := cookieMap(w.Result().Cookies())["wardyn_session"]
	if c == nil || c.Value == "" {
		return writoidc.Session{}, false
	}
	sess, err := writoidc.DecodeSessionForTest(auth, c)
	if err != nil {
		t.Fatalf("re-minted cookie does not decode: %v", err)
	}
	return sess, true
}

// loginQuery runs LoginHandler with the given cookies and returns the authorization request's query.
func loginQuery(t *testing.T, auth *writoidc.Authenticator, cookies ...*http.Cookie) (url.Values, map[string]*http.Cookie) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	auth.LoginHandler(w, r)
	loc, err := url.Parse(w.Result().Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return loc.Query(), cookieMap(w.Result().Cookies())
}

func signIn(t *testing.T, env *idpEnv, auth *writoidc.Authenticator) map[string]*http.Cookie {
	t.Helper()
	env.buildIDTokenRawClaim(t, "sub-life", "life@example.com", "preferred_username", "life@corp.example")
	w, _ := doCallbackVia(t, auth, auth.CallbackHandler)
	got := cookieMap(w.Result().Cookies())
	if got["wardyn_session"] == nil {
		t.Fatalf("sign-in issued no session: %q", w.Result().Header.Get("Location"))
	}
	return got
}

// With every behaviour off, nothing new reaches the wire: activity re-mints no cookie, an idle-old
// session stays live, the login asks with neither prompt nor login_hint even when the markers are
// present, a sign-in sets no marker, and the session cookie keeps its Expires.
func TestSessionLifeOffChangesNothing(t *testing.T) {
	env, auth := lifeAuth(t, nil)
	w, live, _ := lifeRequest(auth, lifeCookie(t, auth, 50*time.Minute, 5*time.Minute, 0), http.MethodPost, "same-origin")
	if !live || len(w.Result().Cookies()) != 0 {
		t.Fatalf("off: live=%v, Set-Cookie=%v", live, w.Result().Header.Values("Set-Cookie"))
	}
	q, set := loginQuery(t, auth, &http.Cookie{Name: "wardyn_oidc_reauth", Value: "1"},
		&http.Cookie{Name: "wardyn_oidc_hint", Value: base64.RawURLEncoding.EncodeToString([]byte("x@example.com"))})
	if q.Has("prompt") || q.Has("login_hint") || set["wardyn_oidc_silent"] != nil {
		t.Fatalf("off: login asked %v and set %v", q, set)
	}
	got := signIn(t, env, auth)
	if got["wardyn_oidc_reauth"] != nil || got["wardyn_oidc_hint"] != nil {
		t.Fatalf("off: sign-in set a marker: %v", got)
	}
	if got["wardyn_session"].RawExpires == "" {
		t.Fatal("off: the session cookie lost its Expires")
	}
}

func TestSessionRenewCap(t *testing.T) {
	t.Run("activity past half the span renews by the span, keeping every stamp", func(t *testing.T) {
		_, auth := lifeAuth(t, func(c *writoidc.Config) { c.SessionRenewCap = 8 * time.Hour })
		w, live, _ := lifeRequest(auth, lifeCookie(t, auth, 50*time.Minute, 10*time.Minute, 0), http.MethodPost, "same-origin")
		sess, ok := reminted(t, auth, w)
		if !live || !ok {
			t.Fatalf("live=%v reminted=%v", live, ok)
		}
		if d := time.Until(sess.Expiry); d < 59*time.Minute || d > time.Hour {
			t.Errorf("renewed expiry in %s, want about 1h", d)
		}
		if sess.RenewSpan != 3600 || sess.AuthorityEpoch != 2 || sess.RevocationSeq != 3 || time.Since(sess.IssuedAt) < 49*time.Minute {
			t.Errorf("stamps moved: span=%d epoch=%d seq=%d iat=%s", sess.RenewSpan, sess.AuthorityEpoch, sess.RevocationSeq, sess.IssuedAt)
		}
	})
	t.Run("never past the cap or the role-stamp deadline", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			roleTTL  time.Duration
			deadline time.Duration // after sign-in
		}{{"cap", 0, 8 * time.Hour}, {"role stamp", 7*time.Hour + 55*time.Minute, 7*time.Hour + 55*time.Minute}} {
			_, auth := lifeAuth(t, func(c *writoidc.Config) { c.SessionRenewCap = 8 * time.Hour; c.RoleStampTTL = tc.roleTTL })
			issued := 7*time.Hour + 50*time.Minute
			w, _, _ := lifeRequest(auth, lifeCookie(t, auth, issued, 2*time.Minute, 0), http.MethodPost, "same-origin")
			sess, ok := reminted(t, auth, w)
			if !ok {
				t.Fatalf("%s: not renewed", tc.name)
			}
			if d := sess.Expiry.Sub(sess.IssuedAt); d != tc.deadline {
				t.Errorf("%s: renewed to %s after sign-in, want %s", tc.name, d, tc.deadline)
			}
		}
	})
	t.Run("reads, cross-site writes and young sessions are not renewed", func(t *testing.T) {
		_, auth := lifeAuth(t, func(c *writoidc.Config) { c.SessionRenewCap = 8 * time.Hour })
		for _, tc := range []struct {
			name, method, site string
			issued, left       time.Duration
		}{
			{"read", http.MethodGet, "same-origin", 50 * time.Minute, 10 * time.Minute},
			{"cross-site write", http.MethodPost, "cross-site", 50 * time.Minute, 10 * time.Minute},
			{"no fetch metadata", http.MethodPost, "", 50 * time.Minute, 10 * time.Minute},
			{"under half its span", http.MethodPost, "same-origin", 20 * time.Minute, 40 * time.Minute},
		} {
			w, live, _ := lifeRequest(auth, lifeCookie(t, auth, tc.issued, tc.left, 0), tc.method, tc.site)
			if _, ok := reminted(t, auth, w); ok || !live {
				t.Errorf("%s: reminted=%v live=%v", tc.name, ok, live)
			}
		}
	})
}

func TestSessionIdleTimeout(t *testing.T) {
	_, auth := lifeAuth(t, func(c *writoidc.Config) { c.SessionIdleTimeout = 30 * time.Minute })
	recent := time.Now().Add(-5 * time.Minute).Unix()
	for _, tc := range []struct {
		name       string
		issued     time.Duration
		lastActive int64
		wantLive   bool
	}{
		{"idle since sign-in", 40 * time.Minute, 0, false},
		{"active recently", 40 * time.Minute, recent, true},
		{"young", 10 * time.Minute, 0, true},
	} {
		w, live, reason := lifeRequest(auth, lifeCookie(t, auth, tc.issued, time.Hour, tc.lastActive), http.MethodGet, "same-origin")
		if live != tc.wantLive {
			t.Errorf("%s: live=%v reason=%q", tc.name, live, reason)
		}
		if !tc.wantLive {
			c := cookieMap(w.Result().Cookies())["wardyn_session"]
			if reason != "expired_session" || c == nil || c.MaxAge >= 0 {
				t.Errorf("%s: reason=%q, cookie not cleared: %v", tc.name, reason, c)
			}
		}
	}
	w, _, _ := lifeRequest(auth, lifeCookie(t, auth, 10*time.Minute, time.Hour, 0), http.MethodPost, "same-origin")
	if sess, ok := reminted(t, auth, w); !ok || time.Now().Unix()-sess.LastActive > 2 {
		t.Errorf("a write did not reset the idle clock: reminted=%v last=%d", ok, sess.LastActive)
	}
	for _, tc := range []struct {
		name, method string
		issued       time.Duration
	}{{"read", http.MethodGet, 10 * time.Minute}, {"write within a tenth", http.MethodPost, time.Minute}} {
		w, _, _ := lifeRequest(auth, lifeCookie(t, auth, tc.issued, time.Hour, 0), tc.method, "same-origin")
		if _, ok := reminted(t, auth, w); ok {
			t.Errorf("%s re-stamped the idle clock", tc.name)
		}
	}
}

func TestSilentReauth(t *testing.T) {
	env, auth := lifeAuth(t, func(c *writoidc.Config) { c.SilentReauth = true })
	marker := &http.Cookie{Name: "wardyn_oidc_reauth", Value: "1"}

	if q, set := loginQuery(t, auth); q.Has("prompt") || set["wardyn_oidc_silent"] != nil {
		t.Fatalf("a browser that never signed in asked silently: %v", q)
	}
	q, set := loginQuery(t, auth, marker)
	if q.Get("prompt") != "none" || set["wardyn_oidc_silent"] == nil {
		t.Fatalf("returning browser: prompt=%q silent=%v", q.Get("prompt"), set["wardyn_oidc_silent"])
	}

	// The provider refuses: the same window continues once with the interactive sign-in.
	r := httptest.NewRequest(http.MethodGet, "/auth/callback?state=s1&error=login_required", nil)
	for _, c := range []*http.Cookie{{Name: "wardyn_oidc_state", Value: "s1"}, {Name: "wardyn_oidc_nonce", Value: "n"},
		{Name: "wardyn_oidc_pkce", Value: "p"}, {Name: "wardyn_oidc_silent", Value: "1"}, marker} {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	auth.CallbackHandler(w, r)
	loc, _ := url.Parse(w.Result().Header.Get("Location"))
	if w.Code != http.StatusFound || loc.Query().Get("state") == "" || loc.Query().Has("prompt") {
		t.Fatalf("refused silent sign-in did not fall back to the interactive one: %d %q", w.Code, loc)
	}
	if c := cookieMap(w.Result().Cookies())["wardyn_oidc_silent"]; c == nil || c.MaxAge >= 0 {
		t.Errorf("the silent marker was not expired: %v", c)
	}

	got := signIn(t, env, auth)
	if c := got["wardyn_oidc_reauth"]; c == nil || c.MaxAge != 30*24*3600 || !c.HttpOnly {
		t.Fatalf("a completed sign-in did not set the reauth marker: %v", c)
	}
	if c := cookieMap(signOutWith(auth, marker))["wardyn_oidc_reauth"]; c == nil || c.MaxAge >= 0 {
		t.Errorf("sign-out did not clear the reauth marker: %v", c)
	}
}

// signOutWith signs out a browser carrying one cookie and returns the cookies the answer sets.
func signOutWith(auth *writoidc.Authenticator, extra *http.Cookie) []*http.Cookie {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	r.AddCookie(extra)
	w := httptest.NewRecorder()
	auth.LogoutHandler(w, r)
	return w.Result().Cookies()
}

func TestLoginHint(t *testing.T) {
	env, auth := lifeAuth(t, func(c *writoidc.Config) { c.LoginHint = true })
	got := signIn(t, env, auth)
	hint := got["wardyn_oidc_hint"]
	if hint == nil || hint.MaxAge != 30*24*3600 || !hint.HttpOnly {
		t.Fatalf("a completed sign-in did not set the hint: %v", hint)
	}
	if q, _ := loginQuery(t, auth, hint); q.Get("login_hint") != "life@corp.example" {
		t.Errorf("login_hint = %q, want the preferred_username", q.Get("login_hint"))
	}
	if q, _ := loginQuery(t, auth, &http.Cookie{Name: "wardyn_oidc_hint", Value: "%%not-base64"}); q.Has("login_hint") {
		t.Errorf("an unreadable hint was sent: %v", q)
	}
	if c := cookieMap(signOutWith(auth, hint))["wardyn_oidc_hint"]; c == nil || c.MaxAge >= 0 {
		t.Errorf("sign-out did not clear the hint: %v", c)
	}
}

func TestBrowserSessionCookie(t *testing.T) {
	env, auth := lifeAuth(t, func(c *writoidc.Config) { c.BrowserSessionCookie = true })
	c := signIn(t, env, auth)["wardyn_session"]
	if c.RawExpires != "" || c.MaxAge != 0 {
		t.Fatalf("session cookie still persistent: expires=%q max-age=%d", c.RawExpires, c.MaxAge)
	}
	if _, live, _ := lifeRequest(auth, c, http.MethodGet, ""); !live {
		t.Error("the browser-session cookie does not authenticate")
	}
	if _, live, _ := lifeRequest(auth, lifeCookie(t, auth, 2*time.Hour, -time.Minute, 0), http.MethodGet, ""); live {
		t.Error("the server stopped enforcing the session's expiry")
	}
}
