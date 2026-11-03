// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// endpoint_oidc.go is a custom_endpoint's own sign-in: each person signs in to the
// endpoint's authority (a provider row's `oidc` block) instead of pasting a token. It
// is the Entra capture's machinery (ado_entra_store.go, entra_capture.go) run as one
// more kind, with a token endpoint, client and identity binding of its own.
//
// WHERE A TOKEN LIVES. The refresh token is sealed in the person's own namespace at
// wardyn-provider-<uid>-oidc, written only through putOwned (storeEntraBlob), read only
// by the owner-namespace list-then-get (readEntraBlob), and reserved at every sink
// (providerSignInSecret), so no grant can name it and nothing hands it to a sandbox. An
// access token is minted per use and never stored. Neither token, nor the identity token,
// nor a code or verifier reaches a log line, an audit row, or an error a viewer sees.
//
// A TOKEN RESPONSE WITH NO `scope` is read as the requested set (RFC 6749 section 5.1),
// and a refresh sends no `scope`, for this kind only. Every Entra kind keeps refusing a
// response that names none (entra_capture.go).

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-chi/chi/v5"
	"golang.org/x/oauth2"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The refusals a provider row's `oidc` block is held to.
const (
	mp400OIDCKind      = "model_providers: %q: oidc applies only to a custom_endpoint provider"
	mp400OIDCIssuer    = "model_providers: %q: oidc.issuer is required — sign-in is discovered from it and the identity token is verified against it"
	mp400OIDCURL       = "model_providers: %q: oidc.%s: %w"
	mp400OIDCClient    = "model_providers: %q: oidc.client_id must be 1 to 256 printable characters with no spaces"
	mp400OIDCSecretRef = "model_providers: %q: oidc.client_secret_ref must be the name of an operator secret — letters, digits and ._- , at most 128 characters"
	mp400OIDCScopes    = "model_providers: %q: oidc.scopes must name 1 to %d scopes, each a single value with no .default"
	mp400OIDCBinding   = "model_providers: %q: oidc.identity_binding must be %q or %q"
)

var (
	// oidcClientIDPattern is a client id: printable ASCII, no space, bounded.
	oidcClientIDPattern = regexp.MustCompile(`^[\x21-\x7e]{1,256}$`)
	// oidcSecretRefPattern is an operator secret name a client secret may be read from.
	oidcSecretRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// normalizeProviderOIDC trims the block's free-text fields on its way to storage.
func normalizeProviderOIDC(o *types.ProviderOIDC) {
	if o == nil {
		return
	}
	o.Issuer, o.AuthorizeURL, o.TokenURL = strings.TrimSpace(o.Issuer), strings.TrimSpace(o.AuthorizeURL), strings.TrimSpace(o.TokenURL)
	o.ClientID, o.ClientSecretRef = strings.TrimSpace(o.ClientID), strings.TrimSpace(o.ClientSecretRef)
	o.IdentityBinding = strings.TrimSpace(o.IdentityBinding)
	for i := range o.Scopes {
		o.Scopes[i] = strings.TrimSpace(o.Scopes[i])
	}
}

// validateProviderOIDC holds a row's `oidc` block to its rules. allowHTTP is the test-endpoint
// posture, the one place a plain-http authority is admitted. A private address follows the
// gateway posture every admin-typed endpoint here has (validateOneLLMGateway).
func validateProviderOIDC(mp types.ModelProvider, allowHTTP bool) error {
	o := mp.OIDC
	if o == nil {
		return nil
	}
	if mp.Kind != types.ModelProviderCustomEndpoint {
		return atField("oidc", fmt.Errorf(mp400OIDCKind, mp.ID))
	}
	if o.Issuer == "" {
		return atField("oidc.issuer", fmt.Errorf(mp400OIDCIssuer, mp.ID))
	}
	for _, f := range []struct{ name, value string }{{"issuer", o.Issuer}, {"authorize_url", o.AuthorizeURL}, {"token_url", o.TokenURL}} {
		if f.value == "" {
			continue
		}
		if _, err := validateOneLLMGateway("", f.value, allowHTTP); err != nil {
			return atField("oidc."+f.name, fmt.Errorf(mp400OIDCURL, mp.ID, f.name, err))
		}
	}
	if !oidcClientIDPattern.MatchString(o.ClientID) {
		return atField("oidc.client_id", fmt.Errorf(mp400OIDCClient, mp.ID))
	}
	if o.ClientSecretRef != "" && (!oidcSecretRefPattern.MatchString(o.ClientSecretRef) || nameSinkReservedSecret(o.ClientSecretRef)) {
		return atField("oidc.client_secret_ref", fmt.Errorf(mp400OIDCSecretRef, mp.ID))
	}
	if len(o.Scopes) == 0 || len(o.Scopes) > adoEntraMaxScopes {
		return atField("oidc.scopes", fmt.Errorf(mp400OIDCScopes, mp.ID, adoEntraMaxScopes))
	}
	for _, sc := range o.Scopes {
		if sc == "" || strings.ContainsFunc(sc, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) ||
			sc == ".default" || strings.HasSuffix(sc, azureDefaultSuffix) {
			return atField("oidc.scopes", fmt.Errorf(mp400OIDCScopes, mp.ID, adoEntraMaxScopes))
		}
	}
	if b := o.IdentityBinding; b != "" && b != types.OIDCBindingConsole && b != types.OIDCBindingNone {
		return atField("oidc.identity_binding", fmt.Errorf(mp400OIDCBinding, mp.ID, types.OIDCBindingConsole, types.OIDCBindingNone))
	}
	return nil
}

// oidcAddress is a row's sign-in authority and client: what an old refresh token must never be
// redeemed against after it changes. "" for a row with no `oidc` block. Scopes are an unordered
// set. The binding is in it too, so a person's sign-in made under one rule is never kept under
// another.
func oidcAddress(p types.ModelProvider) string {
	o := p.OIDC
	if o == nil {
		return ""
	}
	return string(mustJSON([]any{o.Issuer, o.AuthorizeURL, o.TokenURL, o.ClientID, slices.Sorted(slices.Values(o.Scopes)), o.Binding()}))
}

// ── routes, names and reasons ───────────────────────────────────────────────

const (
	// endpointOIDCSignInRoute starts a sign-in for the provider row {id}; the callback is the one URL an
	// admin registers on the endpoint's client.
	endpointOIDCSignInRoute   = "/model-providers/{id}/oidc/sign-in"
	endpointOIDCCallbackRoute = "/auth/provider-oidc/callback"
	// EndpointOIDCCallbackPath is the callback as the absolute path a browser is sent back to, exported for
	// cmd/wardynd, which composes the registered redirect URL from it.
	EndpointOIDCCallbackPath = "/api/v1" + endpointOIDCCallbackRoute

	endpointOIDCDonePath  = "/?provider_oidc=connected"
	endpointOIDCErrorPath = "/?provider_oidc_error="

	// endpointOIDCCapturedAction is the one audit action a capture owns, on both outcomes.
	endpointOIDCCapturedAction = "provider.oidc.signin.capture"

	endpointOIDCStateCookie = "wardyn_poidc_state"
	endpointOIDCNonceCookie = "wardyn_poidc_nonce"
	endpointOIDCPKCECookie  = "wardyn_poidc_pkce"
)

// The reasons these doors answer with. Kept here, beside the doors.
const (
	reasonModelProviderCredentialSignsIn = "model_provider_credential_signs_in" // the paste door's 409 for a row that signs people in
	reasonEndpointOIDCUnconfigured       = "endpoint_oidc_unconfigured"         // the process offers no endpoint sign-in
	reasonEndpointOIDCUnknownRow         = "endpoint_oidc_unknown_row"          // the uid is not a custom_endpoint that signs people in
	reasonEndpointOIDCNoSession          = "endpoint_oidc_no_session"           // no session subject to bind the capture to
	reasonEndpointOIDCAuthority          = "endpoint_oidc_authority_unavailable"
	reasonEndpointOIDCCallbackInvalid    = "endpoint_oidc_callback_invalid" // the state, a one-time cookie or the stamped row is missing, repeated, replayed or malformed
	reasonEndpointOIDCMissingCode        = "endpoint_oidc_callback_missing_code"
)

const (
	endpointOIDCNoSessionRefusal = "An endpoint sign-in must be started from a signed-in browser session: " +
		"the sign-in is stored under your own identity, and an admin-token caller has none"
	endpointOIDCUnconfiguredRefusal = "This deployment offers no endpoint sign-in"
	endpointOIDCUnknownRowRefusal   = "No model provider with that uid signs people in"
	endpointOIDCAuthorityRefusal    = "The endpoint's sign-in service could not be reached, so the sign-in was not started"
	// endpointOIDCWrongIssuerRefusal and endpointOIDCSubjectMismatchRefusal name no issuer and no subject.
	endpointOIDCWrongIssuerRefusal = "the identity token was issued by an authority other than the one the console signs you in with, " +
		"and this provider is set to match your console sign-in"
	endpointOIDCSubjectMismatchRefusal = "the identity token is not for the person signed in to this browser session"
)

// EndpointOIDCEnv is what an endpoint sign-in takes from the process rather than from a provider row.
type EndpointOIDCEnv struct {
	// ConsoleIssuer is the issuer the console signs people in with. The `console` binding requires the
	// identity token's issuer to be exactly this one.
	ConsoleIssuer string
	// CallbackURL is the absolute URL of EndpointOIDCCallbackPath on the console's browser-facing origin:
	// the one redirect URI registered on every endpoint client.
	CallbackURL string
}

// endpointOIDCEnv is the process's answer, ok=false when it offers no endpoint sign-in.
func (s *Server) endpointOIDCEnv() (EndpointOIDCEnv, bool) {
	if s.cfg.EndpointOIDC == nil {
		return EndpointOIDCEnv{}, false
	}
	env, ok := s.cfg.EndpointOIDC()
	return env, ok && env.CallbackURL != ""
}

// endpointOIDCRowFor reads the enabled custom_endpoint row with this uid that signs people in.
func endpointOIDCRowFor(sc types.SiteConfig, uid string) (types.ModelProvider, bool) {
	if uid == "" {
		return types.ModelProvider{}, false
	}
	for _, p := range modelProviderRows(sc) {
		if p.UID == uid {
			return p, p.Kind == types.ModelProviderCustomEndpoint && p.OIDC != nil && !p.Disabled
		}
	}
	return types.ModelProvider{}, false
}

// endpointOIDCRequestedScopes is what a sign-in asks for: openid and offline_access, which are not settings,
// then the row's scopes.
func endpointOIDCRequestedScopes(o *types.ProviderOIDC) []string {
	out := []string{entraOpenIDScope, entraOfflineAccessScope}
	for _, sc := range o.Scopes {
		if !slices.Contains(out, sc) {
			out = append(out, sc)
		}
	}
	return out
}

// endpointOIDCBinding is what a capture binds its identity token against, and what the bind step learned.
type endpointOIDCBinding struct {
	row           types.ModelProvider
	disc          endpointOIDCDiscovery
	consoleIssuer string
	// issuer and idpSub are the verified token's, set by a successful bind.
	issuer, idpSub string
}

// endpointOIDCCapture is the capture a row that signs people in serves: stored under its own sealed part. ta
// and bind are nil for a caller that only reads the stored sign-in.
func endpointOIDCCapture(row types.ModelProvider, ta *tokenAuthority, bind *endpointOIDCBinding) (entraCapture, error) {
	if row.Kind != types.ModelProviderCustomEndpoint || row.OIDC == nil {
		return entraCapture{}, fmt.Errorf("endpoint sign-in: provider %q does not sign people in", row.ID)
	}
	if !adoEntraValidRowID(row.UID) {
		return entraCapture{}, fmt.Errorf("endpoint sign-in: provider uid %q is not usable as a store name", row.UID)
	}
	return entraCapture{
		kind:       entraKindEndpointOIDC,
		scopes:     endpointOIDCRequestedScopes(row.OIDC),
		secretName: providerSecretName(row.UID, providerOIDCPart),
		donePath:   endpointOIDCDonePath,
		rowUID:     row.UID,
		authority:  ta,
		oidc:       bind,
	}, nil
}

// ── the authority: discovery, the client and the identity token ─────────────

// endpointOIDCClient talks to the authority: bounded, and it follows no redirect, since a body carries the
// client secret and the refresh token.
func endpointOIDCClient() *http.Client {
	return &http.Client{
		Transport:     http.DefaultTransport,
		Timeout:       adoEntraRedeemTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// endpointOIDCDiscovery is what the authority's discovery document gave, with the row's overrides applied.
type endpointOIDCDiscovery struct {
	provider      *gooidc.Provider
	authorizeURL  string
	tokenURL      string
	revocationURL string
	basicAuth     bool
}

// endpointOIDCURLUsable holds a discovered endpoint to the posture the row's own URLs are held to.
func endpointOIDCURLUsable(raw string, allowHTTP bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" || u.Fragment != "" {
		return false
	}
	return u.Scheme == "https" || (allowHTTP && u.Scheme == "http")
}

// discoverEndpointOIDC resolves the authority from the row's issuer. The issuer in the document must be the
// row's, which gooidc.NewProvider checks.
func (s *Server) discoverEndpointOIDC(ctx context.Context, o *types.ProviderOIDC) (endpointOIDCDiscovery, error) {
	provider, err := gooidc.NewProvider(gooidc.ClientContext(ctx, endpointOIDCClient()), o.Issuer)
	if err != nil {
		return endpointOIDCDiscovery{}, fmt.Errorf("discover the endpoint's authority: %w", err)
	}
	var meta struct {
		Revocation  string   `json:"revocation_endpoint"`
		AuthMethods []string `json:"token_endpoint_auth_methods_supported"`
	}
	if err := provider.Claims(&meta); err != nil {
		return endpointOIDCDiscovery{}, fmt.Errorf("read the endpoint's discovery document: %w", err)
	}
	ep := provider.Endpoint()
	d := endpointOIDCDiscovery{
		provider:      provider,
		authorizeURL:  cmp.Or(o.AuthorizeURL, ep.AuthURL),
		tokenURL:      cmp.Or(o.TokenURL, ep.TokenURL),
		revocationURL: meta.Revocation,
		// RFC 6749 section 2.3.1: Basic is what every authority supports, so it is used unless discovery lists
		// methods that leave it out and name the form.
		basicAuth: len(meta.AuthMethods) == 0 || slices.Contains(meta.AuthMethods, "client_secret_basic") || !slices.Contains(meta.AuthMethods, "client_secret_post"),
	}
	allowHTTP := s.cfg.AllowTestEndpoints
	if !endpointOIDCURLUsable(d.authorizeURL, allowHTTP) || !endpointOIDCURLUsable(d.tokenURL, allowHTTP) {
		return endpointOIDCDiscovery{}, errors.New("the endpoint's authority names an authorize or token URL this deployment will not use")
	}
	if !endpointOIDCURLUsable(d.revocationURL, allowHTTP) {
		d.revocationURL = ""
	}
	return d, nil
}

// endpointOIDCClientSecret reads the client secret from the operator namespace by the row's ref; "" for a
// public client. A sealed name is refused here as well as at the write.
func (s *Server) endpointOIDCClientSecret(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	if s.cfg.Secrets == nil || nameSinkReservedSecret(ref) {
		return "", errors.New("the client secret cannot be read")
	}
	raw, err := s.cfg.Secrets.Get(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("read the client secret %q: %w", ref, err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// endpointOIDCAuthority is the token endpoint and client a row's requests go to, with the discovery behind it.
func (s *Server) endpointOIDCAuthority(ctx context.Context, o *types.ProviderOIDC) (tokenAuthority, endpointOIDCDiscovery, error) {
	d, err := s.discoverEndpointOIDC(ctx, o)
	if err != nil {
		return tokenAuthority{}, d, err
	}
	secret, err := s.endpointOIDCClientSecret(ctx, o.ClientSecretRef)
	if err != nil {
		return tokenAuthority{}, d, err
	}
	return tokenAuthority{tokenURL: d.tokenURL, clientID: o.ClientID, clientSecret: secret, basicAuth: d.basicAuth && secret != ""}, d, nil
}

// endpointOIDCSigningAlgs are the algorithms an identity token may be signed with. Symmetric ones and "none"
// are not in it.
var endpointOIDCSigningAlgs = []string{"RS256", "ES256", "PS256"}

// endpointOIDCHeaderRefusal refuses a token before any key is fetched: it must be a three-part token whose
// header names an accepted algorithm and a key id.
func endpointOIDCHeaderRefusal(raw string) string {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "the identity token is not a signed token"
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[0])
	var h struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err != nil || json.Unmarshal(b, &h) != nil {
		return "the identity token's header could not be read"
	}
	if !slices.Contains(endpointOIDCSigningAlgs, h.Alg) {
		return "the identity token is signed with an algorithm this sign-in does not accept"
	}
	if h.Kid == "" {
		return "the identity token names no signing key"
	}
	return ""
}

// scrubSecrets removes every non-empty secret from a message a log line is about to carry.
func scrubSecrets(msg string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			msg = strings.ReplaceAll(msg, sec, "[redacted]")
		}
	}
	return msg
}

// verifyEndpointOIDCIDToken checks the identity token against the issuer's keys from discovery: the signature
// (an accepted algorithm and a key id), iss, aud and exp, then the nonce. A non-empty reason is a refusal.
func (s *Server) verifyEndpointOIDCIDToken(ctx context.Context, b *endpointOIDCBinding, raw, nonce string) (*gooidc.IDToken, string) {
	if raw == "" {
		return nil, "the authority returned no identity token, so this sign-in could not be bound to your session"
	}
	if reason := endpointOIDCHeaderRefusal(raw); reason != "" {
		return nil, reason
	}
	cfg := &gooidc.Config{ClientID: b.row.OIDC.ClientID, SupportedSigningAlgs: endpointOIDCSigningAlgs}
	idToken, err := b.disc.provider.Verifier(cfg).Verify(gooidc.ClientContext(ctx, endpointOIDCClient()), raw)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: verifying an endpoint sign-in identity token failed",
			slog.String("row", b.row.UID), slog.String("err", scrubSecrets(err.Error(), raw)))
		return nil, "the identity token could not be verified"
	}
	if nonce == "" || subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(nonce)) != 1 {
		return nil, "the identity token does not carry the nonce this sign-in was started with"
	}
	return idToken, ""
}

// bindEndpointOIDCIdentity verifies the identity token and, under the row's binding, ties it to the signed-in
// person. It fails closed and its refusal names neither an issuer nor a subject. A successful bind leaves the
// token's issuer and subject on b.
//
// Matching on email or preferred_username is not an option of either binding: both are mutable at the authority.
func (s *Server) bindEndpointOIDCIdentity(ctx context.Context, b *endpointOIDCBinding, rawIDToken, nonce, subject string) (string, bool) {
	idToken, reason := s.verifyEndpointOIDCIDToken(ctx, b, rawIDToken, nonce)
	if reason != "" {
		return reason, false
	}
	if idToken.Subject == "" {
		return "the identity token names no subject", false
	}
	if b.row.OIDC.Binding() == types.OIDCBindingConsole {
		if reason, ok := s.bindEndpointOIDCConsole(ctx, b, idToken, subject); !ok {
			return reason, false
		}
	}
	b.issuer, b.idpSub = idToken.Issuer, idToken.Subject
	return "", true
}

// bindEndpointOIDCConsole is the `console` binding: the token's issuer must be the console login's, then the
// person it names resolves as the console resolves them, and that principal must be the session's, compared in
// constant time on the raw values.
func (s *Server) bindEndpointOIDCConsole(ctx context.Context, b *endpointOIDCBinding, idToken *gooidc.IDToken, subject string) (string, bool) {
	if b.consoleIssuer == "" || subtle.ConstantTimeCompare([]byte(idToken.Issuer), []byte(b.consoleIssuer)) != 1 {
		return endpointOIDCWrongIssuerRefusal, false
	}
	var claims struct {
		TenantID string `json:"tid"`
		ObjectID string `json:"oid"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return "the identity token's claims could not be read", false
	}
	principal := idToken.Subject
	if ps, ok := s.cfg.Store.(store.PersonStore); ok {
		who, refused, err := peopleKeying{s: s, ps: ps}.PrincipalFor(ctx, oidc.Subject{
			Issuer: idToken.Issuer, Sub: idToken.Subject, TenantID: claims.TenantID, ObjectID: claims.ObjectID,
		})
		if err != nil {
			slog.WarnContext(ctx, "wardynd: resolving the endpoint sign-in person failed", slog.String("row", b.row.UID), slog.Any("err", err))
			return "the person this identity token names could not be looked up, so this sign-in could not be bound to your session", false
		}
		if refused {
			who = ""
		}
		principal = who
	}
	if principal == "" || subtle.ConstantTimeCompare([]byte(principal), []byte(subject)) != 1 {
		slog.WarnContext(ctx, "wardynd: refused an endpoint sign-in whose identity token is not the session's person", slog.String("row", b.row.UID))
		return endpointOIDCSubjectMismatchRefusal, false
	}
	return "", true
}

// ── the door and its callback ───────────────────────────────────────────────

// endpointOIDCStamp is what the start leg stamps in the state cookie beside the random state. The cookie is
// not signed, so the callback treats every field but State as a claim to re-check against the live row.
type endpointOIDCStamp struct {
	State  string `json:"s"`
	RowUID string `json:"u"`
	Digest string `json:"d"`
}

func (st endpointOIDCStamp) encode() string {
	raw, err := json.Marshal(st)
	if err != nil {
		panic(fmt.Errorf("api: encode the endpoint sign-in state: %w", err))
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeEndpointOIDCStamp(v string) (endpointOIDCStamp, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return endpointOIDCStamp{}, false
	}
	var st endpointOIDCStamp
	if err := json.Unmarshal(raw, &st); err != nil || st.State == "" || st.RowUID == "" {
		return endpointOIDCStamp{}, false
	}
	return st, true
}

// endpointOIDCSpent holds the states this process has seen come back, so a state is single use even when a
// browser replays its cookies. An unbounded table would be a memory lever, so a full one refuses.
var endpointOIDCSpent = struct {
	sync.Mutex
	until map[[sha256.Size]byte]time.Time
}{until: map[[sha256.Size]byte]time.Time{}}

const endpointOIDCSpentMax = 10000

// spendEndpointOIDCState records state as used and reports whether it was unused. False when it was spent
// already, or when the table is full.
func spendEndpointOIDCState(state string, now time.Time) bool {
	key := sha256.Sum256([]byte(state))
	endpointOIDCSpent.Lock()
	defer endpointOIDCSpent.Unlock()
	for k, until := range endpointOIDCSpent.until {
		if now.After(until) {
			delete(endpointOIDCSpent.until, k)
		}
	}
	if _, used := endpointOIDCSpent.until[key]; used || len(endpointOIDCSpent.until) >= endpointOIDCSpentMax {
		return false
	}
	endpointOIDCSpent.until[key] = now.Add(2 * adoCookieMaxAge * time.Second)
	return true
}

// endpointOIDCAuthorizeURL composes the authorization request: the code flow with an S256 challenge, the exact
// redirect URI the admin registered, and the state and nonce of this attempt.
func endpointOIDCAuthorizeURL(base, clientID, redirectURL, state, nonce, challenge string, scopes []string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("client_id", clientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURL)
	q.Set("scope", strings.Join(scopes, " "))
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// handleEndpointOIDCSignIn starts a sign-in for the provider row {id}: it sets the one-time state, nonce and
// verifier cookies and redirects to the authority's authorization endpoint.
//
//	GET /api/v1/model-providers/{id}/oidc/sign-in
func (s *Server) handleEndpointOIDCSignIn(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if oidcHumanFromContext(ctx) == "" {
		writeErrorReason(w, http.StatusForbidden, reasonEndpointOIDCNoSession, endpointOIDCNoSessionRefusal)
		return
	}
	env, ok := s.endpointOIDCEnv()
	if !ok || s.cfg.Store == nil {
		writeErrorReason(w, http.StatusNotFound, reasonEndpointOIDCUnconfigured, endpointOIDCUnconfiguredRefusal)
		return
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		writeServerError(w, r, "reading the site configuration failed", err)
		return
	}
	row, ok := endpointOIDCRowFor(sc, chi.URLParam(r, "id"))
	if !ok {
		writeErrorReason(w, http.StatusNotFound, reasonEndpointOIDCUnknownRow, endpointOIDCUnknownRowRefusal)
		return
	}
	disc, err := s.discoverEndpointOIDC(ctx, row.OIDC)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: resolving an endpoint sign-in authority failed", slog.String("row", row.UID), slog.Any("err", err))
		writeErrorReason(w, http.StatusBadGateway, reasonEndpointOIDCAuthority, endpointOIDCAuthorityRefusal)
		return
	}
	verifier := oauth2.GenerateVerifier()
	state, nonce := adoRandomToken(), adoRandomToken()
	authURL, err := endpointOIDCAuthorizeURL(disc.authorizeURL, row.OIDC.ClientID, env.CallbackURL, state, nonce,
		oauth2.S256ChallengeFromVerifier(verifier), endpointOIDCRequestedScopes(row.OIDC))
	if err != nil {
		writeServerError(w, r, "composing the authorization request failed", err)
		return
	}
	stamp := endpointOIDCStamp{State: state, RowUID: row.UID, Digest: providerAddressDigest(row)}
	http.SetCookie(w, s.adoCookie(endpointOIDCStateCookie, stamp.encode()))
	http.SetCookie(w, s.adoCookie(endpointOIDCNonceCookie, nonce))
	http.SetCookie(w, s.adoCookie(endpointOIDCPKCECookie, verifier))
	http.Redirect(w, r, authURL, http.StatusFound)
}

// endpointOIDCCallbackFlow proves this redirect is the attempt THIS browser started and spends it. The state
// must appear exactly once in the query and equal the one in the state cookie, the nonce and verifier cookies
// must be there, and the state must not have come back before. Only then are the cookies cleared. Any failure
// answers one in-band 400 that says nothing about which check failed.
func (s *Server) endpointOIDCCallbackFlow(w http.ResponseWriter, r *http.Request) (stamp endpointOIDCStamp, nonce, verifier string, ok bool) {
	bad := func() (endpointOIDCStamp, string, string, bool) {
		writeErrorReason(w, http.StatusBadRequest, reasonEndpointOIDCCallbackInvalid, "invalid state parameter")
		return endpointOIDCStamp{}, "", "", false
	}
	q := r.URL.Query()
	if len(q["state"]) != 1 || q.Get("state") == "" || len(q["code"]) > 1 || len(q["error"]) > 1 {
		return bad()
	}
	stateCookie, err := r.Cookie(s.consoleCookieName(endpointOIDCStateCookie))
	if err != nil {
		return bad()
	}
	stamp, decoded := decodeEndpointOIDCStamp(stateCookie.Value)
	if !decoded || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(stamp.State)) != 1 || !adoEntraValidRowID(stamp.RowUID) {
		return bad()
	}
	nonceCookie, nerr := r.Cookie(s.consoleCookieName(endpointOIDCNonceCookie))
	pkceCookie, perr := r.Cookie(s.consoleCookieName(endpointOIDCPKCECookie))
	if nerr != nil || perr != nil || nonceCookie.Value == "" || pkceCookie.Value == "" {
		return bad()
	}
	if !spendEndpointOIDCState(stamp.State, s.cfg.Now()) {
		return bad()
	}
	s.clearADOCookie(w, endpointOIDCStateCookie)
	s.clearADOCookie(w, endpointOIDCNonceCookie)
	s.clearADOCookie(w, endpointOIDCPKCECookie)
	return stamp, nonceCookie.Value, pkceCookie.Value, true
}

// handleEndpointOIDCCallback finishes a sign-in. The row is the state cookie's claim, re-read from the live site
// configuration before the code is spent and again under the site-config lock at the write; nothing is stored
// unless every check holds. A refusal the person can act on redirects to the console with a fixed code.
//
//	GET /api/v1/auth/provider-oidc/callback
func (s *Server) handleEndpointOIDCCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	subject := oidcHumanFromContext(ctx)
	if subject == "" {
		writeErrorReason(w, http.StatusForbidden, reasonEndpointOIDCNoSession, endpointOIDCNoSessionRefusal)
		return
	}
	stamp, nonce, verifier, ok := s.endpointOIDCCallbackFlow(w, r)
	if !ok {
		return
	}
	target := providerSecretName(stamp.RowUID, providerOIDCPart)
	fail := func(reason string) {
		s.auditEndpointOIDC(ctx, subject, target, "failure", map[string]any{"reason": reason})
		http.Redirect(w, r, s.cfg.BasePath+endpointOIDCErrorPath+reason+"&provider="+url.QueryEscape(stamp.RowUID), http.StatusFound)
	}
	env, offered := s.endpointOIDCEnv()
	if !offered || s.cfg.Store == nil {
		fail(azureCaptureRowChanged)
		return
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: reading the site configuration failed", slog.String("row", stamp.RowUID), slog.Any("err", err))
		fail(reasonStoreError)
		return
	}
	row, found := endpointOIDCRowFor(sc, stamp.RowUID)
	if !found || providerAddressDigest(row) != stamp.Digest {
		fail(azureCaptureRowChanged)
		return
	}
	if code := r.URL.Query().Get("error"); code != "" {
		fail(adoCaptureErrorCode(classifyADOEntraError(code, r.URL.Query().Get("error_description"))))
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		writeErrorReason(w, http.StatusBadRequest, reasonEndpointOIDCMissingCode, "missing code parameter")
		return
	}
	ta, disc, err := s.endpointOIDCAuthority(ctx, row.OIDC)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: resolving an endpoint sign-in authority failed", slog.String("row", row.UID), slog.Any("err", err))
		fail(adoCaptureExchangeFailed)
		return
	}
	bind := &endpointOIDCBinding{row: row, disc: disc, consoleIssuer: env.ConsoleIssuer}
	ec, err := endpointOIDCCapture(row, &ta, bind)
	if err != nil {
		fail(azureCaptureRowChanged)
		return
	}
	cfg := ADOEntraConfig{RowID: row.UID, ClientID: row.OIDC.ClientID, RedirectURL: env.CallbackURL}
	grant, reason := s.redeemEntraCode(ctx, cfg, ec, subject, code, verifier, nonce)
	if reason != "" {
		fail(reason)
		return
	}
	blob := grant.blob
	// The sign-in lock is taken after the exchange, as the Azure capture does, and the store below takes the
	// site-config lock inside it.
	ctx, unlock, err := s.lockADOSignIn(ctx, subject, ec.rowUID)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: could not take the endpoint sign-in lock", slog.String("row", ec.rowUID), slog.Any("err", err))
		fail(reasonStoreError)
		return
	}
	defer unlock()
	reason, err = s.storeEndpointOIDCCapture(ctx, subject, stamp, ec, blob)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: storing the captured endpoint sign-in failed", slog.String("row", ec.rowUID), slog.Any("err", err))
		fail(reasonStoreError)
		return
	}
	if reason != "" {
		fail(reason)
		return
	}
	s.retireReplacedEntraTokens(ctx, subject, ec, grant)
	s.auditEndpointOIDC(ctx, subject, target, "success", map[string]any{
		"scopes": blob.Scopes, "expires_at": blob.ExpiresAt.Format(time.RFC3339),
		"issuer": blob.Issuer, "idp_subject": blob.IDPSubject, "binding": row.OIDC.Binding(),
	})
	http.Redirect(w, r, s.cfg.BasePath+ec.donePath+"&provider="+url.QueryEscape(stamp.RowUID), http.StatusFound)
}

// storeEndpointOIDCCapture stores blob only while the row is still the one the sign-in was started for, under
// the site-config lock a purge also holds, so a purge cannot land between the check and the write. A non-empty
// reason means refused and nothing stored.
func (s *Server) storeEndpointOIDCCapture(ctx context.Context, subject string, stamp endpointOIDCStamp, ec entraCapture, blob adoEntraBlob) (string, error) {
	ctx, unlock, err := s.lock(ctx, db.SiteConfigLockClass)
	if err != nil {
		return "", err
	}
	defer unlock()
	if s.cfg.Store == nil {
		return "", errors.New("no store configured")
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return "", fmt.Errorf("read model providers: %w", err)
	}
	if row, found := endpointOIDCRowFor(sc, stamp.RowUID); !found || providerAddressDigest(row) != stamp.Digest {
		return azureCaptureRowChanged, nil
	}
	return "", s.storeEntraBlob(ctx, subject, ec, blob)
}

// auditEndpointOIDC writes the capture's one audit row, on both outcomes. data never carries a token, a code, a
// verifier or an identity token. Target is the sealed secret name, so whose sign-in, for which provider, is
// answerable without a join.
func (s *Server) auditEndpointOIDC(ctx context.Context, actor, target, outcome string, data map[string]any) {
	data["owner"] = actor
	data["provider"] = string(types.ModelProviderCustomEndpoint)
	data["source"] = adoEntraSourceSignIn
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorHuman, actor, endpointOIDCCapturedAction, target, outcome, mustJSON(data)))
}

// ── run time: what the dispatch lane reads and redeems ──────────────────────

// endpointOIDCBlob is owner's stored sign-in for row, read from the owner's own namespace only (never the
// operator's), with found=false when none is stored. The dispatch and status lanes read DeadAt and the shape
// from it; the refresh token in it is never theirs to use.
func (s *Server) endpointOIDCBlob(ctx context.Context, owner string, row types.ModelProvider) (adoEntraBlob, bool, error) {
	ec, err := endpointOIDCCapture(row, nil, nil)
	if err != nil {
		return adoEntraBlob{}, false, err
	}
	return s.readEntraBlob(ctx, owner, ec)
}

// RedeemEndpointOIDCAccess mints an access token for owner at row's authority and persists the rotation, under
// the (owner, row) single-flight the Entra captures use: the read is inside the lock, the Put is guarded by the
// revision read before it, and the new tokens are masked before anything can log or store them. A person whose
// sign-in was purged, erased or never made gets ErrADOEntraNotCaptured, and a refusal only a new sign-in can
// answer deletes the stored sign-in or marks it dead (noteEntraSignInEnded).
//
// A Put lost after a successful rotation serves this caller from memory, as every Entra kind does, and the
// next redemption presents a spent token: on an authority that revokes a family on reuse that is
// ErrADOEntraDeadCredential and a new sign-in, which fails closed.
func (s *Server) RedeemEndpointOIDCAccess(ctx context.Context, owner string, row types.ModelProvider) (ADOEntraAccess, error) {
	if owner == "" {
		return ADOEntraAccess{}, fmt.Errorf("%w: no principal to redeem it for", ErrADOEntraNotCaptured)
	}
	ec, err := endpointOIDCCapture(row, nil, nil)
	if err != nil {
		return ADOEntraAccess{}, err
	}
	if err := ec.checkRequested(ec.scopes); err != nil {
		return ADOEntraAccess{}, err
	}
	ta, _, err := s.endpointOIDCAuthority(ctx, row.OIDC)
	if err != nil {
		return ADOEntraAccess{}, fmt.Errorf("%w: %w", ErrADOEntraUnavailable, err)
	}
	ec.authority = &ta
	lctx, unlock, err := s.lockADOSignInRedeem(ctx, owner, row.UID)
	if err != nil {
		return ADOEntraAccess{}, err
	}
	defer unlock()
	return s.redeemEntraAccessLocked(lctx, ADOEntraConfig{}, ec, owner, ec.scopes)
}

// revokeEndpointOIDC asks the authority to revoke owner's refresh token (RFC 7009) when discovery lists a
// revocation endpoint. Best effort: a person's disconnect deletes the stored sign-in whether or not this
// worked, and nothing it logs carries a token.
func (s *Server) revokeEndpointOIDC(ctx context.Context, owner string, row types.ModelProvider) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	blob, found, err := s.endpointOIDCBlob(ctx, owner, row)
	if err != nil || !found {
		return
	}
	ta, disc, err := s.endpointOIDCAuthority(ctx, row.OIDC)
	if err != nil || disc.revocationURL == "" {
		return
	}
	form := url.Values{"token": {blob.RefreshToken}, "token_type_hint": {"refresh_token"}}
	if !ta.basicAuth {
		form.Set("client_id", ta.clientID)
		if ta.clientSecret != "" {
			form.Set("client_secret", ta.clientSecret)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, disc.revocationURL, strings.NewReader(form.Encode()))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if ta.basicAuth {
		req.SetBasicAuth(url.QueryEscape(ta.clientID), url.QueryEscape(ta.clientSecret))
	}
	resp, err := endpointOIDCClient().Do(req)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: revoking an endpoint sign-in at its authority failed", slog.String("row", row.UID))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		slog.WarnContext(ctx, "wardynd: an authority refused to revoke an endpoint sign-in",
			slog.String("row", row.UID), slog.Int("status", resp.StatusCode))
	}
}
