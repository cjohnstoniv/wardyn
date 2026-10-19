// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// With WARDYN_CAPABILITY_GRANTS_VERIFIED_EMAIL on, an allow written for the
// caller's email matches only when sign-in vouches for the email; a deny on it
// binds either way, and the switch off changes nothing.
func TestCapAllowed_EmailAllowFollowsTheEmailTrustRule(t *testing.T) {
	allow, deny := types.CapabilityAllow, types.CapabilityDeny
	enf := map[string]bool{capEgressHost: true}
	onEmail := grant(types.CapabilitySubjectUser, capEmail, capEgressHost, "pypi.org", allow)
	for _, tc := range []struct {
		name          string
		grants        []types.CapabilityGrant
		on, emailKeys bool
		want          bool
	}{
		{"switch off: an allow on the unverified email still matches", []types.CapabilityGrant{onEmail}, false, false, true},
		{"switch on: an allow on the unverified email grants nothing", []types.CapabilityGrant{onEmail}, true, false, false},
		{"switch on, email keys opted in: the email is trusted", []types.CapabilityGrant{onEmail}, true, true, true},
		{"switch on: an allow on the sub matches", []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capSub, capEgressHost, "pypi.org", allow)}, true, false, true},
		{"switch on: a deny on the unverified email still binds", []types.CapabilityGrant{
			grant(types.CapabilitySubjectUser, capSub, capEgressHost, "pypi.org", allow),
			grant(types.CapabilitySubjectUser, capEmail, capEgressHost, "pypi.org", deny),
		}, true, false, false},
	} {
		srv := capServer(&capStore{grants: tc.grants, enf: enf})
		srv.cfg.CapabilityGrantsVerifiedEmail, srv.cfg.AllowEmailMappings = tc.on, tc.emailKeys
		got, err := srv.capAllowed(memberCtx(nil), capEgressHost, "pypi.org")
		if err != nil || got != tc.want {
			t.Errorf("%s: capAllowed = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
}
