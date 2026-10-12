// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"log/slog"
	"net/http"
	"slices"
)

// The subject half of a portal's token exchange (RFC 8693): a trusted portal presents the
// signed-in person's OWN identity-provider token, and VerifySubjectToken proves who that person
// is exactly like a sign-in would. The portal itself — credential, group scope, token handed
// back — belongs to internal/api.

// Subject-token refusals VerifySubjectToken names itself; a refusal from shared admission
// (reserved subject, email-domain gate, role/user-type derivation) uses that step's own
// auth_error code instead.
const (
	// SubjectTokenInvalid: token failed to verify (signature, issuer, expiry, alg=none included) or its claims didn't decode.
	SubjectTokenInvalid = "subject_token_invalid"
	// SubjectTokenAudience: token was neither issued TO the portal nor FOR this deployment at the portal's request.
	SubjectTokenAudience = "subject_token_audience"
	// SubjectTokenNoIssuedAt: token carries no iat, so the session cutoff (SessionRevocations) has nothing to compare against.
	SubjectTokenNoIssuedAt = "subject_token_no_iat"
	// SubjectTokenUnbound: an Entra token issued to the portal names an identity (tenant and object
	// id) that no sign-in here has bound to a principal and no person was set up by.
	SubjectTokenUnbound = "subject_token_unbound"
)

// ClientID is this deployment's own client id at the identity provider — what
// a portal's access token carries in aud when it was requested for Wardyn.
func (a *Authenticator) ClientID() string { return a.cfg.ClientID }

// VerifySubjectToken verifies raw against this deployment's issuer/key set and admits its subject
// through the SAME decision the sign-in callback makes, so a portal can never act for someone who
// couldn't sign in, nor get a role or group snapshot a sign-in wouldn't derive.
//
// The audience rule is the one thing sign-in doesn't share: the token must be either an access
// token issued for Wardyn (aud holds ClientID) at the portal's request (azp is portalClientID),
// or issued to the portal itself (aud is exactly portalClientID; azp, if present, agrees). A
// token for any other client is refused, and a portal registered under Wardyn's own client id
// matches nothing — else every Wardyn ID token would double as a subject token for it.
//
// On Entra ID, `sub` is pairwise per app registration, so a token issued to the portal names the
// person under the portal's sub, not Wardyn's. Its principal is the one the person's own sign-in
// bound to the token's (issuer, tid, oid), else a person set up by that object id; with neither,
// the exchange is refused (boundPortalSubject).
//
// The returned Session carries the token's iat as IssuedAt (compared against the session cutoff)
// and exp as Expiry. denied is non-empty on any refusal, with a zero Session. The caller records
// an attach to a person set up by object id with RecordAttach once it has admitted the exchange.
func (a *Authenticator) VerifySubjectToken(r *http.Request, raw, portalClientID string, reserved func(string) bool) (Session, string) {
	if a.subjectVerifier == nil {
		return Session{}, SubjectTokenInvalid
	}
	tok, err := a.subjectVerifier.Verify(r.Context(), raw)
	if err != nil {
		return Session{}, SubjectTokenInvalid
	}
	var c struct {
		Azp string `json:"azp"`
	}
	if err := tok.Claims(&c); err != nil {
		return Session{}, SubjectTokenInvalid
	}
	if !subjectAudienceOK(tok.Audience, c.Azp, a.cfg.ClientID, portalClientID) {
		return Session{}, SubjectTokenAudience
	}
	if tok.IssuedAt.IsZero() {
		return Session{}, SubjectTokenNoIssuedAt
	}
	cc, err := decodeCallbackClaims(tok)
	if err != nil {
		return Session{}, SubjectTokenInvalid
	}
	sess, denied := a.admit(r, tok.Subject, cc, reserved, nil)
	if denied != "" {
		return Session{}, denied
	}
	if a.entra && !slices.Contains(tok.Audience, a.cfg.ClientID) {
		if sess, denied = a.boundPortalSubject(r, sess, cc); denied != "" {
			return Session{}, denied
		}
	}
	sess.IssuedAt, sess.Expiry = tok.IssuedAt, tok.Expiry
	return sess, ""
}

// subjectAudienceOK is VerifySubjectToken's audience rule; see there.
func subjectAudienceOK(aud []string, azp, wardyn, portal string) bool {
	if portal == "" || portal == wardyn {
		return false
	}
	if slices.Contains(aud, wardyn) {
		return azp == portal
	}
	return len(aud) == 1 && aud[0] == portal && (azp == "" || azp == portal)
}

// boundPortalSubject resolves an admitted Entra token issued to the portal to the principal bound to
// its (issuer, tid, oid). A binding to another principal than admission resolved replaces it and is
// checked by the identity gate as a sign-in under it would be. Without a binding, only a person set
// up by that object id (sess.attached) is kept; a token with no tenant or well-formed object id names
// no identity and is refused.
func (a *Authenticator) boundPortalSubject(r *http.Request, sess Session, cc callbackClaims) (Session, string) {
	ref := a.identityRef(sess.Sub, cc)
	if ref.ObjectID == "" {
		return Session{}, SubjectTokenUnbound
	}
	var bound string
	if a.cfg.Identities != nil {
		var err error
		if bound, err = a.cfg.Identities.Bound(r.Context(), ref); err != nil {
			slog.Error("oidc: identity binding unavailable, refusing the token exchange (fail closed)", "error", err)
			return Session{}, authErrorRoleCheckUnavailable
		}
	}
	switch {
	case bound == "" && sess.attached == nil:
		slog.Warn("oidc: token exchange refused — no sign-in has bound the identity the portal's token names", "issuer", ref.Issuer)
		return Session{}, SubjectTokenUnbound
	case bound != "" && bound != sess.Sub:
		sess.Sub, sess.attached = bound, nil
		if denied := a.admitIdentity(r, a.identityRef(bound, cc), nil); denied != "" {
			return Session{}, denied
		}
	}
	return sess, ""
}
