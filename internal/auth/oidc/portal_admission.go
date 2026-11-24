// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// portalGroupsClaimDefault names the claim a portal's group is read from when the registration names none.
const portalGroupsClaimDefault = "groups"

// portalAdmission is what the optional checks of an issuer portal found.
type portalAdmission struct {
	matched []string // names of the claim matches that held
	groups  []string // the token's groups, canonical; only when the portal sets a group
}

// checkAdmission applies the portal's optional checks to a verified token, in order: the one claim match, then
// the group, then (ID-token profile only) the email domain list. A portal that sets none admits on the token
// checks alone. The claim match reads the verified claims only: the claim must be a JSON string equal byte for
// byte to the configured value, and every other shape refuses the same way.
func (reg PortalRegistration) checkAdmission(c portalClaims) (portalAdmission, string) {
	var out portalAdmission
	if reg.RequiredClaim != "" {
		got, ok := c.str(reg.RequiredClaim)
		if !ok || got != reg.RequiredClaimValue {
			return portalAdmission{}, SubjectTokenClaimMismatch
		}
		out.matched = []string{reg.RequiredClaim}
	}
	if reg.Group != "" {
		want, ok := types.CanonicalGroupSubject(reg.Group)
		if !ok {
			return portalAdmission{}, PortalOutsideScope
		}
		name := reg.GroupsClaim
		if name == "" {
			name = portalGroupsClaimDefault
		}
		// Decoded tolerantly: a scalar or any other shape is no groups at all, so the portal's group is missing.
		listed, _ := c.strList(name)
		for _, g := range listed {
			if canon, ok := types.CanonicalGroupSubject(g); ok {
				out.groups = append(out.groups, canon)
			}
		}
		if !portalHasAll(out.groups, []string{want}) {
			return portalAdmission{}, PortalOutsideScope
		}
	}
	if reg.TokenProfile == PortalProfileIDToken && len(reg.EmailDomains) > 0 {
		if denial := reg.checkEmailDomain(c); denial != "" {
			return portalAdmission{}, denial
		}
	}
	return out, ""
}

// checkEmailDomain is the optional domain list of the ID-token profile: the email must be verified and its domain
// listed. It gates admission only; the email is never used to name or authorize a person.
func (reg PortalRegistration) checkEmailDomain(c portalClaims) string {
	raw, present := c["email_verified"]
	if !present {
		return DenialEmailVerifiedAbsent
	}
	if string(raw) != "true" {
		return DenialEmailUnverified
	}
	email, _ := c.str("email")
	if !emailDomainAllowed(email, reg.EmailDomains) {
		return DenialEmailDomain
	}
	return ""
}
