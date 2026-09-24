// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// TestParseDefaultRoleAliasesLegacyMember: WARDYN_OIDC_DEFAULT_ROLE=member
// still boots, as the user tier, with the frozen WARN; the real values pass
// silently and security_admin stays refused.
func TestParseDefaultRoleAliasesLegacyMember(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	got, err := parseDefaultRole(" member ")
	if err != nil || got != oidc.RoleUser {
		t.Fatalf("parseDefaultRole(member) = %q, %v; want %q", got, err, oidc.RoleUser)
	}
	want := `WARDYN_OIDC_DEFAULT_ROLE: "member" is no longer a role. WARDYN_OIDC_DEFAULT_ROLE=member maps to the built-in user type "standard" (Standard user) until you change it in your chart. The alias is removed in 0.9.`
	if w := oidc.LegacyRoleMemberWarning("WARDYN_OIDC_DEFAULT_ROLE", ""); w != want {
		t.Errorf("warning = %q\nwant      %q", w, want)
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "The alias is removed in 0.9.") {
		t.Errorf("no boot WARN for the aliased default:\n%s", buf.String())
	}

	buf.Reset()
	for _, v := range []string{"", oidc.RoleAdmin, oidc.RoleUser} {
		if got, err := parseDefaultRole(v); err != nil || got != v {
			t.Errorf("parseDefaultRole(%q) = %q, %v; want it unchanged", v, got, err)
		}
	}
	if buf.Len() != 0 {
		t.Errorf("a current value logged a warning:\n%s", buf.String())
	}
	for _, v := range []string{oidc.RoleSecurityAdmin, "owner"} {
		if _, err := parseDefaultRole(v); err == nil {
			t.Errorf("parseDefaultRole(%q) accepted it", v)
		}
	}
}
