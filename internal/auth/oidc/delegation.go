// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"net/http"
	"slices"
)

// The subject half of a portal's token exchange (RFC 8693, #1142): a trusted
// portal presents the signed-in person's OWN identity-provider token, and
// VerifySubjectToken proves who that person is exactly the way a sign-in
// would. Everything about the portal itself — its credential, its group scope,
// the token it is handed back — is internal/api's.

// The subject-token refusals VerifySubjectToken names itself. A refusal from
// the shared admission (a reserved subject, the email-domain gate, role or
// user-type derivation) is that step's own auth_error code instead.
const (
	// SubjectTokenInvalid: the token did not verify — signature, issuer,
	// expiry, algorithm (alg=none included) — or its claims did not decode.
	SubjectTokenInvalid = "subject_token_invalid"
	// SubjectTokenAudience: the token was neither issued TO the portal nor
	// issued FOR this deployment at the portal's request.
	SubjectTokenAudience = "subject_token_audience"
	// SubjectTokenNoIssuedAt: the token carries no iat, so the person's
	// session cutoff (SessionRevocations) has nothing to compare against.
	SubjectTokenNoIssuedAt = "subject_token_no_iat"
)

// ClientID is this deployment's own client id at the identity provider — what
// a portal's access token carries in aud when it was requested for Wardyn.
func (a *Authenticator) ClientID() string { return a.cfg.ClientID }

// VerifySubjectToken verifies raw against this deployment's issuer and key set
// and admits its subject through the SAME decision the sign-in callback makes
// (admit), so a portal can never act for someone who could not sign in, nor be
// handed a role or group snapshot a sign-in would not derive.
//
// The audience rule is the one thing a sign-in does not share. The token must
// be either an access token issued for Wardyn (aud holds ClientID) at the
// portal's request (azp is portalClientID), or a token issued to the portal
// itself (aud is exactly portalClientID; an azp, if present, agrees). A token
// the person holds for any other client is refused, and a portal registered
// under Wardyn's own client id matches nothing — otherwise every Wardyn ID
// token would be a subject token for it.
//
// The Session it returns carries the token's own iat as IssuedAt (the value a
// person's session cutoff is compared against) and its exp as Expiry. denied
// is non-empty on any refusal and the Session is then zero.
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
