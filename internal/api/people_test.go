// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPersonMintRefusal_DerivationArms pins the two refusals the PG tests do
// not reach through their role map: a derivation that admits no sign-in, and
// an elevated role that rests only on the default role, which the person's
// still-unknown groups might have narrowed (a login with unreadable claims is
// refused for the same reason).
func TestPersonMintRefusal_DerivationArms(t *testing.T) {
	s := New(Config{})
	super := roleSnapshotCtx(oidc.RoleAdmin)
	byDefault := []oidc.Match{{Source: oidc.MatchSourceDefaultRole, Role: oidc.RoleAdmin}}
	for _, tc := range []struct {
		name   string
		d      oidc.Derivation
		status int
		// reason is the LITERAL wire string (#656 slice 2 review round S4),
		// not the Go constant, so a rename of the constant's value cannot
		// pass this test unnoticed. "" for the two admitted rows.
		reason string
	}{
		{"no sign-in", oidc.Derivation{Denial: oidc.DenialNoRole}, http.StatusConflict, "no_sign_in"},
		{"elevated default role", oidc.Derivation{Role: oidc.RoleAdmin, UserType: types.UserTypeStandard, Matches: byDefault}, http.StatusConflict, "default_role_unknown_groups"},
		{"user default role", oidc.Derivation{Role: oidc.RoleUser, UserType: types.UserTypeStandard,
			Matches: []oidc.Match{{Source: oidc.MatchSourceDefaultRole, Role: oidc.RoleUser}}}, 0, ""},
		{"mapped admin, super admin caller", oidc.Derivation{Role: oidc.RoleAdmin, UserType: types.UserTypeStandard}, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason, _ := s.personMintRefusal(super, tc.d)
			if got != tc.status {
				t.Fatalf("status = %d, want %d", got, tc.status)
			}
			if reason != tc.reason {
				t.Errorf("reason = %q, want the literal %q", reason, tc.reason)
			}
		})
	}
	if got, reason, _ := s.personMintRefusal(roleSnapshotCtx(oidc.RoleSecurityAdmin),
		oidc.Derivation{Role: oidc.RoleSecurityAdmin, UserType: types.UserTypeStandard}); got != http.StatusForbidden || reason != "elevated_target" {
		t.Fatalf("security admin minting for a security admin: %d %q, want 403 elevated_target", got, reason)
	}
}
