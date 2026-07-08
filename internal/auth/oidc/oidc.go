// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package oidc implements human SSO for Wardyn via OpenID Connect (Dex-compatible).
//
// # CSRF posture (v0)
//
// CSRF protection is two-layered:
//  1. State parameter: a random 128-bit value stored in an HttpOnly SameSite=Lax
//     cookie ("wardyn_oidc_state") and compared to the IdP callback parameter.
//  2. SameSite=Lax on all session cookies: protects all same-site navigations
//     from cross-site request forgery without requiring a synchronizer token.
//
// A PKCE code_challenge (S256) is included in the authorization request and
// verified by the token endpoint. This provides additional security even when
// the state check is bypassed (e.g. by a mix-up attack).
//
// Known gap: the nonce is verified in the ID token but is not bound to the
// device (mitigated by state + PKCE). A future milestone can pin it.
//
// # Session storage
//
// Sessions live entirely in a signed HttpOnly SameSite=Lax cookie named
// "wardyn_session". The cookie payload is a JSON struct containing sub, email,
// and expiry, HMAC-SHA256 signed with the key passed to New. The key is never
// logged and never leaves the process.
//
// # Integration
//
// The Middleware exposed here accepts either a valid session cookie (sets a
// HumanPrincipal on the context via a package-private key) or falls through to
// the next handler (which the integrator wraps with the existing adminAuth
// bearer path). Use PrincipalFromContext to read the principal; it returns ""
// when no SSO session is present so the caller can fall through gracefully.
package oidc

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Config holds the OIDC client configuration. All fields except
// AllowedEmailDomains are required.
type Config struct {
	// IssuerURL is the OIDC provider's PUBLIC issuer — the URL the user's
	// browser is redirected to and the value of the "iss" claim in ID tokens
	// (e.g. http://localhost:5556). Must match the IdP's configured issuer.
	IssuerURL string
	// InternalIssuerURL, when set, is the address at which wardynd itself
	// reaches the IdP for server-side calls (discovery, token exchange, JWKS)
	// — e.g. http://dex:5556 on a Docker network. It solves the split-horizon
	// problem where the browser and the control plane reach the IdP at
	// different hostnames: the browser-facing endpoints keep the public
	// IssuerURL, while wardynd's HTTP client transparently rewrites the public
	// authority to this internal one. Empty => IssuerURL is used for both.
	InternalIssuerURL string
	// ClientID is the OAuth2 client identifier registered with the IdP.
	ClientID string
	// ClientSecret is the OAuth2 client secret. Never log this value.
	ClientSecret string
	// RedirectURL is the callback URL registered with the IdP.
	// Must be <wardynd-base>/auth/callback.
	RedirectURL string
	// AllowedEmailDomains, when non-empty, restricts login to email addresses
	// whose domain suffix matches one of the listed values (e.g. "example.com").
	// An empty list allows any verified email. Fail closed: if the IdP does not
	// return a verified email and AllowedEmailDomains is non-empty, login is denied.
	AllowedEmailDomains []string
	// SecureCookies, when true, marks every cookie Wardyn issues (the session
	// cookie and the one-time login state/nonce/pkce cookies) with the Secure
	// attribute, so browsers only send them over HTTPS. It MUST be true exactly
	// when the connection is TLS-protected — either wardynd serves TLS directly
	// or TLS terminates at an upstream reverse proxy. CRITICAL: Secure cookies
	// are never sent over plain HTTP, so leaving this false (the default) is
	// required for plain-HTTP demo deployments — otherwise login silently breaks.
	SecureCookies bool
}

// Session is the content of the wardyn_session cookie, signed and stored
// client-side. Only sub, email, and expiry are persisted.
type Session struct {
	Sub    string    `json:"sub"`
	Email  string    `json:"email"`
	Expiry time.Time `json:"expiry"`
}

// Authenticator provides OIDC login, callback, logout, and session-check handlers.
type Authenticator struct {
	cfg        Config
	provider   *gooidc.Provider
	oauth2     oauth2.Config
	verifier   *gooidc.IDTokenVerifier
	hmacKey    []byte
	httpClient *http.Client // nil means http.DefaultClient; stored for test injection
}

// New constructs an Authenticator by performing OIDC discovery against
// cfg.IssuerURL. hmacKey is the secret used to sign session cookies; it must
// be provided by the caller (e.g. loaded from the secret store). The key is
// never logged.
func New(ctx context.Context, cfg Config, hmacKey []byte) (*Authenticator, error) {
	if cfg.IssuerURL == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.RedirectURL == "" {
		return nil, errors.New("oidc: IssuerURL, ClientID, ClientSecret, and RedirectURL are required")
	}
	if len(hmacKey) < 32 {
		return nil, errors.New("oidc: hmacKey must be at least 32 bytes")
	}

	// Capture any HTTP client the caller stored in ctx (tests inject a custom
	// transport this way; production ctx carries nil => http.DefaultClient).
	var httpClient *http.Client
	if c, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok {
		httpClient = c
	}

	// Split-horizon issuer: discover against the internal URL but expect (and
	// advertise to the browser) the public issuer. wardynd's HTTP client
	// rewrites the public authority -> internal authority for every
	// server-side call (discovery doc endpoints, token exchange, JWKS).
	discoverURL := cfg.IssuerURL
	if cfg.InternalIssuerURL != "" && cfg.InternalIssuerURL != cfg.IssuerURL {
		rt, err := newRewriteTransport(cfg.IssuerURL, cfg.InternalIssuerURL, httpClient)
		if err != nil {
			return nil, err
		}
		httpClient = &http.Client{Transport: rt}
		discoverURL = cfg.InternalIssuerURL
		// Tell go-oidc the discovery doc's issuer (the public URL) is expected
		// even though we fetched it from the internal URL.
		ctx = gooidc.InsecureIssuerURLContext(ctx, cfg.IssuerURL)
	}
	if httpClient != nil {
		ctx = gooidc.ClientContext(ctx, httpClient)
	}

	provider, err := gooidc.NewProvider(ctx, discoverURL)
	if err != nil {
		return nil, fmt.Errorf("oidc: provider discovery for %q: %w", discoverURL, err)
	}

	oa := oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       []string{gooidc.ScopeOpenID, "profile", "email"},
	}

	verifier := provider.Verifier(&gooidc.Config{ClientID: cfg.ClientID})

	return &Authenticator{
		cfg:        cfg,
		provider:   provider,
		oauth2:     oa,
		verifier:   verifier,
		hmacKey:    hmacKey,
		httpClient: httpClient,
	}, nil
}

// LoginHandler initiates the OIDC authorization code flow. It generates a
// random state and nonce, stores them in HttpOnly SameSite=Lax cookies, and
// redirects the user to the IdP authorization endpoint.
func (a *Authenticator) LoginHandler(w http.ResponseWriter, r *http.Request) {
	state, err := randomToken()
	if err != nil {
		http.Error(w, "internal error generating state", http.StatusInternalServerError)
		return
	}
	nonce, err := randomToken()
	if err != nil {
		http.Error(w, "internal error generating nonce", http.StatusInternalServerError)
		return
	}
	// PKCE: code verifier + challenge (S256).
	codeVerifier, err := randomToken()
	if err != nil {
		http.Error(w, "internal error generating pkce verifier", http.StatusInternalServerError)
		return
	}
	codeChallenge := pkceS256Challenge(codeVerifier)

	// State cookie: bound to this browser, compared in CallbackHandler.
	http.SetCookie(w, a.loginCookie(stateCookieName, state))
	// Nonce cookie: verified in the ID token.
	http.SetCookie(w, a.loginCookie(nonceCookieName, nonce))
	// PKCE verifier cookie: sent to token endpoint in CallbackHandler.
	http.SetCookie(w, a.loginCookie(pkceCookieName, codeVerifier))

	authURL := a.oauth2.AuthCodeURL(state,
		oauth2.SetAuthURLParam("nonce", nonce),
		oauth2.SetAuthURLParam("code_challenge", codeChallenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// CallbackHandler handles the IdP redirect. It:
//  1. Verifies the state parameter against the state cookie (CSRF).
//  2. Exchanges the code for tokens using PKCE.
//  3. Verifies the ID token signature, issuer, audience, expiry, and nonce.
//  4. Optionally checks email domain (fail closed when AllowedEmailDomains is set).
//  5. Creates a signed Wardyn session cookie.
func (a *Authenticator) CallbackHandler(w http.ResponseWriter, r *http.Request) {
	// (1) CSRF: compare state parameter to cookie.
	stateParam := r.URL.Query().Get("state")
	stateCookie, err := r.Cookie(stateCookieName)
	if err != nil || stateCookie.Value == "" || stateParam != stateCookie.Value {
		http.Error(w, "invalid state parameter", http.StatusBadRequest)
		return
	}

	// Read nonce and PKCE verifier cookies.
	nonceCookie, err := r.Cookie(nonceCookieName)
	if err != nil || nonceCookie.Value == "" {
		http.Error(w, "missing nonce cookie", http.StatusBadRequest)
		return
	}
	pkceCookie, err := r.Cookie(pkceCookieName)
	if err != nil || pkceCookie.Value == "" {
		http.Error(w, "missing pkce cookie", http.StatusBadRequest)
		return
	}

	// Clear the one-time cookies immediately (they are single-use).
	clearCookie(w, stateCookieName)
	clearCookie(w, nonceCookieName)
	clearCookie(w, pkceCookieName)

	// (2) Exchange code for tokens, supplying the PKCE verifier.
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code parameter", http.StatusBadRequest)
		return
	}
	// Inject the stored HTTP client into the exchange context so tests that
	// use a custom transport (e.g. rewriteTokenRT) also work for token exchange.
	exchangeCtx := r.Context()
	if a.httpClient != nil {
		exchangeCtx = gooidc.ClientContext(exchangeCtx, a.httpClient)
	}
	token, err := a.oauth2.Exchange(exchangeCtx, code,
		oauth2.SetAuthURLParam("code_verifier", pkceCookie.Value),
	)
	if err != nil {
		http.Error(w, "token exchange failed", http.StatusUnauthorized)
		return
	}

	// (3) Extract and verify the ID token.
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		// Fail closed: a missing ID token is not a valid OIDC response.
		http.Error(w, "id_token absent in token response", http.StatusUnauthorized)
		return
	}
	idToken, err := a.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		http.Error(w, "id_token verification failed", http.StatusUnauthorized)
		return
	}
	// Nonce verification: the ID token nonce must match the cookie.
	if idToken.Nonce != nonceCookie.Value {
		http.Error(w, "nonce mismatch", http.StatusUnauthorized)
		return
	}

	// Extract standard claims from the ID token.
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "id_token claims extraction failed", http.StatusUnauthorized)
		return
	}

	// (4) Domain check — fail closed.
	if len(a.cfg.AllowedEmailDomains) > 0 {
		if !claims.EmailVerified {
			http.Error(w, "email not verified by IdP", http.StatusForbidden)
			return
		}
		if !emailDomainAllowed(claims.Email, a.cfg.AllowedEmailDomains) {
			http.Error(w, "email domain not permitted", http.StatusForbidden)
			return
		}
	}

	// (5) Create a Wardyn session.
	sess := Session{
		Sub:    idToken.Subject,
		Email:  claims.Email,
		Expiry: idToken.Expiry,
	}
	if sess.Expiry.IsZero() {
		// Default to 1 hour if the IdP didn't set an expiry.
		sess.Expiry = time.Now().UTC().Add(time.Hour)
	}
	cookie, err := a.encodeSession(sess)
	if err != nil {
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, cookie)
	http.Redirect(w, r, "/", http.StatusFound)
}

// LogoutHandler clears the Wardyn session cookie and redirects to "/".
func (a *Authenticator) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	clearCookie(w, sessionCookieName)
	http.Redirect(w, r, "/", http.StatusFound)
}

// Middleware returns an http.Handler wrapper that:
//   - If a valid (non-expired, correctly signed) session cookie is present,
//     sets the HumanPrincipal on the request context and calls next.
//   - Otherwise falls through to next without a principal, allowing the
//     integrator's adminAuth bearer path to handle the request.
//
// This design lets the integrator compose: oidc.Middleware(adminAuth(handler)).
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sess, err := a.decodeSession(r); err == nil {
			if time.Now().UTC().Before(sess.Expiry) {
				// Valid session: stash the principal and continue.
				ctx := contextWithPrincipal(r.Context(), sess.Sub)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			// Expired session: clear the stale cookie so the browser doesn't
			// keep sending it, then fall through.
			clearCookie(w, sessionCookieName)
		}
		next.ServeHTTP(w, r)
	})
}

// PrincipalFromContext returns the human principal set by Middleware, or ""
// if no SSO session is present on the context. The returned value is the OIDC
// "sub" claim (a stable opaque identifier from the IdP).
//
// Integration note: internal/api's principalFromRequest should call this first
// and fall back to the admin-token path when the result is "".
func PrincipalFromContext(ctx context.Context) string {
	p, _ := ctx.Value(principalCtxKey{}).(string)
	return p
}

// ─── session encoding ────────────────────────────────────────────────────────

// encodeSession JSON-encodes the session, appends an HMAC-SHA256 tag, and
// returns a signed HttpOnly SameSite=Lax cookie.
func (a *Authenticator) encodeSession(sess Session) (*http.Cookie, error) {
	payload, err := json.Marshal(sess)
	if err != nil {
		return nil, fmt.Errorf("oidc: marshal session: %w", err)
	}
	sig := sessionHMAC(a.hmacKey, payload)
	// Encode as base64(payload) + "." + base64(sig).
	encoded := base64.RawURLEncoding.EncodeToString(payload) +
		"." +
		base64.RawURLEncoding.EncodeToString(sig)

	cookie := &http.Cookie{
		Name:     sessionCookieName,
		Value:    encoded,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.cfg.SecureCookies, // true only under TLS (direct or terminated); false over plain HTTP
		Expires:  sess.Expiry,
	}
	return cookie, nil
}

// decodeSession reads and verifies the session cookie from the request.
// Returns ErrNoSession if the cookie is absent, ErrInvalidSession if tampered
// or expired according to the signature.
func (a *Authenticator) decodeSession(r *http.Request) (Session, error) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return Session{}, ErrNoSession
	}
	parts := strings.SplitN(c.Value, ".", 2)
	if len(parts) != 2 {
		return Session{}, ErrInvalidSession
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Session{}, ErrInvalidSession
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Session{}, ErrInvalidSession
	}
	expected := sessionHMAC(a.hmacKey, payload)
	if !hmac.Equal(sig, expected) {
		return Session{}, ErrInvalidSession
	}
	var sess Session
	if err := json.Unmarshal(payload, &sess); err != nil {
		return Session{}, ErrInvalidSession
	}
	return sess, nil
}

// sessionHMAC returns the HMAC-SHA256 of payload under key.
func sessionHMAC(key, payload []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(payload)
	return h.Sum(nil)
}

// ─── PKCE ────────────────────────────────────────────────────────────────────

// pkceS256Challenge returns the base64url(SHA256(verifier)) per RFC 7636.
func pkceS256Challenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// randomToken generates a cryptographically-random 128-bit base64url string.
func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// emailDomainAllowed returns true if the email's domain (part after last '@')
// matches one of the allowed domains (case-insensitive). Fail closed: returns
// false for empty/malformed email addresses.
func emailDomainAllowed(email string, allowed []string) bool {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	domain := strings.ToLower(email[at+1:])
	for _, a := range allowed {
		if strings.ToLower(a) == domain {
			return true
		}
	}
	return false
}

// loginCookie returns a short-lived HttpOnly SameSite=Lax cookie. These are
// one-time cookies used during the login flow; they expire after 10 minutes.
// Secure is set from cfg.SecureCookies so the login leg matches the session
// cookie: marked Secure only under TLS (direct or terminated), false over plain
// HTTP (else the browser drops them and the demo login breaks).
func (a *Authenticator) loginCookie(name, value string) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   600, // 10 minutes
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.cfg.SecureCookies,
	}
}

// clearCookie instructs the browser to delete a named cookie.
func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// contextWithPrincipal stores sub on the context.
func contextWithPrincipal(ctx context.Context, sub string) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, sub)
}

// ─── constants ───────────────────────────────────────────────────────────────

const (
	sessionCookieName = "wardyn_session"
	stateCookieName   = "wardyn_oidc_state"
	nonceCookieName   = "wardyn_oidc_nonce"
	pkceCookieName    = "wardyn_oidc_pkce"
)

// principalCtxKey is the context key for the human SSO principal.
// Unexported: use PrincipalFromContext.
type principalCtxKey struct{}

// ─── sentinel errors ─────────────────────────────────────────────────────────

// ErrNoSession is returned by decodeSession when no session cookie is present.
var ErrNoSession = errors.New("oidc: no session cookie")

// ErrInvalidSession is returned by decodeSession when the cookie is present
// but tampered, malformed, or uses a different HMAC key.
var ErrInvalidSession = errors.New("oidc: invalid session cookie")

// ─── split-horizon issuer rewrite transport ──────────────────────────────────

// rewriteTransport rewrites the authority (host:port) of every outbound
// request from the public issuer authority to the internal one, so wardynd can
// reach the IdP at an internal hostname while the browser uses the public one.
// Only the matching authority is rewritten; all other requests pass through.
type rewriteTransport struct {
	fromHost string // public authority, e.g. "localhost:5556"
	toHost   string // internal authority, e.g. "dex:5556"
	base     http.RoundTripper
}

// newRewriteTransport builds a rewriteTransport from two base URLs. The schemes
// are ignored (only the authority is matched/rewritten). base may be nil.
func newRewriteTransport(publicURL, internalURL string, baseClient *http.Client) (*rewriteTransport, error) {
	pu, err := url.Parse(publicURL)
	if err != nil || pu.Host == "" {
		return nil, fmt.Errorf("oidc: invalid public issuer URL %q", publicURL)
	}
	iu, err := url.Parse(internalURL)
	if err != nil || iu.Host == "" {
		return nil, fmt.Errorf("oidc: invalid internal issuer URL %q", internalURL)
	}
	var base http.RoundTripper
	if baseClient != nil {
		base = baseClient.Transport
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &rewriteTransport{fromHost: pu.Host, toHost: iu.Host, base: base}, nil
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == t.fromHost {
		// Clone so we never mutate the caller's request.
		r2 := req.Clone(req.Context())
		r2.URL.Host = t.toHost
		r2.Host = t.toHost
		return t.base.RoundTrip(r2)
	}
	return t.base.RoundTrip(req)
}
