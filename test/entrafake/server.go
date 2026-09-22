// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package entrafake is a local fake of the Microsoft Entra ID v2.0 endpoints a
// per-user Azure DevOps sign-in talks to:
//
//   - OIDC discovery + JWKS, so a real go-oidc verifier can verify the
//     id_token this fake signs
//   - /authorize, the authorization-code leg with PKCE
//   - /token, the authorization_code and refresh_token grants
//
// It exists for the same reason the AWS IAM Identity Center fake does: without
// it, "a person signs in and their Azure DevOps access is captured, narrowed
// and renewed" is unprovable outside a real tenant.
//
// THE FAKE'S TOKENS ARE OPAQUE, deliberately. Real Entra access tokens for
// Azure DevOps are JWTs, but nothing Wardyn does may depend on reading one: the
// resource owns the token's shape and Microsoft documents it as a black box to
// the client. So this fake hands out random strings and keeps the granted scope
// set SERVER-SIDE, answering "what may this token do" through
// ScopesForAccessToken — exactly as a real resource server would ask its own
// introspection, and without parsing a token no client may parse.
//
// CONSENT DECIDES A TOKEN'S SCOPE; THE REQUEST DOES NOT. This is measured
// behaviour against a real tenant, not an approximation: a refresh grant asking
// for one `vso.*` scope comes back with a token whose own scope claim carries
// EVERY scope the person consented to for the Azure DevOps resource, and the
// response's `scope` lists all of them. Requesting `vso.code_write` alone and
// requesting `vso.code vso.work_write` both behaved that way. So this fake
// answers every refresh with the whole consented set, whatever was asked for.
// A fake that narrowed would teach every consumer a false premise — that the
// token bounds what a run can do — and the truth is the opposite: the token is
// the person's own identity with everything they consented to, and the caller's
// own enforcement is what bounds a run.
//
// What the authorize leg establishes is the CONSENT, and that is where a
// request still decides something: a scope never consented to is refused, on
// both legs.
//
// The id_token is the one signed artifact, because that is the one the control
// plane really does verify: it carries the subject the capture binds identity
// on.
package entrafake

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// The Entra error codes this fake speaks. They are the OAuth code the client
// switches on plus, for the consent case, the AADSTS number Microsoft puts in
// the description — the only machine-readable part of a consent refusal, and
// what the capture classifies on.
const (
	// ErrConsentRequired is the OAuth code; AADSTSConsentRequired is the
	// Entra-specific sub-code that rides its description. A client that reads
	// only the OAuth code cannot tell "this app needs admin consent" from
	// "this scope was never consented", so both arms of this fake carry the
	// number.
	ErrConsentRequired = "consent_required"
	// AADSTSConsentRequired is Entra's "the user or administrator has not
	// consented to use the application" code.
	AADSTSConsentRequired = "AADSTS65001"
	// ErrInteractionRequired is what Entra answers when a silent request needs
	// a human at the keyboard (Conditional Access, MFA, a stale session).
	ErrInteractionRequired = "interaction_required"
	// ErrInvalidGrant is a dead code, verifier or refresh token.
	ErrInvalidGrant = "invalid_grant"
	// ErrInvalidClient is an unknown client id or a bad client secret.
	ErrInvalidClient = "invalid_client"
	// ErrInvalidRequest is a malformed request — a missing PKCE challenge, an
	// unsupported challenge method.
	ErrInvalidRequest = "invalid_request"
	// ErrUnsupportedGrantType is a grant this fake does not model. Device code
	// is deliberately among them: Entra's own Conditional Access baseline
	// recommends blocking it, and it cannot be bound to a browser session.
	ErrUnsupportedGrantType = "unsupported_grant_type"
	// ErrUnsupportedResponseType is any response_type but `code`. The implicit
	// and hybrid flows are not modelled because the capture must never use one.
	ErrUnsupportedResponseType = "unsupported_response_type"
)

// accessTokenTTL is how long an access token this fake issues is good for. It
// is the real service's hour, so a consumer's own skew arithmetic is exercised
// against the number it will meet in production.
const accessTokenTTL = time.Hour

// alwaysConsented are the OIDC scopes an Entra app registration holds without
// an admin consenting to anything: they are on every app by default. A
// requested scope in this set is never a consent refusal, which is what keeps
// `openid`/`offline_access` out of the resource-scope bookkeeping.
var alwaysConsented = []string{"openid", "offline_access", "profile", "email"}

// codeGrant is one outstanding authorization code: the PKCE challenge it must
// be redeemed against, the scopes the human consented to on it, and the
// redirect it was issued for.
type codeGrant struct {
	challenge   string
	scopes      []string
	nonce       string
	redirectURI string
}

// refreshGrant is one live refresh token. scopes is the CONSENTED set, and it
// is also exactly what every access token minted from this grant carries — see
// the package doc: the request does not narrow a token, consent does.
//
// retired is what makes ROTATION observable. A rotated refresh token is not
// deleted — it is marked, so a test can tell "the old token was refused"
// (retired) apart from "the token was never issued here" (absent), which are
// the same `invalid_grant` on the wire and very different bugs behind it.
type refreshGrant struct {
	scopes  []string
	retired bool
}

// Server is the fake Entra tenant. Zero value is not usable; use New.
type Server struct {
	httpSrv *httptest.Server

	// signer + jwks are fixed at construction: one RSA key, published as the
	// tenant's only signing key.
	signer jose.Signer
	jwks   []byte
	keyID  string

	mu sync.Mutex

	tenantID     string
	clientID     string
	clientSecret string
	redirectURI  string

	// subject is the `sub` every id_token carries. Per-app by construction in
	// real Entra, which is exactly why the capture binds on it — and why a test
	// forges a DIFFERENT one to prove the binding refuses.
	subject string
	// issuerOverride replaces the `iss` claim without moving discovery, so a
	// test can mint an id_token from the wrong tenant.
	issuerOverride string
	// audienceOverride replaces the `aud` claim for the same reason.
	audienceOverride string
	// tenantClaimOverride replaces the `tid` claim.
	tenantClaimOverride string

	// consented is the resource-scope set this app registration holds. A
	// request for anything outside it (and outside alwaysConsented) is a
	// consent refusal.
	consented []string

	// The three injectable refusals. Each is a knob rather than a fixture
	// because the interesting cases are transitions: consent granted, then
	// revoked; a live session that goes stale mid-estate.
	consentRequired     bool
	interactionRequired bool
	invalidGrant        bool

	codes   map[string]codeGrant
	access  map[string][]string
	refresh map[string]*refreshGrant
}

// New starts a fake Entra tenant on an ephemeral port.
func New() *Server {
	s, h := NewHandler()
	s.httpSrv = httptest.NewServer(h)
	return s
}

// NewHandler returns an UNSTARTED fake plus its handler, for a caller that owns
// its own listener (a fixed port, a TLS server). Mirrors the AWS SSO fake's
// two-constructor shape; an unstarted Server's URL is "".
func NewHandler() (*Server, http.Handler) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("entrafake: generate signing key: " + err.Error())
	}
	keyID := randHex(8)
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: priv, KeyID: keyID}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		panic("entrafake: new signer: " + err.Error())
	}
	jwk, err := json.Marshal(jose.JSONWebKey{
		Key: priv.Public(), KeyID: keyID, Algorithm: string(jose.RS256), Use: "sig",
	})
	if err != nil {
		panic("entrafake: marshal jwk: " + err.Error())
	}
	s := &Server{
		signer:       signer,
		jwks:         []byte(`{"keys":[` + string(jwk) + `]}`),
		keyID:        keyID,
		tenantID:     "11111111-2222-3333-4444-555555555555",
		clientID:     "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		clientSecret: "",
		redirectURI:  "http://127.0.0.1:8080/api/v1/scm/azure-devops/callback",
		subject:      "fake-entra-subject-" + randHex(6),
		consented: []string{
			"499b84ac-1321-427f-aa17-267ca6975798/.default",
			"499b84ac-1321-427f-aa17-267ca6975798/vso.code_write",
			"499b84ac-1321-427f-aa17-267ca6975798/vso.code_status",
			"499b84ac-1321-427f-aa17-267ca6975798/vso.work_write",
		},
		codes:   map[string]codeGrant{},
		access:  map[string][]string{},
		refresh: map[string]*refreshGrant{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{tenant}/v2.0/.well-known/openid-configuration", s.handleDiscovery)
	mux.HandleFunc("GET /{tenant}/discovery/v2.0/keys", s.handleJWKS)
	mux.HandleFunc("GET /{tenant}/oauth2/v2.0/authorize", s.handleAuthorize)
	mux.HandleFunc("POST /{tenant}/oauth2/v2.0/token", s.handleToken)
	return s, mux
}

// URL is the fake's base URL — the AUTHORITY, in Entra's own vocabulary. "" on
// an unstarted fake from NewHandler.
func (s *Server) URL() string {
	if s.httpSrv == nil {
		return ""
	}
	return s.httpSrv.URL
}

// Close shuts the listener down.
func (s *Server) Close() {
	if s.httpSrv != nil {
		s.httpSrv.Close()
	}
}

// Issuer is the `iss` claim and the URL a discovery client resolves — base plus
// tenant plus /v2.0, exactly as a real tenant spells it.
func (s *Server) Issuer() string {
	s.mu.Lock()
	tenant := s.tenantID
	s.mu.Unlock()
	return s.URL() + "/" + tenant + "/v2.0"
}

// TenantID / ClientID / ClientSecret / RedirectURI are the app registration a
// consumer must configure itself with.
func (s *Server) TenantID() string     { return s.get(func() string { return s.tenantID }) }
func (s *Server) ClientID() string     { return s.get(func() string { return s.clientID }) }
func (s *Server) ClientSecret() string { return s.get(func() string { return s.clientSecret }) }
func (s *Server) RedirectURI() string  { return s.get(func() string { return s.redirectURI }) }

// Subject is the `sub` every id_token this fake signs will carry.
func (s *Server) Subject() string { return s.get(func() string { return s.subject }) }

// ConsentedScopes is the resource-scope set this app registration holds.
func (s *Server) ConsentedScopes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.consented)
}

func (s *Server) get(f func() string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return f()
}

// SetClientSecret makes this a CONFIDENTIAL client: /token then requires the
// secret. "" (the default) is a public client authenticating with PKCE alone.
func (s *Server) SetClientSecret(secret string) { s.set(func() { s.clientSecret = secret }) }

// SetRedirectURI pins the one redirect_uri /authorize will accept.
func (s *Server) SetRedirectURI(uri string) { s.set(func() { s.redirectURI = uri }) }

// SetSubject sets the `sub` claim. A test proving the capture's identity
// binding sets it to something OTHER than the session's subject.
func (s *Server) SetSubject(sub string) { s.set(func() { s.subject = sub }) }

// SetClientID re-registers the app under a different client id — how a test
// reaches "this is not the login app's client id".
func (s *Server) SetClientID(id string) { s.set(func() { s.clientID = id }) }

// SetIssuerClaim forges the `iss` claim without moving discovery, so an
// id_token can be made to come from the wrong issuer. "" restores Issuer().
func (s *Server) SetIssuerClaim(iss string) { s.set(func() { s.issuerOverride = iss }) }

// SetAudienceClaim forges the `aud` claim. "" restores the client id.
func (s *Server) SetAudienceClaim(aud string) { s.set(func() { s.audienceOverride = aud }) }

// SetTenantClaim forges the `tid` claim. "" restores the tenant id.
func (s *Server) SetTenantClaim(tid string) { s.set(func() { s.tenantClaimOverride = tid }) }

// SetConsentedScopes replaces the consented resource-scope set.
func (s *Server) SetConsentedScopes(scopes ...string) {
	s.set(func() { s.consented = slices.Clone(scopes) })
}

// SetConsentRequired makes EVERY /authorize and every scope-widening /token
// answer consent_required + AADSTS65001 — the app's consent revoked under it.
func (s *Server) SetConsentRequired(v bool) { s.set(func() { s.consentRequired = v }) }

// SetInteractionRequired makes /authorize and /token answer
// interaction_required — a Conditional Access policy that wants a human.
func (s *Server) SetInteractionRequired(v bool) { s.set(func() { s.interactionRequired = v }) }

// SetInvalidGrant makes every refresh_token redemption answer invalid_grant —
// the shape a revoked or consumed grant has, and the only one a control plane
// can read as "this credential is dead".
func (s *Server) SetInvalidGrant(v bool) { s.set(func() { s.invalidGrant = v }) }

func (s *Server) set(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
}

// ScopesForAccessToken answers what an OPAQUE access token may do. It is this
// fake's introspection endpoint in Go form: a resource server (the Azure DevOps
// fake, or a test asserting a narrowed token) asks here instead of parsing a
// token whose shape no client may depend on. ok=false for a token this fake
// never issued.
func (s *Server) ScopesForAccessToken(token string) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scopes, ok := s.access[token]
	return slices.Clone(scopes), ok
}

// RefreshTokenState reports what this fake thinks of a refresh token: live
// means it can still be redeemed, retired means it was ROTATED away and must
// now be refused, and known=false means it was never issued here. The three are
// one `invalid_grant` on the wire, so a rotation test needs them apart.
func (s *Server) RefreshTokenState(token string) (live, known bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.refresh[token]
	if !ok {
		return false, false
	}
	return !g.retired, true
}

// ── discovery ───────────────────────────────────────────────────────────────

func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	if !s.tenantMatches(w, r) {
		return
	}
	issuer := s.Issuer()
	base := s.URL() + "/" + r.PathValue("tenant")
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                base + "/oauth2/v2.0/authorize",
		"token_endpoint":                        base + "/oauth2/v2.0/token",
		"jwks_uri":                              base + "/discovery/v2.0/keys",
		"response_types_supported":              []string{"code"},
		"response_modes_supported":              []string{"query"},
		"subject_types_supported":               []string{"pairwise"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"scopes_supported":                      append(slices.Clone(alwaysConsented), s.ConsentedScopes()...),
		"token_endpoint_auth_methods_supported": []string{"client_secret_post", "none"},
	})
}

func (s *Server) handleJWKS(w http.ResponseWriter, r *http.Request) {
	if !s.tenantMatches(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(s.jwks)
}

// tenantMatches refuses a path naming a tenant this fake is not. A real
// authority serves every tenant it hosts and 400s the rest, and a consumer that
// composed the wrong tenant into its authority must see that rather than a
// working sign-in for the tenant it meant.
func (s *Server) tenantMatches(w http.ResponseWriter, r *http.Request) bool {
	if r.PathValue("tenant") == s.TenantID() {
		return true
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error":             ErrInvalidRequest,
		"error_description": "AADSTS90002: tenant " + r.PathValue("tenant") + " not found in this directory",
	})
	return false
}

// ── /authorize ──────────────────────────────────────────────────────────────

// handleAuthorize is the authorization-code leg. It AUTO-CONSENTS by default:
// the human is already signed in with Entra, so the realistic case is a
// redirect straight back, and the consent prompt is reached through
// SetConsentRequired instead of by scripting a browser.
//
// The client_id and redirect_uri refusals answer IN-BAND (a 400, no redirect)
// and every other refusal answers AS A REDIRECT, which is not a stylistic
// split: a request whose client or redirect cannot be trusted must never be
// bounced to the URI it named, because that URI is the attacker's in exactly
// the case the check exists for.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if !s.tenantMatches(w, r) {
		return
	}
	q := r.URL.Query()
	s.mu.Lock()
	clientID, redirectURI := s.clientID, s.redirectURI
	consentRequired, interactionRequired := s.consentRequired, s.interactionRequired
	consented := slices.Clone(s.consented)
	s.mu.Unlock()

	if q.Get("client_id") != clientID {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":             ErrInvalidClient,
			"error_description": "AADSTS700016: application with identifier " + q.Get("client_id") + " was not found in the directory",
		})
		return
	}
	if q.Get("redirect_uri") != redirectURI {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":             ErrInvalidRequest,
			"error_description": "AADSTS50011: the redirect URI specified in the request does not match the redirect URIs configured for the application",
		})
		return
	}
	state := q.Get("state")
	if rt := q.Get("response_type"); rt != "code" {
		redirectError(w, r, redirectURI, state, ErrUnsupportedResponseType, "only the authorization code flow is supported (got "+rt+")")
		return
	}
	if q.Get("code_challenge_method") != "S256" {
		redirectError(w, r, redirectURI, state, ErrInvalidRequest,
			"code_challenge_method must be S256 (got "+q.Get("code_challenge_method")+")")
		return
	}
	if q.Get("code_challenge") == "" {
		redirectError(w, r, redirectURI, state, ErrInvalidRequest, "code_challenge is required")
		return
	}
	if interactionRequired {
		redirectError(w, r, redirectURI, state, ErrInteractionRequired,
			"AADSTS50076: due to a configuration change made by your administrator, you must use multi-factor authentication")
		return
	}
	requested := splitScope(q.Get("scope"))
	if len(requested) == 0 {
		redirectError(w, r, redirectURI, state, ErrInvalidRequest, "scope is required")
		return
	}
	if consentRequired {
		redirectError(w, r, redirectURI, state, ErrConsentRequired,
			AADSTSConsentRequired+": the user or administrator has not consented to use the application")
		return
	}
	if bad, ok := firstUnconsented(requested, consented); !ok {
		redirectError(w, r, redirectURI, state, ErrConsentRequired,
			AADSTSConsentRequired+": the user or administrator has not consented to use the application with scope "+bad)
		return
	}

	code := randHex(16)
	s.mu.Lock()
	s.codes[code] = codeGrant{
		challenge:   q.Get("code_challenge"),
		scopes:      requested,
		nonce:       q.Get("nonce"),
		redirectURI: redirectURI,
	}
	s.mu.Unlock()

	dest := redirectURI + "?code=" + url.QueryEscape(code)
	if state != "" {
		dest += "&state=" + url.QueryEscape(state)
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// redirectError bounces an OAuth error back to the redirect URI, which is how
// every refusal a client can handle must arrive.
func redirectError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, desc string) {
	dest := redirectURI + "?error=" + url.QueryEscape(code) + "&error_description=" + url.QueryEscape(desc)
	if state != "" {
		dest += "&state=" + url.QueryEscape(state)
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// ── /token ──────────────────────────────────────────────────────────────────

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if !s.tenantMatches(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeTokenError(w, http.StatusBadRequest, ErrInvalidRequest, "unparseable request body")
		return
	}
	s.mu.Lock()
	clientID, clientSecret := s.clientID, s.clientSecret
	s.mu.Unlock()
	if r.Form.Get("client_id") != clientID {
		writeTokenError(w, http.StatusUnauthorized, ErrInvalidClient,
			"AADSTS700016: application with identifier "+r.Form.Get("client_id")+" was not found in the directory")
		return
	}
	// A confidential client must present its secret; a public one must not be
	// asked for one it does not have.
	if clientSecret != "" && r.Form.Get("client_secret") != clientSecret {
		writeTokenError(w, http.StatusUnauthorized, ErrInvalidClient,
			"AADSTS7000215: invalid client secret provided")
		return
	}

	switch grant := r.Form.Get("grant_type"); grant {
	case "authorization_code":
		s.tokenFromCode(w, r)
	case "refresh_token":
		s.tokenFromRefresh(w, r)
	default:
		writeTokenError(w, http.StatusBadRequest, ErrUnsupportedGrantType,
			"this fake models authorization_code and refresh_token only (got "+grant+")")
	}
}

// tokenFromCode redeems an authorization code. The code is SINGLE-USE: it is
// removed whether or not the verifier checks out, so a replay is invalid_grant
// on its own and a failed PKCE check cannot be retried with a guessed verifier.
func (s *Server) tokenFromCode(w http.ResponseWriter, r *http.Request) {
	code := r.Form.Get("code")
	s.mu.Lock()
	grant, ok := s.codes[code]
	delete(s.codes, code)
	s.mu.Unlock()
	if !ok {
		writeTokenError(w, http.StatusBadRequest, ErrInvalidGrant,
			"AADSTS54005: the authorization code was already redeemed, or was never issued")
		return
	}
	verifier := r.Form.Get("code_verifier")
	if verifier == "" || s256(verifier) != grant.challenge {
		writeTokenError(w, http.StatusBadRequest, ErrInvalidGrant,
			"AADSTS501481: the code_verifier does not match the code_challenge supplied in the authorization request")
		return
	}
	if ru := r.Form.Get("redirect_uri"); ru != "" && ru != grant.redirectURI {
		writeTokenError(w, http.StatusBadRequest, ErrInvalidGrant,
			"AADSTS50011: the redirect URI does not match the one the code was issued for")
		return
	}
	s.issueTokens(w, grant.scopes, grant.scopes, grant.nonce, true)
}

// tokenFromRefresh redeems a refresh token, and is where the two properties the
// capture depends on live:
//
//   - NO NARROWING. Whatever `scope` the request names, the access token
//     carries the whole CONSENTED set and the response's `scope` lists all of
//     it — the measured behaviour of a real tenant (see the package doc). A
//     request naming a scope that was never consented is still refused, because
//     that is a consent question and consent is the thing that does decide.
//   - ROTATION. The response carries a new refresh token and the presented one
//     is retired, so exactly one party can hold a redeemable grant. A control
//     plane that does not persist the new one has a dead credential on its next
//     attempt, which is the failure this models.
func (s *Server) tokenFromRefresh(w http.ResponseWriter, r *http.Request) {
	presented := r.Form.Get("refresh_token")
	s.mu.Lock()
	grant, known := s.refresh[presented]
	invalidGrant, consentRequired, interactionRequired := s.invalidGrant, s.consentRequired, s.interactionRequired
	s.mu.Unlock()

	if invalidGrant {
		writeTokenError(w, http.StatusBadRequest, ErrInvalidGrant,
			"AADSTS700082: the refresh token has expired or has been revoked")
		return
	}
	if interactionRequired {
		writeTokenError(w, http.StatusBadRequest, ErrInteractionRequired,
			"AADSTS50076: a Conditional Access policy requires interactive authentication")
		return
	}
	if !known || grant.retired {
		writeTokenError(w, http.StatusBadRequest, ErrInvalidGrant,
			"AADSTS700082: the refresh token has expired or has been revoked")
		return
	}
	if consentRequired {
		writeTokenError(w, http.StatusBadRequest, ErrConsentRequired,
			AADSTSConsentRequired+": the user or administrator has not consented to use the application")
		return
	}

	consented := slices.Clone(grant.scopes)
	if requested := splitScope(r.Form.Get("scope")); len(requested) > 0 {
		if bad, ok := firstUnconsented(requested, consented); !ok {
			writeTokenError(w, http.StatusBadRequest, ErrConsentRequired,
				AADSTSConsentRequired+": the user or administrator has not consented to use the application with scope "+bad)
			return
		}
		// And then the requested subset is DISCARDED, which is the whole of
		// F-LIVE-1: the token that comes back carries everything consented.
	}

	// Retire the presented token BEFORE the new pair is minted, so there is no
	// instant in which both are redeemable.
	s.mu.Lock()
	s.refresh[presented].retired = true
	s.mu.Unlock()

	s.issueTokens(w, consented, consented, "", false)
}

// issueTokens mints the access/refresh/id triple and answers with the GRANTED
// scope string. granted is what the access token carries and consented is what
// the new refresh token keeps; on this service they are the same set, and the
// two parameters stay separate only so the difference is stated rather than
// assumed.
//
// withIDToken is false on a refresh: the real service only returns an id_token
// when `openid` is in the request, and a control-plane renewal has no use for
// one — the identity was bound once, at capture.
func (s *Server) issueTokens(w http.ResponseWriter, granted, consented []string, nonce string, withIDToken bool) {
	access := "fake-entra-access-" + randHex(16)
	refresh := "fake-entra-refresh-" + randHex(16)

	s.mu.Lock()
	s.access[access] = slices.Clone(granted)
	s.refresh[refresh] = &refreshGrant{scopes: slices.Clone(consented)}
	s.mu.Unlock()

	body := map[string]any{
		"token_type":     "Bearer",
		"expires_in":     int(accessTokenTTL.Seconds()),
		"ext_expires_in": int(accessTokenTTL.Seconds()),
		"access_token":   access,
		"refresh_token":  refresh,
		// THE GRANTED SET, which is not the requested one: a client must read
		// what it was given rather than assume it got what it asked for, and on
		// this service those differ whenever a caller asks for a subset.
		"scope": strings.Join(granted, " "),
	}
	if withIDToken {
		idToken, err := s.signIDToken(nonce)
		if err != nil {
			writeTokenError(w, http.StatusInternalServerError, "server_error", "signing the id_token failed: "+err.Error())
			return
		}
		body["id_token"] = idToken
	}
	writeJSON(w, http.StatusOK, body)
}

// signIDToken signs the tenant's id_token. The claims are the ones a consumer
// verifies (iss/aud/exp/nbf/iat/nonce) plus the two Entra adds that matter
// here: `tid`, the tenant, and `sub`, which is per-app and therefore the only
// claim an authorization binding may use. preferred_username rides along
// BECAUSE it is documented as mutable and unusable for authorization — a
// consumer that compares it has a token here that will let it.
func (s *Server) signIDToken(nonce string) (string, error) {
	s.mu.Lock()
	sub, clientID, tenant := s.subject, s.clientID, s.tenantID
	issOverride, audOverride, tidOverride := s.issuerOverride, s.audienceOverride, s.tenantClaimOverride
	s.mu.Unlock()

	iss := s.Issuer()
	if issOverride != "" {
		iss = issOverride
	}
	aud := clientID
	if audOverride != "" {
		aud = audOverride
	}
	tid := tenant
	if tidOverride != "" {
		tid = tidOverride
	}
	now := time.Now()
	claims := map[string]any{
		"iss":                iss,
		"sub":                sub,
		"aud":                aud,
		"tid":                tid,
		"iat":                now.Unix(),
		"nbf":                now.Unix(),
		"exp":                now.Add(time.Hour).Unix(),
		"ver":                "2.0",
		"name":               "Fake Person",
		"preferred_username": "fake.person@example.invalid",
		"oid":                "00000000-1111-2222-3333-444444444444",
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	sig, err := s.signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return sig.CompactSerialize()
}

// ── helpers ─────────────────────────────────────────────────────────────────

// firstUnconsented names the first requested scope that is neither in the
// consented set nor one of the OIDC scopes every app holds. ok=true means every
// requested scope is covered.
func firstUnconsented(requested, consented []string) (string, bool) {
	for _, want := range requested {
		if slices.Contains(alwaysConsented, want) || slices.Contains(consented, want) {
			continue
		}
		return want, false
	}
	return "", true
}

// splitScope splits an OAuth scope string on whitespace, dropping empties.
func splitScope(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool { return r == ' ' || r == '\t' || r == '+' })
}

// s256 is the PKCE S256 transformation: base64url(SHA-256(verifier)), unpadded.
func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("entrafake: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeTokenError is the /token error shape: the OAuth code plus Entra's
// description, which is where the AADSTS number a client classifies on lives.
func writeTokenError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]any{
		"error":             code,
		"error_description": desc,
		// error_codes is the numeric array the real service sends beside the
		// description. Kept empty rather than faked: nothing should parse it,
		// and an invented number would invite something to.
		"error_codes": []int{},
		"timestamp":   time.Now().UTC().Format("2006-01-02 15:04:05Z"),
		"trace_id":    randHex(8),
	})
}

// AuthorizeURL builds the authorization URL a browser would follow, for a
// caller that drives the flow without a control plane in front of it.
func (s *Server) AuthorizeURL(state, challenge, nonce string, scopes ...string) string {
	q := url.Values{
		"client_id":             {s.ClientID()},
		"response_type":         {"code"},
		"redirect_uri":          {s.RedirectURI()},
		"scope":                 {strings.Join(scopes, " ")},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	if nonce != "" {
		q.Set("nonce", nonce)
	}
	return fmt.Sprintf("%s/%s/oauth2/v2.0/authorize?%s", s.URL(), s.TenantID(), q.Encode())
}

// TokenURL is the token endpoint for this tenant.
func (s *Server) TokenURL() string {
	return s.URL() + "/" + s.TenantID() + "/oauth2/v2.0/token"
}

// Challenge is the S256 transformation, exported so a caller driving the flow
// by hand computes it the same way the fake checks it.
func Challenge(verifier string) string { return s256(verifier) }
