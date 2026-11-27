// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The two token profiles an issuer portal can name, and the declared subject-token type each one accepts.
const (
	PortalProfileAccessToken = "okta_access_token"
	PortalProfileIDToken     = "okta_id_token"

	PortalTokenTypeAccess = "urn:ietf:params:oauth:token-type:access_token"
	PortalTokenTypeID     = "urn:ietf:params:oauth:token-type:id_token"
)

// Denial reasons the issuer verifier adds to the console exchange's own (SubjectTokenInvalid among them).
const (
	SubjectTokenTypeMismatch     = "subject_token_type_mismatch"
	SubjectTokenNotIDToken       = "subject_token_not_id_token"
	SubjectTokenNoUser           = "subject_token_no_user"
	SubjectTokenScopeMissing     = "subject_token_scope_missing"
	SubjectTokenAudienceMismatch = "subject_token_audience_mismatch"
	SubjectTokenCIDMismatch      = "subject_token_cid_mismatch"
	SubjectTokenStale            = "subject_token_stale"
	SubjectTokenIatFuture        = "subject_token_iat_future"
	SubjectTokenIatInvalid       = "subject_token_iat_invalid"
	SubjectTokenClaimMismatch    = "subject_token_claim_mismatch"
	PortalOutsideScope           = "outside_scope"
	PortalIssuerUnavailable      = "portal_issuer_unavailable"
	PortalProfileUnknown         = "portal_profile_unknown"
	PortalRegistrationInvalid    = "portal_registration_invalid"
)

const (
	// portalClockAllowance is how far ahead of Wardyn's clock a token's iat may be.
	portalClockAllowance = 60 * time.Second
	// portalMinGrantLife is the least life a signed end may leave: under it the token is stale.
	portalMinGrantLife = 5 * time.Second
	// portalDefaultMaxAge applies when a registration carries no maximum token age.
	portalDefaultMaxAge = 300 * time.Second
	// portalMaxNumericDate bounds a NumericDate claim, so a huge value cannot overflow a time.
	portalMaxNumericDate = 1 << 40
)

// portalClaims are the top-level claims of a verified token, undecoded.
type portalClaims map[string]json.RawMessage

func (c portalClaims) has(name string) bool {
	_, ok := c[name]
	return ok
}

// str reads a claim that must be a JSON string.
func (c portalClaims) str(name string) (string, bool) {
	raw, ok := c[name]
	if !ok || len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// strictStrList reads a claim that must be a JSON array whose every element is a JSON string: a null or any
// other element makes the whole claim unusable.
func (c portalClaims) strictStrList(name string) ([]string, bool) {
	raw, ok := c[name]
	if !ok || len(raw) == 0 || raw[0] != '[' {
		return nil, false
	}
	var elems []json.RawMessage
	if json.Unmarshal(raw, &elems) != nil {
		return nil, false
	}
	out := make([]string, 0, len(elems))
	for _, e := range elems {
		if len(e) == 0 || e[0] != '"' {
			return nil, false
		}
		var s string
		if json.Unmarshal(e, &s) != nil {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// strList reads a claim that must be a JSON array of strings, tolerantly: an element that is not a string
// decodes as empty. Group admission uses it, and drops empty names.
func (c portalClaims) strList(name string) ([]string, bool) {
	raw, ok := c[name]
	if !ok || len(raw) == 0 || raw[0] != '[' {
		return nil, false
	}
	var l []string
	if json.Unmarshal(raw, &l) != nil {
		return nil, false
	}
	return l, true
}

// audience reads aud as a string or an array of strings.
func (c portalClaims) audience() ([]string, bool) {
	if s, ok := c.str("aud"); ok {
		return []string{s}, true
	}
	return c.strictStrList("aud")
}

// date reads a NumericDate claim. ok is false for a missing claim, a string, a non-finite or negative number,
// or one too large to be a date.
func (c portalClaims) date(name string) (time.Time, bool) {
	raw, ok := c[name]
	if !ok || len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return time.Time{}, false
	}
	f, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > portalMaxNumericDate {
		return time.Time{}, false
	}
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(frac*1e9)), true
}

// portalProfileType is the declared subject-token type a profile accepts, "" for an unknown profile.
func portalProfileType(profile string) string {
	switch profile {
	case PortalProfileAccessToken:
		return PortalTokenTypeAccess
	case PortalProfileIDToken:
		return PortalTokenTypeID
	}
	return ""
}

// checkAccessToken applies the access-token profile's own rules to a verified token and returns the person id.
// Order is fixed: client, scopes, audience, person.
func (reg PortalRegistration) checkAccessToken(c portalClaims) (string, string) {
	if cid, ok := c.str("cid"); !ok || cid != reg.ClientID {
		return "", SubjectTokenCIDMismatch
	}
	if scp, ok := c.strictStrList("scp"); !ok || !portalHasAll(scp, reg.RequiredScopes) {
		return "", SubjectTokenScopeMissing
	}
	if aud, ok := c.audience(); !ok || len(aud) != 1 || aud[0] != reg.Audience {
		return "", SubjectTokenAudienceMismatch
	}
	uid, ok := c.str("uid")
	if !ok || uid == "" {
		return "", SubjectTokenNoUser
	}
	return uid, ""
}

// checkIDToken applies the ID-token profile's rules, with its absence checks as a second guard, and returns the
// person id.
func (reg PortalRegistration) checkIDToken(c portalClaims, headerTyp portalHeaderTyp) (string, string) {
	if aud, ok := c.audience(); !ok || len(aud) != 1 || aud[0] != reg.ClientID {
		return "", SubjectTokenAudienceMismatch
	}
	if c.has("azp") {
		if azp, ok := c.str("azp"); !ok || azp != reg.ClientID {
			return "", SubjectTokenAudienceMismatch
		}
	}
	if headerTyp.present && (!headerTyp.isString || headerTyp.value != "JWT") {
		return "", SubjectTokenNotIDToken
	}
	for _, name := range []string{"cid", "scp", "scope", "client_id"} {
		if c.has(name) {
			return "", SubjectTokenNotIDToken
		}
	}
	if typ, ok := c.str("typ"); ok && strings.EqualFold(typ, "Bearer") {
		return "", SubjectTokenNotIDToken
	}
	sub, ok := c.str("sub")
	if !ok || sub == "" {
		return "", SubjectTokenNoUser
	}
	return sub, ""
}

// portalHasAll reports whether have holds every entry of want.
func portalHasAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// portalTimes is the time checks for one verified token, on Wardyn's clock.
type portalTimes struct {
	issuedAt, expiry, grantEnd time.Time
}

// portalCheckTimes applies the time rules of every issuer portal. exp must be present and ahead, nbf (if present) not
// ahead, iat a number no more than the clock allowance ahead and no older than the maximum age. The signed end,
// the earlier of iat plus the maximum age and exp, must leave at least the minimum life.
func portalCheckTimes(c portalClaims, now time.Time, maxAge time.Duration) (portalTimes, string) {
	exp, ok := c.date("exp")
	if !ok || !exp.After(now) {
		return portalTimes{}, SubjectTokenInvalid
	}
	if c.has("nbf") {
		if nbf, ok := c.date("nbf"); !ok || nbf.After(now) {
			return portalTimes{}, SubjectTokenInvalid
		}
	}
	iat, ok := c.date("iat")
	if !ok {
		return portalTimes{}, SubjectTokenIatInvalid
	}
	if iat.Sub(now) > portalClockAllowance {
		return portalTimes{}, SubjectTokenIatFuture
	}
	end := iat.Add(maxAge)
	if exp.Before(end) {
		end = exp
	}
	if now.Sub(iat) > maxAge || end.Sub(now) < portalMinGrantLife {
		return portalTimes{}, SubjectTokenStale
	}
	return portalTimes{issuedAt: iat, expiry: exp, grantEnd: end}, ""
}
