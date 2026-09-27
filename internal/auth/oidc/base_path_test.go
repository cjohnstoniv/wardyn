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
// neighbouring application on the same host never receives it.
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
		cleared := cookieMap(out.Result().Cookies())["wardyn_session"]
		if cleared == nil || cleared.MaxAge >= 0 || cleared.Path != tc.cookiePath {
			t.Errorf("base %q: cleared wardyn_session = %+v, want an expiry at Path %q", tc.base, cleared, tc.cookiePath)
		}
		if got := out.Header().Get("Location"); got != tc.home {
			t.Errorf("base %q: logout Location = %q, want %q", tc.base, got, tc.home)
		}
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
