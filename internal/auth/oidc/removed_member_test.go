// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// TestParseRoleMapRefusesRemovedMember: 0.9 no longer reads "member" as the user tier. A role map that
// still holds it fails to parse, and the error names the entry and says what to use instead.
func TestParseRoleMapRefusesRemovedMember(t *testing.T) {
	for _, csv := range []string{"Wardyn.Member=member", "Wardyn.Admin=admin, eng-team = member"} {
		_, err := oidc.ParseRoleMap(csv)
		if err == nil {
			t.Fatalf("ParseRoleMap(%q) accepted the removed member role", csv)
		}
		for _, want := range []string{`invalid role "member"`, "removed in 0.9", `"user"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ParseRoleMap(%q) error lacks %q: %v", csv, want, err)
			}
		}
	}
	if _, err := oidc.ParseRoleMap("Wardyn.Member=member"); err == nil || !strings.Contains(err.Error(), `"Wardyn.Member=member"`) {
		t.Errorf("the error must name the entry as written, got %v", err)
	}
	if m, err := oidc.ParseRoleMap("a=admin,b=user,c=portfolio-manager"); err != nil || len(m) != 3 {
		t.Errorf("a current map must still parse: %v, %v", m, err)
	}
}

// TestRemovedMemberIsNeverARole: "member" is not a role a console row, a session or a token snapshot
// can carry, is never a user type id, and a map row holding it contributes nothing.
func TestRemovedMemberIsNeverARole(t *testing.T) {
	if oidc.ValidRole(oidc.RemovedRoleMember) || oidc.ValidMappingTarget(oidc.RemovedRoleMember) {
		t.Fatal(`"member" is still accepted as a role or mapping target`)
	}
	if !oidc.UserTypeIDReserved(oidc.RemovedRoleMember) {
		t.Fatal(`"member" must stay reserved so no user type can take the word`)
	}
	if role, _, ok := oidc.DeriveRoleForTest(nil, []string{"eng"}, "", map[string]string{"eng": "member"}, nil, ""); ok {
		t.Fatalf(`a map row holding "member" derived %q; it must contribute nothing`, role)
	}
}
