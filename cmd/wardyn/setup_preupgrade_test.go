// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

var removedEnvNames = []string{
	"WARDYN_MEMBER_MODE", "WARDYN_MEMBER_WORKSPACE_ROOTS", "WARDYN_MEMBER_WORKSPACE_ROOTS_MAP",
	"WARDYN_MEMBER_WRITABLE_ROOTS", "WARDYN_MEMBER_WRITABLE_DENY", "WARDYN_ALLOW_MEMBER_ENV_SECRET",
}

func runPreUpgrade(t *testing.T, args ...string) (string, error) {
	t.Helper()
	for _, n := range removedEnvNames {
		t.Setenv(n, "")
	}
	root := rootCmd()
	out := &strings.Builder{}
	root.SetArgs(append([]string{"setup", "status", "--pre-upgrade"}, args...))
	root.SetOut(out)
	root.SetErr(&strings.Builder{})
	err := root.Execute()
	return out.String(), err
}

// TestSetupStatusPreUpgradeListsLeftoverMember: the 0.9 pre-flight names each role-map pair and the
// default role that still say "member" and fails, without contacting any server.
func TestSetupStatusPreUpgradeListsLeftoverMember(t *testing.T) {
	out, err := runPreUpgrade(t,
		"--role-map", "Wardyn.Admin=admin, Wardyn.Member = member ,eng=member,ops=user,broken",
		"--default-role", "member")
	if err == nil {
		t.Fatal("a leftover member role must fail the check")
	}
	for _, want := range []string{
		`3 leftover setting(s)`,
		`WARDYN_OIDC_ROLE_MAP entry "Wardyn.Member = member"`,
		`WARDYN_OIDC_ROLE_MAP entry "eng=member"`,
		"WARDYN_OIDC_DEFAULT_ROLE=member",
		`remap a "member" role to "user"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ops=user") || strings.Contains(out, "Wardyn.Admin") {
		t.Errorf("a current entry was listed:\n%s", out)
	}
}

func TestSetupStatusPreUpgradePassesACleanConfig(t *testing.T) {
	out, err := runPreUpgrade(t, "--role-map", "a=admin,b=user,c=portfolio-manager", "--default-role", "user")
	if err != nil || !strings.Contains(out, "no leftover") {
		t.Fatalf("clean config: err = %v, out = %s", err, out)
	}
	for _, want := range []string{"this shell's environment", "set -a; . FILE; set +a"} {
		if !strings.Contains(out, want) {
			t.Errorf("a clean result must say what it did not read; output lacks %q:\n%s", want, out)
		}
	}
}

func TestSetupStatusPreUpgradeReadsTheEnvironment(t *testing.T) {
	t.Setenv("WARDYN_OIDC_ROLE_MAP", "g=member")
	t.Setenv("WARDYN_OIDC_DEFAULT_ROLE", "")
	if _, err := runPreUpgrade(t); err == nil {
		t.Fatal("the environment's role map was not checked")
	}
}

// TestSetupStatusPreUpgradeListsRemovedEnv: a leftover removed variable fails the check, naming it and
// its replacement; the empty value compose forwards for an unset one does not.
func TestSetupStatusPreUpgradeListsRemovedEnv(t *testing.T) {
	root := rootCmd()
	out := &strings.Builder{}
	for _, n := range removedEnvNames {
		t.Setenv(n, "")
	}
	t.Setenv("WARDYN_MEMBER_WRITABLE_DENY", "/srv/a")
	root.SetArgs([]string{"setup", "status", "--pre-upgrade", "--role-map", "a=admin"})
	root.SetOut(out)
	root.SetErr(&strings.Builder{})
	if err := root.Execute(); err == nil {
		t.Fatal("a leftover WARDYN_MEMBER_WRITABLE_DENY must fail the check")
	}
	if !strings.Contains(out.String(), "WARDYN_MEMBER_WRITABLE_DENY (use WARDYN_USER_WRITABLE_DENY)") {
		t.Errorf("output lacks the variable and its replacement:\n%s", out.String())
	}
	if strings.Contains(out.String(), "WARDYN_MEMBER_MODE") {
		t.Errorf("an empty variable was listed:\n%s", out.String())
	}
}
