// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// export_test.go exposes internal functions for white-box testing.
// This file is only compiled in test builds.
package oidc

import (
	"encoding/base64"
	"net/http"
)

// EncodeSessionForTest calls the unexported encodeSession method so that
// test files can build synthetic session cookies without going through the
// full OAuth2 flow.
func EncodeSessionForTest(a *Authenticator, sess Session) (*http.Cookie, error) {
	return a.encodeSession(sess)
}

// EncodeRawSessionForTest signs an ARBITRARY payload with the session HMAC. It
// exists for exactly one case encodeSession cannot reach: a cookie written by
// an OLDER binary, whose JSON carries no "groups" key at all rather than a null
// one. Session.Groups's nil-vs-empty contract only means anything if that
// cookie is reproducible byte for byte.
func EncodeRawSessionForTest(a *Authenticator, payload []byte) *http.Cookie {
	return &http.Cookie{
		Name: sessionCookieName,
		Value: base64.RawURLEncoding.EncodeToString(payload) + "." +
			base64.RawURLEncoding.EncodeToString(sessionHMAC(a.hmacKey, payload)),
		Path: "/",
	}
}

// SessionGroupsForTest exposes the unexported claim normalizer so the union /
// dedupe / sort / byte-cap rules can be pinned directly, without driving a
// signed ID token through the whole callback once per case.
func SessionGroupsForTest(rolesClaim, groupsClaim []string) []string {
	g, _ := sessionGroups(rolesClaim, groupsClaim, nil)
	return g
}

// SessionGroupsTruncatedForTest exposes sessionGroups' PF-26 truncation bit —
// the half that is an authorization input rather than a normalization result.
// claimNames is the token's `_claim_names` object (nil for the byte-cap cases),
// so the IdP-side overage rule pins without a signed token too.
func SessionGroupsTruncatedForTest(rolesClaim, groupsClaim []string, claimNames map[string]any) bool {
	_, truncated := sessionGroups(rolesClaim, groupsClaim, claimNames)
	return truncated
}

// OverageWidensRoleForTest exposes the login guard that refuses to let an IdP
// claim overage promote a session through the DefaultRole fallthrough. It is
// the ROLE half of the same "was this token answerable" question
// SessionGroupsTruncatedForTest asks about the group snapshot; the two share
// claimsOverage, and pinning them separately is what keeps them from diverging
// again.
func OverageWidensRoleForTest(claimNames map[string]any, role string, matches []Match) bool {
	return overageWidensRole(claimNames, role, matches)
}

// MaxSessionGroupsBytesForTest exposes the cookie byte budget.
const MaxSessionGroupsBytesForTest = maxSessionGroupsBytes

// NewRewriteTransportForTest exposes the unexported split-horizon transport
// constructor for white-box testing.
func NewRewriteTransportForTest(publicURL, internalURL string, base *http.Client) (http.RoundTripper, error) {
	return newRewriteTransport(publicURL, internalURL, base)
}

// SetRevocationsForTest wires a SessionRevocations store onto an already-built
// Authenticator (D16's Middleware revocation-check tests build the
// Authenticator through the normal full-discovery newAuth helper, then add
// the store afterward — cfg is otherwise unexported).
func SetRevocationsForTest(a *Authenticator, r SessionRevocations) {
	a.cfg.Revocations = r
}

// DeriveRoleForTest exposes deriveRole for direct table-testing of role
// derivation precedence and Match provenance, without driving a signed ID
// token through the whole callback for every case.
func DeriveRoleForTest(rolesClaim, groupsClaim []string, email string, roleMap map[string]string, legacyAdminEmails []string, defaultRole string) (role string, matches []Match, ok bool) {
	return deriveRole(rolesClaim, groupsClaim, email, roleMap, legacyAdminEmails, defaultRole)
}

// MergeRoleMapsForTest exposes mergeRoleMaps so the chart/console merge rules
// (shadowing, disjoint union, posture-flip transitions) table-test as a pure
// function, without a store or a signed ID token.
func MergeRoleMapsForTest(chart map[string]string, legacyAdminEmails []string, rows []RoleMapping) (merged map[string]string, shadowed []string) {
	return mergeRoleMaps(chart, legacyAdminEmails, rows)
}

// EmailDomainAllowedForTest exposes the WARDYN_OIDC_EMAIL_DOMAINS gate so the
// fold-escalation ordering (guard the RAW domain, THEN lower) pins directly as
// a table, the way emailInList's and CanonicalGroupSubject's twin guards
// already do — without signing an id_token per case.
func EmailDomainAllowedForTest(email string, allowed []string) bool {
	return emailDomainAllowed(email, allowed)
}
