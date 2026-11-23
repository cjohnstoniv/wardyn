// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"testing"
)

// unitAdminSubjects reads the same email trust rule as the capability grants: an email sign-in does not
// vouch for is no subject, whatever WARDYN_CAPABILITY_GRANTS_VERIFIED_EMAIL says.
func TestUnitAdminSubjects_DropsAnEmailSignInDoesNotVouchFor(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		emailKeys, grantSwitchesOff bool
		wantUsers                   []string
	}{
		{"email not vouched for", false, false, []string{capSub}},
		{"email keys opted in", true, false, []string{capSub, capEmail}},
		{"the capability switch off does not change it", false, true, []string{capSub}},
	} {
		srv := capServer(&capStore{})
		srv.cfg.AllowEmailMappings, srv.cfg.UnverifiedEmailCapGrantsMatch = tc.emailKeys, tc.grantSwitchesOff
		users, groups := srv.unitAdminSubjects(memberCtx([]string{"eng"}))
		if !slices.Equal(users, tc.wantUsers) || !slices.Equal(groups, []string{"eng"}) {
			t.Errorf("%s: users, groups = %v, %v; want %v, [eng]", tc.name, users, groups, tc.wantUsers)
		}
	}
}
