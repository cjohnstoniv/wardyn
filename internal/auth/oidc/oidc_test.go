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

// ─── TestSessionRoundTrip ─────────────────────────────────────────────────────

func TestSessionRoundTrip(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, nil)

	sess := buildSession(t, auth, "sub-alice", "alice@example.com", time.Now().Add(time.Hour))

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
	expired := buildSession(t, auth, "sub-bob", "bob@example.com", time.Now().Add(-time.Second))

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

	valid := buildSession(t, auth, "sub-charlie", "charlie@example.com", time.Now().Add(time.Hour))

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
			sess := buildSession(t, auth, "sub-secure", "s@example.com", time.Now().Add(time.Hour))
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
	sessA := buildSession(t, authA, "sub-a", "a@example.com", time.Now().Add(time.Hour))

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
func buildSession(t *testing.T, auth *writoidc.Authenticator, sub, email string, expiry time.Time) *http.Cookie {
	t.Helper()
	cookie, err := writoidc.EncodeSessionForTest(auth, writoidc.Session{
		Sub:    sub,
		Email:  email,
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
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("email %q (denied): status = %d, want %d (body: %s)",
				email, resp.StatusCode, http.StatusForbidden, w.Body.String())
		}
	}
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
