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
// role, and expiry, HMAC-SHA256 signed with the key passed to New. The key is
// never logged and never leaves the process. A cookie with no role — signed
// before the role field existed (pre-0.5), or otherwise carrying an empty one
// — decodes as NO session (see decodeSession), forcing a re-login that derives
// one fresh rather than granting an undefined role.
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
	"unicode"

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
	// whose domain — the part after the last '@' — exactly equals one of the
	// listed values, case-insensitively. Matching is exact, not suffix-based:
	// listing "example.com" does NOT admit "eng.example.com", which must be
	// listed separately; wildcards are not supported.
	// An empty list allows any verified email. Fail closed: if the IdP does not
	// return a verified email and AllowedEmailDomains is non-empty, login is denied.
	// Entra ID tokens typically OMIT email_verified entirely, so this option
	// fail-closes on every login against an Entra tenant; prefer Entra App Roles
	// (WARDYN_OIDC_ROLE_MAP against the "roles" claim, below) plus the app
	// registration's "assignment required" setting there instead.
	AllowedEmailDomains []string
	// RoleMap maps a case-insensitive claim/email value — an Entra App Role
	// from the ID token's "roles" claim, a "groups" claim entry, or the user's
	// email — to a Wardyn role, RoleAdmin or RoleMember. Parsed from
	// WARDYN_OIDC_ROLE_MAP by ParseRoleMap ("value=role" CSV pairs, e.g.
	// "Wardyn.Admin=admin,eng-team=member,alice@corp.com=admin"); ParseRoleMap
	// is also where a bad role value is rejected, so every entry here is
	// already valid. Precedence when more than one entry matches: ANY match
	// resolving to RoleAdmin wins over one resolving to RoleMember, regardless
	// of which claim produced it (see deriveRole). Empty (the default) disables
	// role derivation entirely: every signed-in human keeps today's pre-0.5
	// behavior (RoleAdmin) — opt-in and upgrade-safe.
	RoleMap map[string]string
	// DefaultRole is the role a signed-in human gets when RoleMap is non-empty
	// but nothing in their roles/groups/email matched an entry: RoleAdmin,
	// RoleMember, or "" (the default) to DENY the login instead, with a message
	// telling them to ask their operator for a WARDYN_OIDC_ROLE_MAP entry.
	// Ignored when RoleMap is empty (see RoleMap's own empty-map behavior).
	DefaultRole string
	// LegacyAdminEmails is WARDYN_OIDC_OPERATOR_EMAILS — the operator allowlist,
	// now the SOLE source of the admin tier (internal/api's isOperator reads the
	// derived role, not this list directly). An email on it always derives
	// RoleAdmin, same top precedence as any RoleMap admin match, and with no
	// RoleMap at all the list alone splits admin from member — so a 0.4.5
	// deployment keeps its operators as admins and everyone else as members
	// (viewers) with zero re-configuration, with or without a role map.
	LegacyAdminEmails []string
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
// client-side. Sub, email, role, and expiry are persisted. Role is always
// non-empty in a cookie this package issues — CallbackHandler denies the
// login rather than write one with an undefined role — and decodeSession
// treats an empty Role (a pre-0.5 cookie, or a corrupt payload) as no session.
type Session struct {
	Sub    string    `json:"sub"`
	Email  string    `json:"email"`
	Role   string    `json:"role"`
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
	// PKCE: code verifier (32 octets => the 43-char minimum RFC 7636 §4.1
	// mandates; a shorter verifier is rejected by conformant IdPs).
	codeVerifier := oauth2.GenerateVerifier()

	// State cookie: bound to this browser, compared in CallbackHandler.
	http.SetCookie(w, a.loginCookie(stateCookieName, state))
	// Nonce cookie: verified in the ID token.
	http.SetCookie(w, a.loginCookie(nonceCookieName, nonce))
	// PKCE verifier cookie: sent to token endpoint in CallbackHandler.
	http.SetCookie(w, a.loginCookie(pkceCookieName, codeVerifier))

	authURL := a.oauth2.AuthCodeURL(state,
		oauth2.SetAuthURLParam("nonce", nonce),
		oauth2.S256ChallengeOption(codeVerifier),
	)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// CallbackHandler handles the IdP redirect. It:
//  1. Verifies the state parameter against the state cookie (CSRF).
//  2. Exchanges the code for tokens using PKCE.
//  3. Verifies the ID token signature, issuer, audience, expiry, and nonce.
//  4. Optionally checks email domain (fail closed when AllowedEmailDomains is set).
//  5. Derives the session's role from the roles/groups/email claims (see
//     Config.RoleMap / deriveRole); denies the login if nothing matches and no
//     DefaultRole is configured.
//  6. Creates a signed Wardyn session cookie.
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
		oauth2.VerifierOption(pkceCookie.Value),
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

	// Extract standard claims from the ID token — UNCHANGED shape and fatal
	// error from before role derivation existed: this is the ONE claims
	// struct whose failure to parse must abort the login.
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "id_token claims extraction failed", http.StatusUnauthorized)
		return
	}

	// Role-derivation claims are decoded SEPARATELY and TOLERANTLY, one
	// struct per claim. "roles" (Entra App Roles — the priority path) and
	// "groups" are supposed to be JSON string arrays, but a real IdP
	// sometimes emits a scalar string (or object) instead — e.g. a single
	// group as bare "eng-team" rather than ["eng-team"]. Folding these into
	// the claims struct above turned that into a FATAL unmarshal error for
	// every such IdP, a 100% login outage even with WARDYN_OIDC_ROLE_MAP
	// unset. A malformed claim here decodes to nil (contributes nothing to
	// deriveRole — fail closed on that one claim, not the whole login), and a
	// malformed roles claim can't discard a valid groups claim or vice versa
	// since each has its own struct.
	var rc struct {
		Roles []string `json:"roles"`
	}
	_ = idToken.Claims(&rc)
	var gc struct {
		Groups []string `json:"groups"`
	}
	_ = idToken.Claims(&gc)

	// (4) Domain check — fail closed.
	if len(a.cfg.AllowedEmailDomains) > 0 {
		if !claims.EmailVerified {
			clearCookie(w, sessionCookieName)
			http.Error(w, "email not verified by IdP", http.StatusForbidden)
			return
		}
		if !emailDomainAllowed(claims.Email, a.cfg.AllowedEmailDomains) {
			clearCookie(w, sessionCookieName)
			http.Error(w, "email domain not permitted", http.StatusForbidden)
			return
		}
	}

	// (5) Role derivation — see Config.RoleMap / deriveRole for precedence.
	// Denying here (rather than issuing a roleless session) is what keeps
	// decodeSession simple: every cookie this package ever writes has a
	// non-empty Role.
	role, ok := deriveRole(rc.Roles, gc.Groups, claims.Email, a.cfg.RoleMap, a.cfg.LegacyAdminEmails, a.cfg.DefaultRole)
	if !ok {
		// L6: a denied login must not leave a PRE-EXISTING session cookie
		// (from before this re-login attempt) still valid in the browser.
		clearCookie(w, sessionCookieName)
		http.Error(w, "no Wardyn role assigned; ask your operator to map you via WARDYN_OIDC_ROLE_MAP", http.StatusForbidden)
		return
	}

	// (6) Create a Wardyn session.
	sess := Session{
		Sub:    idToken.Subject,
		Email:  claims.Email,
		Role:   role,
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
				ctx := contextWithPrincipal(r.Context(), sess)
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

// EmailFromContext returns the email claim of the session Middleware verified,
// or "" when there is no SSO session (or the IdP returned no email — which is
// possible whenever AllowedEmailDomains is empty, since that is the only check
// that requires one). It is the identity internal/api resolves the minimal
// viewer/operator role from; the "sub" is opaque and cannot be matched against
// an operator allowlist an admin can actually write down.
func EmailFromContext(ctx context.Context) string {
	e, _ := ctx.Value(emailCtxKey{}).(string)
	return e
}

// RoleFromContext returns the Wardyn role (RoleAdmin or RoleMember) derived
// for the session Middleware verified, or "" when there is no SSO session.
// This package only DERIVES and CARRIES the role — see CallbackHandler /
// deriveRole for how it is computed. Enforcing it (deciding what an admin vs
// a member may do) belongs to internal/api, the same split
// PrincipalFromContext/EmailFromContext already follow.
func RoleFromContext(ctx context.Context) string {
	r, _ := ctx.Value(roleCtxKey{}).(string)
	return r
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
	if sess.Role == "" {
		// Pre-0.5 cookie (the role field didn't exist yet) or a corrupt/empty
		// payload: never treat an undefined role as authenticated. Middleware
		// falls through on this exactly like any other invalid session,
		// forcing a re-login where CallbackHandler derives and stamps a role
		// fresh — never a 500.
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

// ─── role derivation ─────────────────────────────────────────────────────────

// Wardyn roles a session can carry. See Session.Role / Config.RoleMap.
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// ValidRole reports whether s is a recognized role value. Used to validate
// WARDYN_OIDC_ROLE_MAP entries (ParseRoleMap) and WARDYN_OIDC_DEFAULT_ROLE
// (cmd/wardynd, at boot) — both fail closed on a typo rather than letting a
// garbage role value silently reach a session cookie.
func ValidRole(s string) bool {
	return s == RoleAdmin || s == RoleMember
}

// ParseRoleMap parses WARDYN_OIDC_ROLE_MAP: a comma-separated list of
// "value=role" pairs, e.g.
// "Wardyn.Admin=admin,eng-team=member,alice@corp.com=admin". value is matched
// case-insensitively against an ID token's roles/groups claims or its email
// (see deriveRole); role must be RoleAdmin or RoleMember. Empty/blank input
// returns a nil map (role derivation disabled — Config.RoleMap's empty
// behavior) and no error; non-empty input that yields no usable entry (e.g.
// "," or a single malformed pair) is an error, never a silent nil — nil means
// "everyone is admin" (deriveRole), which must never be an accident.
func ParseRoleMap(csv string) (map[string]string, error) {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil, nil
	}
	out := make(map[string]string)
	for _, pair := range strings.Split(csv, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, cut := strings.Cut(pair, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !cut || k == "" {
			return nil, fmt.Errorf("malformed entry %q: want value=role", pair)
		}
		if !ValidRole(v) {
			return nil, fmt.Errorf("entry %q: invalid role %q (want %q or %q)", pair, v, RoleAdmin, RoleMember)
		}
		// A non-ASCII key can NEVER match: deriveRole skips non-ASCII claim
		// values before lookup (asciiOnly, the fold-escalation guard), so
		// this would silently be a dead entry — worse, one that INVERTS
		// intent under WARDYN_OIDC_DEFAULT_ROLE=admin, where the operator
		// meant to name this value out for a lesser role but it can never
		// match and every such login instead gets the default.
		if !asciiOnly(k) {
			return nil, fmt.Errorf("entry %q: non-ASCII value can never match (matching is ASCII-only)", pair)
		}
		key := strings.ToLower(k)
		// A duplicate key silently let the LAST entry win — in the
		// escalating direction when an earlier entry mapped to member and a
		// later, easy-to-miss duplicate maps the same value to admin. An
		// operator reading the file top-to-bottom would expect the first
		// entry to hold; reject instead of guessing which one they meant.
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("entry %q: duplicate value %q (already mapped by an earlier entry)", pair, k)
		}
		out[key] = v
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no valid entries in %q", csv)
	}
	return out, nil
}

// deriveRole computes the Wardyn role for a signed-in human from the ID
// token's roles/groups claims, their email, and the derivation config
// (Config.RoleMap / Config.LegacyAdminEmails / Config.DefaultRole). ok is
// false only when roleMap is non-empty, nothing matched, and defaultRole is
// empty — the caller (CallbackHandler) must then deny the login.
//
// Precedence:
//  1. An empty roleMap disables claim-based derivation: the role comes from the
//     legacy operator allowlist alone — an email on legacyAdminEmails is
//     RoleAdmin, anyone else RoleMember (main's operator/viewer split, preserved
//     with no role map). With NEITHER a role map nor an allowlist every human is
//     RoleAdmin (true pre-0.5) — so adopting WARDYN_OIDC_ROLE_MAP is opt-in and
//     upgrade-safe, and so is running on only WARDYN_OIDC_OPERATOR_EMAILS.
//  2. Otherwise, build the case-insensitive union of rolesClaim, groupsClaim,
//     and email, and look each value up in roleMap. ANY match resolving to
//     RoleAdmin wins over one resolving to RoleMember, no matter which claim
//     produced it. An email on legacyAdminEmails (WARDYN_OIDC_OPERATOR_EMAILS)
//     counts as an additional RoleAdmin match — it wins even over a
//     RoleMember entry the same email also hits.
//  3. If nothing matched at all: defaultRole if set, else deny.
func deriveRole(rolesClaim, groupsClaim []string, email string, roleMap map[string]string, legacyAdminEmails []string, defaultRole string) (role string, ok bool) {
	if len(roleMap) == 0 {
		// No role map: claim-based derivation is disabled, but the legacy
		// operator allowlist still splits admin from member. WARDYN_OIDC_OPERATOR_EMAILS
		// is the mandatory-minimum SSO config (validateOperatorPosture) and a role
		// map is opt-in on top, so honoring the list here is what keeps main's
		// operator/viewer split working after an upgrade — without this, a 0.4.5
		// deployment that set only the allowlist would silently promote every
		// signed-in human to admin. Only when NEITHER is set does every human
		// default to admin (true pre-0.5, before the operator allowlist existed).
		if len(legacyAdminEmails) == 0 {
			return RoleAdmin, true
		}
		if emailInList(email, legacyAdminEmails) {
			return RoleAdmin, true
		}
		return RoleMember, true
	}
	admin := emailInList(email, legacyAdminEmails)
	member := false
	values := make([]string, 0, len(rolesClaim)+len(groupsClaim)+1)
	values = append(values, rolesClaim...)
	values = append(values, groupsClaim...)
	if email != "" {
		values = append(values, email)
	}
	for _, v := range values {
		if !asciiOnly(v) {
			continue // fail closed: see asciiOnly
		}
		switch roleMap[strings.ToLower(strings.TrimSpace(v))] {
		case RoleAdmin:
			admin = true
		case RoleMember:
			member = true
		}
	}
	switch {
	case admin:
		return RoleAdmin, true
	case member:
		return RoleMember, true
	case defaultRole != "":
		return defaultRole, true
	default:
		return "", false
	}
}

// emailInList reports whether email case-insensitively matches an entry in
// list (mirrors the operator-allowlist match in internal/api's isOperator).
// email is trimmed here too (each list entry is trimmed below, at the point
// of comparison) — without trimming email, a padded ID-token claim would
// silently miss legacyAdminEmails while still matching the role map, whose
// own lookup (deriveRole's loop) already trims its values.
func emailInList(email string, list []string) bool {
	email = strings.TrimSpace(email)
	if email == "" || !asciiOnly(email) {
		return false
	}
	for _, e := range list {
		if strings.EqualFold(strings.TrimSpace(e), email) {
			return true
		}
	}
	return false
}

// asciiOnly reports whether s contains no rune above ASCII. Case-insensitive
// matching (ToLower/EqualFold) does Unicode case folding, under which e.g.
// "roſs" (U+017F) or a KELVIN SIGN "k" (U+212A) MATCHES an ASCII string — the
// escalating direction (same fold-escalation guard as internal/api's
// isOperator, applied here to the RoleMap / LegacyAdminEmails match: both are
// operator-authored ASCII allowlists — WARDYN_OIDC_ROLE_MAP and
// WARDYN_OIDC_OPERATOR_EMAILS — that a crafted non-ASCII claim must never
// fold onto).
func asciiOnly(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r > unicode.MaxASCII }) < 0
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

// contextWithPrincipal stores the verified session's sub, email, and role on
// the context (read back via PrincipalFromContext / EmailFromContext /
// RoleFromContext).
func contextWithPrincipal(ctx context.Context, sess Session) context.Context {
	ctx = context.WithValue(ctx, principalCtxKey{}, sess.Sub)
	ctx = context.WithValue(ctx, emailCtxKey{}, sess.Email)
	return context.WithValue(ctx, roleCtxKey{}, sess.Role)
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

// emailCtxKey is the context key for the session's email claim.
// Unexported: use EmailFromContext.
type emailCtxKey struct{}

// roleCtxKey is the context key for the session's derived role.
// Unexported: use RoleFromContext.
type roleCtxKey struct{}

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
