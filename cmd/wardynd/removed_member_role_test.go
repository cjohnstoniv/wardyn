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

// TestRefuseRemovedEnv: a leftover removed variable refuses boot, naming it and its replacement. Every
// one is covered; an empty value (compose's unset) and the current names boot.
func TestRefuseRemovedEnv(t *testing.T) {
	for old, repl := range map[string]string{
		"WARDYN_MEMBER_MODE":                "WARDYN_USER_DESKTOP",
		"WARDYN_MEMBER_WORKSPACE_ROOTS":     "WARDYN_USER_WORKSPACE_ROOTS",
		"WARDYN_MEMBER_WORKSPACE_ROOTS_MAP": "WARDYN_USER_WORKSPACE_ROOTS_MAP",
		"WARDYN_MEMBER_WRITABLE_ROOTS":      "WARDYN_USER_WRITABLE_ROOTS",
		"WARDYN_MEMBER_WRITABLE_DENY":       "WARDYN_USER_WRITABLE_DENY",
		"WARDYN_ALLOW_MEMBER_ENV_SECRET":    "WARDYN_ALLOW_USER_ENV_SECRET",
	} {
		err := refuseRemovedEnv([]string{"PATH=/bin", old + "=x"})
		if err == nil || !strings.Contains(err.Error(), old+" (use "+repl+")") || !strings.Contains(err.Error(), "refusing to start") {
			t.Errorf("%s=x: err = %v, want a refusal naming it and %s", old, err, repl)
		}
		if err := refuseRemovedEnv([]string{old + "=", repl + "=x"}); err != nil {
			t.Errorf("%s empty beside the current name refused: %v", old, err)
		}
	}
}

// TestValidateBootPostureRefusesRemovedEnv: the refusal is wired into boot, ahead of everything that
// would read the flags.
func TestValidateBootPostureRefusesRemovedEnv(t *testing.T) {
	t.Setenv("WARDYN_MEMBER_WRITABLE_DENY", "/srv/a")
	err := validateBootPosture(&bootFlags{}, tlsPosture{})
	if err == nil || !strings.Contains(err.Error(), "WARDYN_MEMBER_WRITABLE_DENY (use WARDYN_USER_WRITABLE_DENY)") {
		t.Fatalf("validateBootPosture err = %v, want the removed-variable refusal", err)
	}
}
