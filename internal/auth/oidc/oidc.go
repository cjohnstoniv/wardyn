// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package oidc implements human SSO for Wardyn via OpenID Connect (Dex-compatible).
//
// CSRF is two-layered: a random 128-bit state value in an HttpOnly SameSite=Lax cookie, compared
// to the IdP callback; and SameSite=Lax on every session cookie. SameSite doesn't stop a same-site
// host from PLANTING a cookie, so under SecureCookies every cookie carries the __Host- prefix —
// the browser refuses a Domain= one, and the unprefixed name is never read. A PKCE code_challenge
// (S256) is verified by the token endpoint, holding even if the state check is bypassed (a mix-up
// attack). Known gap: the nonce is verified in the ID token but not bound to the device (mitigated
// by state+PKCE).
//
// Sessions live entirely in a signed HttpOnly SameSite=Lax cookie: sub, email, role, expiry,
// HMAC-SHA256 signed with the key passed to New, never logged and never leaving the process. A
// cookie with no role decodes as NO session, forcing a fresh re-derivation rather than granting an
// undefined role.
//
// Middleware accepts a valid session cookie (sets HumanPrincipal on the context) or falls through
// to the next handler (the integrator's own bearer-auth path).
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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Config holds the OIDC client configuration. All fields except
// AllowedEmailDomains and ClientSecret (C2: optional, for a public client)
// are required.
type Config struct {
	// IssuerURL is the OIDC provider's public issuer — the browser redirect target and "iss" claim.
	IssuerURL string
	// InternalIssuerURL, when set, is where wardynd itself reaches the IdP for server-side calls,
	// solving split-horizon where the browser and control plane see different hostnames.
	InternalIssuerURL string
	// ClientID is the OAuth2 client identifier registered with the IdP.
	ClientID string
	// ClientSecret is the OAuth2 client secret; never logged. Optional: empty registers a public
	// client (PKCE S256 is still sent on every login), not a weaker registration.
	ClientSecret string
	// RedirectURL is the callback URL registered with the IdP; must be <wardynd-base>/auth/callback.
	RedirectURL string
	// BasePath is WARDYN_BASE_PATH ("" = host root): prefix of every console redirect and cookie Path.
	BasePath string
	// AllowedEmailDomains, when non-empty, restricts login to emails whose domain exactly matches
	// one of the listed values case-insensitively, AND requires email_verified=true. Entra tokens
	// typically omit email_verified, fail-closing every Entra login — prefer Entra App Roles instead.
	AllowedEmailDomains []string
	// ExtraScopes is WARDYN_OIDC_EXTRA_SCOPES (CSV), appended to the fixed "openid profile email"
	// request. Validated at boot against discovery scopes_supported, so an unadvertised scope
	// refuses boot by name instead of locking every human out at login with invalid_scope.
	ExtraScopes []string
	// RoleMap maps a case-insensitive claim/email value to RoleAdmin or RoleUser (WARDYN_OIDC_ROLE_MAP,
	// parsed by ParseRoleMap). Any RoleAdmin match wins. Empty (default) keeps every human RoleAdmin.
	RoleMap map[string]string
	// DefaultRole is the role a human gets when RoleMap is non-empty but nothing matched: RoleAdmin,
	// RoleUser, or "" (default) to DENY the login.
	DefaultRole string
	// LegacyAdminEmails (WARDYN_OIDC_OPERATOR_EMAILS) is the sole admin-tier source when no RoleMap
	// is set; an email on it always derives RoleAdmin, top precedence like a RoleMap match.
	LegacyAdminEmails []string
	// SecureCookies, when true, marks every cookie Secure (HTTPS only) and adds __Host-/Path=/. MUST
	// be true exactly when the connection is TLS-protected; false is required for plain-HTTP demo
	// deployments, since a Secure cookie is never sent over plain HTTP and login would silently break.
	SecureCookies bool
	// Revocations is the pg-backed revoke-a-human-now lever: sessions are stateless signed cookies
	// with nothing to delete, so this tracks a per-principal (and global) cutoff time and Middleware
	// treats any session issued at-or-before it as invalid. nil (default) means never checked.
	Revocations SessionRevocations

	// RoleMappings is the console-managed role-mapping store, merged with RoleMap on each login
	// (RoleMap wins a duplicate key). A read error DENIES the login rather than falling back to the
	// env-only map, which could widen access under DefaultRole=admin. nil is env-only derivation.
	RoleMappings RoleMappingSource

	// UserTypes is the store of user types, read once per login beside RoleMappings. A read error
	// denies the login. nil means only "standard" exists.
	UserTypes UserTypeSource

	// OnLogin, when set, is called synchronously after an APPROVED login with the derived role/user
	// type and group snapshot. A failure inside it must never fail the login; nil is a no-op.
	OnLogin func(ctx context.Context, sub, role, userType string, groups []string, groupsTruncated bool)
}

// SessionRevocations is the store the revoke-a-human-now admin action reads and writes. Scoped to
// the stateless OIDC session cookie — distinct from internal/identity's per-run SPIFFE denylist.
type SessionRevocations interface {
	// IsSessionRevoked reports whether a session for this human, issued at issuedAt, must be treated
	// as revoked, by a revoke targeting either identity or the reserved "" (global) sub, whichever
	// cutoff is later. A zero issuedAt is always revoked once any matching cutoff exists (fail
	// closed). Checked against BOTH identities since sub and email can diverge (Entra's opaque
	// per-app sub): sub compares exactly, email case-insensitively.
	//
	// ponytail: no in-process cache; add a short-TTL cache keyed on sub if latency shows up.
	IsSessionRevoked(ctx context.Context, sub, email string, issuedAt time.Time) (bool, error)
	// RevokeSub invalidates every CURRENT session for sub, matched against both identities.
	RevokeSub(ctx context.Context, sub string) error
	// RevokeAll invalidates every CURRENT session for every principal.
	RevokeAll(ctx context.Context) error
}

// Session is the content of the wardyn_session cookie, signed and stored client-side. Role is
// always non-empty in a cookie this package issues — CallbackHandler denies rather than write an
// undefined role — and decodeSession treats an empty Role (pre-0.5, or corrupt) as no session.
type Session struct {
	// V is the payload format version (SessionCodecVersion); any other value, including an absent
	// key, is not a session.
	V     int    `json:"v"`
	Sub   string `json:"sub"`
	Email string `json:"email"`
	// Name is the IdP's display-name claim, for the console header ONLY — gates nothing, keys
	// nothing, never logged. omitempty: an absent key falls back to email (fail-safe).
	Name string `json:"name,omitempty"`
	// ObjectID is the Entra object id (`oid`) the ID token named, stamped only on an Entra sign-in.
	// It is an identity INPUT (ObjectIDFromContext: an own Azure DevOps token is bound to the person
	// by it), never logged and never echoed. omitempty: a cookie without it is still a session and
	// the reader falls back to the person row or the email, so its absence is never a refusal.
	ObjectID string    `json:"oid,omitempty"`
	Role     string    `json:"role"`
	Expiry   time.Time `json:"expiry"`
	// UserType is stamped beside the tier at sign-in; everyone carries "standard" until the role
	// map names custom ones.
	UserType string `json:"ut"`
	// IssuedAt is when CallbackHandler minted this cookie, compared against a revoke cutoff by
	// IsSessionRevoked. omitempty: an absent key decodes to the zero time either way.
	IssuedAt time.Time `json:"iat,omitempty"`
	// Groups is the LOGIN-TIME SNAPSHOT of the human's group identity (normalized union of the ID
	// token's "roles"/"groups" claims), against which a `group`-subject grant matches. Nothing
	// refreshes it — a group added at the IdP reaches Wardyn on the next login. NO omitempty,
	// deliberately: nil vs empty is the only signal for groups_snapshot_stale (nil = "never asked").
	Groups []string `json:"groups"`
	// GroupsTruncated reports Groups is a PARTIAL snapshot (hit maxSessionGroupsBytes). AUTHORIZATION
	// INPUT, not telemetry: without it the ceiling resolver would silently widen instead of refusing.
	GroupsTruncated bool `json:"groups_truncated,omitempty"`
	// MemberMode is the "view as member" flag. CLAMP INPUT only — Role stays the sign-in-derived
	// truth; the effective role is only ever LOWERED, keeping a cookie flag from being a privilege
	// primitive. omitempty: absent decodes to false (fail-safe); a rolling k8s upgrade can let an
	// old replica answer as admin for that window (documented ceiling, not proof of refusal).
	MemberMode bool `json:"mm,omitempty"`
	// MemberModeNoCredential is the mode's second posture: "view as a NEW member, not yet signed
	// in". Clamps the ROLE only — Sub (ownership, secrets, audit rows) stays the admin's own.
	MemberModeNoCredential bool `json:"mmnc,omitempty"`
	// UserViewType is the user type the user view looks through; the view always clamps the tier
	// to user regardless.
	UserViewType string `json:"uvt,omitempty"`
	// UserViewTypeName caches that type's display name from the instant the
	// view entered it (SetUserView). Meaningful only with MemberMode, same as
	// UserViewType. It exists ONLY so DropUserView can still name the type in
	// UserViewDroppedName after its row is gone — the row's deletion is WHY
	// the drop fires, so nothing can look the name up again at that point.
	UserViewTypeName string `json:"uvtn,omitempty"`
	// UserViewDropped names the type whose deletion turned the view off, so GET /me can say why
	// until the next switch clears it.
	UserViewDropped string `json:"uvd,omitempty"`
	// UserViewDroppedName is that same type's display name, copied from
	// UserViewTypeName the instant DropUserView fires, before it is cleared —
	// see UserViewTypeName's doc for why this is the only chance to record it.
	UserViewDroppedName string `json:"uvdn,omitempty"`

	// attached is the token's own Subject when admission resolved it to a
	// person set up by object id (RecordAttach). Never encoded: unexported.
	attached *Subject
}

// Authenticator provides OIDC login, callback, logout, and session-check handlers.
type Authenticator struct {
	cfg        Config
	oauth2     oauth2.Config
	verifier   *gooidc.IDTokenVerifier
	hmacKey    []byte
	httpClient *http.Client // nil means http.DefaultClient; stored for test injection

	subjectVerifier *gooidc.IDTokenVerifier // any audience: VerifySubjectToken checks it

	// groupsScopeUnrequested records whether discovery advertised a `groups` scope the
	// authorization request doesn't ask for, computed once in New. False for every Entra tenant.
	groupsScopeUnrequested bool
	// warnedMergedGroupsScope bounds the login-time warning line to ONE per process; sawGroupClaim
	// silences it for good once any login proves the IdP does send the claim.
	warnedMergedGroupsScope sync.Once
	sawGroupClaim           atomic.Bool

	// grants is the optional login-grant sink, letting a login also acquire a downstream credential.
	// Unattached (zero value), behavior matches a deployment that never heard of it.
	grants loginGrantHook

	// entra is whether IssuerURL is Microsoft Entra ID, fixed in New: the only issuer whose
	// sign-ins consult people (PersonKeying).
	entra     bool
	peopleMu  sync.RWMutex
	peopleKey PersonKeying
}

// Subject is who a verified token names, as PersonKeying reads it: the issuer and raw `sub` every
// token carries, and Entra ID's tenant (`tid`) and tenant-stable object id (`oid`) claims.
type Subject struct {
	Issuer, Sub, TenantID, ObjectID string
}

// PersonKeying resolves an Entra ID sign-in to a person an admin set up by object id before their
// first sign-in (#1195). Entra's `sub` is pairwise — per application registration, unknowable
// until that first sign-in — so such a person is keyed by (issuer, tid, oid) instead. Consulted
// only when KeysPeopleByObjectID; every other issuer signs in as its `sub`, exactly as before.
type PersonKeying interface {
	// PrincipalFor is the principal s signs in as: the person keyed by exactly s.Issuer,
	// s.TenantID and s.ObjectID if there is one and s.Sub names no one yet, else s.Sub. refused is true when s.Sub itself
	// names a person keyed by an object id this token does not carry. An email never enters it.
	PrincipalFor(ctx context.Context, s Subject) (principal string, refused bool, err error)
	// Attached records an admitted sign-in that resolved to such a person, naming both parties.
	Attached(r *http.Request, s Subject, principal string)
}

// RecordAttach reports sess's attach to PersonKeying.Attached when admission
// resolved it to a person set up by object id. Each door calls it once it has
// no refusal left to make, so a refused sign-in or exchange records none.
func (a *Authenticator) RecordAttach(r *http.Request, sess Session) {
	if k := a.personKeying(); k != nil && sess.attached != nil {
		k.Attached(r, *sess.attached, sess.Sub)
	}
}

// AttachPersonKeying joins k to this Authenticator; nil detaches.
func (a *Authenticator) AttachPersonKeying(k PersonKeying) {
	a.peopleMu.Lock()
	defer a.peopleMu.Unlock()
	a.peopleKey = k
}

func (a *Authenticator) personKeying() PersonKeying {
	a.peopleMu.RLock()
	defer a.peopleMu.RUnlock()
	return a.peopleKey
}

// KeysPeopleByObjectID reports whether a person may be set up by Entra object id here: the
// issuer is Entra ID.
func (a *Authenticator) KeysPeopleByObjectID() bool { return a.entra }

// entraIssuerHosts are the Microsoft Entra ID sign-in authorities (global, US Government, China)
// for v2.0 issuers, plus the global v1.0 issuer host, matched on the exact host.
var entraIssuerHosts = map[string]bool{
	"login.microsoftonline.com": true, "login.microsoftonline.us": true,
	"login.partner.microsoftonline.cn": true, "sts.windows.net": true,
}

func isEntraIssuer(issuer string) bool {
	u, err := url.Parse(issuer)
	return err == nil && u.Scheme == "https" && entraIssuerHosts[strings.ToLower(u.Host)]
}

// resolvePerson is admit's PersonKeying step: the principal subj signs in as, or the auth_error
// of a refusal. With nothing attached, or on any issuer but Entra, it is subj.Sub untouched. A
// lookup error denies the login, like an unreadable role-mapping store.
func (a *Authenticator) resolvePerson(r *http.Request, subj Subject, reserved func(string) bool, onDenied func(*http.Request, string)) (principal, denied string) {
	k := a.personKeying()
	if k == nil || !a.entra {
		return subj.Sub, ""
	}
	principal, refused, err := k.PrincipalFor(r.Context(), subj)
	if err != nil {
		slog.Error("oidc: pre-created person lookup unavailable, denying login (fail closed)", "error", err)
		return "", authErrorRoleCheckUnavailable
	}
	if refused || (reserved != nil && reserved(principal)) {
		slog.Warn("oidc: login denied — the subject names a person set up under an Entra object id this token does not carry",
			"sub", subj.Sub, "issuer", subj.Issuer)
		if onDenied != nil {
			onDenied(r, DenialReservedPrincipal)
		}
		return "", authErrorSignInRefused
	}
	return principal, ""
}

// New constructs an Authenticator by performing OIDC discovery against cfg.IssuerURL. hmacKey signs
// session cookies and is never logged.
func New(ctx context.Context, cfg Config, hmacKey []byte) (*Authenticator, error) {
	if cfg.IssuerURL == "" || cfg.ClientID == "" || cfg.RedirectURL == "" {
		return nil, errors.New("oidc: IssuerURL, ClientID, and RedirectURL are required (ClientSecret is optional for a public client)")
	}
	if len(hmacKey) < 32 {
		return nil, errors.New("oidc: hmacKey must be at least 32 bytes")
	}

	// Tests inject a custom transport via ctx; production ctx carries nil => http.DefaultClient.
	var httpClient *http.Client
	if c, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok {
		httpClient = c
	}

	// Split-horizon issuer: discover against the internal URL but expect the public issuer.
	discoverURL := cfg.IssuerURL
	if cfg.InternalIssuerURL != "" && cfg.InternalIssuerURL != cfg.IssuerURL {
		rt, err := newRewriteTransport(cfg.IssuerURL, cfg.InternalIssuerURL, httpClient)
		if err != nil {
			return nil, err
		}
		httpClient = &http.Client{Transport: rt}
		discoverURL = cfg.InternalIssuerURL
		// Tell go-oidc the public URL is the expected issuer even though we fetched from internal.
		ctx = gooidc.InsecureIssuerURLContext(ctx, cfg.IssuerURL)
	}
	if httpClient != nil {
		ctx = gooidc.ClientContext(ctx, httpClient)
	}

	provider, err := gooidc.NewProvider(ctx, discoverURL)
	if err != nil {
		return nil, fmt.Errorf("oidc: provider discovery for %q: %w", discoverURL, err)
	}
	// Refuse boot on an unadvertised extra scope BEFORE it reaches oa.Scopes.
	if err := validateExtraScopes(provider, cfg.ExtraScopes); err != nil {
		return nil, err
	}

	oa := oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       append([]string{gooidc.ScopeOpenID, "profile", "email"}, cfg.ExtraScopes...),
	}
	if cfg.ClientSecret == "" {
		// A public client's token request must never include a client_secret param: AutoDetect
		// tries HTTP Basic first ("clientID:" with an empty password, still client_secret-shaped),
		// so force AuthStyleInParams, the one style x/oauth2 skips the field for entirely when empty.
		oa.Endpoint.AuthStyle = oauth2.AuthStyleInParams
	}
	groupsScopeUnrequested := providerGatesGroupsScope(provider, oa.Scopes)
	warnUnrequestedGroupsScope(groupsScopeUnrequested, oa.Scopes, cfg)

	// The ID-token key set uses a JWKS-TOLERANT client so one unparseable entry skips instead of
	// failing the whole document and locking every human out. VerifierContext (not Verifier) is only
	// so the key set gets its own HTTP client, using background context since it outlives this call.
	keySetCtx := gooidc.ClientContext(context.Background(), newTolerantJWKSClient(httpClient))
	verifier := provider.VerifierContext(keySetCtx, &gooidc.Config{ClientID: cfg.ClientID})

	return &Authenticator{
		cfg:                    cfg,
		oauth2:                 oa,
		verifier:               verifier,
		subjectVerifier:        provider.VerifierContext(keySetCtx, &gooidc.Config{SkipClientIDCheck: true}),
		hmacKey:                hmacKey,
		httpClient:             httpClient,
		groupsScopeUnrequested: groupsScopeUnrequested,
		entra:                  isEntraIssuer(cfg.IssuerURL),
	}, nil
}

// groupsScope is the scope name an IdP that gates its group claim expects. Wardyn does NOT request
// it — see warnUnrequestedGroupsScope.
const groupsScope = "groups"

// warnUnrequestedGroupsScope emits the single boot line an operator gets when this deployment's
// role map is keyed on a claim the fixed scope request never asks for. Requesting `groups`
// unconditionally is not safe (Entra rejects an undefined scope outright, and an advertising IdP
// may not have granted it to this registration), so this only ever warns. The cost of not asking is
// real: a scope-gated IdP simply omits the claim, indistinguishable from "in no groups". Scoped
// narrowly (fires only when discovery advertises `groups` AND the map holds a claim-only key) so it
// isn't noise; the console-managed half is warned separately by warnMergedMapNeedsGroupsScope.
func warnUnrequestedGroupsScope(gated bool, requested []string, cfg Config) {
	if !gated {
		return
	}
	claimKeyed := claimKeyedRoleMapValues(cfg.RoleMap)
	if len(claimKeyed) == 0 {
		return
	}
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
	a.startLogin(w, r, true)
}

// startLogin is LoginHandler's body; LoginHandler always passes widen=true. The callback passes
// false to restart the SAME login without extra scopes — the recovery path that keeps a downstream
// resource's refusal from costing anyone their console session. See widenRetryableError.
func (a *Authenticator) startLogin(w http.ResponseWriter, r *http.Request, widen bool) {
	state := randomToken()
	nonce := randomToken()
	// PKCE: 32-octet verifier, meeting RFC 7636 §4.1's 43-char minimum.
	codeVerifier := oauth2.GenerateVerifier()

	// state is compared in CallbackHandler; nonce is verified in the ID token; codeVerifier is sent
	// to the token endpoint in CallbackHandler.
	http.SetCookie(w, a.loginCookie(stateCookieName, state))
	http.SetCookie(w, a.loginCookie(nonceCookieName, nonce))
	http.SetCookie(w, a.loginCookie(pkceCookieName, codeVerifier))

	opts := []oauth2.AuthCodeOption{
		oauth2.SetAuthURLParam("nonce", nonce),
		oauth2.S256ChallengeOption(codeVerifier),
	}
	// The one place this login is ever widened: an attached sink may add downstream scopes so signing
	// into the console also acquires their credential. Otherwise `scope` is left unset.
	param := ""
	if widen {
		param = a.loginScopeParam(a.extraLoginScopes(r.Context()))
	}
	if param != "" {
		opts = append(opts, oauth2.SetAuthURLParam("scope", param))
		// Marker the callback reads to know THIS redirect asked for more than a login.
		http.SetCookie(w, a.loginCookie(widenedCookieName, "1"))
	} else if _, err := r.Cookie(a.cookieName(widenedCookieName)); err == nil {
		// Clear a marker left by an earlier attempt so an unwidened callback is never retried.
		a.expireWidenedMarker(w)
	}
	authURL := a.oauth2.AuthCodeURL(state, opts...)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// LogoutHandler clears the Wardyn session cookie and redirects to the console root.
func (a *Authenticator) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	a.clearSessionCookie(w)
	http.Redirect(w, r, a.cfg.BasePath+"/", http.StatusFound)
}

// Middleware sets HumanPrincipal on the context for a valid session cookie, or falls through to
// next without one so the integrator's own bearer-auth path can handle it — composed as
// oidc.Middleware(adminAuth(handler)). A cookie presented but rejected (tampered/expired) has its
// reason ride the context (SessionRejectedFromContext) so the eventual 401 can name it.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, err := a.decodeSession(r)
		if err == nil {
			if time.Now().UTC().Before(sess.Expiry) {
				// A revoked session must stop working on its VERY NEXT request, not linger until Expiry.
				if a.cfg.Revocations != nil {
					revoked, rerr := a.cfg.Revocations.IsSessionRevoked(r.Context(), sess.Sub, sess.Email, sess.IssuedAt)
					if rerr != nil {
						// Fail CLOSED: a store error must never look like "not revoked" on a security gate.
						next.ServeHTTP(w, r.WithContext(withSessionRejected(r.Context(), "session_revocation_unavailable")))
						return
					}
					if revoked {
						a.clearSessionCookie(w)
						next.ServeHTTP(w, r.WithContext(withSessionRejected(r.Context(), "revoked_session")))
						return
					}
				}
				// Valid session: stash the principal and continue.
				ctx := contextWithPrincipal(r.Context(), sess)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			// Expired session: clear the stale cookie so the browser doesn't keep sending it.
			a.clearSessionCookie(w)
			next.ServeHTTP(w, r.WithContext(withSessionRejected(r.Context(), "expired_session")))
			return
		}
		if err == ErrInvalidSession {
			// Distinct from ErrNoSession, the ordinary no-cookie case that isn't audit-worthy.
			r = r.WithContext(withSessionRejected(r.Context(), "invalid_session"))
		}
		next.ServeHTTP(w, r)
	})
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// randomToken generates a cryptographically-random 128-bit base64url string. crypto/rand.Read
// never returns an error (go1.24+) — it crashes the program instead, so this cannot fail.
func randomToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// emailDomainAllowed returns true if the email's domain (part after last '@') matches one of the
// allowed domains (case-insensitive). Fail closed on empty/malformed addresses.
//
// SECURITY: the ASCII guard runs on the RAW domain, BEFORE the fold (same order emailInList,
// deriveRole, ParseRoleMap and CanonicalGroupSubject use): strings.ToLower does Unicode case
// MAPPING, not an ASCII fold (KELVIN SIGN U+212A maps to 'k'), so lowering first would let a domain "Korp.com" fold onto
// operator's ASCII entry on an id_token the attacker's own tenant signed; refusing a non-ASCII
// domain before the fold costs a real login nothing and closes the escalation.
func emailDomainAllowed(email string, allowed []string) bool {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	raw := email[at+1:]
	// Also makes "user@" (no domain) fail closed rather than match an empty allowlist entry.
	if raw == "" || !printableASCII(raw) {
		return false
	}
	domain := strings.ToLower(raw)
	for _, a := range allowed {
		if strings.ToLower(a) == domain {
			return true
		}
	}
	return false
}

// Auth-error codes carried on the "/?auth_error=<code>" redirect: stable, machine-readable strings
// a sign-in screen maps to a human message; never the raw internal error text.
const (
	authErrorEmailUnverified = "email_unverified"
	// authErrorEmailVerifiedAbsent: no email_verified claim at all (the common Entra denial shape),
	// distinct from authErrorEmailUnverified (IdP explicitly sent false).
	authErrorEmailVerifiedAbsent = "email_verified_absent"
	authErrorEmailDomain         = "email_domain"
	authErrorNoRole              = DenialNoRole
	// authErrorRoleCheckUnavailable: RoleMappings is wired but ListRoleMappings errored — fails
	// closed rather than falling back to the env-only map, which could WIDEN access under DefaultRole=admin.
	authErrorRoleCheckUnavailable = "role_check_unavailable"
	// authErrorClaimsOverage: the IdP omitted groups/roles from an Entra overage, so DefaultRole
	// could hand out a role wider than intended on an unreadable claim. Remedy is the operator's.
	authErrorClaimsOverage = "claims_overage"
	// authErrorOIDCTransient: exchange kept failing after retries — an IdP bad moment, not misconfig.
	authErrorOIDCTransient = "oidc_transient"
	// authErrorOIDCConfig: exchange failed with an unretriable error; the SAME attempt can't be redone.
	authErrorOIDCConfig = "oidc_config"
	// authErrorSignInRefused: a refusal (DenialReservedPrincipal) the sign-in screen shows a generic
	// sentence for; the cause is in the log, not shown to the person.
	authErrorSignInRefused = "sign_in_refused"
)

// redirectAuthError sends the browser back to "/" with ?auth_error=<code>, a real page it can act
// on, rather than a bare http.Error text response with no way back to the console.
func (a *Authenticator) redirectAuthError(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, a.cfg.BasePath+"/?auth_error="+url.QueryEscape(code), http.StatusFound)
}

// ─── bounded retry for a transient IdP error on the token endpoint ───────────

// tokenExchangeRetries is total attempts before giving up; tokenExchangeBackoff is the linear base
// delay (attempt N waits N*backoff) — short enough to not be noticed, long enough for a blip to pass.
const (
	tokenExchangeRetries = 3
	tokenExchangeBackoff = 250 * time.Millisecond
)

// retryExchange calls oauth2.Config.Exchange, retrying up to tokenExchangeRetries times only when
// isTransientOIDCErr judges the failure retriable; invalid_grant and other non-transient errors
// return on the first attempt since a retry can't fix a single-use code.
//
// ponytail: fixed linear backoff, no jitter — a human waiting on ~3 attempts, not a fleet.
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

// isTransientOIDCErr reports whether err looks like a passing IdP hiccup worth a retry: a 5xx from
// the token endpoint (a 4xx is an active rejection; retrying changes nothing) or a network timeout.
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
	// widenedCookieName marks an authorization request that asked for more than the login's own
	// scopes, so a refusal of the extras can be retried without them (login_grant.go). No secret.
	widenedCookieName = "wardyn_oidc_widened"
)

// ─── sentinel errors ─────────────────────────────────────────────────────────

// ErrNoSession is returned by decodeSession when no session cookie is present.
var ErrNoSession = errors.New("oidc: no session cookie")

// ErrInvalidSession is returned when the cookie is present but tampered, malformed, or mis-keyed.
var ErrInvalidSession = errors.New("oidc: invalid session cookie")

// ─── split-horizon issuer rewrite transport ──────────────────────────────────

// rewriteTransport rewrites the authority of every outbound request from the public issuer to the
// internal one, so wardynd reaches the IdP internally while the browser uses the public host.
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
