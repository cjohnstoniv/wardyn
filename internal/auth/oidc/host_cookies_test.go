// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// Under secure cookies every console cookie is a __Host- cookie (#1258): the
// browser stores one only when it is Secure, Path=/ and carries no Domain,
// so a sibling host under the console's registrable domain (a relayed
// sandbox page) cannot plant one. What it CAN plant is the plain name, which
// the console must then never read.

// newSecureAuth is newBasePathAuth with secure cookies on.
func (e *idpEnv) newSecureAuth(t *testing.T, base string, secure bool) *writoidc.Authenticator {
	t.Helper()
	rt := &rewriteTokenRT{base: http.DefaultTransport, originalToken: e.httpSrv.URL + "/token", replacedToken: e.tokenSrv.URL + "/"}
	ctx := gooidc.ClientContext(context.Background(), &http.Client{Transport: rt})
	auth, err := writoidc.New(ctx, writoidc.Config{
		IssuerURL: e.httpSrv.URL, ClientID: e.clientID, ClientSecret: "secret",
		RedirectURL:   "https://console.example.com" + base + "/auth/callback",
		BasePath:      base,
		SecureCookies: secure,
	}, testHMACKey)
	if err != nil {
		t.Fatalf("writoidc.New: %v", err)
	}
	return auth
}

// assertHostCookies fails unless every Set-Cookie is one a browser would
// store as a __Host- cookie: the prefix, Secure, Path=/, no Domain. A clear
// that misses any of them is ignored by the browser, so clears count too.
func assertHostCookies(t *testing.T, step string, cookies []*http.Cookie) {
	t.Helper()
	if len(cookies) == 0 {
		t.Fatalf("%s: set no cookies", step)
	}
	for _, c := range cookies {
		if !strings.HasPrefix(c.Name, "__Host-wardyn_") || !c.Secure || c.Path != "/" || c.Domain != "" {
			t.Errorf("%s: cookie %q Secure=%v Path=%q Domain=%q, want a __Host-wardyn_ name, Secure, Path=/ and no Domain",
				step, c.Name, c.Secure, c.Path, c.Domain)
		}
	}
}

// secureSignIn walks login and callback the way a browser does: it returns
// to the callback every cookie the login set, under the name it was set with.
func secureSignIn(t *testing.T, env *idpEnv, auth *writoidc.Authenticator, base string) *httptest.ResponseRecorder {
	t.Helper()
	login := httptest.NewRecorder()
	auth.LoginHandler(login, httptest.NewRequest(http.MethodGet, base+"/auth/login", nil))
	set := login.Result().Cookies()
	assertHostCookies(t, "login", set)
	jar := cookieMap(set)
	nonce, state := jar["__Host-wardyn_oidc_nonce"], jar["__Host-wardyn_oidc_state"]
	if nonce == nil || state == nil || jar["__Host-wardyn_oidc_pkce"] == nil {
		t.Fatalf("login set %v, want the __Host- state, nonce and pkce cookies", set)
	}
	env.buildIDToken(t, "sub-host", "host@example.com", nonce.Value, time.Now().Add(time.Hour))
	cb := httptest.NewRequest(http.MethodGet, base+"/auth/callback?state="+state.Value+"&code=testcode", nil)
	for _, c := range set {
		cb.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	w := httptest.NewRecorder()
	auth.CallbackHandler(w, cb)
	return w
}

// principalFor runs Middleware on a request carrying the raw Cookie header
// and returns the principal it published ("" when none).
func principalFor(auth *writoidc.Authenticator, cookieHeader string) string {
	var got string
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.Header.Set("Cookie", cookieHeader)
	auth.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = writoidc.PrincipalFromContext(r.Context())
	})).ServeHTTP(httptest.NewRecorder(), r)
	return got
}

// TestHostCookies_SignInRoundTrip: the whole sign-in works on __Host- cookies,
// at the root and under WARDYN_BASE_PATH. Under a base path the cookies still
// carry Path=/ — __Host- requires it — and the callback, the session and
// sign-out all line up with that: sign-out clears the one cookie at Path=/.
func TestHostCookies_SignInRoundTrip(t *testing.T) {
	for _, base := range []string{"", "/wardyn"} {
		t.Run("base="+base, func(t *testing.T) {
			env := newIdPEnv(t)
			auth := env.newSecureAuth(t, base, true)
			cb := secureSignIn(t, env, auth, base)
			if cb.Code != http.StatusFound || cb.Header().Get("Location") != base+"/" {
				t.Fatalf("callback = %d Location %q body %q, want 302 to %q", cb.Code, cb.Header().Get("Location"), cb.Body.String(), base+"/")
			}
			set := cb.Result().Cookies()
			assertHostCookies(t, "callback", set)
			session := cookieMap(set)["__Host-wardyn_session"]
			if session == nil || session.Value == "" {
				t.Fatalf("callback set %v, want a __Host-wardyn_session", set)
			}
			if got := principalFor(auth, "__Host-wardyn_session="+session.Value); got != "sub-host" {
				t.Fatalf("Middleware principal = %q, want sub-host", got)
			}

			out := httptest.NewRecorder()
			auth.LogoutHandler(out, httptest.NewRequest(http.MethodPost, base+"/api/v1/auth/logout", nil))
			clears := out.Result().Cookies()
			assertHostCookies(t, "logout", clears)
			if len(clears) != 1 || clears[0].Name != "__Host-wardyn_session" || clears[0].MaxAge >= 0 {
				t.Fatalf("logout set %+v, want one expired __Host-wardyn_session at Path=/", clears)
			}
		})
	}
}

// TestHostCookies_PlantedSessionIsNotRead: login CSRF onto the console. The
// attacker holds a real session of their own and plants its value in the
// victim's browser from a sibling host. A Domain= cookie cannot carry the
// __Host- prefix (the browser refuses it), so it arrives under the plain or
// the __Secure- name — and neither is read as the session, alone or ahead of
// the victim's own. The plain-HTTP posture, where __Host- cannot exist, still
// reads the plain name.
func TestHostCookies_PlantedSessionIsNotRead(t *testing.T) {
	env := newIdPEnv(t)
	secure := env.newSecureAuth(t, "", true)
	cookie, err := writoidc.EncodeSessionForTest(secure, writoidc.Session{
		Sub: "sub-attacker", Email: "attacker@example.com", Role: writoidc.RoleAdmin, UserType: "standard",
		Expiry: time.Now().Add(time.Hour), IssuedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("EncodeSessionForTest: %v", err)
	}
	if cookie.Name != "__Host-wardyn_session" {
		t.Fatalf("session cookie name = %q, want __Host-wardyn_session", cookie.Name)
	}
	v := cookie.Value
	for _, planted := range []string{"wardyn_session=" + v, "__Secure-wardyn_session=" + v, "__host-wardyn_session=" + v} {
		if got := principalFor(secure, planted); got != "" {
			t.Errorf("planted %q was read as the session of %q", strings.SplitN(planted, "=", 2)[0], got)
		}
	}
	if got := principalFor(secure, "__Host-wardyn_session="+v); got != "sub-attacker" {
		t.Fatalf("control: the __Host- cookie itself = %q, want sub-attacker", got)
	}

	plain := env.newSecureAuth(t, "", false)
	plainCookie, err := writoidc.EncodeSessionForTest(plain, writoidc.Session{
		Sub: "sub-plain", Email: "plain@example.com", Role: writoidc.RoleAdmin, UserType: "standard",
		Expiry: time.Now().Add(time.Hour), IssuedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("EncodeSessionForTest: %v", err)
	}
	if plainCookie.Name != "wardyn_session" || plainCookie.Path != "/" {
		t.Fatalf("plain-HTTP session cookie = %q Path %q, want wardyn_session at /", plainCookie.Name, plainCookie.Path)
	}
	if got := principalFor(plain, "wardyn_session="+plainCookie.Value); got != "sub-plain" {
		t.Fatalf("plain HTTP: wardyn_session read as %q, want sub-plain", got)
	}
}

// TestHostCookies_PlantedSignInStateIsRefused: the same plant against the
// sign-in's one-time cookies. An attacker who began a sign-in of their own
// holds a state, nonce and verifier; planted under the plain names they must
// not satisfy the callback. The control shows the same values under the
// __Host- names do pass the state check (the handler then stops at the empty
// code), so the refusal is the name and nothing else.
func TestHostCookies_PlantedSignInStateIsRefused(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newSecureAuth(t, "", true)
	callback := func(prefix string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/auth/callback?state=s1&code=", nil)
		for _, c := range []*http.Cookie{
			{Name: prefix + "wardyn_oidc_state", Value: "s1"},
			{Name: prefix + "wardyn_oidc_nonce", Value: "n1"},
			{Name: prefix + "wardyn_oidc_pkce", Value: "v1"},
		} {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		auth.CallbackHandler(w, r)
		return w
	}
	if w := callback(""); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid state parameter") {
		t.Fatalf("planted plain-name sign-in cookies: %d %q, want 400 invalid state parameter", w.Code, w.Body.String())
	}
	if w := callback("__Host-"); !strings.Contains(w.Body.String(), "missing code parameter") {
		t.Fatalf("control: __Host- sign-in cookies: %d %q, want the state check passed and a stop at the empty code", w.Code, w.Body.String())
	}
}
