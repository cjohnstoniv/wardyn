// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

func runPreUpgrade(t *testing.T, args ...string) (string, error) {
	t.Helper()
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
		`3 leftover "member" role(s)`,
		`WARDYN_OIDC_ROLE_MAP entry "Wardyn.Member = member"`,
		`WARDYN_OIDC_ROLE_MAP entry "eng=member"`,
		"WARDYN_OIDC_DEFAULT_ROLE=member",
		`remap each to "user"`,
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
}

func TestSetupStatusPreUpgradeReadsTheEnvironment(t *testing.T) {
	t.Setenv("WARDYN_OIDC_ROLE_MAP", "g=member")
	t.Setenv("WARDYN_OIDC_DEFAULT_ROLE", "")
	if _, err := runPreUpgrade(t); err == nil {
		t.Fatal("the environment's role map was not checked")
	}
}
