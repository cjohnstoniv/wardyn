// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// derive_rank_test.go pins the THREE-TIER role fold: deriveRole's arm 2
// resolves the highest-ranking match (member < security_admin < admin) rather
// than the pre-0.7 pair of booleans, and the tiers around that fold — arm 1,
// the operator allowlist, and the default-role fallthrough — are unchanged by
// its arrival.
package oidc_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// TestDeriveRoleHighestWins is the fold itself. Every case builds a claim set
// that matches MORE THAN ONE map entry (or an allowlist plus a map entry) so
// the outcome is decided by rank and by nothing else.
//
// The claim order is varied deliberately across cases: a fold that took the
// LAST match rather than the highest would pass half of these and fail the
// other half, which no single-ordering table would catch.
func TestDeriveRoleHighestWins(t *testing.T) {
	roleMap := map[string]string{
		"wardyn.admin":    oidc.RoleAdmin,
		"wardyn.security": oidc.RoleSecurityAdmin,
		"eng-team":        oidc.RoleMember,
	}
	for _, tc := range []struct {
		name           string
		roles, groups  []string
		email          string
		operatorEmails []string
		want           string
	}{
		// The new tier, and the pre-0.7 code's blind spot: "security_admin"
		// was not a case in the old switch, so it contributed nothing and the
		// member row decided this.
		{"security_admin beats member", []string{"Wardyn.Security"}, []string{"eng-team"}, "", nil, oidc.RoleSecurityAdmin},
		{"security_admin beats member (order flipped)", []string{"eng-team"}, []string{"Wardyn.Security"}, "", nil, oidc.RoleSecurityAdmin},

		{"admin beats security_admin", []string{"Wardyn.Security", "Wardyn.Admin"}, nil, "", nil, oidc.RoleAdmin},
		{"admin beats security_admin (order flipped)", []string{"Wardyn.Admin", "Wardyn.Security"}, nil, "", nil, oidc.RoleAdmin},

		// The pre-0.7 rule, still exactly true.
		{"admin beats member", nil, []string{"eng-team", "Wardyn.Admin"}, "", nil, oidc.RoleAdmin},
		{"all three at once resolves admin", []string{"eng-team"}, []string{"Wardyn.Security", "Wardyn.Admin"}, "", nil, oidc.RoleAdmin},

		// The allowlist injects a TOP-RANK match, so it outranks a
		// security_admin map row the same email or claim also hits.
		{"operator allowlist beats a security_admin row", []string{"Wardyn.Security"}, nil, "ops@corp.example",
			[]string{"ops@corp.example"}, oidc.RoleAdmin},
		{"operator allowlist beats a member row", []string{"eng-team"}, nil, "ops@corp.example",
			[]string{"ops@corp.example"}, oidc.RoleAdmin},

		// A lone match of each tier still resolves to itself.
		{"lone security_admin match", []string{"Wardyn.Security"}, nil, "", nil, oidc.RoleSecurityAdmin},
		{"lone member match", []string{"eng-team"}, nil, "", nil, oidc.RoleMember},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, ok := oidc.DeriveRoleForTest(tc.roles, tc.groups, tc.email, roleMap, tc.operatorEmails, "")
			if !ok {
				t.Fatalf("derive denied the login; want role %q", tc.want)
			}
			if got != tc.want {
				t.Fatalf("role = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDeriveRoleSecurityAdminProvenance: a security_admin map row produces a
// Match carrying ITS OWN role, not the winning one — PreviewRole renders this
// list to explain an outcome, so a losing match must still show what it was.
func TestDeriveRoleSecurityAdminProvenance(t *testing.T) {
	roleMap := map[string]string{
		"wardyn.security": oidc.RoleSecurityAdmin,
		"eng-team":        oidc.RoleMember,
	}
	role, matches, ok := oidc.DeriveRoleForTest([]string{"Wardyn.Security"}, []string{"eng-team"}, "", roleMap, nil, "")
	if !ok || role != oidc.RoleSecurityAdmin {
		t.Fatalf("role = %q ok=%v, want %q true", role, ok, oidc.RoleSecurityAdmin)
	}
	var wantSec, wantMember bool
	for _, m := range matches {
		if m.Source != oidc.MatchSourceMapRow {
			t.Fatalf("unexpected match source %q", m.Source)
		}
		switch m.Role {
		case oidc.RoleSecurityAdmin:
			wantSec = true
			if m.Value != "Wardyn.Security" {
				t.Fatalf("match value = %q, want the literal claim %q", m.Value, "Wardyn.Security")
			}
		case oidc.RoleMember:
			wantMember = true
		default:
			t.Fatalf("match carries role %q, want the row's own role", m.Role)
		}
	}
	if !wantSec || !wantMember {
		t.Fatalf("provenance dropped a match: %+v (want both the winning security_admin row and the losing member row)", matches)
	}
}

// TestDeriveRoleArm1UntouchedBySecurityAdmin: with NO role map there is no way
// to express the third tier, and nothing about arm 1 may change — this is the
// upgrade-safety guarantee for every deployment running on the operator
// allowlist alone (or on nothing at all).
func TestDeriveRoleArm1UntouchedBySecurityAdmin(t *testing.T) {
	for _, tc := range []struct {
		name           string
		email          string
		operatorEmails []string
		want           string
	}{
		{"no map, no allowlist: everyone is admin", "dev@corp.example", nil, oidc.RoleAdmin},
		{"no map, listed email: admin", "ops@corp.example", []string{"ops@corp.example"}, oidc.RoleAdmin},
		{"no map, unlisted email: member", "dev@corp.example", []string{"ops@corp.example"}, oidc.RoleMember},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A security_admin-shaped claim is present in every case and must
			// be inert: arm 1 never consults claims at all.
			got, _, ok := oidc.DeriveRoleForTest([]string{"Wardyn.Security"}, []string{"security_admin"}, tc.email, nil, tc.operatorEmails, "")
			if !ok {
				t.Fatal("arm 1 denied a login; it never denies")
			}
			if got != tc.want {
				t.Fatalf("role = %q, want %q", got, tc.want)
			}
			if got == oidc.RoleSecurityAdmin {
				t.Fatal("arm 1 produced security_admin; it is a MAPPED tier only")
			}
		})
	}
}

// TestDeriveRoleUnknownMapValueIsInert: a value that is not a valid role
// contributes nothing to the fold AND produces no Match — the exhaustive-case
// behavior of the pre-0.7 switch, now expressed as a ValidRole guard. Without
// it, a garbage row would rank 0 yet still appear in the provenance list as if
// it had been consulted.
func TestDeriveRoleUnknownMapValueIsInert(t *testing.T) {
	roleMap := map[string]string{"eng-team": oidc.RoleMember, "weird": "superadmin"}
	role, matches, ok := oidc.DeriveRoleForTest([]string{"weird"}, []string{"eng-team"}, "", roleMap, nil, "")
	if !ok || role != oidc.RoleMember {
		t.Fatalf("role = %q ok=%v, want %q true", role, ok, oidc.RoleMember)
	}
	if slices.ContainsFunc(matches, func(m oidc.Match) bool { return m.Value == "weird" }) {
		t.Fatalf("an invalid role value produced a Match: %+v", matches)
	}
}

// TestDeriveRoleDefaultRoleFallthroughUnchanged: the third tier does not
// change arm 3. A map that grants ONLY security_admin still falls through to
// defaultRole for anyone it does not match, and still denies when defaultRole
// is unset — the boot side is what refuses security_admin AS that default
// (cmd/wardynd's validDefaultRole), never this function.
func TestDeriveRoleDefaultRoleFallthroughUnchanged(t *testing.T) {
	roleMap := map[string]string{"wardyn.security": oidc.RoleSecurityAdmin}
	if role, _, ok := oidc.DeriveRoleForTest(nil, []string{"eng-team"}, "", roleMap, nil, oidc.RoleMember); !ok || role != oidc.RoleMember {
		t.Fatalf("no-match with a default = %q/%v, want member/true", role, ok)
	}
	if role, _, ok := oidc.DeriveRoleForTest(nil, []string{"eng-team"}, "", roleMap, nil, ""); ok {
		t.Fatalf("no-match with no default = %q/%v, want deny", role, ok)
	}
}

// TestParseRoleMapAcceptsSecurityAdmin: the chart carries the third tier the
// moment ValidRole accepts it — this is the ONLY boot knob that grants it, so
// a refusal here would leave the tier unreachable. The error text for a real
// typo must name all three, or an operator reading it concludes the value they
// just used does not exist.
func TestParseRoleMapAcceptsSecurityAdmin(t *testing.T) {
	m, err := oidc.ParseRoleMap("Wardyn.Admin=admin,Wardyn.Security=security_admin,eng-team=member")
	if err != nil {
		t.Fatalf("ParseRoleMap rejected security_admin: %v", err)
	}
	if m["wardyn.security"] != oidc.RoleSecurityAdmin {
		t.Fatalf("parsed map = %v, want wardyn.security => %q", m, oidc.RoleSecurityAdmin)
	}
	_, err = oidc.ParseRoleMap("x=superadmin")
	if err == nil {
		t.Fatal("ParseRoleMap accepted an invalid role")
	}
	if !strings.Contains(err.Error(), oidc.RoleSecurityAdmin) {
		t.Fatalf("invalid-role error does not name %q: %v", oidc.RoleSecurityAdmin, err)
	}
}

// TestValidRoleAcceptsExactlyThree bounds the set: mergeRoleMaps and the
// console /access write boundary both gate on ValidRole, so a fourth value
// slipping in here would reach a session cookie.
func TestValidRoleAcceptsExactlyThree(t *testing.T) {
	for _, ok := range []string{oidc.RoleAdmin, oidc.RoleSecurityAdmin, oidc.RoleMember} {
		if !oidc.ValidRole(ok) {
			t.Fatalf("ValidRole(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "Admin", "security-admin", "securityadmin", "superadmin", "owner"} {
		if oidc.ValidRole(bad) {
			t.Fatalf("ValidRole(%q) = true", bad)
		}
	}
}
