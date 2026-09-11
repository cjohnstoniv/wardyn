// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

// The login CALLBACK: the IdP redirect handler and the two helpers it is built
// from — the single-use state/nonce/PKCE cookie consumption that proves the
// redirect belongs to the browser that started this login, and the ID-token
// claim decode. Split from oidc.go by seam (the file-size gate); no behaviour
// lives here that oidc.go's doc does not describe.

import (
	"log/slog"
	"net/http"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
)

// CallbackHandler handles the IdP redirect. It:
//  1. Verifies the state parameter against the state cookie (CSRF).
//  2. Exchanges the code for tokens using PKCE.
//  3. Verifies the ID token signature, issuer, audience, expiry, and nonce.
//  4. Optionally checks email domain (fail closed when AllowedEmailDomains is set).
//  5. Derives the session's role from the roles/groups/email claims (see
//     Config.RoleMap / deriveRole); denies the login if nothing matches and no
//     DefaultRole is configured.
//  6. Creates a signed Wardyn session cookie.
//
// W31-S1-5: the USER-actionable denials (5's role-denied, 4's domain/
// unverified-email) redirect to "/?auth_error=<code>" (302) instead of a
// bare http.Error text page — a login failure otherwise dead-ended the
// browser on plain text with no way back to the console, and no chance for
// the sign-in screen to explain what to do next (ask the operator to map a
// role, use a corp email, etc). The state/nonce/PKCE branches in (1)-(3) stay
// http.Error: those are ATTACK-shaped (a forged/replayed/mismatched
// callback), not a real user hitting a real policy denial, and a redirect
// there would be a worse UX for a case an operator needs to see failed
// loudly, not routed back into a retry loop.
// consumeCallbackCookies is CallbackHandler's PHASE 1: prove this redirect is
// the one THIS browser started, and spend the single-use cookies that prove it.
//
// EVERY COOKIE IS READ BEFORE ANY IS CLEARED, and that order is the point. The
// three are single-use by construction — state guards this redirect, nonce binds
// this ID token, the verifier proves this exchange — so clearing one before the
// others are known to be present would spend it on a request that never reaches
// the token exchange, turning a retryable error into a login the human cannot
// repeat by pressing back. On the failure paths nothing is cleared at all: the
// browser keeps the cookies and the flow can be retried.
//
// The state comparison is the CSRF check and stays FIRST: a callback whose state
// does not match a cookie this server set is not a login this browser began, and
// nothing else about the request is worth reading until that holds.
//
// Returns the nonce the ID token must carry and the PKCE verifier the exchange
// must present. ok=false means the response has ALREADY been written — the same
// (value, ok) shape parseIDParam and the getWorkspace* helpers use, so a caller
// that forgets to return on !ok is a familiar bug rather than a new one.
func consumeCallbackCookies(w http.ResponseWriter, r *http.Request) (nonce, verifier string, ok bool) {
	stateParam := r.URL.Query().Get("state")
	stateCookie, err := r.Cookie(stateCookieName)
	if err != nil || stateCookie.Value == "" || stateParam != stateCookie.Value {
		http.Error(w, "invalid state parameter", http.StatusBadRequest)
		return "", "", false
	}
	nonceCookie, err := r.Cookie(nonceCookieName)
	if err != nil || nonceCookie.Value == "" {
		http.Error(w, "missing nonce cookie", http.StatusBadRequest)
		return "", "", false
	}
	pkceCookie, err := r.Cookie(pkceCookieName)
	if err != nil || pkceCookie.Value == "" {
		http.Error(w, "missing pkce cookie", http.StatusBadRequest)
		return "", "", false
	}
	clearCookie(w, stateCookieName)
	clearCookie(w, nonceCookieName)
	clearCookie(w, pkceCookieName)
	return nonceCookie.Value, pkceCookie.Value, true
}

// callbackClaims is everything CallbackHandler reads out of a verified
// id_token, decoded by decodeCallbackClaims. Split out of the handler (round-3
// lint gate: funlen) with NO behaviour change — the same three tolerant decodes,
// the same one fatal decode, the same unreadable list.
type callbackClaims struct {
	email         string
	emailVerified *bool
	// name is the IdP's display-name claim, carried on the session for the
	// console header only (Session.Name) — never gates, never logged.
	name       string
	roles      []string
	groups     []string
	claimNames map[string]any
	// unreadable lists the claims the IdP sent in a shape this build cannot
	// decode; CallbackHandler stamps the snapshot partial and refuses a
	// role-widening default on it.
	unreadable []string
}

// decodeCallbackClaims reads the login claims off a verified id_token. Only the
// standard-claims decode is fatal; the role/group/distributed-claim decodes are
// tolerant and report their loss through callbackClaims.unreadable.
func decodeCallbackClaims(idToken *gooidc.IDToken) (callbackClaims, error) {
	// Extract standard claims from the ID token — UNCHANGED shape and fatal
	// error from before role derivation existed: this is the ONE claims
	// struct whose failure to parse must abort the login.
	var claims struct {
		Email string `json:"email"`
		// *bool, not bool: an Entra ID token typically OMITS email_verified
		// entirely rather than sending it false (see AllowedEmailDomains'
		// doc), and a plain bool would silently decode that absence as
		// false — the SAME denial as an IdP that explicitly told us the
		// email is unverified, when in truth the IdP said nothing at all.
		// C1: the gate below (4) treats nil and false as distinct denials,
		// each with its own auth_error code.
		EmailVerified *bool `json:"email_verified"`
		// Display name for the console header (0.7.1). Optional by nature — an
		// absent key decodes to "" and is not an error; ONLY "name", never
		// preferred_username/upn, which are email substitutes and would
		// silently re-base the AllowedEmailDomains gate if routed anywhere.
		Name string `json:"name"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return callbackClaims{}, err // CallbackHandler answers 401 "id_token claims extraction failed", unchanged
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
	//
	// TOLERATING THE SHAPE AND REPORTING THE LOSS ARE DIFFERENT JOBS, and the
	// decode error used to be discarded, which collapsed them. A claim the IdP
	// DID send in a shape this build cannot read then arrived at derivation as
	// nil — byte-for-byte "asked, there were none". The group the human really
	// holds vanished from the snapshot with the PF-26 partial bit CLEAR, so
	// capScan never consulted capUnresolvableGroupDeny and effectiveCeiling
	// never took ceilingWithUnusableGroups: a group-subject DENY protected
	// nothing and a group-tier ceiling degraded to the deployment default, with
	// no refusal, no audit row and nothing to notice. It is the same evaporation
	// the byte cap and the Entra overage are already closed for, on the one
	// input neither can see, so unreadableClaims below stamps the same bit and
	// takes the same role-widening refusal. The claim still contributes NOTHING
	// to derivation: coercing a scalar into a one-element list would let the raw
	// string match a role-map row (TestCallbackScalarGroupsClaimMapSetContributesNothing).
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
	// The distributed-claim pointer, decoded just as tolerantly and for the
	// same fail-closed reason: when `_claim_names` names "groups" (or "roles"),
	// the claim above is absent because the IdP OMITTED it — an overage — not
	// because the human is in no groups. sessionGroups turns that into the
	// truncation bit. map[string]any rather than map[string]string so a value
	// shape this package does not read cannot fail the decode and hide the
	// marker.
	//
	// A marker this build cannot read is a question that cannot be answered, so
	// it joins unreadableClaims too — defense in depth rather than a live arm:
	// go-oidc parses the distributed-claim block during Verify and refuses such
	// an id_token outright, one step earlier (TestUnreadableClaimNamesIsRefusedUpstream).
	var dc struct {
		ClaimNames map[string]any `json:"_claim_names"`
	}
	if err := idToken.Claims(&dc); err != nil {
		unreadableClaims = append(unreadableClaims, "_claim_names")
	}
	return callbackClaims{
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
	// (1) CSRF and the one-time cookies — consumeCallbackCookies below.
	nonce, verifier, ok := consumeCallbackCookies(w, r)
	if !ok {
		return
	}

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
	// D12: a transient IdP hiccup on the token endpoint (5xx, timeout) used to
	// hard-fail the whole login on the FIRST blip — retryExchange gives it
	// tokenExchangeRetries short-backoff attempts before giving up. A
	// PERMANENT rejection (bad client secret, expired/replayed code —
	// invalid_grant is the common case) is never retried: the code is
	// single-use, so re-sending it after the IdP has already consumed it
	// would just trade one clear error for a confusing "invalid_grant" one.
	token, exchangeErr := retryExchange(exchangeCtx, a.oauth2, code, verifier)
	if exchangeErr != nil {
		if isTransientOIDCErr(exchangeErr) {
			redirectAuthError(w, r, authErrorOIDCTransient)
		} else {
			redirectAuthError(w, r, authErrorOIDCConfig)
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
	// (4) Domain check — fail closed.
	if len(a.cfg.AllowedEmailDomains) > 0 {
		switch {
		case cc.emailVerified == nil:
			// C1: distinct from "false" — the IdP said nothing at all about
			// verification (Entra's normal shape), not that it explicitly
			// failed. No opt-in flag to relax this: allowlisting a
			// self-asserted, unverifiable email is exactly the risk
			// AllowedEmailDomains exists to fail closed on. WARDYN_OIDC_ROLE_MAP
			// (App Roles) is the documented better answer for an IdP that
			// never sends this claim.
			slog.Warn("oidc: login denied — the id_token carries no email_verified claim",
				"issuer", a.cfg.IssuerURL, "claim", "email_verified", "env", "WARDYN_OIDC_EMAIL_DOMAINS")
			clearCookie(w, sessionCookieName)
			redirectAuthError(w, r, authErrorEmailVerifiedAbsent)
			return
		case !*cc.emailVerified:
			clearCookie(w, sessionCookieName)
			redirectAuthError(w, r, authErrorEmailUnverified)
			return
		}
		if !emailDomainAllowed(cc.email, a.cfg.AllowedEmailDomains) {
			clearCookie(w, sessionCookieName)
			redirectAuthError(w, r, authErrorEmailDomain)
			return
		}
	}

	// (5) Role derivation — see Config.RoleMap / deriveRole for precedence.
	// When RoleMappings (the console's Getting Started -> People store) is
	// wired, its rows are merged with the chart map FIRST (mergeRoleMaps) — a
	// store read error denies this login (authErrorRoleCheckUnavailable)
	// rather than silently falling back to the env-only map, which could
	// WIDEN access under WARDYN_OIDC_DEFAULT_ROLE=admin. Denying on !ok
	// (rather than issuing a roleless session) is what keeps decodeSession
	// simple: every cookie this package ever writes has a non-empty Role.
	roleMap := a.cfg.RoleMap
	if a.cfg.RoleMappings != nil {
		rows, rerr := a.cfg.RoleMappings.ListRoleMappings(r.Context())
		if rerr != nil {
			slog.Error("oidc: console role-mapping store unavailable, denying login (fail closed)", "error", rerr)
			clearCookie(w, sessionCookieName)
			redirectAuthError(w, r, authErrorRoleCheckUnavailable)
			return
		}
		var shadowed []string
		roleMap, shadowed = mergeRoleMaps(a.cfg.RoleMap, a.cfg.LegacyAdminEmails, rows)
		if len(shadowed) > 0 {
			// shadowed now covers two distinct causes mergeRoleMaps folds
			// into one list: a chart/operator entry that collides with an
			// already-saved console row (the API layer refuses CREATING a
			// new collision, so a later helm upgrade is the only way one
			// reaches here), or a console row that was itself rejected as
			// non-canonical/invalid (see mergeRoleMaps). Either way the row
			// contributed nothing to this login's role derivation.
			slog.Warn("oidc: one or more console-managed role mappings were shadowed by chart/operator config or rejected as invalid", "shadowed", shadowed)
		}
	}
	// The MERGED map is the only place the console's group->role rows are
	// visible, and it exists nowhere but here — so this is where the
	// groups-scope question gets asked about them. One line per process,
	// never a denial; see warnMergedMapNeedsGroupsScope.
	a.warnMergedMapNeedsGroupsScope(roleMap, cc.roles, cc.groups)
	role, matches, ok := deriveRole(cc.roles, cc.groups, cc.email, roleMap, a.cfg.LegacyAdminEmails, a.cfg.DefaultRole)
	if !ok {
		// L6: a denied login must not leave a PRE-EXISTING session cookie
		// (from before this re-login attempt) still valid in the browser.
		clearCookie(w, sessionCookieName)
		redirectAuthError(w, r, authErrorNoRole)
		return
	}
	// The overage half of role derivation. deriveRole is a pure function of the
	// claims it was HANDED, and on an overage the claim the role map is keyed on
	// is simply not in the token — so its "nothing matched, take the default"
	// is an absence of evidence, not a fact. Denying here rather than inside
	// deriveRole keeps that function pure and puts the refusal beside every
	// other login denial, and it is the same fail-closed choice
	// authErrorRoleCheckUnavailable already makes when the role-mapping store
	// cannot be read: an unanswerable input never widens a session.
	if overageWidensRole(cc.claimNames, role, matches) {
		slog.Warn("oidc: login denied — the IdP omitted a claim role derivation depends on (overage) and the default role would widen this session",
			"sub", idToken.Subject, "default_role", a.cfg.DefaultRole,
			"env", "WARDYN_OIDC_DEFAULT_ROLE", "claim_names", claimNamesKeys(cc.claimNames))
		clearCookie(w, sessionCookieName)
		redirectAuthError(w, r, authErrorClaimsOverage)
		return
	}
	// The UNREADABLE half of the same question, kept as its own branch so each
	// denial logs the cause it actually knows. The IdP sent the claim, so
	// `_claim_names` says nothing and claimNamesKeys would log an empty list;
	// what an operator needs here is WHICH claim arrived in a shape this build
	// could not decode. The auth_error code is shared deliberately: to the human
	// at the sign-in screen both mean "this console could not read the claim
	// your access depends on and will not guess", and retrying sends the same
	// token either way.
	if unanswerableWidensRole(len(cc.unreadable) > 0, role, matches) {
		slog.Warn("oidc: login denied — the IdP sent a claim role derivation depends on in a shape this build cannot decode, and the default role would widen this session",
			"sub", idToken.Subject, "default_role", a.cfg.DefaultRole,
			"env", "WARDYN_OIDC_DEFAULT_ROLE", "unreadable_claims", cc.unreadable)
		clearCookie(w, sessionCookieName)
		redirectAuthError(w, r, authErrorClaimsOverage)
		return
	}
	if len(matches) > 0 {
		slog.Debug("oidc: role derivation matched", "sub", idToken.Subject, "role", role, "matches", matches)
	}

	// OnLogin fires once the login is APPROVED (past every denial branch
	// above) but before the session cookie is written — a real login, not a
	// probe. Best-effort: nil is a no-op, and the integrator's own callback is
	// responsible for not letting a backend hiccup fail the login (see the
	// Config.OnLogin doc).
	if a.cfg.OnLogin != nil {
		a.cfg.OnLogin(r.Context(), idToken.Subject, role)
	}

	// (6) Create a Wardyn session. Groups is stamped from the SAME two
	// tolerantly-decoded claims deriveRole just consumed — a claim malformed
	// enough to contribute nothing to the role contributes nothing here either,
	// and never fails the login. The `_claim_names` pointer rides along so an
	// IdP-side overage stamps the snapshot partial instead of empty.
	groups, groupsTruncated := sessionGroups(cc.roles, cc.groups, cc.claimNames)
	if len(cc.unreadable) > 0 {
		// PF-26's third cause, stamped here rather than inside sessionGroups
		// because it is the DECODE that failed, and sessionGroups only ever
		// sees what survived one. "Contributes nothing to the role" and
		// "contributes nothing to the snapshot" were both already true; what
		// was missing is that the snapshot said so. Without this the login
		// carries a COMPLETE-and-empty group identity for a human whose IdP
		// just named their groups, and every group-subject DENY and group-tier
		// ceiling written for them evaporates silently.
		groupsTruncated = true
		slog.Warn("oidc: group snapshot marked partial — the id_token carried a role/group claim in a shape this build cannot decode, so the human's real groups are not in it",
			"sub", idToken.Subject, "unreadable_claims", cc.unreadable)
	}
	sess := Session{
		Sub:             idToken.Subject,
		Email:           cc.email,
		Name:            cc.name,
		Role:            role,
		Expiry:          idToken.Expiry,
		IssuedAt:        time.Now().UTC(), // D16: the cutoff SessionRevocations compares against
		Groups:          groups,
		GroupsTruncated: groupsTruncated,
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
