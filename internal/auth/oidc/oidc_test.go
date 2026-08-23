// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// testHMACKey is 32 bytes — minimum accepted by New.
var testHMACKey = []byte("00112233445566778899aabbccddeeff")

// idpEnv is a fully-wired test IdP environment with a controllable token endpoint.
type idpEnv struct {
	priv        *rsa.PrivateKey
	oidcSrv     *oidctest.Server
	httpSrv     *httptest.Server
	tokenSrv    *httptest.Server
	clientID    string
	latestIDTok string // set by buildIDToken; returned by tokenSrv
}

// newIdPEnv starts a fake OIDC server plus a fake token endpoint that is
// reachable via the rewriteTokenRT round-tripper. The token endpoint returns
// whatever rawIDToken the caller last stored in latestIDTok.
func newIdPEnv(t *testing.T) *idpEnv {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	env := &idpEnv{priv: priv, clientID: "wardyn-client"}

	env.oidcSrv = &oidctest.Server{
		PublicKeys: []oidctest.PublicKey{{
			PublicKey: priv.Public(),
			KeyID:     "test-key",
			Algorithm: "RS256",
		}},
	}
	env.httpSrv = httptest.NewServer(env.oidcSrv)
	env.oidcSrv.SetIssuer(env.httpSrv.URL)

	// Fake token endpoint.
	env.tokenSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "at",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     env.latestIDTok,
		})
	}))
	t.Cleanup(func() {
		env.httpSrv.Close()
		env.tokenSrv.Close()
	})
	return env
}

// buildIDToken signs a token with the given fields and stores it as latestIDTok.
func (e *idpEnv) buildIDToken(t *testing.T, sub, email, nonce string, expiry time.Time) string {
	t.Helper()
	return e.buildIDTokenWithRoles(t, sub, email, nil, nil, nonce, expiry)
}

// buildIDTokenWithRoles is buildIDToken plus Entra-shaped "roles" and
// "groups" claims (each omitted from the token when nil, matching an IdP that
// sends neither) — used by the role-derivation test suite (TestCallbackRole*).
func (e *idpEnv) buildIDTokenWithRoles(t *testing.T, sub, email string, roles, groups []string, nonce string, expiry time.Time) string {
	t.Helper()
	claims := map[string]interface{}{
		"iss":            e.httpSrv.URL,
		"sub":            sub,
		"aud":            e.clientID,
		"email":          email,
		"email_verified": true,
		"nonce":          nonce,
		"iat":            time.Now().Unix(),
		"exp":            expiry.Unix(),
	}
	if roles != nil {
		claims["roles"] = roles
	}
	if groups != nil {
		claims["groups"] = groups
	}
	raw, _ := json.Marshal(claims)
	tok := oidctest.SignIDToken(e.priv, "test-key", "RS256", string(raw))
	e.latestIDTok = tok
	return tok
}

// buildIDTokenRawClaim is buildIDToken plus one arbitrary extra claim of any
// shape — H1's tolerant-decode regression coverage: a real IdP sometimes
// sends "roles"/"groups" as a scalar string (or object) instead of the JSON
// array deriveRole expects. Signs with roleCallbackNonce so the result works
// directly with doCallback.
func (e *idpEnv) buildIDTokenRawClaim(t *testing.T, sub, email, extraKey string, extraVal interface{}) string {
	t.Helper()
	claims := map[string]interface{}{
		"iss": e.httpSrv.URL, "sub": sub, "aud": e.clientID,
		"email": email, "email_verified": true, "nonce": roleCallbackNonce,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		extraKey: extraVal,
	}
	raw, _ := json.Marshal(claims)
	tok := oidctest.SignIDToken(e.priv, "test-key", "RS256", string(raw))
	e.latestIDTok = tok
	return tok
}

// newAuth builds an Authenticator that routes the oauth2 token exchange through
// env.tokenSrv (via the rewriteTokenRT round-tripper) for full end-to-end tests.
func (e *idpEnv) newAuth(t *testing.T, allowedDomains []string) *writoidc.Authenticator {
	t.Helper()
	rt := &rewriteTokenRT{
		base:          http.DefaultTransport,
		originalToken: e.httpSrv.URL + "/token",
		replacedToken: e.tokenSrv.URL + "/",
	}
	ctx := gooidc.ClientContext(context.Background(), &http.Client{Transport: rt})
	cfg := writoidc.Config{
		IssuerURL:           e.httpSrv.URL,
		ClientID:            e.clientID,
		ClientSecret:        "secret",
		RedirectURL:         "http://localhost/auth/callback",
		AllowedEmailDomains: allowedDomains,
	}
	auth, err := writoidc.New(ctx, cfg, testHMACKey)
	if err != nil {
		t.Fatalf("writoidc.New: %v", err)
	}
	return auth
}

// newRoleAuth is newAuth plus role-derivation config (WARDYN_OIDC_ROLE_MAP /
// WARDYN_OIDC_DEFAULT_ROLE / the legacy WARDYN_OIDC_OPERATOR_EMAILS source),
// for the role-derivation test suite (TestCallbackRole*). No domain
// restriction: those tests are orthogonal to AllowedEmailDomains.
func (e *idpEnv) newRoleAuth(t *testing.T, roleMap map[string]string, defaultRole string, legacyAdminEmails []string) *writoidc.Authenticator {
	t.Helper()
	rt := &rewriteTokenRT{
		base:          http.DefaultTransport,
		originalToken: e.httpSrv.URL + "/token",
		replacedToken: e.tokenSrv.URL + "/",
	}
	ctx := gooidc.ClientContext(context.Background(), &http.Client{Transport: rt})
	cfg := writoidc.Config{
		IssuerURL:         e.httpSrv.URL,
		ClientID:          e.clientID,
		ClientSecret:      "secret",
		RedirectURL:       "http://localhost/auth/callback",
		RoleMap:           roleMap,
		DefaultRole:       defaultRole,
		LegacyAdminEmails: legacyAdminEmails,
	}
	auth, err := writoidc.New(ctx, cfg, testHMACKey)
	if err != nil {
		t.Fatalf("writoidc.New: %v", err)
	}
	return auth
}

// newAuthWithOnLogin is newAuth plus Config.OnLogin (migration 0046's
// callback-refresh hook), for TestCallbackInvokesOnLoginWithSubAndRole.
func (e *idpEnv) newAuthWithOnLogin(t *testing.T, onLogin func(context.Context, string, string)) *writoidc.Authenticator {
	t.Helper()
	rt := &rewriteTokenRT{
		base:          http.DefaultTransport,
		originalToken: e.httpSrv.URL + "/token",
		replacedToken: e.tokenSrv.URL + "/",
	}
	ctx := gooidc.ClientContext(context.Background(), &http.Client{Transport: rt})
	cfg := writoidc.Config{
		IssuerURL:    e.httpSrv.URL,
		ClientID:     e.clientID,
		ClientSecret: "secret",
		RedirectURL:  "http://localhost/auth/callback",
		OnLogin:      onLogin,
	}
	auth, err := writoidc.New(ctx, cfg, testHMACKey)
	if err != nil {
		t.Fatalf("writoidc.New: %v", err)
	}
	return auth
}

// ─── TestLoginHandlerSetsStateCookies ────────────────────────────────────────

func TestLoginHandlerSetsStateCookies(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)

	r := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	w := httptest.NewRecorder()
	auth.LoginHandler(w, r)

	resp := w.Result()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("LoginHandler status = %d, want %d", resp.StatusCode, http.StatusFound)
	}

	// All three one-time cookies must be present.
	cookiesByName := cookieMap(resp.Cookies())
	for _, name := range []string{"wardyn_oidc_state", "wardyn_oidc_nonce", "wardyn_oidc_pkce"} {
		c, ok := cookiesByName[name]
		if !ok {
			t.Errorf("missing cookie %q", name)
			continue
		}
		if !c.HttpOnly {
			t.Errorf("cookie %q: HttpOnly not set", name)
		}
		if c.SameSite != http.SameSiteLaxMode {
			t.Errorf("cookie %q: SameSite not Lax", name)
		}
		if c.Value == "" {
			t.Errorf("cookie %q: empty value", name)
		}
		// RFC 7636 §4.1 floors the PKCE code_verifier at 43 characters;
		// conformant IdPs reject a shorter one at the token endpoint.
		if name == "wardyn_oidc_pkce" && len(c.Value) < 43 {
			t.Errorf("cookie %q: verifier is %d chars, want >= 43 (RFC 7636 §4.1)", name, len(c.Value))
		}
	}

	// Redirect must include state and code_challenge.
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatal("no Location header in redirect")
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse redirect URL: %v", err)
	}
	q := u.Query()
	if q.Get("state") == "" {
		t.Error("state missing from authorization URL")
	}
	if q.Get("code_challenge") == "" {
		t.Error("code_challenge missing from authorization URL")
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
}

// ─── TestCallbackHandlerRejectsInvalidState ──────────────────────────────────

func TestCallbackHandlerRejectsInvalidState(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)

	r := httptest.NewRequest(http.MethodGet, "/auth/callback?state=bad&code=xyz", nil)
	// Provide a different state cookie — should be rejected.
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_state", Value: "correct-state"})
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_nonce", Value: "nonce"})
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_pkce", Value: "verifier"})

	w := httptest.NewRecorder()
	auth.CallbackHandler(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("state mismatch: status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// ─── TestCallbackHandlerRejectsMissingStateCookie ─────────────────────────────

func TestCallbackHandlerRejectsMissingStateCookie(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)

	// No state cookie at all.
	r := httptest.NewRequest(http.MethodGet, "/auth/callback?state=x&code=xyz", nil)
	w := httptest.NewRecorder()
	auth.CallbackHandler(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing state cookie: status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// ─── TestFullCodeFlow ─────────────────────────────────────────────────────────
//
// Full round-trip: LoginHandler → (simulate IdP redirect) → CallbackHandler.
// The fake token endpoint returns a pre-signed ID token containing the correct
// nonce, and we verify that a valid session cookie is issued.

func TestFullCodeFlow(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)

	// Step 1: call LoginHandler to get the state/nonce/pkce cookies.
	loginReq := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	loginW := httptest.NewRecorder()
	auth.LoginHandler(loginW, loginReq)

	loginResp := loginW.Result()
	if loginResp.StatusCode != http.StatusFound {
		t.Fatalf("LoginHandler status = %d, want 302", loginResp.StatusCode)
	}
	cookies := cookieMap(loginResp.Cookies())
	stateVal := cookies["wardyn_oidc_state"].Value
	nonceVal := cookies["wardyn_oidc_nonce"].Value
	pkceVal := cookies["wardyn_oidc_pkce"].Value

	// Step 2: sign an ID token with the nonce the login handler sent.
	env.buildIDToken(t, "sub-user", "user@example.com", nonceVal, time.Now().Add(time.Hour))

	// Step 3: simulate the IdP redirect to the callback.
	cbURL := "/auth/callback?state=" + stateVal + "&code=testcode"
	cbReq := httptest.NewRequest(http.MethodGet, cbURL, nil)
	cbReq.AddCookie(&http.Cookie{Name: "wardyn_oidc_state", Value: stateVal})
	cbReq.AddCookie(&http.Cookie{Name: "wardyn_oidc_nonce", Value: nonceVal})
	cbReq.AddCookie(&http.Cookie{Name: "wardyn_oidc_pkce", Value: pkceVal})
	cbW := httptest.NewRecorder()
	auth.CallbackHandler(cbW, cbReq)

	cbResp := cbW.Result()
	if cbResp.StatusCode != http.StatusFound {
		t.Fatalf("CallbackHandler status = %d, want 302; body: %s", cbResp.StatusCode, cbW.Body.String())
	}

	// A session cookie must have been issued.
	cbCookies := cookieMap(cbResp.Cookies())
	sessCookie, ok := cbCookies["wardyn_session"]
	if !ok {
		t.Fatal("no wardyn_session cookie issued after successful callback")
	}
	if !sessCookie.HttpOnly {
		t.Error("wardyn_session: HttpOnly not set")
	}
	if sessCookie.SameSite != http.SameSiteLaxMode {
		t.Error("wardyn_session: SameSite not Lax")
	}

	// Step 4: verify the session cookie carries the principal through Middleware.
	var capturedPrincipal string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPrincipal = writoidc.PrincipalFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	apiReq := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	apiReq.AddCookie(sessCookie)
	apiW := httptest.NewRecorder()
	auth.Middleware(next).ServeHTTP(apiW, apiReq)

	if capturedPrincipal != "sub-user" {
		t.Errorf("principal from session = %q, want %q", capturedPrincipal, "sub-user")
	}
}

// ─── TestCallbackInvokesOnLoginWithSubAndRole ─────────────────────────────────
//
// Migration 0046's refresh hook: a successful login must call Config.OnLogin
// exactly once, with the ID token's sub and the role CallbackHandler just
// derived — the two values internal/api's wiring (cmd/wardynd/boot_deps.go)
// needs to call store.RefreshSSHKeyRoles. A DENIED login (no role derivable)
// must never call it — internal/api/sshkeys_test.go and
// internal/api/sshgateway_test.go already cover nil (the zero value: every
// existing TestFullCodeFlow-shaped test runs with no OnLogin set at all, and
// passes) being a safe no-op.

func TestCallbackInvokesOnLoginWithSubAndRole(t *testing.T) {
	env := newIdPEnv(t)
	var gotCtx context.Context
	var gotSub, gotRole string
	calls := 0
	auth := env.newAuthWithOnLogin(t, func(ctx context.Context, sub, role string) {
		calls++
		gotCtx, gotSub, gotRole = ctx, sub, role
	})

	loginReq := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	loginW := httptest.NewRecorder()
	auth.LoginHandler(loginW, loginReq)
	cookies := cookieMap(loginW.Result().Cookies())
	stateVal, nonceVal, pkceVal := cookies["wardyn_oidc_state"].Value, cookies["wardyn_oidc_nonce"].Value, cookies["wardyn_oidc_pkce"].Value

	env.buildIDToken(t, "sub-onlogin", "onlogin@example.com", nonceVal, time.Now().Add(time.Hour))

	cbReq := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+stateVal+"&code=testcode", nil)
	cbReq.AddCookie(&http.Cookie{Name: "wardyn_oidc_state", Value: stateVal})
	cbReq.AddCookie(&http.Cookie{Name: "wardyn_oidc_nonce", Value: nonceVal})
	cbReq.AddCookie(&http.Cookie{Name: "wardyn_oidc_pkce", Value: pkceVal})
	cbW := httptest.NewRecorder()
	auth.CallbackHandler(cbW, cbReq)

	if cbW.Code != http.StatusFound {
		t.Fatalf("CallbackHandler status = %d, want 302; body: %s", cbW.Code, cbW.Body.String())
	}
	if calls != 1 {
		t.Fatalf("OnLogin called %d times, want exactly 1", calls)
	}
	if gotCtx == nil {
		t.Error("OnLogin ctx = nil")
	}
	if gotSub != "sub-onlogin" {
		t.Errorf("OnLogin sub = %q, want %q", gotSub, "sub-onlogin")
	}
	// No RoleMap configured: deriveRole's unset-map default is RoleAdmin (see
	// TestCallbackRoleMapUnsetDefaultsAdmin) — that IS the role this login
	// derived, so OnLogin must have received exactly that, not a hardcoded
	// or empty value.
	if gotRole != writoidc.RoleAdmin {
		t.Errorf("OnLogin role = %q, want %q", gotRole, writoidc.RoleAdmin)
	}
}

// TestCallbackDeniedLoginNeverInvokesOnLogin: a role-map miss with no
// DefaultRole denies the login (see TestCallbackRoleNoMatchNoDefaultDenied) —
// OnLogin must not fire for a login that never actually happened.
func TestCallbackDeniedLoginNeverInvokesOnLogin(t *testing.T) {
	env := newIdPEnv(t)
	calls := 0
	rt := &rewriteTokenRT{
		base:          http.DefaultTransport,
		originalToken: env.httpSrv.URL + "/token",
		replacedToken: env.tokenSrv.URL + "/",
	}
	ctx := gooidc.ClientContext(context.Background(), &http.Client{Transport: rt})
	auth, err := writoidc.New(ctx, writoidc.Config{
		IssuerURL:    env.httpSrv.URL,
		ClientID:     env.clientID,
		ClientSecret: "secret",
		RedirectURL:  "http://localhost/auth/callback",
		RoleMap:      map[string]string{"some-other-role": writoidc.RoleAdmin},
		OnLogin:      func(context.Context, string, string) { calls++ },
	}, testHMACKey)
	if err != nil {
		t.Fatalf("writoidc.New: %v", err)
	}

	const stateVal, nonceVal = "state-denied", "nonce-denied"
	env.buildIDToken(t, "sub-denied", "denied@example.com", nonceVal, time.Now().Add(time.Hour))
	r := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+stateVal+"&code=code", nil)
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_state", Value: stateVal})
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_nonce", Value: nonceVal})
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_pkce", Value: "verifier"})
	w := httptest.NewRecorder()
	auth.CallbackHandler(w, r)

	if calls != 0 {
		t.Errorf("OnLogin called %d times for a denied login, want 0", calls)
	}
}

// ─── TestNonceMismatchRejected ────────────────────────────────────────────────
//
// The ID token contains nonce "correct" but the cookie carries "wrong".
// CallbackHandler must reject with 401.

func TestNonceMismatchRejected(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)

	stateVal := "state-abc"
	correctNonce := "correct-nonce"
	wrongNonce := "wrong-nonce"

	// ID token has correctNonce.
	env.buildIDToken(t, "u", "u@example.com", correctNonce, time.Now().Add(time.Hour))

	cbURL := "/auth/callback?state=" + stateVal + "&code=code"
	r := httptest.NewRequest(http.MethodGet, cbURL, nil)
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_state", Value: stateVal})
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_nonce", Value: wrongNonce}) // mismatch
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_pkce", Value: "verifier"})
	w := httptest.NewRecorder()
	auth.CallbackHandler(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("nonce mismatch: status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

// ─── TestCallbackRole* ────────────────────────────────────────────────────────
//
// Role derivation (Config.RoleMap / DefaultRole / LegacyAdminEmails, see
// deriveRole in oidc.go): the union of the ID token's roles/groups claims and
// email, matched against WARDYN_OIDC_ROLE_MAP, with the legacy
// WARDYN_OIDC_OPERATOR_EMAILS source and the admin-beats-member tie rule.

func TestCallbackRoleRolesClaimAdmin(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newRoleAuth(t, map[string]string{"wardyn.admin": writoidc.RoleAdmin}, "", nil)

	// Entra-shaped token: App Roles arrive as a "roles" string array.
	w, sess := doRoleCallback(t, env, auth, "alice@corp.example", []string{"Wardyn.Admin"}, nil)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusFound, w.Body.String())
	}
	if sess.Role != writoidc.RoleAdmin {
		t.Errorf("role = %q, want %q", sess.Role, writoidc.RoleAdmin)
	}
}

func TestCallbackRoleGroupsClaimMember(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newRoleAuth(t, map[string]string{"eng-team": writoidc.RoleMember}, "", nil)

	w, sess := doRoleCallback(t, env, auth, "bob@corp.example", nil, []string{"eng-team"})
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusFound, w.Body.String())
	}
	if sess.Role != writoidc.RoleMember {
		t.Errorf("role = %q, want %q", sess.Role, writoidc.RoleMember)
	}
}

func TestCallbackRoleEmailMapping(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newRoleAuth(t, map[string]string{"carol@corp.example": writoidc.RoleMember}, "", nil)

	w, sess := doRoleCallback(t, env, auth, "carol@corp.example", nil, nil)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusFound, w.Body.String())
	}
	if sess.Role != writoidc.RoleMember {
		t.Errorf("role = %q, want %q", sess.Role, writoidc.RoleMember)
	}
}

func TestCallbackRoleNoMatchNoDefaultDenied(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newRoleAuth(t, map[string]string{"eng-team": writoidc.RoleMember}, "", nil)

	w, sess := doRoleCallback(t, env, auth, "dave@corp.example", nil, nil)
	// W31-S1-5: redirects to "/?auth_error=no_role" (302) instead of a bare
	// http.Error text page.
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusFound, w.Body.String())
	}
	if sess.Role != "" {
		t.Errorf("role = %q, want no session issued", sess.Role)
	}
	if loc := w.Result().Header.Get("Location"); !strings.Contains(loc, "auth_error=no_role") {
		t.Errorf("Location = %q, want the no_role auth_error code", loc)
	}
	// L6: a denied login must clear any pre-existing wardyn_session cookie —
	// otherwise a browser that already held a valid session from a PRIOR
	// login keeps sending it after this attempt was refused.
	assertSessionCookieCleared(t, w)
}

func TestCallbackRoleNoMatchDefaultRoleMember(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newRoleAuth(t, map[string]string{"eng-team": writoidc.RoleMember}, writoidc.RoleMember, nil)

	w, sess := doRoleCallback(t, env, auth, "erin@corp.example", nil, nil)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusFound, w.Body.String())
	}
	if sess.Role != writoidc.RoleMember {
		t.Errorf("role = %q, want %q (DefaultRole)", sess.Role, writoidc.RoleMember)
	}
}

func TestCallbackRoleMapUnsetDefaultsAdmin(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newRoleAuth(t, nil, "", nil) // WARDYN_OIDC_ROLE_MAP entirely unset

	w, sess := doRoleCallback(t, env, auth, "frank@corp.example", nil, nil)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusFound, w.Body.String())
	}
	if sess.Role != writoidc.RoleAdmin {
		t.Errorf("role = %q, want %q (unset role map = today's pre-0.5 behavior)", sess.Role, writoidc.RoleAdmin)
	}
}

func TestCallbackRoleMapUnsetLegacyListStillSplitsAdminMember(t *testing.T) {
	// Regression (silent viewer→admin escalation on upgrade): with the role map
	// UNSET but WARDYN_OIDC_OPERATOR_EMAILS SET — the mandatory-minimum SSO config
	// an upgrading 0.4.5 deployment runs (validateOperatorPosture refuses an empty
	// list; a role map is opt-in on top) — the operator allowlist alone must still
	// split admin from member. Before the fix, deriveRole's empty-map short-circuit
	// returned RoleAdmin for EVERYONE, promoting every existing viewer to admin.
	env := newIdPEnv(t)
	auth := env.newRoleAuth(t, nil, "", []string{"olivia@corp.example"}) // role map unset, operator list set

	// A listed operator email still derives admin.
	wOp, opSess := doRoleCallback(t, env, auth, "Olivia@Corp.Example", nil, nil) // mixed case: case-insensitive match
	if wOp.Code != http.StatusFound {
		t.Fatalf("operator status = %d, want %d (body: %s)", wOp.Code, http.StatusFound, wOp.Body.String())
	}
	if opSess.Role != writoidc.RoleAdmin {
		t.Errorf("listed operator role = %q, want %q", opSess.Role, writoidc.RoleAdmin)
	}
	// Any other signed-in human is a member (main's viewer tier), NOT admin.
	wMem, memSess := doRoleCallback(t, env, auth, "victor@corp.example", nil, nil)
	if wMem.Code != http.StatusFound {
		t.Fatalf("member status = %d, want %d (body: %s)", wMem.Code, http.StatusFound, wMem.Body.String())
	}
	if memSess.Role != writoidc.RoleMember {
		t.Errorf("non-listed role = %q, want %q (must not be promoted to admin on upgrade)", memSess.Role, writoidc.RoleMember)
	}
}

func TestCallbackRoleLegacyOperatorEmailAdmin(t *testing.T) {
	env := newIdPEnv(t)
	// The map is SET and this user also hits a MEMBER entry via groups — the
	// legacy operator-email match must still win to admin (highest
	// precedence), proving it is not merely a fallback for an empty map.
	auth := env.newRoleAuth(t, map[string]string{"eng-team": writoidc.RoleMember}, "", []string{"grace@corp.example"})

	// Mixed case on both sides of the match, proving case-insensitivity too.
	w, sess := doRoleCallback(t, env, auth, "Grace@Corp.Example", nil, []string{"eng-team"})
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusFound, w.Body.String())
	}
	if sess.Role != writoidc.RoleAdmin {
		t.Errorf("role = %q, want %q (legacy WARDYN_OIDC_OPERATOR_EMAILS beats a member match)", sess.Role, writoidc.RoleAdmin)
	}
}

func TestCallbackRoleAdminBeatsMemberTie(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newRoleAuth(t, map[string]string{
		"wardyn.admin": writoidc.RoleAdmin,
		"eng-team":     writoidc.RoleMember,
	}, "", nil)

	// Matches BOTH an admin entry (roles) and a member entry (groups) — any
	// admin match wins, independent of which claim it came from.
	w, sess := doRoleCallback(t, env, auth, "henry@corp.example", []string{"Wardyn.Admin"}, []string{"eng-team"})
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusFound, w.Body.String())
	}
	if sess.Role != writoidc.RoleAdmin {
		t.Errorf("role = %q, want %q (any admin match wins the tie)", sess.Role, writoidc.RoleAdmin)
	}
}

// TestCallbackRoleNonASCIIEmailNoFoldEscalation mirrors internal/api's
// isOperator fold-escalation guard (rbac_test.go's "non-ascii email never
// matches" case): strings.EqualFold/ToLower do Unicode case folding, under
// which "roſs" (U+017F) folds onto ASCII "s". A crafted non-ASCII email must
// never match an ASCII WARDYN_OIDC_OPERATOR_EMAILS / WARDYN_OIDC_ROLE_MAP
// entry — the escalating direction.
func TestCallbackRoleNonASCIIEmailNoFoldEscalation(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newRoleAuth(t, map[string]string{"eng-team": writoidc.RoleMember}, "", []string{"ross@corp.example"})

	// U+017F (LATIN SMALL LETTER LONG S): "roſs@corp.example" must NOT match
	// the ASCII legacy-admin entry "ross@corp.example". The groups claim still
	// hits the member entry, so a passing test proves the email path was
	// skipped rather than the whole callback failing for an unrelated reason.
	w, sess := doRoleCallback(t, env, auth, "roſs@corp.example", nil, []string{"eng-team"})
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusFound, w.Body.String())
	}
	if sess.Role != writoidc.RoleMember {
		t.Errorf("role = %q, want %q (non-ASCII email must not fold onto the ASCII legacy-admin entry)", sess.Role, writoidc.RoleMember)
	}
}

// ─── TestCallbackScalarClaim* ─────────────────────────────────────────────────
//
// H1: a real IdP sometimes sends "roles"/"groups" as a scalar string (or
// object) instead of a JSON array — e.g. a lone group as bare "eng-team"
// rather than ["eng-team"]. That must decode to nothing for that ONE claim,
// never abort the whole login: folding Roles/Groups into the same claims
// struct as Email/EmailVerified would make idToken.Claims a FATAL unmarshal
// for any such IdP, a 100% login outage even with WARDYN_OIDC_ROLE_MAP unset.

func TestCallbackScalarGroupsClaimMapUnsetStillLogsIn(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newRoleAuth(t, nil, "", nil) // WARDYN_OIDC_ROLE_MAP entirely unset

	env.buildIDTokenRawClaim(t, "sub-scalar", "scalar@corp.example", "groups", "not-an-array")
	w, sess := doCallback(t, auth)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (a malformed groups claim must not break login): body: %s", w.Code, http.StatusFound, w.Body.String())
	}
	if sess.Role != writoidc.RoleAdmin {
		t.Errorf("role = %q, want %q (role map unset = today's behavior)", sess.Role, writoidc.RoleAdmin)
	}
}

func TestCallbackScalarGroupsClaimMapSetContributesNothing(t *testing.T) {
	env := newIdPEnv(t)
	// The role map has an entry keyed on the EXACT scalar string the token
	// will send — if the tolerant decode were broken (e.g. silently coerced
	// into a one-element slice instead of failing closed), this would wrongly
	// match and grant member.
	auth := env.newRoleAuth(t, map[string]string{"not-an-array": writoidc.RoleMember}, "", nil)

	env.buildIDTokenRawClaim(t, "sub-scalar2", "scalar2@corp.example", "groups", "not-an-array")
	w, sess := doCallback(t, auth)
	// W31-S1-5: redirects (302) rather than a fatal 401/500 or a bare
	// http.Error text page.
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (malformed claim decodes to nothing, so nothing matches; deny via redirect, not a fatal 401/500): body: %s", w.Code, http.StatusFound, w.Body.String())
	}
	if sess.Role != "" {
		t.Errorf("role = %q, want no session (the scalar claim's raw string must not match the role-map entry)", sess.Role)
	}
}

// ─── TestParseRoleMap ─────────────────────────────────────────────────────────
//
// M2/M3/L8: non-empty input that yields no usable entry, a non-ASCII key
// (can never match — see asciiOnly), and a duplicate key must all be parse
// errors, never a silent "close enough" map.

func TestParseRoleMap(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr bool
	}{
		{"empty is unset, not an error", "", nil, false},
		{"blank is unset, not an error", "   ", nil, false},
		{"one valid entry", "eng-team=member", map[string]string{"eng-team": "member"}, false},
		{"multiple valid entries, key lowered", "Wardyn.Admin=admin,eng-team=member", map[string]string{"wardyn.admin": "admin", "eng-team": "member"}, false},
		{"blank entries between commas are skipped", "eng-team=member,,", map[string]string{"eng-team": "member"}, false},
		{"missing =", "eng-team", nil, true},
		{"empty key before =", "=admin", nil, true},
		{"bad role value", "eng-team=owner", nil, true},
		{"duplicate key", "eng-team=member,eng-team=admin", nil, true},
		{"duplicate key differs only by case", "Eng-Team=member,eng-team=admin", nil, true},
		// M2: non-empty input, but every entry is blank — must error, not
		// silently return nil (which deriveRole reads as "everyone is admin").
		{"all-blank input is an error, not silently unset", ",,,", nil, true},
		// M3: a non-ASCII key can never match a claim (deriveRole's asciiOnly
		// guard skips non-ASCII claim values before lookup) — a silent dead
		// entry that inverts intent under WARDYN_OIDC_DEFAULT_ROLE=admin.
		{"non-ASCII key can never match", "roſs@corp.example=admin", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := writoidc.ParseRoleMap(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseRoleMap(%q): want error, got nil (map = %v)", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRoleMap(%q): unexpected error: %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseRoleMap(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("ParseRoleMap(%q)[%q] = %q, want %q", tc.in, k, got[k], v)
				}
			}
		})
	}
}

// TestPreV05CookieNoRoleTreatedAsNoSession proves the upgrade path: a session
// cookie signed before the Role field existed (so it decodes with Role == "")
// must be treated as NO session — forcing a re-login that derives one fresh —
// never as an authenticated session with an undefined role, and never a 500.
func TestPreV05CookieNoRoleTreatedAsNoSession(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)

	cookie, err := writoidc.EncodeSessionForTest(auth, writoidc.Session{
		Sub: "sub-legacy", Email: "legacy@example.com", Expiry: time.Now().Add(time.Hour),
		// Role deliberately omitted: this is exactly what a pre-0.5 payload
		// unmarshals to, since json.Unmarshal leaves a field the payload never
		// mentions at its zero value.
	})
	if err != nil {
		t.Fatalf("EncodeSessionForTest: %v", err)
	}

	var principalSet bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writoidc.PrincipalFromContext(r.Context()) != "" {
			principalSet = true
		}
		w.WriteHeader(http.StatusOK)
	})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	auth.Middleware(next).ServeHTTP(w, r)

	if principalSet {
		t.Error("a session cookie with no role must be treated as no session (forced re-login), not silently authenticated")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (falls through to next, never a 500)", w.Code, http.StatusOK)
	}
}

// TestSessionRoundTripWithRole proves Role survives the full encode/decode
// round trip alongside Sub/Email, read back via the exported context helpers.
func TestSessionRoundTripWithRole(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)

	sess := buildSession(t, auth, "sub-dana", "dana@example.com", writoidc.RoleMember, time.Now().Add(time.Hour))

	var got writoidc.Session
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = writoidc.Session{
			Sub:   writoidc.PrincipalFromContext(r.Context()),
			Email: writoidc.EmailFromContext(r.Context()),
			Role:  writoidc.RoleFromContext(r.Context()),
		}
		w.WriteHeader(http.StatusOK)
	})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(sess)
	w := httptest.NewRecorder()
	auth.Middleware(next).ServeHTTP(w, r)

	if got.Sub != "sub-dana" || got.Email != "dana@example.com" || got.Role != writoidc.RoleMember {
		t.Errorf("round-tripped session = %+v, want sub=sub-dana email=dana@example.com role=%s", got, writoidc.RoleMember)
	}
}

// ─── TestSessionRoundTrip ─────────────────────────────────────────────────────

func TestSessionRoundTrip(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)

	sess := buildSession(t, auth, "sub-alice", "alice@example.com", writoidc.RoleAdmin, time.Now().Add(time.Hour))

	// Middleware: valid session → principal set.
	var capturedPrincipal string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPrincipal = writoidc.PrincipalFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(sess)
	w := httptest.NewRecorder()
	auth.Middleware(next).ServeHTTP(w, r)

	if capturedPrincipal != "sub-alice" {
		t.Errorf("principal = %q, want %q", capturedPrincipal, "sub-alice")
	}
}

// ─── TestMiddlewareFallsThrough ───────────────────────────────────────────────

func TestMiddlewareFallsThrough(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)

	var reached bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if p := writoidc.PrincipalFromContext(r.Context()); p != "" {
			t.Errorf("unexpected principal %q on context without session", p)
		}
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	auth.Middleware(next).ServeHTTP(w, r)

	if !reached {
		t.Error("next handler was not called (middleware did not fall through)")
	}
}

// ─── TestExpiredSessionRejected ───────────────────────────────────────────────

func TestExpiredSessionRejected(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)

	// Build a session that expired 1 second ago.
	expired := buildSession(t, auth, "sub-bob", "bob@example.com", writoidc.RoleAdmin, time.Now().Add(-time.Second))

	var principalSet bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writoidc.PrincipalFromContext(r.Context()) != "" {
			principalSet = true
		}
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(expired)
	w := httptest.NewRecorder()
	auth.Middleware(next).ServeHTTP(w, r)

	if principalSet {
		t.Error("expired session should not set a principal")
	}
	// The stale cookie should be cleared (MaxAge=-1).
	resp := w.Result()
	for _, c := range resp.Cookies() {
		if c.Name == "wardyn_session" && c.MaxAge != -1 {
			t.Errorf("stale cookie not cleared (MaxAge=%d, want -1)", c.MaxAge)
		}
	}
}

// ─── TestTamperedCookieRejected ───────────────────────────────────────────────

func TestTamperedCookieRejected(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)

	valid := buildSession(t, auth, "sub-charlie", "charlie@example.com", writoidc.RoleAdmin, time.Now().Add(time.Hour))

	// Flip a byte in the signature portion (after the ".").
	parts := strings.SplitN(valid.Value, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("unexpected cookie format: %q", valid.Value)
	}
	sig := []byte(parts[1])
	sig[len(sig)-1] ^= 0xFF
	valid.Value = parts[0] + "." + string(sig)

	var principalSet bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writoidc.PrincipalFromContext(r.Context()) != "" {
			principalSet = true
		}
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(valid)
	w := httptest.NewRecorder()
	auth.Middleware(next).ServeHTTP(w, r)

	if principalSet {
		t.Error("tampered cookie should not set a principal")
	}
}

// ─── TestDomainCheck* ─────────────────────────────────────────────────────────

func TestDomainCheckAllowedEmail(t *testing.T) {
	checkDomainFilter(t, "alice@example.com", []string{"example.com"}, true)
}

func TestDomainCheckDeniedEmail(t *testing.T) {
	checkDomainFilter(t, "evil@attacker.com", []string{"example.com"}, false)
}

// Matching is exact, not suffix-based: a subdomain must be listed separately.
func TestDomainCheckDeniedSubdomain(t *testing.T) {
	checkDomainFilter(t, "alice@eng.example.com", []string{"example.com"}, false)
}

func TestDomainCheckEmptyAllowed(t *testing.T) {
	checkDomainFilter(t, "anyone@anywhere.io", nil, true)
}

func TestDomainCheckMalformedEmail(t *testing.T) {
	checkDomainFilter(t, "notanemail", []string{"example.com"}, false)
}

// ─── TestLogoutClearsCookie ───────────────────────────────────────────────────

func TestLogoutClearsCookie(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)

	r := httptest.NewRequest(http.MethodGet, "/auth/logout", nil)
	r.AddCookie(&http.Cookie{Name: "wardyn_session", Value: "some-value"})
	w := httptest.NewRecorder()
	auth.LogoutHandler(w, r)

	resp := w.Result()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("LogoutHandler status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "wardyn_session" {
			if c.MaxAge != -1 {
				t.Errorf("wardyn_session MaxAge = %d, want -1 (cleared)", c.MaxAge)
			}
			return
		}
	}
	// Cookie absent is also acceptable (browser would drop it).
}

// ─── TestSecureCookieAttribute ────────────────────────────────────────────────
//
// The session cookie's Secure attribute must follow Config.SecureCookies:
// false over plain HTTP (else the browser drops the cookie and login breaks),
// true under TLS (direct or terminated at a reverse proxy).

func TestSecureCookieAttribute(t *testing.T) {
	env := newIdPEnv(t)

	cases := []struct {
		name          string
		secureCookies bool
		wantSecure    bool
	}{
		{"plain http leaves Secure false", false, false},
		{"tls marks Secure true", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := writoidc.Config{
				IssuerURL:     env.httpSrv.URL,
				ClientID:      env.clientID,
				ClientSecret:  "secret",
				RedirectURL:   "http://localhost/auth/callback",
				SecureCookies: tc.secureCookies,
			}
			ctx := gooidc.ClientContext(context.Background(), &http.Client{})
			auth, err := writoidc.New(ctx, cfg, testHMACKey)
			if err != nil {
				t.Fatalf("writoidc.New: %v", err)
			}

			// Session cookie carries the Secure attribute per config.
			sess := buildSession(t, auth, "sub-secure", "s@example.com", writoidc.RoleAdmin, time.Now().Add(time.Hour))
			if sess.Secure != tc.wantSecure {
				t.Errorf("session cookie Secure = %v, want %v", sess.Secure, tc.wantSecure)
			}

			// The one-time login cookies (state/nonce/pkce) must match too.
			r := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
			w := httptest.NewRecorder()
			auth.LoginHandler(w, r)
			for _, c := range w.Result().Cookies() {
				if c.Secure != tc.wantSecure {
					t.Errorf("login cookie %q Secure = %v, want %v", c.Name, c.Secure, tc.wantSecure)
				}
			}
		})
	}
}

// ─── TestNewRejectsMissingConfig ──────────────────────────────────────────────

func TestNewRejectsMissingConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  writoidc.Config
	}{
		{"empty", writoidc.Config{}},
		{"no client id", writoidc.Config{IssuerURL: "http://x", ClientSecret: "s", RedirectURL: "http://r"}},
		{"no issuer", writoidc.Config{ClientID: "c", ClientSecret: "s", RedirectURL: "http://r"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := writoidc.New(context.Background(), tc.cfg, testHMACKey)
			if err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

// ─── TestNewRejectsShortHMACKey ───────────────────────────────────────────────

func TestNewRejectsShortHMACKey(t *testing.T) {
	env := newIdPEnv(t)

	cfg := writoidc.Config{
		IssuerURL:    env.httpSrv.URL,
		ClientID:     "c",
		ClientSecret: "s",
		RedirectURL:  "http://r",
	}
	_, err := writoidc.New(context.Background(), cfg, []byte("short"))
	if err == nil {
		t.Error("expected error for short HMAC key, got nil")
	}
}

// ─── TestWrongHMACKeyRejected ─────────────────────────────────────────────────
//
// A session cookie signed with key A must be rejected when decoded with key B.

func TestWrongHMACKeyRejected(t *testing.T) {
	env := newIdPEnv(t)
	authA := env.newAuth(t, nil) // uses testHMACKey

	// Build a different authenticator with a different key.
	altKey := []byte("ffeeddccbbaa99887766554433221100")
	cfg := writoidc.Config{
		IssuerURL:    env.httpSrv.URL,
		ClientID:     env.clientID,
		ClientSecret: "secret",
		RedirectURL:  "http://localhost/auth/callback",
	}
	ctx := gooidc.ClientContext(context.Background(), &http.Client{})
	authB, err := writoidc.New(ctx, cfg, altKey)
	if err != nil {
		t.Fatalf("New(altKey): %v", err)
	}

	// Cookie issued by authA.
	sessA := buildSession(t, authA, "sub-a", "a@example.com", writoidc.RoleAdmin, time.Now().Add(time.Hour))

	// authB must not accept authA's cookie (different key).
	var principalSet bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writoidc.PrincipalFromContext(r.Context()) != "" {
			principalSet = true
		}
		w.WriteHeader(http.StatusOK)
	})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(sessA)
	authB.Middleware(next).ServeHTTP(httptest.NewRecorder(), r)

	if principalSet {
		t.Error("session cookie signed with key A must not be accepted by authenticator using key B")
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// buildSession creates a signed session cookie via the exported test helper.
func buildSession(t *testing.T, auth *writoidc.Authenticator, sub, email, role string, expiry time.Time) *http.Cookie {
	t.Helper()
	cookie, err := writoidc.EncodeSessionForTest(auth, writoidc.Session{
		Sub:    sub,
		Email:  email,
		Role:   role,
		Expiry: expiry,
	})
	if err != nil {
		t.Fatalf("EncodeSessionForTest: %v", err)
	}
	return cookie
}

// checkDomainFilter exercises the email domain check via a full callback flow.
func checkDomainFilter(t *testing.T, email string, allowedDomains []string, wantAllowed bool) {
	t.Helper()
	env := newIdPEnv(t)
	auth := env.newAuth(t, allowedDomains)

	stateVal := "state-123"
	nonceVal := "nonce-456"
	verifierVal := "verifier-789"

	env.buildIDToken(t, "u1", email, nonceVal, time.Now().Add(time.Hour))

	r := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+stateVal+"&code=testcode", nil)
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_state", Value: stateVal})
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_nonce", Value: nonceVal})
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_pkce", Value: verifierVal})

	w := httptest.NewRecorder()
	auth.CallbackHandler(w, r)

	resp := w.Result()
	if wantAllowed {
		if resp.StatusCode != http.StatusFound {
			t.Errorf("email %q (allowed): status = %d, want %d (body: %s)",
				email, resp.StatusCode, http.StatusFound, w.Body.String())
		}
	} else {
		// W31-S1-5: a domain-check denial redirects to "/?auth_error=..."
		// (302) rather than a bare http.Error text page — a real page the
		// browser can act on instead of a dead end with no way back.
		if resp.StatusCode != http.StatusFound {
			t.Errorf("email %q (denied): status = %d, want %d (body: %s)",
				email, resp.StatusCode, http.StatusFound, w.Body.String())
		}
		if loc := resp.Header.Get("Location"); !strings.Contains(loc, "auth_error=") {
			t.Errorf("email %q (denied): Location = %q, want an auth_error redirect", email, loc)
		}
		// L6: the domain-check deny paths must also clear any pre-existing
		// session cookie, same as the no-role deny path.
		assertSessionCookieCleared(t, w)
	}
}

// roleCallbackNonce is the fixed nonce doCallback's request cookie carries;
// every token built for a doCallback test (buildIDTokenWithRoles,
// buildIDTokenRawClaim) signs this same value.
const roleCallbackNonce = "nonce-role"

// doCallback drives CallbackHandler with a fixed state/nonce/pkce (like
// checkDomainFilter) against whatever ID token is currently staged in the
// idpEnv (the caller must have signed one carrying roleCallbackNonce), then —
// if a session cookie was issued — round-trips it through Middleware to read
// back the derived Sub/Email/Role. Returns the recorder (status + body, for
// the denied-login assertions) and the round-tripped session (zero value
// when no cookie was issued, e.g. a denied login).
func doCallback(t *testing.T, auth *writoidc.Authenticator) (*httptest.ResponseRecorder, writoidc.Session) {
	t.Helper()
	const stateVal, verifierVal = "state-role", "verifier-role"
	r := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+stateVal+"&code=testcode", nil)
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_state", Value: stateVal})
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_nonce", Value: roleCallbackNonce})
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_pkce", Value: verifierVal})
	w := httptest.NewRecorder()
	auth.CallbackHandler(w, r)

	var sessCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "wardyn_session" {
			sessCookie = c
		}
	}
	if sessCookie == nil {
		return w, writoidc.Session{}
	}

	var got writoidc.Session
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = writoidc.Session{
			Sub:    writoidc.PrincipalFromContext(r.Context()),
			Email:  writoidc.EmailFromContext(r.Context()),
			Role:   writoidc.RoleFromContext(r.Context()),
			Groups: writoidc.GroupsFromContext(r.Context()),
		}
	})
	checkReq := httptest.NewRequest(http.MethodGet, "/", nil)
	checkReq.AddCookie(sessCookie)
	auth.Middleware(next).ServeHTTP(httptest.NewRecorder(), checkReq)
	return w, got
}

// doRoleCallback signs an ID token carrying the given roles/groups/email,
// then drives it through doCallback.
func doRoleCallback(t *testing.T, env *idpEnv, auth *writoidc.Authenticator, email string, roles, groups []string) (*httptest.ResponseRecorder, writoidc.Session) {
	t.Helper()
	env.buildIDTokenWithRoles(t, "sub-role", email, roles, groups, roleCallbackNonce, time.Now().Add(time.Hour))
	return doCallback(t, auth)
}

// assertSessionCookieCleared checks the response set a wardyn_session cookie
// with MaxAge=-1 (browser-delete) — the L6 invariant every deny path in
// CallbackHandler must uphold, so a refused login attempt never leaves a
// pre-existing session cookie valid in the browser.
func assertSessionCookieCleared(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == "wardyn_session" {
			if c.MaxAge != -1 {
				t.Errorf("wardyn_session MaxAge = %d, want -1 (cleared)", c.MaxAge)
			}
			return
		}
	}
	t.Error("deny response did not clear wardyn_session (no Set-Cookie wardyn_session with MaxAge=-1)")
}

// cookieMap indexes a slice of cookies by name.
func cookieMap(cookies []*http.Cookie) map[string]*http.Cookie {
	m := make(map[string]*http.Cookie, len(cookies))
	for _, c := range cookies {
		m[c.Name] = c
	}
	return m
}

// rewriteTokenRT is an http.RoundTripper that replaces token endpoint URLs to
// point at a test server instead of the real IdP.
type rewriteTokenRT struct {
	base          http.RoundTripper
	originalToken string
	replacedToken string
}

func (rt *rewriteTokenRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasPrefix(req.URL.String(), rt.originalToken) {
		newURL, _ := url.Parse(rt.replacedToken)
		req = req.Clone(req.Context())
		req.URL = newURL
	}
	return rt.base.RoundTrip(req)
}

func TestRewriteTransport(t *testing.T) {
	// Records the host the base transport actually saw.
	var sawHost string
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sawHost = r.URL.Host
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header)}, nil
	})
	rt, err := writoidc.NewRewriteTransportForTest("http://localhost:5556", "http://dex:5556", &http.Client{Transport: base})
	if err != nil {
		t.Fatalf("newRewriteTransport: %v", err)
	}

	// A request to the public authority is rewritten to the internal one.
	req, _ := http.NewRequest("GET", "http://localhost:5556/token", nil)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if sawHost != "dex:5556" {
		t.Errorf("public authority not rewritten: base saw %q, want dex:5556", sawHost)
	}
	// The caller's request URL must NOT be mutated.
	if req.URL.Host != "localhost:5556" {
		t.Errorf("caller request mutated: %q", req.URL.Host)
	}
	// A request to any other host passes through untouched.
	other, _ := http.NewRequest("GET", "http://api.anthropic.com/v1", nil)
	if _, err := rt.RoundTrip(other); err != nil {
		t.Fatalf("RoundTrip other: %v", err)
	}
	if sawHost != "api.anthropic.com" {
		t.Errorf("unrelated host rewritten: %q", sawHost)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
