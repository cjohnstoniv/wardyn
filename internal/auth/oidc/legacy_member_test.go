// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// captureWarnings swaps the default slog logger for the length of the test and
// returns what it wrote.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestParseRoleMapAliasesLegacyMember: a chart that still maps an App Role to
// "member" boots and signs that person in as the user tier, and says so once
// per entry in the frozen words, naming 0.9 as the release that removes it.
func TestParseRoleMapAliasesLegacyMember(t *testing.T) {
	logs := captureWarnings(t)

	m, err := oidc.ParseRoleMap("Wardyn.Admin=admin,Wardyn.Member=member,eng-team=member")
	if err != nil {
		t.Fatalf("ParseRoleMap refused the legacy member value: %v", err)
	}
	if m["wardyn.member"] != oidc.RoleUser || m["eng-team"] != oidc.RoleUser || m["wardyn.admin"] != oidc.RoleAdmin {
		t.Fatalf("parsed map = %v, want both member entries on %q", m, oidc.RoleUser)
	}
	role, _, ok := oidc.DeriveRoleForTest([]string{"Wardyn.Member"}, nil, "dev@corp.example", m, nil, "")
	if !ok || role != oidc.RoleUser {
		t.Fatalf("a Wardyn.Member sign-in derived %q/%v, want %q/true", role, ok, oidc.RoleUser)
	}

	want := `WARDYN_OIDC_ROLE_MAP: "member" is no longer a role. Entry "Wardyn.Member=member" maps to the built-in user type "standard" (Standard user) until you remap it in Getting started -> People, or in your chart. The alias is removed in 0.9.`
	if got := oidc.LegacyRoleMemberWarning("WARDYN_OIDC_ROLE_MAP", "Wardyn.Member=member"); got != want {
		t.Errorf("warning = %q\nwant      %q", got, want)
	}
	out := logs.String()
	if n := strings.Count(out, "level=WARN"); n != 2 {
		t.Errorf("logged %d warnings, want one per aliased entry (2):\n%s", n, out)
	}
	if !strings.Contains(out, `Entry \"Wardyn.Member=member\"`) || !strings.Contains(out, `Entry \"eng-team=member\"`) {
		t.Errorf("the warnings do not name each entry:\n%s", out)
	}
}

// TestLegacyMemberAliasLosesToCustomUserType (0.8 testing plan T-23, UT-2b):
// proves the FULL path from a raw WARDYN_OIDC_ROLE_MAP string through
// ParseRoleMap's "=member" substitution into the type-precedence decision —
// not DeriveForTest fed a hand-built roleMap that already assumes the
// substitution happened (TestDeriveUserType's "the built-in type never wins
// against a custom one" case does that). ParseRoleMap turns "=member" into
// the plain user-tier value, which SplitMappingTarget resolves to the
// built-in type; pickUserType must still let a custom type mapped alongside
// it win, so a leftover chart alias can never lock an org onto "standard".
func TestLegacyMemberAliasLosesToCustomUserType(t *testing.T) {
	logs := captureWarnings(t)

	m, err := oidc.ParseRoleMap("eng-team=member,pm-group=portfolio-manager")
	if err != nil {
		t.Fatalf("ParseRoleMap refused a legacy alias beside a custom type: %v", err)
	}
	d := oidc.DeriveForTest(nil, []string{"eng-team", "pm-group"}, "", m, nil, "", orgTypes)
	if d.Denial != "" || d.Role != oidc.RoleUser || d.UserType != "portfolio-manager" {
		t.Fatalf("derivation = %+v, want the user tier on the custom type \"portfolio-manager\", not the aliased built-in one", d)
	}
	if !strings.Contains(logs.String(), `Entry \"eng-team=member\"`) {
		t.Errorf("the alias substitution logged no warning:\n%s", logs.String())
	}
}

// TestLegacyMemberIsNeverARole: the alias lives only where configuration is
// parsed. "member" is not a role a console row, a session or a token snapshot
// can carry, so ValidRole refuses it and the fold ignores a map that holds it.
func TestLegacyMemberIsNeverARole(t *testing.T) {
	if oidc.ValidRole(oidc.LegacyRoleMember) {
		t.Fatal(`ValidRole("member") = true; the retired word must never reach a session`)
	}
	if role, _, ok := oidc.DeriveRoleForTest(nil, []string{"eng"}, "", map[string]string{"eng": "member"}, nil, ""); ok {
		t.Fatalf(`a map row holding "member" derived %q; it must contribute nothing`, role)
	}
}
