// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"testing"
)

// TestDeprecatedEnvAliases_AllSixPairs pins UT-5's six WARDYN_MEMBER_* ->
// WARDYN_USER_* renames (user-types-design.md rev 4 §6): the deprecated name
// still works, one minor (0.8.x), and the new name always wins when both are
// set. Runs resolveDeprecatedEnvAliases directly rather than the full
// parseBootFlags — cliutil.EnvAlias itself is pinned in internal/cliutil, so
// this test's job is only that boot_flags.go wired all six pairs correctly.
func TestDeprecatedEnvAliases_AllSixPairs(t *testing.T) {
	for _, pair := range deprecatedEnvAliases {
		newEnv, oldEnv := pair[0], pair[1]
		t.Run(oldEnv, func(t *testing.T) {
			t.Setenv(newEnv, "")
			t.Setenv(oldEnv, "aliased-value")
			resolveDeprecatedEnvAliases()
			if got := os.Getenv(newEnv); got != "aliased-value" {
				t.Errorf("%s = %q after resolveDeprecatedEnvAliases with only %s set, want the deprecated value carried over", newEnv, got, oldEnv)
			}
		})
	}
}

// TestDeprecatedEnvAliases_NewNameWins pins that an operator who sets BOTH
// spellings is never surprised: the new name is never overwritten by the
// deprecated one.
func TestDeprecatedEnvAliases_NewNameWins(t *testing.T) {
	newEnv, oldEnv := "WARDYN_USER_DESKTOP", "WARDYN_MEMBER_MODE"
	t.Setenv(newEnv, "true")
	t.Setenv(oldEnv, "false")
	resolveDeprecatedEnvAliases()
	if got := os.Getenv(newEnv); got != "true" {
		t.Errorf("%s = %q, want unchanged (new name must win over the deprecated one)", newEnv, got)
	}
}

// TestParseBootFlags_HonoursDeprecatedMemberModeEnv is an end-to-end pin,
// through the real parseBootFlags, that a chart or compose file still setting
// WARDYN_MEMBER_MODE keeps working — the exact scenario D5's boot WARN exists
// for (user-types-design.md rev 4 §4).
func TestParseBootFlags_HonoursDeprecatedMemberModeEnv(t *testing.T) {
	resetFlags(t)
	t.Setenv("WARDYN_USER_DESKTOP", "")
	t.Setenv("WARDYN_MEMBER_MODE", "true")
	oldArgs := os.Args
	os.Args = []string{"wardynd-test"}
	t.Cleanup(func() { os.Args = oldArgs })
	f := parseBootFlags()
	if !*f.memberMode {
		t.Error("parseBootFlags did not honour the deprecated WARDYN_MEMBER_MODE=true with WARDYN_USER_DESKTOP unset")
	}
}
