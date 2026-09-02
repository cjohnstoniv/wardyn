// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// F2-sso-to-ceiling PROBE 2 — destination: internal/auth/oidc/role_precedence_probe_test.go
//
// Package oidc_test (black-box through export_test.go's hooks), same style as
// role_mappings_test.go / derive_rank_test.go. Pure table test — no store, no
// signed ID token, no IdP.
//
// INVARIANT UNDER TEST: role precedence across the THREE config sources and the
// IdP's claims is exactly (derive.go:400-418, 238-275):
//
//	source:  operator allowlist  >  chart WARDYN_OIDC_ROLE_MAP  >  console rows
//	         (a console row can never override a chart key or an allowlisted
//	         email — it is SHADOWED, not merged);
//	claims:  highest-ranking match wins regardless of which claim (roles /
//	         groups / email) produced it (admin > security_admin > member);
//	fallthrough: DefaultRole only when NOTHING matched; never security_admin
//	         by fallthrough (validDefaultRole is cmd/wardynd's, not testable
//	         here — the derive-side half is that "" denies).
//
// Run:
//
//	cd /home/cjohn/wt-v07-profiles && \
//	cp local/review-0.7/deep/F2-sso-to-ceiling/role_precedence_probe_test.go internal/auth/oidc/ && \
//	nice -n 10 GOMAXPROCS=8 go test ./internal/auth/oidc/ -run 'TestF2_' -count=1 -p 4 -v ; \
//	rm -f internal/auth/oidc/role_precedence_probe_test.go
package oidc_test

import (
	"testing"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

func TestF2_RoleMapPrecedence_ChartOverConsoleOverClaims(t *testing.T) {
	const (
		bob   = "bob@corp.example"
		alice = "alice@corp.example"
	)
	chart := map[string]string{
		"wardyn.admin": writoidc.RoleAdmin,
		"kernel-team":  writoidc.RoleAdmin,
		"eng-team":     writoidc.RoleMember,
	}
	allowlist := []string{"Alice@Corp.Example"} // mixed case on purpose: matching is EqualFold

	cases := []struct {
		name     string
		rows     []writoidc.RoleMapping
		roles    []string
		groups   []string
		email    string
		def      string
		wantRole string
		wantOK   bool
		// wantShadowed lists console rows that must contribute NOTHING.
		wantShadowed []string
	}{
		{
			name:   "console row cannot promote a chart MEMBER key to admin",
			rows:   []writoidc.RoleMapping{{Value: "eng-team", Role: writoidc.RoleAdmin}},
			groups: []string{"eng-team"}, email: bob,
			wantRole: writoidc.RoleMember, wantOK: true, wantShadowed: []string{"eng-team"},
		},
		{
			name:  "console row cannot demote a chart ADMIN key to member",
			rows:  []writoidc.RoleMapping{{Value: "wardyn.admin", Role: writoidc.RoleMember}},
			roles: []string{"Wardyn.Admin"}, email: bob,
			wantRole: writoidc.RoleAdmin, wantOK: true, wantShadowed: []string{"wardyn.admin"},
		},
		{
			name:     "console row cannot demote an operator-allowlisted email",
			rows:     []writoidc.RoleMapping{{Value: alice, Role: writoidc.RoleMember}},
			email:    "ALICE@corp.example",
			wantRole: writoidc.RoleAdmin, wantOK: true, wantShadowed: []string{alice},
		},
		{
			name:     "allowlisted email beats a security_admin map row on the same email",
			rows:     []writoidc.RoleMapping{{Value: alice, Role: writoidc.RoleSecurityAdmin}},
			email:    alice,
			wantRole: writoidc.RoleAdmin, wantOK: true, wantShadowed: []string{alice},
		},
		{
			name:   "console row on a fresh key IS honoured (security_admin via groups)",
			rows:   []writoidc.RoleMapping{{Value: "sec-ops", Role: writoidc.RoleSecurityAdmin}},
			groups: []string{"eng-team", "Sec-Ops"}, email: bob,
			wantRole: writoidc.RoleSecurityAdmin, wantOK: true,
		},
		{
			name:   "highest rank wins across claims: member via groups, admin via roles",
			groups: []string{"eng-team"}, roles: []string{"WARDYN.ADMIN"}, email: bob,
			wantRole: writoidc.RoleAdmin, wantOK: true,
		},
		{
			name:  "highest rank wins across claims: security_admin via console groups, member via chart roles",
			rows:  []writoidc.RoleMapping{{Value: "sec-ops", Role: writoidc.RoleSecurityAdmin}},
			roles: []string{"eng-team"}, groups: []string{"sec-ops"}, email: bob,
			wantRole: writoidc.RoleSecurityAdmin, wantOK: true,
		},
		{
			name:   "nothing matched, no default: DENIED",
			groups: []string{"unmapped"}, email: bob,
			wantRole: "", wantOK: false,
		},
		{
			name:   "nothing matched, default member",
			groups: []string{"unmapped"}, email: bob, def: writoidc.RoleMember,
			wantRole: writoidc.RoleMember, wantOK: true,
		},
		{
			name:   "a match beats the default even when the default is higher",
			groups: []string{"eng-team"}, email: bob, def: writoidc.RoleAdmin,
			wantRole: writoidc.RoleMember, wantOK: true,
		},
		{
			name:     "non-ASCII claim never folds onto an ASCII admin key (KELVIN SIGN for k)",
			roles:    []string{"\u212aernel-team"}, // strings.ToLower folds U+212A to 'k'; ASCIIOnly must refuse first
			email:    bob,
			wantRole: "", wantOK: false,
		},
		{
			name:   "a non-canonical console row (mixed case) is dropped, not matched",
			rows:   []writoidc.RoleMapping{{Value: "Sec-Ops", Role: writoidc.RoleAdmin}},
			groups: []string{"sec-ops"}, email: bob,
			wantRole: "", wantOK: false, wantShadowed: []string{"Sec-Ops"},
		},
		{
			name:   "an empty console row never matches an empty claim",
			rows:   []writoidc.RoleMapping{{Value: "", Role: writoidc.RoleAdmin}},
			groups: []string{""}, email: bob,
			wantRole: "", wantOK: false, wantShadowed: []string{""},
		},
		{
			name:   "an invalid role in a console row is dropped and does not decide",
			rows:   []writoidc.RoleMapping{{Value: "sec-ops", Role: "superuser"}},
			groups: []string{"sec-ops"}, email: bob,
			wantRole: "", wantOK: false, wantShadowed: []string{"sec-ops"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, shadowed := writoidc.MergeRoleMapsForTest(chart, allowlist, tc.rows)
			for _, want := range tc.wantShadowed {
				found := false
				for _, s := range shadowed {
					if s == want {
						found = true
					}
				}
				if !found {
					t.Errorf("row %q was NOT shadowed (shadowed=%v) — a console row overrode a stronger source", want, shadowed)
				}
			}
			role, _, ok := writoidc.DeriveRoleForTest(tc.roles, tc.groups, tc.email, merged, allowlist, tc.def)
			if ok != tc.wantOK || role != tc.wantRole {
				t.Errorf("deriveRole = (%q, ok=%v), want (%q, ok=%v)", role, ok, tc.wantRole, tc.wantOK)
			}
		})
	}
}

// TestF2_RoleMapArm1_NoMapMeansAllowlistOnly pins arm 1 (derive.go:420-436):
// with an EMPTY merged map the allowlist alone splits admin/member, and with
// neither source EVERY human is admin — the posture under which no ceiling ever
// binds anyone (effectiveCeiling short-circuits on isOperator).
func TestF2_RoleMapArm1_NoMapMeansAllowlistOnly(t *testing.T) {
	merged, _ := writoidc.MergeRoleMapsForTest(nil, []string{"ops@corp.example"}, nil)
	if len(merged) != 0 {
		t.Fatalf("merged = %v, want empty", merged)
	}
	if role, _, ok := writoidc.DeriveRoleForTest([]string{"Wardyn.Admin"}, []string{"anything"}, "dev@corp.example", merged, []string{"ops@corp.example"}, ""); !ok || role != writoidc.RoleMember {
		t.Errorf("no map, not allowlisted = (%q,%v), want member (claims are IGNORED under arm 1)", role, ok)
	}
	if role, _, ok := writoidc.DeriveRoleForTest(nil, nil, "OPS@corp.example", merged, []string{"ops@corp.example"}, ""); !ok || role != writoidc.RoleAdmin {
		t.Errorf("no map, allowlisted = (%q,%v), want admin", role, ok)
	}
	// The dangerous posture, stated so a reviewer sees it: no map, no allowlist.
	if role, _, ok := writoidc.DeriveRoleForTest(nil, nil, "anyone@anywhere.example", nil, nil, ""); !ok || role != writoidc.RoleAdmin {
		t.Errorf("no map, no allowlist = (%q,%v); derive.go:429-431 says everyone is admin — if this changed, cmd/wardynd's validateOperatorPosture and the docs must change with it", role, ok)
	}
}
