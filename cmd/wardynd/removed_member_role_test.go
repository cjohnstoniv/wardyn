// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// TestParseDefaultRoleRefusesRemovedMember: WARDYN_OIDC_DEFAULT_ROLE=member no longer boots as the
// user tier; the real values pass and security_admin stays refused.
func TestParseDefaultRoleRefusesRemovedMember(t *testing.T) {
	_, err := parseDefaultRole(" member ")
	if err == nil {
		t.Fatal("parseDefaultRole(member) accepted the removed role")
	}
	for _, want := range []string{`invalid WARDYN_OIDC_DEFAULT_ROLE "member"`, "removed in 0.9", `"user"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal lacks %q: %v", want, err)
		}
	}
	for _, v := range []string{"", oidc.RoleAdmin, oidc.RoleUser} {
		if got, err := parseDefaultRole(v); err != nil || got != v {
			t.Errorf("parseDefaultRole(%q) = %q, %v; want it unchanged", v, got, err)
		}
	}
	// "Owner" is not a type id either (ids are lowercase slugs).
	for _, v := range []string{oidc.RoleSecurityAdmin, "Owner", "denied"} {
		if _, err := parseDefaultRole(v); err == nil {
			t.Errorf("parseDefaultRole(%q) accepted it", v)
		}
	}
}
