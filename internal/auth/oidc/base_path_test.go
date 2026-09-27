// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// newBasePathAuth is idpEnv.newAuth with WARDYN_BASE_PATH set and the
// redirect URL registered under it, as boot requires.
func (e *idpEnv) newBasePathAuth(t *testing.T, base string, allowedDomains []string) *writoidc.Authenticator {
	t.Helper()
	rt := &rewriteTokenRT{base: http.DefaultTransport, originalToken: e.httpSrv.URL + "/token", replacedToken: e.tokenSrv.URL + "/"}
	ctx := gooidc.ClientContext(context.Background(), &http.Client{Transport: rt})
	auth, err := writoidc.New(ctx, writoidc.Config{
		IssuerURL: e.httpSrv.URL, ClientID: e.clientID, ClientSecret: "secret",
		RedirectURL:         "http://localhost" + base + "/auth/callback",
		AllowedEmailDomains: allowedDomains,
		BasePath:            base,
	}, testHMACKey)
	if err != nil {
		t.Fatalf("writoidc.New: %v", err)
	}
	return auth
}

func assertCookiePaths(t *testing.T, step string, cookies []*http.Cookie, want string) {
	t.Helper()
	if len(cookies) == 0 {
		t.Fatalf("%s: set no cookies", step)
	}
	for _, c := range cookies {
		if c.Path != want {
			t.Errorf("%s: cookie %s Path = %q, want %q", step, c.Name, c.Path, want)
		}
	}
}

// cookiesNamed returns every cookie matching name, in Set-Cookie header
// order — unlike cookieMap (which indexes by name and so keeps only the
// last), this is how a clear-at-two-paths is observed: two Set-Cookie
// headers sharing one name.
func cookiesNamed(cookies []*http.Cookie, name string) []*http.Cookie {
	var out []*http.Cookie
	for _, c := range cookies {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

// assertClearsSessionEverywhere asserts that clears contains exactly one
// expired (MaxAge<0) wardyn_session cookie per path in wantPaths — the set
// of Path values a pre- and (when under a base) a post-migration browser
// cookie could carry, so both are actually removed.
func assertClearsSessionEverywhere(t *testing.T, step string, clears []*http.Cookie, wantPaths ...string) {
	t.Helper()
	sess := cookiesNamed(clears, "wardyn_session")
	if len(sess) != len(wantPaths) {
		t.Fatalf("%s: cleared %d wardyn_session cookies %+v, want %d at paths %v", step, len(sess), sess, len(wantPaths), wantPaths)
	}
	seen := map[string]bool{}
	for _, c := range sess {
		if c.MaxAge >= 0 {
			t.Errorf("%s: cleared wardyn_session at Path %q has MaxAge %d, want < 0", step, c.Path, c.MaxAge)
		}
		seen[c.Path] = true
	}
	for _, p := range wantPaths {
		if !seen[p] {
			t.Errorf("%s: no wardyn_session clear at Path %q (got %+v)", step, p, sess)
		}
	}
}

var basePathCases = []struct{ base, cookiePath, home string }{
	{"/wardyn", "/wardyn", "/wardyn/"},
	{"", "/", "/"}, // unset: today's placement, exactly
}

// TestBasePathPostLoginRedirect: a completed sign-in lands on the console
// under the base path, not on whatever the host serves at its root.
func TestBasePathPostLoginRedirect(t *testing.T) {
	for _, tc := range basePathCases {
		env := newIdPEnv(t)
		cb := doCallbackFlow(t, env, env.newBasePathAuth(t, tc.base, nil))
		if cb.Code != http.StatusFound || cb.Header().Get("Location") != tc.home {
			t.Errorf("base %q: callback = %d Location %q, want 302 to %q", tc.base, cb.Code, cb.Header().Get("Location"), tc.home)
		}
	}
}

// TestBasePathSessionCookiePath: the wardyn_session cookie — minted at the
// callback, cleared at sign-out — is scoped to the base path, so a
// neighbouring application on the same host never receives it. Under a base
// path, sign-out ALSO clears a Path=/ wardyn_session: a session issued
// before the console moved under WARDYN_BASE_PATH (a same-host migration)
// carries that path, and RFC 6265 cookie identity is name+domain+path, so
// the Path=<base> clear alone would leave it authenticating (F1). With no
// base set, cookiePath() is already "/", so exactly one clear is expected —
// unchanged from before F1.
func TestBasePathSessionCookiePath(t *testing.T) {
	for _, tc := range basePathCases {
		env := newIdPEnv(t)
		auth := env.newBasePathAuth(t, tc.base, nil)
		session := cookieMap(doCallbackFlow(t, env, auth).Result().Cookies())["wardyn_session"]
		if session == nil || session.Value == "" || session.Path != tc.cookiePath {
			t.Errorf("base %q: minted wardyn_session = %+v, want Path %q", tc.base, session, tc.cookiePath)
		}
		out := httptest.NewRecorder()
		auth.LogoutHandler(out, httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil))
		wantPaths := []string{tc.cookiePath}
		if tc.base != "" {
			wantPaths = append(wantPaths, "/")
		}
		assertClearsSessionEverywhere(t, "base "+tc.base+" logout", out.Result().Cookies(), wantPaths...)
		if got := out.Header().Get("Location"); got != tc.home {
			t.Errorf("base %q: logout Location = %q, want %q", tc.base, got, tc.home)
		}
	}
}

// TestBasePathMiddlewareClearsSessionEverywhere: the same double-clear F1
// requires from LogoutHandler applies to Middleware's own two clears — a
// revoked session (D16) and an expired one — since both remove a
// wardyn_session cookie for exactly the same reason: a stale cookie the
// browser must stop sending.
func TestBasePathMiddlewareClearsSessionEverywhere(t *testing.T) {
	for _, tc := range basePathCases {
		wantPaths := []string{tc.cookiePath}
		if tc.base != "" {
			wantPaths = append(wantPaths, "/")
		}

		t.Run("revoked base="+tc.base, func(t *testing.T) {
			env := newIdPEnv(t)
			auth := env.newBasePathAuth(t, tc.base, nil)
			writoidc.SetRevocationsForTest(auth, &fakeSessionRevocations{revoked: true})
			cookie, err := writoidc.EncodeSessionForTest(auth, writoidc.Session{
				Sub: "sub-revoked", Email: "revoked@example.com", Role: writoidc.RoleAdmin, UserType: "standard",
				Expiry: time.Now().Add(time.Hour), IssuedAt: time.Now(),
			})
			if err != nil {
				t.Fatalf("EncodeSessionForTest: %v", err)
			}
			out := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, tc.base+"/", nil)
			r.AddCookie(cookie)
			auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })).ServeHTTP(out, r)
			assertClearsSessionEverywhere(t, "base "+tc.base+" revoked", out.Result().Cookies(), wantPaths...)
		})

		t.Run("expired base="+tc.base, func(t *testing.T) {
			env := newIdPEnv(t)
			auth := env.newBasePathAuth(t, tc.base, nil)
			cookie, err := writoidc.EncodeSessionForTest(auth, writoidc.Session{
				Sub: "sub-expired", Email: "expired@example.com", Role: writoidc.RoleAdmin, UserType: "standard",
				Expiry: time.Now().Add(-time.Hour), IssuedAt: time.Now().Add(-2 * time.Hour),
			})
			if err != nil {
				t.Fatalf("EncodeSessionForTest: %v", err)
			}
			out := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, tc.base+"/", nil)
			r.AddCookie(cookie)
			auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })).ServeHTTP(out, r)
			assertClearsSessionEverywhere(t, "base "+tc.base+" expired", out.Result().Cookies(), wantPaths...)
		})
	}
}

// TestBasePathLoginCookiesAndCallback: the one-time login cookies share the
// session's Path — the browser only returns them to a callback under it —
// and the IdP is told the callback under the base.
func TestBasePathLoginCookiesAndCallback(t *testing.T) {
	for _, tc := range basePathCases {
		env := newIdPEnv(t)
		login := httptest.NewRecorder()
		env.newBasePathAuth(t, tc.base, nil).LoginHandler(login, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
		assertCookiePaths(t, "login", login.Result().Cookies(), tc.cookiePath)
		authURL, err := url.Parse(login.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := authURL.Query().Get("redirect_uri"), "http://localhost"+tc.base+"/auth/callback"; got != want {
			t.Errorf("redirect_uri = %q, want %q", got, want)
		}
	}
}

// TestBasePathRefusedSignInReturnsUnderTheBase: a refused login is sent back
// to the console's sign-in screen, which lives under the base path too.
func TestBasePathRefusedSignInReturnsUnderTheBase(t *testing.T) {
	env := newIdPEnv(t)
	cb := doCallbackFlow(t, env, env.newBasePathAuth(t, "/wardyn", []string{"corp.example"}))
	if loc := cb.Header().Get("Location"); cb.Code != http.StatusFound || !strings.HasPrefix(loc, "/wardyn/?auth_error=") {
		t.Fatalf("refused callback = %d Location %q, want 302 to /wardyn/?auth_error=…", cb.Code, loc)
	}
	assertCookiePaths(t, "refused callback", cb.Result().Cookies(), "/wardyn")
}
