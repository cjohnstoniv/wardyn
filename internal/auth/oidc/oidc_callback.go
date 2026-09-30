// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

// The login callback: the IdP redirect handler and its two helpers — the
// single-use state/nonce/PKCE cookie consumption that proves the redirect
// belongs to the browser that started this login, and the ID-token claim
// decode. Split from oidc.go for the file-size gate; no new behaviour.

import (
	"log/slog"
	"net/http"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
)

// CallbackHandler handles the IdP redirect. It:
//  1. Verifies the state parameter against the state cookie (CSRF).
//  2. Exchanges the code for tokens using PKCE.
//  3. Verifies the ID token signature, issuer, audience, expiry, and nonce.
//  4. Optionally checks email_verified and the email domain (fail closed when
//     AllowedEmailDomains is set; email_verified alone when RequireEmailVerified is on).
//  5. Derives the session's role from the roles/groups/email claims (see
//     Config.RoleMap / deriveRole); denies the login if nothing matches and no
//     DefaultRole is configured.
//  6. Creates a signed Wardyn session cookie.
//
// User-actionable denials (5, 4) redirect to "/?auth_error=<code>" so the
// sign-in screen can explain what to do next; the state/nonce/PKCE checks in
// (1)-(3) stay http.Error since those are attack-shaped, not policy denials,
// and should fail loudly rather than retry.
// consumeCallbackCookies proves this redirect belongs to the browser that
// started the login and spends the single-use state/nonce/PKCE cookies.
//
// All three cookies are read before any is cleared: clearing one before the
// others are confirmed present would spend it on a request that never
// reaches the token exchange, turning a retryable error into one the human
// can't retry. On failure nothing is cleared, so the flow stays retryable.
// The state check is the CSRF check and runs first.
//
// ok=false means the response was already written (same (value, ok) shape as
// parseIDParam/getWorkspace* — a caller that forgets to check ok is a
// familiar bug). widened reports, without carrying any secret, whether this
// browser's authorization request asked for more than the login's own scopes.
func (a *Authenticator) consumeCallbackCookies(w http.ResponseWriter, r *http.Request) (nonce, verifier string, widened, ok bool) {
	stateParam := r.URL.Query().Get("state")
	stateCookie, err := r.Cookie(a.cookieName(stateCookieName))
	if err != nil || stateCookie.Value == "" || stateParam != stateCookie.Value {
		http.Error(w, "invalid state parameter", http.StatusBadRequest)
		return "", "", false, false
	}
	nonceCookie, err := r.Cookie(a.cookieName(nonceCookieName))
	if err != nil || nonceCookie.Value == "" {
		http.Error(w, "missing nonce cookie", http.StatusBadRequest)
		return "", "", false, false
	}
	pkceCookie, err := r.Cookie(a.cookieName(pkceCookieName))
	if err != nil || pkceCookie.Value == "" {
		http.Error(w, "missing pkce cookie", http.StatusBadRequest)
		return "", "", false, false
	}
	widenedCookie, werr := r.Cookie(a.cookieName(widenedCookieName))
	widened = werr == nil && widenedCookie.Value != ""
	a.clearCookie(w, stateCookieName)
	a.clearCookie(w, nonceCookieName)
	a.clearCookie(w, pkceCookieName)
	// Widened marker is left for the caller to expire only when present, so an
	// unwidened login's Set-Cookie headers are unchanged.
	return nonceCookie.Value, pkceCookie.Value, widened, true
}

// callbackClaims is everything CallbackHandler reads from a verified
// id_token, decoded by decodeCallbackClaims.
type callbackClaims struct {
	email         string
	emailVerified *bool
	// name: console header display only — never gates, never logged.
	name       string
	roles      []string
	groups     []string
	claimNames map[string]any
	// unreadable: claims sent in a shape this build can't decode; stamps the
	// snapshot partial and blocks a role-widening default.
	unreadable []string
	// issuer, tid, oid: the Subject a pre-created Entra person is keyed by
	// (resolvePerson). tid/oid are empty when absent or not strings.
	issuer, tid, oid string
}

// decodeCallbackClaims reads the login claims off a verified id_token. Only the
// standard-claims decode is fatal; the role/group/distributed-claim decodes are
// tolerant and report their loss through callbackClaims.unreadable.
func decodeCallbackClaims(idToken *gooidc.IDToken) (callbackClaims, error) {
	// The one claims struct whose failure to parse must abort the login.
	var claims struct {
		Email string `json:"email"`
		// *bool, not bool: Entra typically omits email_verified rather than
		// sending false, and a plain bool would conflate "unset" with
		// "false". The gate below treats nil and false as distinct denials.
		EmailVerified *bool `json:"email_verified"`
		// Console header display only. ONLY "name" — never preferred_username/
		// upn, which are email substitutes and would re-base the
		// AllowedEmailDomains gate.
		Name string `json:"name"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return callbackClaims{}, err // CallbackHandler answers 401 here.
	}

	// roles/groups are decoded separately and tolerantly, one struct per
	// claim: a real IdP sometimes sends a scalar instead of a JSON array
	// (e.g. bare "eng-team" instead of ["eng-team"]), which would otherwise
	// be a fatal unmarshal error for every such IdP. A malformed claim
	// decodes to nil (fails closed on that claim only) and can't discard
	// the other claim.
	//
	// Reporting the loss matters as much as tolerating the shape: a claim the
	// IdP sent in an unreadable shape must not look identical to "the IdP
	// sent none", or a group-subject deny or group-tier ceiling could
	// silently stop applying with no refusal and no audit trail. The claim
	// still contributes nothing to derivation — a coerced scalar could match
	// a role-map row it shouldn't (see
	// TestCallbackScalarGroupsClaimMapSetContributesNothing).
	var unreadableClaims []string
	var rc struct {
		Roles []string `json:"roles"`
	}
	if err := idToken.Claims(&rc); err != nil {
		unreadableClaims = append(unreadableClaims, "roles")
	}
	var gc struct {
		Groups []string `json:"groups"`
	}
	if err := idToken.Claims(&gc); err != nil {
		unreadableClaims = append(unreadableClaims, "groups")
	}
	// The distributed-claim pointer is decoded the same tolerant way and for
	// the same reason: when _claim_names points at "groups" (or "roles"),
	// the claim above is absent because the IdP omitted it, not because the
	// human has none. map[string]any (not map[string]string) so an
	// unexpected value shape can't fail the decode and hide the marker.
	//
	// An unreadable marker joins unreadableClaims too, as defense in depth —
	// go-oidc already refuses an id_token with an unreadable distributed-claim
	// block during Verify (TestUnreadableClaimNamesIsRefusedUpstream).
	var dc struct {
		ClaimNames map[string]any `json:"_claim_names"`
	}
	if err := idToken.Claims(&dc); err != nil {
		unreadableClaims = append(unreadableClaims, "_claim_names")
	}
	// Entra's tenant and object id, decoded apart so an IdP sending either in
	// another shape loses only the object-id keying, never the login.
	var ec struct {
		Tid string `json:"tid"`
		Oid string `json:"oid"`
	}
	_ = idToken.Claims(&ec)
	return callbackClaims{
		issuer:        idToken.Issuer,
		tid:           ec.Tid,
		oid:           ec.Oid,
		email:         claims.Email,
		emailVerified: claims.EmailVerified,
		name:          claims.Name,
		roles:         rc.Roles,
		groups:        gc.Groups,
		claimNames:    dc.ClaimNames,
		unreadable:    unreadableClaims,
	}, nil
}

func (a *Authenticator) CallbackHandler(w http.ResponseWriter, r *http.Request) {
	a.callback(w, r, nil, nil)
}

// DenialReservedPrincipal reports a sign-in refused because a subject names
// an identity that isn't a person. Not an auth_error code — the browser sees
// the generic authErrorSignInRefused, since nothing the user can do clears it.
const DenialReservedPrincipal = "reserved_principal"

// CallbackHandlerWithDenials is CallbackHandler with a reserved-subject check
// (DenialReservedPrincipal) and sign-in denials (including
// DenialUserTypeAmbiguous, DenialUserTypeUnknown) reported to onDenied so
// internal/api can audit them as auth.fail. This package stays store- and
// audit-agnostic; internal/api decides which principals are reserved.
func (a *Authenticator) CallbackHandlerWithDenials(reserved func(sub string) bool, onDenied func(r *http.Request, reason string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { a.callback(w, r, reserved, onDenied) }
}

func (a *Authenticator) callback(w http.ResponseWriter, r *http.Request, reserved func(string) bool, onDenied func(*http.Request, string)) {
	// (1) CSRF and the one-time cookies — consumeCallbackCookies below.
	nonce, verifier, widened, ok := a.consumeCallbackCookies(w, r)
	if !ok {
		return
	}
	if widened {
		a.expireWidenedMarker(w)
	}

	// A refusal of the extra (widened) scopes restarts the login with just the
	// login's own scopes, once, so it doesn't cost the person the console.
	// Every other refusal falls through unchanged (login_grant.go).
	if idpErr := r.URL.Query().Get("error"); idpErr != "" {
		if a.retryLoginUnwidened(w, r, widened, idpErr) {
			return
		}
		// Not retried: log the cause so a refusal outside the retry set isn't
		// a bare 400 with nothing to explain why.
		a.logUnretriedRefusal(r, widened, idpErr)
	}

	// (2) Exchange code for tokens, supplying the PKCE verifier.
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code parameter", http.StatusBadRequest)
		return
	}
	// Inject the stored HTTP client so tests with a custom transport (e.g.
	// rewriteTokenRT) work for token exchange.
	exchangeCtx := r.Context()
	if a.httpClient != nil {
		exchangeCtx = gooidc.ClientContext(exchangeCtx, a.httpClient)
	}
	// A transient IdP hiccup (5xx, timeout) gets tokenExchangeRetries
	// short-backoff attempts. A permanent rejection (bad secret, expired/
	// replayed code) is never retried — the code is single-use, so resending
	// it would just trade a clear error for a confusing invalid_grant one.
	token, exchangeErr := retryExchange(exchangeCtx, a.oauth2, code, verifier)
	if exchangeErr != nil {
		if isTransientOIDCErr(exchangeErr) {
			a.redirectAuthError(w, r, authErrorOIDCTransient)
		} else {
			a.redirectAuthError(w, r, authErrorOIDCConfig)
		}
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
	if idToken.Nonce != nonce {
		http.Error(w, "nonce mismatch", http.StatusUnauthorized)
		return
	}
	cc, err := decodeCallbackClaims(idToken)
	if err != nil {
		http.Error(w, "id_token claims extraction failed", http.StatusUnauthorized)
		return
	}
	sess, denied := a.admit(r, idToken.Subject, cc, reserved, onDenied)
	if denied != "" {
		// A denied login must not leave a pre-existing session cookie still valid.
		a.clearCookie(w, sessionCookieName)
		a.redirectAuthError(w, r, denied)
		return
	}

	// OnLogin fires once the login is approved but before the session cookie
	// is written. Best-effort: nil is a no-op; the integrator's callback is
	// responsible for not letting a backend hiccup fail the login. groups/
	// groupsTruncated are the same values the session below carries.
	if a.cfg.OnLogin != nil {
		a.cfg.OnLogin(r.Context(), sess.Sub, sess.Role, sess.UserType, sess.Groups, sess.GroupsTruncated)
	}
	// The login-grant sink runs here for the same reason as OnLogin: a
	// refused login never yields a downstream credential, and a credential
	// that fails to store must not cost this person the session they just
	// earned (login_grant.go). The session itself carries no token.
	a.captureLoginGrant(r.Context(), sess.Sub, token)

	// (6) Create a Wardyn session.
	sess.Expiry = idToken.Expiry
	sess.IssuedAt = time.Now().UTC() // The cutoff SessionRevocations compares against.
	if sess.Expiry.IsZero() {
		sess.Expiry = time.Now().UTC().Add(time.Hour)
	}
	cookie, err := a.encodeSession(sess)
	if err != nil {
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}
	a.RecordAttach(r, sess)
	http.SetCookie(w, cookie)
	http.Redirect(w, r, a.cfg.BasePath+"/", http.StatusFound)
}

// emailVerifiedEnv names the setting that put the email_verified gate in force,
// for the refusal's log line.
func (a *Authenticator) emailVerifiedEnv() string {
	if len(a.cfg.AllowedEmailDomains) > 0 {
		return "WARDYN_OIDC_EMAIL_DOMAINS"
	}
	return "WARDYN_OIDC_REQUIRE_EMAIL_VERIFIED"
}

// admit is the sign-in decision over a verified token's claims — reserved
// subject, email-domain gate, role/user-type derivation, overage and
// unreadable-claim refusals, and the group snapshot — shared by the callback
// and a portal's token exchange (VerifySubjectToken) so both admit the same
// way. denied is the auth_error code of a refusal, "" when admitted.
func (a *Authenticator) admit(r *http.Request, sub string, cc callbackClaims, reserved func(string) bool, onDenied func(*http.Request, string)) (Session, string) {
	// A subject naming a non-person identity (admin token, local operator, a
	// device) is refused before anything derives from it, so OnLogin and the
	// login-grant sink never see it.
	if reserved != nil && reserved(sub) {
		slog.Warn("oidc: login denied — the identity provider's subject is reserved for a non-person Wardyn identity",
			"sub", sub, "issuer", a.cfg.IssuerURL)
		if onDenied != nil {
			onDenied(r, DenialReservedPrincipal)
		}
		return Session{}, authErrorSignInRefused
	}
	// On Entra, a person set up by object id signs in as that person
	// (resolvePerson); everyone else is their sub.
	subj := Subject{Issuer: cc.issuer, Sub: sub, TenantID: cc.tid, ObjectID: cc.oid}
	sub, denied := a.resolvePerson(r, subj, reserved, onDenied)
	if denied != "" {
		return Session{}, denied
	}
	// (4) email_verified and domain checks — fail closed. The verified gate
	// applies under a domain allowlist or WARDYN_OIDC_REQUIRE_EMAIL_VERIFIED.
	if len(a.cfg.AllowedEmailDomains) > 0 || a.cfg.RequireEmailVerified {
		switch {
		case cc.emailVerified == nil:
			// Distinct from "false": the IdP said nothing about verification
			// (Entra's normal shape). No opt-in flag relaxes this —
			// allowlisting a self-asserted, unverifiable email is exactly
			// the risk AllowedEmailDomains fails closed on.
			// WARDYN_OIDC_ROLE_MAP (App Roles) is the better answer for an
			// IdP that never sends this claim.
			slog.Warn("oidc: login denied — the id_token carries no email_verified claim",
				"issuer", a.cfg.IssuerURL, "claim", "email_verified", "env", a.emailVerifiedEnv())
			return Session{}, authErrorEmailVerifiedAbsent
		case !*cc.emailVerified:
			slog.Warn("oidc: login denied — the id_token says email_verified=false",
				"issuer", a.cfg.IssuerURL, "claim", "email_verified", "env", a.emailVerifiedEnv())
			return Session{}, authErrorEmailUnverified
		}
	}
	if len(a.cfg.AllowedEmailDomains) > 0 && !emailDomainAllowed(cc.email, a.cfg.AllowedEmailDomains) {
		return Session{}, authErrorEmailDomain
	}

	// (5) Role derivation — deriveLogin. A refusal names its auth_error code.
	d, denied := a.deriveLogin(r, sub, cc, onDenied)
	if denied != "" {
		return Session{}, denied
	}
	role, matches := d.Role, d.Matches
	// On an overage the role-map's key claim is simply absent from the
	// token, so "nothing matched, take the default" is absence of evidence,
	// not fact. Denied here (not inside deriveRole, which stays pure) for
	// the same fail-closed reason as authErrorRoleCheckUnavailable: an
	// unanswerable input never widens a session.
	if overageWidensRole(cc.claimNames, role, matches) {
		slog.Warn("oidc: login denied — the IdP omitted a claim role derivation depends on (overage) and the default role would widen this session",
			"sub", sub, "default_role", a.cfg.DefaultRole,
			"env", "WARDYN_OIDC_DEFAULT_ROLE", "claim_names", claimNamesKeys(cc.claimNames))
		return Session{}, authErrorClaimsOverage
	}
	// The unreadable half of the same question, kept separate so the log
	// names which claim arrived in an undecodable shape (here _claim_names
	// is empty, unlike a true overage). Shares the same auth_error code
	// deliberately: to the user both mean the same thing, and retrying sends
	// the same token.
	if unanswerableWidensRole(len(cc.unreadable) > 0, role, matches) {
		slog.Warn("oidc: login denied — the IdP sent a claim role derivation depends on in a shape this build cannot decode, and the default role would widen this session",
			"sub", sub, "default_role", a.cfg.DefaultRole,
			"env", "WARDYN_OIDC_DEFAULT_ROLE", "unreadable_claims", cc.unreadable)
		return Session{}, authErrorClaimsOverage
	}
	if len(matches) > 0 {
		slog.Debug("oidc: role derivation matched", "sub", sub, "role", role, "matches", matches)
	}

	// Groups is stamped from the same tolerantly-decoded claims deriveRole
	// consumed, so a malformed claim contributes nothing here either and
	// never fails the login. Computed before OnLogin so both see the same
	// values rather than two derivations that could drift apart.
	groups, groupsTruncated := sessionGroups(cc.roles, cc.groups, cc.claimNames)
	if len(cc.unreadable) > 0 {
		// Stamped here (not inside sessionGroups, which only sees what
		// survived a decode) so a login doesn't carry a complete-and-empty
		// group identity for someone whose IdP did name groups — otherwise
		// group-subject denies and group-tier ceilings for them evaporate
		// silently.
		groupsTruncated = true
		slog.Warn("oidc: group snapshot marked partial — the id_token carried a role/group claim in a shape this build cannot decode, so the human's real groups are not in it",
			"sub", sub, "unreadable_claims", cc.unreadable)
	}
	sess := Session{
		Sub: sub, Email: cc.email, Name: cc.name, Role: role, UserType: d.UserType,
		Groups: groups, GroupsTruncated: groupsTruncated,
	}
	if sub != subj.Sub {
		sess.attached = &subj
	}
	// Only an Entra token's oid is an object id, and only a well-formed one is
	// kept: the cookie carries one short field, never a claim of any length.
	if a.entra {
		if id, err := uuid.Parse(cc.oid); err == nil {
			sess.ObjectID = id.String()
		}
	}
	return sess, ""
}

// deriveLogin is CallbackHandler's step (5), role and user-type derivation —
// see Config.RoleMap / deriveRole for precedence. denied is the auth_error
// code of a refusal, "" when the login may proceed.
func (a *Authenticator) deriveLogin(r *http.Request, sub string, cc callbackClaims, onUserTypeDenied func(*http.Request, string)) (Derivation, string) {
	// When RoleMappings (console People store) is wired, its rows merge with
	// the chart map first; a store read error denies the login rather than
	// silently falling back to the env-only map, which could widen access
	// under WARDYN_OIDC_DEFAULT_ROLE=admin. Every session cookie this
	// package writes has a non-empty Role.
	roleMap := a.cfg.RoleMap
	if a.cfg.RoleMappings != nil {
		rows, rerr := a.cfg.RoleMappings.ListRoleMappings(r.Context())
		if rerr != nil {
			slog.Error("oidc: console role-mapping store unavailable, denying login (fail closed)", "error", rerr)
			return Derivation{}, authErrorRoleCheckUnavailable
		}
		var shadowed []string
		roleMap, shadowed = mergeRoleMaps(a.cfg.RoleMap, a.cfg.LegacyAdminEmails, rows)
		if len(shadowed) > 0 {
			// shadowed covers two causes: a chart/operator entry colliding
			// with an existing console row, or a console row rejected as
			// invalid (see mergeRoleMaps). Either way it contributed nothing
			// to this derivation.
			slog.Warn("oidc: one or more console-managed role mappings were shadowed by chart/operator config or rejected as invalid", "shadowed", shadowed)
		}
	}
	// The merged map is the only place console group->role rows are visible,
	// so the groups-scope check runs here — one log line, never a denial.
	a.warnMergedMapNeedsGroupsScope(roleMap, cc.roles, cc.groups)
	// User types are read once, failing closed the same way — priority
	// decides the session as much as the role rows do.
	userTypes, terr := a.loadUserTypes(r.Context())
	if terr != nil {
		slog.Error("oidc: user-type store unavailable, denying login (fail closed)", "error", terr)
		return Derivation{}, authErrorRoleCheckUnavailable
	}
	d := deriveRole(cc.roles, cc.groups, cc.email, roleMap, a.cfg.LegacyAdminEmails, a.cfg.DefaultRole, userTypes)
	if !d.OK() {
		if d.Denial != DenialNoRole {
			slog.Warn("oidc: login denied over the user type the role map names",
				"sub", sub, "reason", d.Denial, "tied_user_types", d.Tied, "unknown_user_types", d.Unknown)
			if onUserTypeDenied != nil {
				onUserTypeDenied(r, d.Denial)
			}
		}
		return d, d.Denial
	}
	if len(d.Tied) > 0 || len(d.Unknown) > 0 {
		slog.Warn("oidc: admin sign-in put on the standard user type; the types the role map names for it tie or don't exist",
			"sub", sub, "tied_user_types", d.Tied, "unknown_user_types", d.Unknown)
	}
	return d, ""
}
