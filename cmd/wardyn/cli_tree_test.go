// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// #206: the CLI is one noun-verb tree (`wardyn <singular-noun> <verb>`, `set`
// the one upsert verb), clean break, no aliases.

// TestTopLevelCommandNamesAreUnique pins that no top-level command is
// registered twice (drive once was).
func TestTopLevelCommandNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range rootCmd().Commands() {
		if seen[c.Name()] {
			t.Errorf("top-level command %q is registered more than once", c.Name())
		}
		seen[c.Name()] = true
	}
}

// TestRemovedSpellingsAreUnknownCommands pins every old spelling as an
// unknown-command refusal, not a silent alias.
func TestRemovedSpellingsAreUnknownCommands(t *testing.T) {
	id := uuid.New().String()
	for _, args := range [][]string{
		{"approvals", "list"},
		{"approve", id},
		{"deny", id},
		{"logs", id},
		{"sessions", "list"},
		{"drive", "apply"},
		{"governance", "apply"},
		{"preset", "apply"},
		{"policy", "create"},
		{"policy", "update"},
		{"site-config", "apply"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			err := execCmd(t, args...)
			if err == nil {
				t.Fatalf("wardyn %s exited 0 — the old spelling must be gone", strings.Join(args, " "))
			}
			if !strings.Contains(err.Error(), "unknown command") {
				t.Errorf("error = %q, want an unknown-command refusal", err)
			}
		})
	}
}

// TestNewSpellingsAreReachable drives each new leaf far enough to prove the
// leaf itself ran (its own client-side validation fired).
func TestNewSpellingsAreReachable(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"approval", "approve", "not-a-uuid"}, "invalid approval id"},
		{[]string{"approval", "deny", "not-a-uuid"}, "invalid approval id"},
		{[]string{"run", "logs", "not-a-uuid"}, "invalid run id"},
		{[]string{"session", "revoke"}, "pass --sub"},
	} {
		err := execCmd(t, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("wardyn %s = %v, want an error containing %q", strings.Join(tc.args, " "), err, tc.want)
		}
	}
}

// TestSessionRevokeRequiresExactlyOneOfSubOrAll keeps the guard through the
// rename: both is refused, neither is refused, before any request is made.
func TestSessionRevokeRequiresExactlyOneOfSubOrAll(t *testing.T) {
	if err := execCmd(t, "session", "revoke", "--sub", "alice", "--all"); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("--sub with --all = %v, want the not-both refusal", err)
	}
	if err := execCmd(t, "session", "revoke"); err == nil || !strings.Contains(err.Error(), "pass --sub") {
		t.Errorf("neither flag = %v, want the pass-one refusal", err)
	}
}
