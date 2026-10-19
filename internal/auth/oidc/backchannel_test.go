// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

const logoutEvent = "http://schemas.openid.net/event/backchannel-logout"

// logoutToken signs a logout token for the test provider; edit changes the claim set before signing.
func (e *idpEnv) logoutToken(t *testing.T, sub string, edit func(map[string]any)) string {
	t.Helper()
	claims := map[string]any{
		"iss": e.httpSrv.URL, "aud": e.clientID, "sub": sub, "jti": "jti-1",
		"iat": time.Now().Unix(), "exp": time.Now().Add(2 * time.Minute).Unix(),
		"events": map[string]any{logoutEvent: map[string]any{}},
	}
	if edit != nil {
		edit(claims)
	}
	raw, _ := json.Marshal(claims)
	return oidctest.SignIDToken(e.priv, "test-key", "RS256", string(raw))
}

func postLogout(h http.HandlerFunc, token string) *httptest.ResponseRecorder {
	form := url.Values{}
	if token != "" {
		form.Set("logout_token", token)
	}
	r := httptest.NewRequest(http.MethodPost, "/auth/backchannel-logout", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func backchannelAuth(t *testing.T, env *idpEnv, rev writoidc.SessionRevocations) *writoidc.Authenticator {
	t.Helper()
	return env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) {
		cfg.Revocations, cfg.BackchannelLogout = rev, true
	})
}

// The identity provider's signed logout token ends the person's console sessions the way a sign-out
// does: a cookie taken before is refused afterwards, and no token or key is revoked.
func TestBackchannelLogoutCutsTheSessions(t *testing.T) {
	env := newIdPEnv(t)
	rev := &cutLedger{seq: 10}
	auth := backchannelAuth(t, env, rev)
	cookie := signedInCookie(t, env, auth)
	if live, reason := replay(auth, cookie); !live {
		t.Fatalf("a fresh session was refused: %q", reason)
	}
	var heard []string
	h := auth.BackchannelLogoutHandler(nil, func(_ *http.Request, sub string, err error) { heard = append(heard, sub) })

	w := postLogout(h, env.logoutToken(t, "pairwise-1", nil))
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("logout = %d %s (Cache-Control %q), want 200 no-store", w.Code, w.Body.String(), w.Header().Get("Cache-Control"))
	}
	if live, reason := replay(auth, cookie); live || reason != "revoked_session" {
		t.Errorf("a cookie from before the logout = live %v, %q; want it revoked", live, reason)
	}
	if rev.cutCalls != 1 || rev.revokedSub != 0 || len(heard) != 1 || heard[0] != "pairwise-1" {
		t.Errorf("cuts %d, tokens-and-keys revokes %d, audit heard %v; want one session cut for pairwise-1 and nothing else", rev.cutCalls, rev.revokedSub, heard)
	}
}

// Only a token the provider signed for this client, carrying the logout event and a sub, cuts anyone.
func TestBackchannelLogoutRefusesWhatIsNotALogoutToken(t *testing.T) {
	env := newIdPEnv(t)
	rev := &cutLedger{seq: 10}
	h := backchannelAuth(t, env, rev).BackchannelLogoutHandler(func(sub string) bool { return sub == "admin-token" }, nil)
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	forged := func() string {
		raw, _ := json.Marshal(map[string]any{"iss": env.httpSrv.URL, "aud": env.clientID, "sub": "pairwise-1", "jti": "j",
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(), "events": map[string]any{logoutEvent: map[string]any{}}})
		return oidctest.SignIDToken(otherKey, "test-key", "RS256", string(raw))
	}()
	for name, token := range map[string]string{
		"no token":         "",
		"not a jwt":        "garbage",
		"another key":      forged,
		"another audience": env.logoutToken(t, "pairwise-1", func(c map[string]any) { c["aud"] = "someone-else" }),
		"another issuer":   env.logoutToken(t, "pairwise-1", func(c map[string]any) { c["iss"] = "https://evil.example" }),
		"expired":          env.logoutToken(t, "pairwise-1", func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }),
		"stale iat":        env.logoutToken(t, "pairwise-1", func(c map[string]any) { c["iat"] = time.Now().Add(-time.Hour).Unix() }),
		"no event":         env.logoutToken(t, "pairwise-1", func(c map[string]any) { c["events"] = map[string]any{} }),
		"a null event":     env.logoutToken(t, "pairwise-1", func(c map[string]any) { c["events"] = map[string]any{logoutEvent: nil} }),
		"a string event":   env.logoutToken(t, "pairwise-1", func(c map[string]any) { c["events"] = map[string]any{logoutEvent: "x"} }),
		"a number event":   env.logoutToken(t, "pairwise-1", func(c map[string]any) { c["events"] = map[string]any{logoutEvent: 1} }),
		"a nonce":          env.logoutToken(t, "pairwise-1", func(c map[string]any) { c["nonce"] = "n" }),
		"no jti":           env.logoutToken(t, "pairwise-1", func(c map[string]any) { delete(c, "jti") }),
		"an ID token":      env.buildIDToken(t, "pairwise-1", "pat@corp.example", "n", time.Now().Add(time.Hour)),
		"a sid and no sub": env.logoutToken(t, "", func(c map[string]any) { delete(c, "sub"); c["sid"] = "s1" }),
		"an empty sub":     env.logoutToken(t, "", nil),
		"a reserved sub":   env.logoutToken(t, "admin-token", nil),
	} {
		if w := postLogout(h, token); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_request") {
			t.Errorf("%s = %d %s, want 400 invalid_request", name, w.Code, w.Body.String())
		}
	}
	if rev.cutCalls != 0 {
		t.Errorf("a refused token cut %d times", rev.cutCalls)
	}
}

// A cut that cannot be written is a 503 the provider can retry, and is heard with its error.
func TestBackchannelLogoutAnswers503WhenTheCutFails(t *testing.T) {
	env := newIdPEnv(t)
	var heard error
	h := backchannelAuth(t, env, &cutLedger{cutErr: errors.New("db down")}).
		BackchannelLogoutHandler(nil, func(_ *http.Request, _ string, err error) { heard = err })
	if w := postLogout(h, env.logoutToken(t, "pairwise-1", nil)); w.Code != http.StatusServiceUnavailable {
		t.Errorf("failed cut = %d %s, want 503", w.Code, w.Body.String())
	}
	if heard == nil {
		t.Error("the audit hook never heard the failed cut")
	}
	noStore := env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) { cfg.BackchannelLogout = true })
	if w := postLogout(noStore.BackchannelLogoutHandler(nil, nil), env.logoutToken(t, "pairwise-1", nil)); w.Code != http.StatusServiceUnavailable {
		t.Errorf("no revocation store = %d, want 503 rather than a logout that cut nothing", w.Code)
	}
}

// The endpoint is off unless an administrator turns it on.
func TestBackchannelLogoutIsOffByDefault(t *testing.T) {
	env := newIdPEnv(t)
	if env.newAuth(t, nil).BackchannelLogoutEnabled() {
		t.Error("an Authenticator with no BackchannelLogout reports the route enabled")
	}
	if !backchannelAuth(t, env, &cutLedger{}).BackchannelLogoutEnabled() {
		t.Error("BackchannelLogout set but the route is reported off")
	}
}
