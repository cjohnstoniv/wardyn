// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// ado_entra.go is the per-user Azure DevOps SIGN-IN: the two browser doors that
// capture one person's Azure DevOps refresh token against the organisation's
// existing Entra app registration.
//
// WHY A SECOND SIGN-IN AT ALL. Wardyn is an authentication client, not an OAuth
// client: the login session keeps no access or refresh token, so there is
// nothing to exchange for an Azure DevOps token. Per-user Azure DevOps access
// needs its own capture, and this is it — authorization code with a code
// challenge, the row's scope ceiling plus offline access, redirecting to a
// control-plane callback. The person is already signed in with Entra, so this is
// usually one consent click.
//
// THE BINDING IS THE SUBJECT, AND IT FAILS CLOSED. The captured credential is
// stored under the caller's own principal, so a capture that bound to the wrong
// person would hand one human's Azure DevOps reach to another. The id_token's
// `sub` must equal the session's subject or the capture is refused with that
// reason. That comparison is only sound while both tokens come from the SAME app
// registration — `sub` is per-app by construction — which is why 0.7.10 supports
// only the login app's own client id in the login issuer's tenant and refuses
// anything else by name. Binding across app registrations would have to compare
// `preferred_username` or `email`, which Microsoft documents as mutable and
// unusable for authorization; the stable pair is not carried in the session.
//
// DEVICE CODE IS NOT AN OPTION HERE. It cannot be bound to the Wardyn session at
// all, and it is blocked outright by the Conditional Access baseline a real
// tenant was measured against.
//
// WHAT THIS CAPTURE DOES NOT BUY. The credential it stores is the person's OWN
// identity carrying everything they consented to — Entra does not narrow an
// Azure DevOps token to the scopes a request names, so no token minted from it
// bounds what one run may do. What this lane delivers is PER-PERSON
// authorization (Azure DevOps evaluates that person's own entitlements, which
// is the shared-token problem solved) and an auditable record of what each
// minted token could do. Bounding a single run is the job of Wardyn's own
// capability check in front of the resource, and it has to be airtight because
// the token is not a second line of defence.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-chi/chi/v5"
	"golang.org/x/oauth2"
)

// adoSignInCapturedAction is the one audit action the sign-in owns. Both outcomes
// ride it: `success` once the blob is stored, `failure` with a `reason` for
// every refusal, so a review reads one action rather than correlating two.
const adoSignInCapturedAction = "scm.ado.signin.captured"

// entraDefaultAuthority is the public Entra authority. Every endpoint this lane
// dials is derived from it plus the tenant, exactly as Microsoft's own client
// libraries compose them.
const entraDefaultAuthority = "https://login.microsoftonline.com"

// The offline and identity scopes every capture requests on top of the row's
// ceiling. `offline_access` is what makes a refresh token exist at all, and
// `openid` is what makes an id_token exist — without which there is no subject
// to bind identity on and the whole flow is unauthenticated capture.
const (
	entraOfflineAccessScope = "offline_access"
	entraOpenIDScope        = "openid"
)

// The one-time cookies the sign-in leg sets and the callback spends. Named and
// shaped like the login flow's: state guards the redirect, nonce binds the
// id_token, the verifier proves the exchange.
const (
	adoStateCookieName = "wardyn_ado_state"
	adoNonceCookieName = "wardyn_ado_nonce"
	adoPKCECookieName  = "wardyn_ado_pkce"
)

// adoCookieMaxAge bounds an abandoned sign-in. Ten minutes, the same as the
// login flow's one-time cookies.
const adoCookieMaxAge = 600

// The refusals this lane answers with. Held as constants so tests assert
// through them rather than through their literals, and so the one that states a
// security property reads as a sentence an operator can act on.
const (
	// adoSignInNoSessionRefusal: there is no session subject to bind to. An
	// admin-token or local-mode caller reaches the route (it is in the
	// authenticated group) and has no IdP subject, so there is nothing this
	// capture could be bound to and no honest namespace to store it under.
	adoSignInNoSessionRefusal = "an Azure DevOps sign-in must be started from a signed-in browser session: " +
		"the captured credential is bound to your identity provider subject, and an admin-token caller has none"
	// adoSignInUnconfiguredRefusal: no Azure DevOps Entra block is configured.
	adoSignInUnconfiguredRefusal = "this deployment has no Azure DevOps sign-in configured"
	// adoSignInForeignAppRefusal is THE 0.7.10 boundary. %s is the client id the
	// row named.
	adoSignInForeignAppRefusal = "refusing this Azure DevOps sign-in: it is configured against application %s, " +
		"which is not the application this console signs people in with. " +
		"The identity binding compares the identity token's subject, which is per-application, " +
		"so a different application's token cannot be bound to your session — and the claims that would " +
		"survive such a comparison (preferred_username, email) are documented as mutable and unusable for authorization. " +
		"Configure the Azure DevOps block against the sign-in application's own client id, in the sign-in tenant"
	// adoSignInSubjectMismatchRefusal is the fail-closed identity binding. It
	// names no subject: the two values are identities, and an error page is not
	// where either belongs.
	adoSignInSubjectMismatchRefusal = "refusing this Azure DevOps sign-in: the identity token's subject is not the subject of " +
		"the browser session that started it. The credential would have been stored under the wrong person, so nothing was stored"
	// adoSignInWrongTenantRefusal: the id_token was issued by a tenant other
	// than the configured one.
	adoSignInWrongTenantRefusal = "refusing this Azure DevOps sign-in: the identity token was issued by a different tenant " +
		"than the one this deployment is configured for"
)

// The console destinations the callback lands on. The split mirrors the login
// callback's: a refusal the PERSON can act on redirects so the console can
// explain it, and an ATTACK-shaped refusal (a forged state, a missing one-time
// cookie, a subject that is not theirs) answers loudly in band, because an
// operator needs to see that fail rather than have it routed into a retry loop.
const (
	adoSignInDonePath  = "/?ado_signin=connected"
	adoSignInErrorPath = "/?ado_signin_error="
)

// ADOEntraConfig is the Azure DevOps Entra app registration one provider row
// signs people in against. It is passed in rather than read off a row here on
// purpose: the provider-row type and its validation live elsewhere, and the
// sign-in needs nothing from them but these fields.
type ADOEntraConfig struct {
	// RowID identifies the provider row this capture belongs to. It is part of
	// the stored secret's name, so a deployment offering two Azure DevOps
	// organisations keeps two credentials that cannot overwrite each other.
	RowID string
	// TenantID / ClientID / ClientSecret are the app registration. ClientSecret
	// is optional: a public client authenticates with the code challenge alone,
	// which is the posture a browser-delivered redirect wants.
	TenantID     string
	ClientID     string
	ClientSecret string
	// RedirectURL is the absolute callback URL registered on the application.
	// It must be the control plane's own callback route.
	RedirectURL string
	// Scopes is the row's CEILING: the vso.* scopes this deployment may ever
	// ask for. A sign-in requests these (plus offline access and openid); a
	// later redemption may ask for any subset.
	Scopes []string
	// LoginClientID / LoginTenantID are the application and tenant THIS CONSOLE
	// signs people in with. They are the other half of the 0.7.10 boundary: the
	// subject comparison is only sound when the two tokens come from one app
	// registration, so a row naming anything else is refused by name. Empty
	// values refuse every capture, which is the fail-closed direction.
	LoginClientID string
	LoginTenantID string
	// AuthorityOverride re-points every Entra endpoint at another base URL. It
	// is TEST ONLY and honoured only under AllowTestEndpoints — see
	// ValidateEntraAuthorityOverride.
	AuthorityOverride string
	// AllowTestEndpoints is WARDYN_ALLOW_TEST_ENDPOINTS, the repo's existing
	// acknowledgement that this deployment is a test deployment. Without it an
	// AuthorityOverride is refused rather than ignored.
	AllowTestEndpoints bool
}

// ADOEntraSource resolves the Azure DevOps Entra configuration for this
// deployment. nil (the default) means no Azure DevOps sign-in is offered and
// both routes refuse; ok=false means the same for a deployment whose row is
// absent or disabled.
//
// A function rather than a struct field so the provider row stays the single
// source of truth and this lane never caches a stale copy of it.
type ADOEntraSource func(ctx context.Context) (ADOEntraConfig, bool, error)

// ── configuration ───────────────────────────────────────────────────────────

// validate holds a resolved configuration to what every door needs before it
// composes a URL or a store name.
func (c ADOEntraConfig) validate() error {
	switch {
	case !adoEntraValidRowID(c.RowID):
		return fmt.Errorf("azure devops sign-in: provider row id %q is not usable as a store name", c.RowID)
	case c.TenantID == "" || !entraIDSafe(c.TenantID):
		return fmt.Errorf("azure devops sign-in: tenant id %q is not a usable tenant identifier", c.TenantID)
	case c.ClientID == "" || !entraIDSafe(c.ClientID):
		return fmt.Errorf("azure devops sign-in: client id %q is not a usable client identifier", c.ClientID)
	case c.RedirectURL == "":
		return fmt.Errorf("azure devops sign-in: no redirect URL is configured")
	case len(c.Scopes) == 0:
		return fmt.Errorf("azure devops sign-in: the provider row names no scopes")
	}
	if err := adoEntraCheckRequestedScopes(c.Scopes); err != nil {
		return fmt.Errorf("azure devops sign-in: %w", err)
	}
	return nil
}

// entraIDSafe is a HOST-AND-PATH-SHAPE check: the tenant id is concatenated
// into every endpoint URL and the client id into a query string, so a value
// carrying `/`, `?`, `@` or whitespace would compose a different host or a
// different request. Entra's own identifiers are GUIDs or DNS-style domain
// names, both of which this admits.
func entraIDSafe(v string) bool {
	if v == "" || len(v) > 128 {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '.' || r == '_':
		default:
			return false
		}
	}
	return true
}

// isLoginApplication reports whether this row names the application and tenant
// the console itself signs people in with — the only configuration 0.7.10
// supports, because it is the only one in which the subject comparison means
// anything.
func (c ADOEntraConfig) isLoginApplication() bool {
	if c.LoginClientID == "" || c.LoginTenantID == "" {
		return false
	}
	return strings.EqualFold(c.ClientID, c.LoginClientID) && strings.EqualFold(c.TenantID, c.LoginTenantID)
}

// authority resolves the base URL every endpoint is derived from. The override
// is REFUSED, not ignored, when the test hatch is unset: silently falling back
// to the real authority would make a misconfigured test deployment dial
// Microsoft with the fake's expectations.
func (c ADOEntraConfig) authority() (string, error) {
	if c.AuthorityOverride == "" {
		return entraDefaultAuthority, nil
	}
	return ValidateEntraAuthorityOverride(c.AuthorityOverride, c.AllowTestEndpoints)
}

// issuer is the `iss` an id_token from this tenant carries, and the URL a
// discovery client resolves.
func (c ADOEntraConfig) issuer() (string, error) {
	base, err := c.authority()
	if err != nil {
		return "", err
	}
	return base + "/" + c.TenantID + "/v2.0", nil
}

// tokenURL is the tenant's token endpoint.
func (c ADOEntraConfig) tokenURL() (string, error) {
	base, err := c.authority()
	if err != nil {
		return "", err
	}
	return base + "/" + c.TenantID + "/oauth2/v2.0/token", nil
}

// authorizeURL builds the authorization request: authorization code, an S256
// code challenge, and the scopes this sign-in asks consent for.
func (c ADOEntraConfig) authorizeURL(state, nonce, challenge string, scopes []string) (string, error) {
	base, err := c.authority()
	if err != nil {
		return "", err
	}
	q := url.Values{
		"client_id":             {c.ClientID},
		"response_type":         {"code"},
		"response_mode":         {"query"},
		"redirect_uri":          {c.RedirectURL},
		"scope":                 {strings.Join(scopes, " ")},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	return base + "/" + c.TenantID + "/oauth2/v2.0/authorize?" + q.Encode(), nil
}

// The TEST-ONLY Entra authority override, and the boot refusal/warning that
// gate it. It exists for the reason the AWS SSO endpoint override exists:
// without it, "a person signs in and their Azure DevOps access is captured"
// cannot be exercised outside a real tenant, because every endpoint is derived
// from login.microsoftonline.com and nothing could redirect it at a fake.
//
// It is refused unless WARDYN_ALLOW_TEST_ENDPOINTS is also set — the same two
// deliberate acts that hatch already requires — and everything a typo could
// hide (a missing scheme, an embedded credential, a path, a query, a fragment)
// is refused, so a mistake fails at the door rather than by dialing the wrong
// place with a real credential.
const (
	// EntraAuthorityOverrideRefusal is the refusal when the override is set
	// without the acknowledgement. %q is the offending value.
	EntraAuthorityOverrideRefusal = "refusing the Entra authority override %q — " +
		"it re-points the Azure DevOps sign-in's authorization, token and discovery endpoints at a server of " +
		"your choosing, which is a TEST hatch and never a production posture; unset it, or explicitly set " +
		"WARDYN_ALLOW_TEST_ENDPOINTS=true to acknowledge that this deployment is a test deployment"
	// EntraAuthorityOverrideWarn is what a deployment carrying the hatch should
	// log at boot. It opens with a greppable literal, because the thing that
	// must never happen is this posture going unnoticed in an inherited values
	// file.
	EntraAuthorityOverrideWarn = "wardynd: TEST HATCH ACTIVE — the Entra authority override re-points the Azure DevOps " +
		"sign-in's authorization, token and discovery endpoints at this URL. No Microsoft endpoint is contacted. " +
		"This is never a production posture; unset it and WARDYN_ALLOW_TEST_ENDPOINTS on any deployment holding a real credential."
)

// ValidateEntraAuthorityOverride validates a test-only Entra authority and
// returns the normalized base URL. Empty => ("", nil), byte-identical to a
// deployment that never heard of it.
//
// The rules are the AWS SSO override's, and loosened in the same two places a
// TEST endpoint differs from a production one: plain http:// is accepted (a
// local fake serves no TLS) and a loopback or private address is not refused
// (an in-cluster Service address is private by construction).
func ValidateEntraAuthorityOverride(raw string, allowTestEndpoints bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if !allowTestEndpoints {
		return "", fmt.Errorf(EntraAuthorityOverrideRefusal, raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("entra authority override: invalid URL %q: %w", raw, err)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "", fmt.Errorf("entra authority override: must be http:// or https:// (got %q)", raw)
	case u.User != nil:
		return "", fmt.Errorf("entra authority override: must not embed a credential (user:pass@)")
	case u.Hostname() == "":
		return "", fmt.Errorf("entra authority override: host is empty")
	case u.RawQuery != "":
		return "", fmt.Errorf("entra authority override: must not carry a query string")
	case u.Fragment != "":
		return "", fmt.Errorf("entra authority override: must not carry a fragment")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	if u.Path != "" {
		return "", fmt.Errorf("entra authority override: must not carry a path (got %q) — it is a base URL, and the authorization, token and discovery endpoints are all derived from it", raw)
	}
	return u.String(), nil
}

// ── routes ──────────────────────────────────────────────────────────────────

// mountAzureDevOpsSignInRoutes mounts the two browser doors on the
// already-authenticated human group. Both are GETs because both are browser
// navigations, and both refuse a caller with no session subject.
//
// Mounted unconditionally: with no ADOEntra source configured each one answers
// the "not configured" refusal, which is a clearer answer for a console than a
// 404 that cannot be told apart from a wrong path.
func (s *Server) mountAzureDevOpsSignInRoutes(r chi.Router) {
	r.Get("/scm/azure-devops/signin", s.handleADOSignIn)
	r.Get(adoSignInCallbackRoute, s.handleADOCallback)
}

// resolveADOEntra resolves this deployment's Azure DevOps configuration and
// applies the two checks every door shares: it must be configured and valid,
// and it must name the console's own sign-in application. ok=false means the
// response has already been written.
func (s *Server) resolveADOEntra(w http.ResponseWriter, r *http.Request) (ADOEntraConfig, bool) {
	if s.cfg.ADOEntra == nil {
		writeError(w, http.StatusNotFound, adoSignInUnconfiguredRefusal)
		return ADOEntraConfig{}, false
	}
	cfg, found, err := s.cfg.ADOEntra(r.Context())
	if err != nil {
		writeServerError(w, r, "read the Azure DevOps sign-in configuration", err)
		return ADOEntraConfig{}, false
	}
	if !found {
		writeError(w, http.StatusNotFound, adoSignInUnconfiguredRefusal)
		return ADOEntraConfig{}, false
	}
	if err := cfg.validate(); err != nil {
		writeServerError(w, r, "validate the Azure DevOps sign-in configuration", err)
		return ADOEntraConfig{}, false
	}
	// THE 0.7.10 BOUNDARY, checked before anything is set in motion: a foreign
	// application's id_token cannot be bound to this session, so a sign-in
	// against one must never be started at all.
	if !cfg.isLoginApplication() {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(adoSignInForeignAppRefusal, cfg.ClientID))
		return ADOEntraConfig{}, false
	}
	// The override is validated here too, not only where a URL is composed, so
	// a deployment carrying it without the acknowledgement is refused at the
	// door rather than halfway through a flow.
	if _, err := cfg.authority(); err != nil {
		writeServerError(w, r, "resolve the Azure DevOps sign-in authority", err)
		return ADOEntraConfig{}, false
	}
	return cfg, true
}

// handleADOSignIn starts the capture: it mints the one-time state, nonce and
// code verifier, sets them as HttpOnly cookies bound to this browser, and
// redirects to the tenant's authorization endpoint.
//
// The optional `capabilities` / `scopes` query narrows what CONSENT is asked
// for, so a capability gate can request INCREMENTAL consent for one capability
// instead of the whole ceiling. Anything outside the row's ceiling is refused
// rather than trimmed: a caller that asked for more than the deployment offers
// has a bug, and silently granting less would hide it.
//
// Consent is the only narrowing on this service that has any effect — a later
// token request naming a subset is ignored and answered with everything
// consented — so what is asked for HERE is what decides how much reach the
// captured credential ever has.
func (s *Server) handleADOSignIn(w http.ResponseWriter, r *http.Request) {
	subject := oidcHumanFromContext(r.Context())
	if subject == "" {
		writeError(w, http.StatusForbidden, adoSignInNoSessionRefusal)
		return
	}
	cfg, ok := s.resolveADOEntra(w, r)
	if !ok {
		return
	}
	asked, err := adoRequestedScopes(r.URL.Query(), cfg.Scopes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	verifier := oauth2.GenerateVerifier()
	state, nonce := adoRandomToken(), adoRandomToken()
	authURL, err := cfg.authorizeURL(state, nonce, oauth2.S256ChallengeFromVerifier(verifier),
		append(asked, entraOfflineAccessScope, entraOpenIDScope))
	if err != nil {
		writeServerError(w, r, "compose the Azure DevOps authorization request", err)
		return
	}
	http.SetCookie(w, s.adoCookie(adoStateCookieName, state))
	http.SetCookie(w, s.adoCookie(adoNonceCookieName, nonce))
	http.SetCookie(w, s.adoCookie(adoPKCECookieName, verifier))
	http.Redirect(w, r, authURL, http.StatusFound)
}

// adoRequestedScopes reads the optional narrowing query. `scopes` and
// `capabilities` are accepted as the same thing under two names, because the
// caller that will use this is a capability gate and the wire value is a scope;
// both split on space and comma.
//
// An empty query means the whole ceiling, which is the plain "connect Azure
// DevOps" button's request.
func adoRequestedScopes(q url.Values, ceiling []string) ([]string, error) {
	raw := strings.TrimSpace(q.Get("scopes"))
	if raw == "" {
		raw = strings.TrimSpace(q.Get("capabilities"))
	}
	if raw == "" {
		return slices.Clone(ceiling), nil
	}
	asked := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ' ' || r == ',' || r == '\t' || r == '+'
	})
	if err := adoEntraCheckRequestedScopes(asked); err != nil {
		return nil, err
	}
	for _, want := range asked {
		if !slices.Contains(ceiling, want) {
			return nil, fmt.Errorf("scope %q is outside what this deployment's Azure DevOps row allows", want)
		}
	}
	return asked, nil
}

// adoCookie is a short-lived HttpOnly SameSite=Lax cookie. Lax rather than
// Strict for the reason the login flow's cookies are: the callback arrives as a
// top-level cross-site navigation from the identity provider, and Strict would
// withhold the very cookies that prove it belongs to this browser.
func (s *Server) adoCookie(name, value string) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   adoCookieMaxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cfg.OIDCSecureCookies,
	}
}

func clearADOCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// consumeADOCookies proves this redirect is the one THIS browser started and
// spends the single-use cookies that prove it.
//
// EVERY COOKIE IS READ BEFORE ANY IS CLEARED, and on a failure path none is
// cleared at all — the same discipline the login callback follows and for the
// same reason: the three are single-use, so spending one on a request that
// never reaches the exchange turns a retryable error into a sign-in the human
// cannot repeat by pressing back.
//
// The state comparison is CONSTANT TIME and stays first: a callback whose state
// does not match a cookie this server set is not a sign-in this browser began,
// and nothing else about the request is worth reading until that holds.
func consumeADOCookies(w http.ResponseWriter, r *http.Request) (nonce, verifier string, ok bool) {
	stateParam := r.URL.Query().Get("state")
	stateCookie, err := r.Cookie(adoStateCookieName)
	if err != nil || stateCookie.Value == "" || stateParam == "" ||
		subtle.ConstantTimeCompare([]byte(stateParam), []byte(stateCookie.Value)) != 1 {
		http.Error(w, "invalid state parameter", http.StatusBadRequest)
		return "", "", false
	}
	nonceCookie, err := r.Cookie(adoNonceCookieName)
	if err != nil || nonceCookie.Value == "" {
		http.Error(w, "missing nonce cookie", http.StatusBadRequest)
		return "", "", false
	}
	pkceCookie, err := r.Cookie(adoPKCECookieName)
	if err != nil || pkceCookie.Value == "" {
		http.Error(w, "missing pkce cookie", http.StatusBadRequest)
		return "", "", false
	}
	clearADOCookie(w, adoStateCookieName)
	clearADOCookie(w, adoNonceCookieName)
	clearADOCookie(w, adoPKCECookieName)
	return nonceCookie.Value, pkceCookie.Value, true
}

// handleADOCallback finishes the capture: it spends the one-time cookies,
// exchanges the code with the verifier, verifies the id_token, binds identity
// on the subject and stores the refresh token under the caller's own namespace.
//
// Nothing is stored until every check has passed, so a refused capture leaves
// the person exactly as they were.
func (s *Server) handleADOCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	subject := oidcHumanFromContext(ctx)
	if subject == "" {
		writeError(w, http.StatusForbidden, adoSignInNoSessionRefusal)
		return
	}
	cfg, ok := s.resolveADOEntra(w, r)
	if !ok {
		return
	}
	nonce, verifier, ok := consumeADOCookies(w, r)
	if !ok {
		return
	}
	// The authority's own refusal arrives as a redirect parameter, and it is the
	// one failure the person can act on — so it is classified, audited and
	// routed back to the console rather than answered as a text page.
	if code := r.URL.Query().Get("error"); code != "" {
		reason := adoCaptureErrorCode(classifyADOEntraError(code, r.URL.Query().Get("error_description")))
		s.auditADOCapture(ctx, subject, cfg.RowID, "failure", map[string]any{
			"reason": reason, "tenant_id": cfg.TenantID, "client_id": cfg.ClientID,
		})
		http.Redirect(w, r, adoSignInErrorPath+reason, http.StatusFound)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code parameter", http.StatusBadRequest)
		return
	}

	resp, err := s.postADOEntraToken(ctx, cfg, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {cfg.RedirectURL},
	})
	if err != nil {
		reason := adoCaptureErrorCode(err)
		slog.WarnContext(ctx, "wardynd: the azure devops sign-in code exchange failed",
			slog.String("row", cfg.RowID), slog.String("reason", reason), slog.Any("err", err))
		s.auditADOCapture(ctx, subject, cfg.RowID, "failure", map[string]any{
			"reason": reason, "tenant_id": cfg.TenantID, "client_id": cfg.ClientID,
		})
		http.Redirect(w, r, adoSignInErrorPath+reason, http.StatusFound)
		return
	}
	// Mask BEFORE anything can log or persist either value.
	s.cfg.MaskRegistry.AddGlobal([]byte(resp.AccessToken))
	s.cfg.MaskRegistry.AddGlobal([]byte(resp.RefreshToken))

	if reason, ok := s.bindADOEntraIdentity(ctx, cfg, resp.IDToken, nonce, subject); !ok {
		s.auditADOCapture(ctx, subject, cfg.RowID, "failure", map[string]any{
			"reason": "identity_binding", "tenant_id": cfg.TenantID, "client_id": cfg.ClientID,
		})
		http.Error(w, reason, http.StatusForbidden)
		return
	}

	granted := adoEntraSplitScope(resp.Scope)
	if resp.RefreshToken == "" || len(granted) == 0 {
		// No refresh token means nothing to store and nothing to renew; no
		// granted scope means the authority told us nothing about what this
		// credential may do. Either way there is no usable capture.
		s.auditADOCapture(ctx, subject, cfg.RowID, "failure", map[string]any{
			"reason": "unusable_grant", "tenant_id": cfg.TenantID, "client_id": cfg.ClientID,
		})
		http.Error(w, "the identity provider returned no renewable Azure DevOps grant", http.StatusBadGateway)
		return
	}
	now := s.cfg.Now()
	blob := adoEntraBlob{
		RefreshToken: resp.RefreshToken,
		Scopes:       granted,
		ExpiresAt:    now.Add(time.Duration(resp.ExpiresIn) * time.Second).UTC(),
		TenantID:     cfg.TenantID,
		ClientID:     cfg.ClientID,
		Subject:      subject,
		CapturedAt:   now.UTC(),
		Source:       adoEntraSourceSignIn,
	}
	if err := s.storeADOEntraBlob(ctx, subject, cfg.RowID, blob); err != nil {
		s.auditADOCapture(ctx, subject, cfg.RowID, "failure", map[string]any{
			"reason": "store_error", "error": err.Error(), "tenant_id": cfg.TenantID, "client_id": cfg.ClientID,
		})
		http.Error(w, "storing the captured Azure DevOps sign-in failed", http.StatusInternalServerError)
		return
	}
	s.auditADOCapture(ctx, subject, cfg.RowID, "success", map[string]any{
		"tenant_id": cfg.TenantID, "client_id": cfg.ClientID,
		"scopes": granted, "source": adoEntraSourceSignIn,
		"expires_at": blob.ExpiresAt.Format(time.RFC3339),
	})
	http.Redirect(w, r, adoSignInDonePath, http.StatusFound)
}

// adoCaptureErrorCode names a CAPTURE failure for the console, which is not the
// same question the redemption classifier answers.
//
// Consent and interaction carry over verbatim: both mean the same thing at both
// doors, and both are the person's (or their admin's) to resolve. Everything
// else becomes adoCaptureExchangeFailed, because the redemption classifier's
// remaining classes are claims about a STORED credential — "dead", "never
// captured" — and at capture time there is no stored credential for them to be
// about. Reporting "your credential is dead" for a mistyped code verifier would
// send someone to re-capture a credential they do not have.
func adoCaptureErrorCode(err error) string {
	switch ADOEntraClassify(err) {
	case ADOEntraFailureConsentRequired:
		return string(ADOEntraFailureConsentRequired)
	case ADOEntraFailureInteractionRequired:
		return string(ADOEntraFailureInteractionRequired)
	default:
		return adoCaptureExchangeFailed
	}
}

// adoCaptureExchangeFailed is the console code for a capture that did not
// complete for any reason but consent or interaction.
const adoCaptureExchangeFailed = "exchange_failed"

// bindADOEntraIdentity verifies the id_token and binds the capture to the
// caller. ok=false returns the refusal sentence the caller should answer with;
// it never contains either subject.
//
// THE SUBJECT COMPARISON IS THE SECURITY PROPERTY OF THIS LANE. Everything
// above it proves the redirect belongs to this browser; only this proves the
// CREDENTIAL belongs to the person it is about to be stored under. It fails
// closed: an id_token that cannot be verified, carries the wrong tenant, or
// carries a subject that is not the session's, stores nothing.
//
// `preferred_username` and `email` are deliberately not consulted. Microsoft
// documents both as mutable and unusable for authorization, and a fallback to
// either would quietly re-base this binding on a claim an administrator can
// change.
func (s *Server) bindADOEntraIdentity(ctx context.Context, cfg ADOEntraConfig, rawIDToken, nonce, subject string) (string, bool) {
	if rawIDToken == "" {
		return "the identity provider returned no identity token, so this sign-in could not be bound to your session", false
	}
	issuer, err := cfg.issuer()
	if err != nil {
		return err.Error(), false
	}
	client := &http.Client{Transport: http.DefaultTransport, Timeout: adoEntraRedeemTimeout}
	provider, err := gooidc.NewProvider(gooidc.ClientContext(ctx, client), issuer)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: resolving the azure devops sign-in issuer failed",
			slog.String("row", cfg.RowID), slog.Any("err", err))
		return "the identity provider's configuration could not be read, so the identity token could not be verified", false
	}
	idToken, err := provider.Verifier(&gooidc.Config{ClientID: cfg.ClientID}).Verify(ctx, rawIDToken)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: verifying the azure devops sign-in identity token failed",
			slog.String("row", cfg.RowID), slog.Any("err", err))
		return "the identity token could not be verified", false
	}
	if idToken.Nonce != nonce {
		return "the identity token does not carry the nonce this sign-in was started with", false
	}
	var claims struct {
		TenantID string `json:"tid"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return "the identity token's claims could not be read", false
	}
	if !strings.EqualFold(claims.TenantID, cfg.TenantID) {
		return adoSignInWrongTenantRefusal, false
	}
	// Constant time, and on the RAW subjects: a subject is opaque IdP output,
	// so it is compared byte for byte and never case-folded or trimmed.
	if idToken.Subject == "" || subtle.ConstantTimeCompare([]byte(idToken.Subject), []byte(subject)) != 1 {
		slog.WarnContext(ctx, "wardynd: refused an azure devops sign-in whose identity token subject is not the session's",
			slog.String("row", cfg.RowID))
		return adoSignInSubjectMismatchRefusal, false
	}
	return "", true
}

// adoRandomToken mints a one-time state or nonce: 32 bytes of randomness,
// base64url, unpadded.
func adoRandomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on any platform this daemon runs on, and a
		// state or nonce that is not random is not one. Refusing loudly is the
		// only safe answer.
		panic(errors.New("api: crypto/rand failed while minting an Azure DevOps sign-in state"))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// adoSignInCallbackRoute is the callback's route inside the /api/v1 group, and
// ADOEntraCallbackPath is the same route as the ABSOLUTE path a browser is sent
// back to — exported for cmd/wardynd, which composes the registered redirect
// URL from it rather than typing the path a second time.
const (
	adoSignInCallbackRoute = "/scm/azure-devops/callback"
	ADOEntraCallbackPath   = "/api/v1" + adoSignInCallbackRoute
)
