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
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Config holds the OIDC client configuration. All fields except
// AllowedEmailDomains and ClientSecret (C2: optional, for a public client)
// are required.
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
	// OPTIONAL (C2): empty registers this as a PUBLIC client — PKCE S256 is
	// sent on every login regardless (LoginHandler), so a public client is
	// not weaker, just a different registration shape some IdPs require
	// (a SPA/native-app client type with no secret at all). See New's
	// AuthStyleInParams wiring for why omitting the field here is not
	// enough on its own.
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
	// Revocations is the pg-backed revoke-a-human-now lever (D16). Sessions
	// are stateless signed cookies with no server-side session table (see the
	// package doc's "Session storage" section), so there is nothing to delete
	// on revoke — instead Revocations tracks a per-principal (and a global)
	// CUTOFF time, and Middleware treats any session whose IssuedAt is
	// at-or-before the applicable cutoff as invalid. nil (the default) means
	// revocation is never checked — unset changes nothing, same as every
	// other optional Config field.
	Revocations SessionRevocations

	// RoleMappings is the console-managed (Getting Started → People) role-
	// mapping store. When wired, CallbackHandler reads it once per login and
	// MERGES its rows with the chart's WARDYN_OIDC_ROLE_MAP (RoleMap, above) —
	// see mergeRoleMaps: the chart wins a duplicate key. A wired store's read
	// error DENIES the login exactly like the no-match case, via the distinct
	// authErrorRoleCheckUnavailable code — it never falls back to the
	// env-only map on error, since that could WIDEN access under
	// DefaultRole=admin. nil (the default) is env-only role derivation,
	// byte-identical to before this field existed — the same
	// optional-Config-field rule Revocations, above, follows.
	RoleMappings RoleMappingSource

	// OnLogin, when set, is called synchronously from CallbackHandler after a
	// login is APPROVED (role derived, session about to be issued) with the
	// ID token's sub and the freshly-derived role. It exists for exactly one
	// caller today — internal/api wires it to refresh ssh_public_keys.role /
	// role_checked_at (migration 0046) for every key this principal owns, the
	// bounded-stale re-check the SSH gateway's admin override reads — but this
	// package stays store-agnostic: it knows nothing about SSH keys, only that
	// a login happened. A failure inside OnLogin must never fail the login
	// itself (the integrator is expected to log-and-continue, not panic);
	// CallbackHandler does not inspect its return because it has none. nil
	// (the default) is a plain no-op, so every existing caller is unaffected.
	OnLogin func(ctx context.Context, sub, role string)
}

// SessionRevocations is the store D16's revoke-a-human-now admin action
// reads and writes. It is scoped to the STATELESS OIDC human session cookie
// (Session, above) — a distinct concern from internal/identity's per-run
// SPIFFE-style identity_revocations denylist, which this package never
// touches.
type SessionRevocations interface {
	// IsSessionRevoked reports whether a session for this human, issued at
	// issuedAt, must be treated as revoked — because of a revoke targeting
	// EITHER of their identities, or the reserved "" (global revoke-all) sub,
	// whichever cutoff is later.
	// issuedAt.IsZero() (a pre-D16 cookie with no iat) is always revoked once
	// ANY matching cutoff exists: an old session predating this feature has
	// no reliable issued-at to compare, so it fails closed the moment revoke
	// is used for the first time against it, rather than staying immune.
	//
	// BOTH IDENTITIES, for the reason every other user-addressing surface in
	// the product already takes: an admin naming a human "shouldn't have to
	// guess which the IdP made authoritative" (docs/OPERATIONS.md on a
	// subject_type=user capability grant, which matches the sub OR the email;
	// capabilitySubjects in internal/api offers both the same way). Revocation
	// was the one such surface keyed on sub ALONE, and on any IdP where the two
	// differ — Entra, where sub is an opaque per-app identifier — a revoke
	// naming the email stamped a cutoff that matched nobody: 204, an audited
	// success, and a compromised human still signed in. sub is compared
	// EXACTLY (an OIDC sub is opaque and case-sensitive); email is compared
	// case-insensitively, since that is how humans type one.
	//
	// ponytail: Middleware calls this on every authenticated request with no
	// in-process cache — one extra lookup per request against the store
	// wardynd already requires (Postgres), over a table holding one row per
	// revoked principal plus the global one. Add a short-TTL in-memory cache
	// keyed on sub if that round trip ever shows up in latency; a POC-scale
	// deployment's request volume doesn't justify one yet, and a cache is one
	// more place revocation could go stale.
	IsSessionRevoked(ctx context.Context, sub, email string, issuedAt time.Time) (bool, error)
	// RevokeSub invalidates every CURRENT session for sub, effective now —
	// a targeted "log this one person out everywhere". sub is whichever
	// identity the caller named; IsSessionRevoked matches it against both.
	RevokeSub(ctx context.Context, sub string) error
	// RevokeAll invalidates every CURRENT session for every principal,
	// effective now — the incident-response "log everyone out" lever.
	RevokeAll(ctx context.Context) error
}

// Session is the content of the wardyn_session cookie, signed and stored
// client-side. Sub, email, role, and expiry are persisted. Role is always
// non-empty in a cookie this package issues — CallbackHandler denies the
// login rather than write one with an undefined role — and decodeSession
// treats an empty Role (a pre-0.5 cookie, or a corrupt payload) as no session.
type Session struct {
	// V is the payload format version (SessionCodecVersion), stamped by
	// encodeSession so no caller has to remember it. A payload carrying any
	// other value — a pre-0.7 cookie's absent key decodes to 0 — is not a
	// session.
	V      int       `json:"v"`
	Sub    string    `json:"sub"`
	Email  string    `json:"email"`
	Role   string    `json:"role"`
	Expiry time.Time `json:"expiry"`
	// IssuedAt (D16) is when CallbackHandler minted this cookie — the value
	// SessionRevocations.IsSessionRevoked compares against a revoke cutoff.
	// omitempty, unlike Groups below: an absent key decodes to the zero
	// time either way (a pre-D16 cookie or a same-version cookie that
	// happened to omit it are indistinguishable, and both SHOULD read as
	// "issued at the beginning of time" — see IsSessionRevoked's doc — so
	// there is no second state worth spending cookie bytes to keep apart).
	IssuedAt time.Time `json:"iat,omitempty"`
	// Groups is the LOGIN-TIME SNAPSHOT of the human's group identity: the
	// normalized union of the ID token's "roles" and "groups" claims (see
	// sessionGroups). It is what a `group`-subject capability grant matches
	// against — folding Entra App Roles in for free, since those arrive on
	// "roles" and are the claim an Entra admin can actually assign.
	//
	// It is a SNAPSHOT and nothing refreshes it: a group added at the IdP
	// reaches Wardyn on the human's next login, and that ceiling is published
	// rather than hidden (grants themselves resolve per request from the DB, so
	// only MEMBERSHIP is stale, never the grant list).
	//
	// NO omitempty, deliberately. nil and empty must stay distinguishable
	// across the cookie round trip: a PRE-0.6 cookie has no groups key at all
	// and decodes to nil ("we never asked"), while a 0.6 login with no groups
	// encodes `[]` and decodes to an empty non-nil slice ("we asked, there were
	// none"). That is the ONLY signal for groups_snapshot_stale — with
	// omitempty both cases would encode identically and a member holding a
	// pre-upgrade cookie would be told their group grants simply do not apply.
	// The cost is 12 bytes of cookie.
	//
	// decodeSession is deliberately not widened to require this FIELD — but it
	// does require the codec VERSION, and that is the fact that decides who is
	// signed out. `sess.V != SessionCodecVersion` (session_codec.go) is an exact
	// compare, and a pre-0.7 cookie carries no "v" key at all, so it decodes to
	// 0 and is refused outright: upgrading to 0.7 signs every SSO human out
	// ONCE, on their next request. This comment used to assert the opposite —
	// that an older cookie survived the upgrade and nobody was forced to sign in
	// again — and three shipped documents were written from it.
	//
	// So the nil-vs-empty signal above discriminates within ONE codec version:
	// a session this binary wrote either has groups or has `[]`. The pre-0.6
	// no-groups-key case it was designed for cannot reach the decoder any more —
	// keeping the distinction is what lets the NEXT codec-compatible change
	// carry a "we never asked" session without inventing a second flag.
	Groups []string `json:"groups"`
	// GroupsTruncated reports that Groups is a PARTIAL snapshot — sessionGroups
	// hit the maxSessionGroupsBytes cap and dropped the alphabetically-last
	// entries (PF-26).
	//
	// It is an AUTHORIZATION INPUT, not telemetry. A member in enough groups
	// loses the very group whose governance assignment walls them, and without
	// this bit the ceiling resolver would answer "no group matched" and hand
	// them the deployment ceiling — a silent widening with no refusal and no
	// audit line. internal/api therefore treats a truncated snapshot exactly as
	// it treats a missing one.
	//
	// omitempty is SAFE here only because SessionCodecVersion exists: false and
	// "key absent" mean the same thing within one codec version, and a
	// pre-0.7 cookie — where they would NOT mean the same thing — never
	// decodes at all.
	GroupsTruncated bool `json:"groups_truncated,omitempty"`
}

// Authenticator provides OIDC login, callback, logout, and session-check handlers.
type Authenticator struct {
	cfg        Config
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
	// C2: ClientSecret is OPTIONAL — a public client (PKCE S256, no secret at
	// all) is a valid registration shape, not a weaker one.
	if cfg.IssuerURL == "" || cfg.ClientID == "" || cfg.RedirectURL == "" {
		return nil, errors.New("oidc: IssuerURL, ClientID, and RedirectURL are required (ClientSecret is optional for a public client)")
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
	if cfg.ClientSecret == "" {
		// C2: a public client's token request must never include a
		// client_secret param, blank or otherwise — most IdPs reject that as
		// a malformed confidential-client request. AutoDetect (the default)
		// tries HTTP Basic auth FIRST, which x/oauth2 sends as
		// "clientID:" (an empty password), still a client_secret-shaped
		// credential the IdP may refuse for a registered public client.
		// AuthStyleInParams is the one style x/oauth2 (v0.36.0) skips the
		// client_secret form field for entirely when it's empty — see
		// oauth2.Config.Endpoint's AuthStyle doc.
		oa.Endpoint.AuthStyle = oauth2.AuthStyleInParams
	}
	warnUnrequestedGroupsScope(provider, oa.Scopes, cfg)

	verifier := provider.Verifier(&gooidc.Config{ClientID: cfg.ClientID})

	return &Authenticator{
		cfg:        cfg,
		oauth2:     oa,
		verifier:   verifier,
		hmacKey:    hmacKey,
		httpClient: httpClient,
	}, nil
}

// groupsScope is the scope name an IdP that gates its group claim expects in
// the authorization request. Wardyn does NOT request it — see
// warnUnrequestedGroupsScope for why, and for the one thing it does instead.
const groupsScope = "groups"

// warnUnrequestedGroupsScope emits the single boot line an operator gets when
// this deployment's role map is keyed on a claim the authorization request
// never asks for.
//
// The authorization request is fixed at "openid profile email" and deliberately
// does not append `groups`. Entra ID defines no such scope and rejects
// unrecognised ones, so a blanket addition breaks the documented Entra path
// outright; and even an IdP that ADVERTISES the scope may not have granted it
// to this client registration, where asking is an invalid_scope error — a login
// outage for every human, arriving on an upgrade nobody opted into. Requesting
// a scope is not a safe default, which is why this function only ever warns.
//
// The cost of not asking is real and worth naming: a scope-gated IdP simply
// omits the claim, and an omitted `groups` is byte-for-byte "asked, and there
// were none". sessionGroups reports a COMPLETE empty snapshot, deriveRole reads
// "checked, and nothing matched", and a group-keyed row decides nothing. Unlike
// an Entra overage there is no `_claim_names` marker to fail closed on — the
// IdP is not saying anything at all, so no runtime signal can tell this apart
// from a human genuinely in no groups. Boot is therefore the only honest place
// to raise it, while the operator can still act.
//
// SCOPED TO THE DEPLOYMENTS IT IS ABOUT, or it becomes the line every operator
// learns to scroll past: the provider's own discovery document must advertise a
// `groups` scope, AND the chart role map must hold a key only a CLAIM can
// answer. An email key is matched against the `email` claim this request does
// ask for, so a map keyed purely on emails is unaffected and stays silent — as
// does every Entra tenant, whose scopes_supported carries no `groups` at all.
//
// The chart map (Config.RoleMap) is what is knowable at construction. Console-
// managed rows are read per login from a store this constructor has no context
// to query, and group-subject capability grants and governance assignments live
// in the database entirely — so SILENCE HERE IS NOT A PROOF that nothing on
// this deployment depends on the group claim.
func warnUnrequestedGroupsScope(provider *gooidc.Provider, requested []string, cfg Config) {
	if slices.Contains(requested, groupsScope) {
		return
	}
	// A key holding "@" is an email, answered by a claim already requested.
	// Everything else can only arrive on `roles` or `groups`.
	var claimKeyed []string
	for k := range cfg.RoleMap {
		if !strings.Contains(k, "@") {
			claimKeyed = append(claimKeyed, k)
		}
	}
	if len(claimKeyed) == 0 {
		return
	}
	var meta struct {
		ScopesSupported []string `json:"scopes_supported"`
	}
	// A discovery document this build cannot read is not evidence that the
	// provider gates `groups`; stay quiet rather than guess at a provider's
	// posture and cry wolf on every login screen that follows.
	if err := provider.Claims(&meta); err != nil {
		return
	}
	if !slices.Contains(meta.ScopesSupported, groupsScope) {
		return
	}
	slices.Sort(claimKeyed) // stable line across restarts; map order is not
	slog.Warn("oidc: this provider advertises a `groups` scope that Wardyn does not request, and the role map is keyed on values only a claim can answer — "+
		"if the IdP gates its group claim behind that scope it will send none, which is indistinguishable from `this human is in no groups`, "+
		"so those rows would silently decide nothing",
		"issuer", cfg.IssuerURL, "scope", groupsScope, "requested_scopes", requested,
		"claim_keyed_role_map_values", claimKeyed, "env", "WARDYN_OIDC_ROLE_MAP")
}

// LoginHandler initiates the OIDC authorization code flow. It generates a
// random state and nonce, stores them in HttpOnly SameSite=Lax cookies, and
// redirects the user to the IdP authorization endpoint.
func (a *Authenticator) LoginHandler(w http.ResponseWriter, r *http.Request) {
	state := randomToken()
	nonce := randomToken()
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

// LogoutHandler clears the Wardyn session cookie and redirects to "/".
func (a *Authenticator) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	clearCookie(w, sessionCookieName)
	http.Redirect(w, r, "/", http.StatusFound)
}

// Middleware returns an http.Handler wrapper that:
//   - If a valid (non-expired, correctly signed) session cookie is present,
//     sets the HumanPrincipal on the request context and calls next.
//   - Otherwise falls through to next without a principal, allowing the
//     integrator's adminAuth bearer path to handle the request. When a
//     session cookie WAS presented but rejected (tampered/malformed, or
//     valid-but-expired), the rejection reason rides along on the context —
//     see SessionRejectedFromContext — so the integrator's eventual 401 can
//     name it instead of failing silently.
//
// This design lets the integrator compose: oidc.Middleware(adminAuth(handler)).
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, err := a.decodeSession(r)
		if err == nil {
			if time.Now().UTC().Before(sess.Expiry) {
				// D16: a revoked-but-not-yet-expired session must stop working
				// on its VERY NEXT request, not linger until the cookie's own
				// Expiry — that immediacy is the entire point of "revoke a
				// human now". Checked only when a store is actually wired
				// (nil Revocations => unset changes nothing, the same rule
				// every other optional Config field follows).
				if a.cfg.Revocations != nil {
					revoked, rerr := a.cfg.Revocations.IsSessionRevoked(r.Context(), sess.Sub, sess.Email, sess.IssuedAt)
					if rerr != nil {
						// Fail CLOSED: a store error must never look
						// indistinguishable from "not revoked" on a security
						// gate checked on every authenticated request. The
						// caller falls through with NO principal set, exactly
						// like an invalid/expired cookie — and, like them, with
						// an audit-visible reason (#19a) so the SIEM sees the
						// gate degrade rather than a silent 401.
						next.ServeHTTP(w, r.WithContext(withSessionRejected(r.Context(), "session_revocation_unavailable")))
						return
					}
					if revoked {
						clearCookie(w, sessionCookieName)
						// Post-revocation use is THE event "revoke a human now"
						// exists to make visible: surface it by name.
						next.ServeHTTP(w, r.WithContext(withSessionRejected(r.Context(), "revoked_session")))
						return
					}
				}
				// Valid session: stash the principal and continue.
				ctx := contextWithPrincipal(r.Context(), sess)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			// Expired session: clear the stale cookie so the browser doesn't
			// keep sending it, then fall through.
			clearCookie(w, sessionCookieName)
			next.ServeHTTP(w, r.WithContext(withSessionRejected(r.Context(), "expired_session")))
			return
		}
		if err == ErrInvalidSession {
			// A cookie WAS presented and failed to decode (bad signature,
			// corrupt payload) — distinct from ErrNoSession, the ordinary
			// no-cookie case every non-browser client hits on every call and
			// which is not itself audit-worthy.
			r = r.WithContext(withSessionRejected(r.Context(), "invalid_session"))
		}
		next.ServeHTTP(w, r)
	})
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// randomToken generates a cryptographically-random 128-bit base64url string.
// crypto/rand.Read never returns an error (go1.24+): it crashes the program
// irrecoverably instead, so randomToken cannot fail.
func randomToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
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

// Auth-error codes carried on the "/?auth_error=<code>" redirect
// (W31-S1-5) — stable, machine-readable strings a sign-in screen maps to a
// human message; never the raw internal error text.
const (
	authErrorEmailUnverified = "email_unverified"
	// authErrorEmailVerifiedAbsent (C1): the id_token carries no
	// email_verified claim at all — distinct from authErrorEmailUnverified,
	// which means the IdP explicitly sent false. Entra ID tokens typically
	// omit the claim entirely (AllowedEmailDomains' doc), so this is the
	// common denial shape on an Entra tenant with AllowedEmailDomains set.
	authErrorEmailVerifiedAbsent = "email_verified_absent"
	authErrorEmailDomain         = "email_domain"
	authErrorNoRole              = "no_role"
	// authErrorRoleCheckUnavailable: Config.RoleMappings is wired but
	// ListRoleMappings errored — the console role-mapping store couldn't be
	// read, distinct from authErrorNoRole's "checked, and nothing matched".
	// Fails closed: the login is denied rather than falling back to the
	// env-only WARDYN_OIDC_ROLE_MAP, which could WIDEN access under
	// WARDYN_OIDC_DEFAULT_ROLE=admin.
	authErrorRoleCheckUnavailable = "role_check_unavailable"
	// authErrorClaimsOverage: the IdP declined to send the `groups` (or
	// `roles`) claim because the human is in more groups than its token limit
	// (an Entra overage, signalled by `_claim_names`), and WARDYN_OIDC_DEFAULT_ROLE
	// would then have handed them a role WIDER than the narrowest tier on the
	// strength of a claim nobody read. Distinct from authErrorNoRole's
	// "checked, and nothing matched": here nothing was checkable. The remedy is
	// the operator's — carry the tier on Entra App Roles (the much smaller
	// `roles` claim), map the human's email directly, or stop defaulting
	// unmatched humans to admin — so retrying will not clear it.
	authErrorClaimsOverage = "claims_overage"
	// authErrorOIDCTransient (D12): the token exchange kept failing with a
	// network timeout or a 5xx from the IdP after retryExchange's retries —
	// the IdP is having a bad moment, not the deployment being misconfigured.
	// Distinguishing this from authErrorOIDCConfig is the whole point: a user
	// hitting this should just try signing in again shortly, not go file a
	// ticket about the OIDC client config.
	authErrorOIDCTransient = "oidc_transient"
	// authErrorOIDCConfig (D12): the token exchange failed with anything else
	// (invalid_client, invalid_grant on an already-consumed/expired code, a
	// 4xx the IdP won't retry its way out of) — retrying the SAME login
	// attempt cannot help; the user needs a fresh `/auth/login`, or an
	// operator needs to look at the client credentials.
	authErrorOIDCConfig = "oidc_config"
)

// redirectAuthError sends the browser back to "/" with ?auth_error=<code> —
// a real page it can act on (retry, sign out, ask an operator), rather than
// a bare http.Error text response with no way back to the console.
func redirectAuthError(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/?auth_error="+url.QueryEscape(code), http.StatusFound)
}

// ─── D12: bounded retry for a transient IdP error on the token endpoint ──────

// tokenExchangeRetries is the total number of attempts (the first try plus
// tokenExchangeRetries-1 retries) retryExchange makes against the token
// endpoint before giving up. tokenExchangeBackoff is the base delay between
// attempts, applied linearly (attempt N waits N*tokenExchangeBackoff) — short
// enough that a real user waiting on the redirect barely notices, long enough
// to ride out a blip that clears in under a second.
const (
	tokenExchangeRetries = 3
	tokenExchangeBackoff = 250 * time.Millisecond
)

// retryExchange calls oauth2.Config.Exchange, retrying up to
// tokenExchangeRetries times ONLY when isTransientOIDCErr judges the failure
// retriable (a network timeout or a 5xx from the token endpoint). Any other
// error — including invalid_grant, which a retry can never fix because the
// authorization code is single-use — returns on the first attempt.
//
// ponytail: fixed linear backoff, no jitter — this is a human waiting on a
// browser redirect for at most ~3 attempts, not a fleet of clients that could
// synchronize and thunder the IdP; add jitter if that ever becomes true.
func retryExchange(ctx context.Context, cfg oauth2.Config, code, verifier string) (*oauth2.Token, error) {
	var lastErr error
	for attempt := 1; attempt <= tokenExchangeRetries; attempt++ {
		token, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
		if err == nil {
			return token, nil
		}
		lastErr = err
		if !isTransientOIDCErr(err) || attempt == tokenExchangeRetries {
			break
		}
		select {
		case <-time.After(tokenExchangeBackoff * time.Duration(attempt)):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

// isTransientOIDCErr reports whether err looks like a passing IdP hiccup
// (worth a retry) rather than a durable configuration problem: a 5xx
// response from the token endpoint (oauth2.RetrieveError — 4xx there is the
// IdP actively rejecting the request, e.g. invalid_client/invalid_grant, and
// retrying changes nothing), or a network-level timeout.
func isTransientOIDCErr(err error) bool {
	if err == nil {
		return false
	}
	var retrieveErr *oauth2.RetrieveError
	if errors.As(err, &retrieveErr) {
		return retrieveErr.Response != nil && retrieveErr.Response.StatusCode >= 500
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}

// ─── constants ───────────────────────────────────────────────────────────────

const (
	sessionCookieName = "wardyn_session"
	stateCookieName   = "wardyn_oidc_state"
	nonceCookieName   = "wardyn_oidc_nonce"
	pkceCookieName    = "wardyn_oidc_pkce"
)

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
