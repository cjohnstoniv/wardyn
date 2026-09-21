// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package agentpolicy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// goldenFor reads one of the spike's per-level files. These are EVIDENCE, not
// expectations this package authored: each is the file that was mounted at
// /etc/claude-code/managed-settings.json while the key it carries was observed
// taking effect on the CLI version deploy/images/claude-code/Dockerfile pins.
// When the two disagree, the golden file is right.
func goldenFor(t *testing.T, level types.AutonomyLevel) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", string(level)+"-managed-settings.json"))
	if err != nil {
		t.Fatalf("read golden for %s: %v", level, err)
	}
	return b
}

// TestAgentPolicyMatchesTheVerifiedGoldenPerLevel is the golden-file check, one
// case per rung that gets a file: what ForAgent generates has to be the bytes
// that were actually exercised against the pinned CLI, trailing newline
// included.
func TestAgentPolicyMatchesTheVerifiedGoldenPerLevel(t *testing.T) {
	for _, level := range []types.AutonomyLevel{types.AutonomyL0, types.AutonomyL1, types.AutonomyL2} {
		t.Run(string(level), func(t *testing.T) {
			path, content, ok := ForAgent("claude-code", level)
			if !ok {
				t.Fatalf("ForAgent(claude-code, %s) ok=false, want a managed-settings file", level)
			}
			if path != ClaudeCodeManagedSettingsPath {
				t.Errorf("path = %q, want %q", path, ClaudeCodeManagedSettingsPath)
			}
			if want := goldenFor(t, level); string(content) != string(want) {
				t.Errorf("generated managed settings differ from the verified golden file.\n got: %q\nwant: %q", content, want)
			}
			var decoded map[string]any
			if err := json.Unmarshal(content, &decoded); err != nil {
				t.Errorf("generated managed settings are not valid JSON: %v", err)
			}
		})
	}
}

// TestAgentPolicySupervisedRungsIgnoreRepoPermissions is the trap that
// defeats the whole feature, asserted by name so a golden-file swap fails with
// a sentence that says what broke rather than only as a byte diff.
//
// L0 and L1 are the rungs where something other than the agent answers a
// tool call: a person at the CLI's own prompt (L0), or wardyn-toolgate on the
// hold lane (L1). Each key below closes one way a checked-out repository — a
// file the agent can write — answered that call first on the pinned CLI:
//   - allowManagedHooksOnly: a repo PreToolUse hook answering "allow" (#334);
//   - allowManagedPermissionRulesOnly: a repo `permissions.allow` rule;
//   - defaultMode=default: a repo `defaultMode: acceptEdits`;
//   - disableBypassPermissionsMode / disableAutoMode: the agent handing itself
//     a mode that answers every call.
func TestAgentPolicySupervisedRungsIgnoreRepoPermissions(t *testing.T) {
	for _, level := range []types.AutonomyLevel{types.AutonomyL0, types.AutonomyL1} {
		t.Run(string(level), func(t *testing.T) {
			_, content, ok := ForAgent("claude-code", level)
			if !ok {
				t.Fatalf("ForAgent(claude-code, %s) ok=false, want managed settings", level)
			}
			var doc struct {
				AllowManagedHooksOnly           bool `json:"allowManagedHooksOnly"`
				AllowManagedPermissionRulesOnly bool `json:"allowManagedPermissionRulesOnly"`
				Permissions                     struct {
					DefaultMode                  string `json:"defaultMode"`
					DisableBypassPermissionsMode string `json:"disableBypassPermissionsMode"`
					DisableAutoMode              string `json:"disableAutoMode"`
				} `json:"permissions"`
			}
			if err := json.Unmarshal(content, &doc); err != nil {
				t.Fatalf("decode %s managed settings: %v", level, err)
			}
			if !doc.AllowManagedHooksOnly {
				t.Error("allowManagedHooksOnly unset: a repository PreToolUse hook can answer the call first")
			}
			if !doc.AllowManagedPermissionRulesOnly {
				t.Error("allowManagedPermissionRulesOnly unset: a repository permissions.allow rule can answer the call first")
			}
			if doc.Permissions.DefaultMode != "default" {
				t.Errorf("defaultMode = %q, want \"default\": a repository defaultMode can start the session accepting edits",
					doc.Permissions.DefaultMode)
			}
			if doc.Permissions.DisableBypassPermissionsMode != "disable" {
				t.Errorf("disableBypassPermissionsMode = %q, want \"disable\"", doc.Permissions.DisableBypassPermissionsMode)
			}
			if doc.Permissions.DisableAutoMode != "disable" {
				t.Errorf("disableAutoMode = %q, want \"disable\"", doc.Permissions.DisableAutoMode)
			}
		})
	}
}

// TestAgentPolicyUnattendedRungKeepsTheBypassMode is the same trap read
// backwards, and it is the one that fails SILENTLY in production: L2 permits
// auto-approval and seeded auto tools, and agent-run launches both with
// --dangerously-skip-permissions. Disabling the bypass mode here kills that
// lane at its first tool call while the file looks stricter than L1's.
func TestAgentPolicyUnattendedRungKeepsTheBypassMode(t *testing.T) {
	_, content, ok := ForAgent("claude-code", types.AutonomyL2)
	if !ok {
		t.Fatal("ForAgent(claude-code, L2) ok=false, want the unattended rung's managed settings")
	}
	var doc struct {
		Permissions map[string]any `json:"permissions"`
	}
	if err := json.Unmarshal(content, &doc); err != nil {
		t.Fatalf("decode L2 managed settings: %v", err)
	}
	if _, present := doc.Permissions["disableBypassPermissionsMode"]; present {
		t.Error("L2 disables the bypass permissions mode: the unattended lane launches with " +
			"--dangerously-skip-permissions, so this file refuses the run its own rung exists to permit")
	}
	if doc.Permissions["defaultMode"] != "acceptEdits" {
		t.Errorf("L2 permissions.defaultMode = %v, want \"acceptEdits\"", doc.Permissions["defaultMode"])
	}
	if doc.Permissions["disableAutoMode"] != "disable" {
		t.Errorf("L2 permissions.disableAutoMode = %v, want \"disable\"", doc.Permissions["disableAutoMode"])
	}
}

// TestAgentPolicyNoFileWhereNothingIsBound pins the two rungs that get NO file:
// the unrestricted one, and a run no rubric bound at all. An empty document
// would be worse than none — a managed-policy file on disk for the next reader
// to mistake for a ceiling.
func TestAgentPolicyNoFileWhereNothingIsBound(t *testing.T) {
	for _, level := range []types.AutonomyLevel{types.AutonomyL3, ""} {
		path, content, ok := ForAgent("claude-code", level)
		if ok || path != "" || content != nil {
			t.Errorf("ForAgent(claude-code, %q) = (%q, %q, %v), want no file", level, path, content, ok)
		}
	}
}

// TestAgentPolicyOnlyClaudeCodeGetsAFile pins the allowlist of one. codex-cli
// reads no managed-policy file (the spike found nothing in the tree that does),
// and a BYOA run has no Wardyn launcher at all — a generated document for
// either is a ceiling in the audit trail and nowhere else.
func TestAgentPolicyOnlyClaudeCodeGetsAFile(t *testing.T) {
	for _, agent := range []string{"codex-cli", "", "Claude-Code", "claude-code-next"} {
		for _, level := range []types.AutonomyLevel{types.AutonomyL0, types.AutonomyL1, types.AutonomyL2, types.AutonomyL3, ""} {
			if _, _, ok := ForAgent(agent, level); ok {
				t.Errorf("ForAgent(%q, %q) ok=true, want no managed-settings file for a non-claude-code agent", agent, level)
			}
		}
	}
}

// TestAgentPolicyUndefinedLevelFailsClosed is the corrupted-column case. A
// stored level no AutonomyLevel defines ranks below L0 and the launch gate
// refuses it everything, so the agent-side half must be the most restrictive
// document rather than none — otherwise a bad row is the one way to shed the
// ceiling without changing a rubric.
func TestAgentPolicyUndefinedLevelFailsClosed(t *testing.T) {
	for _, level := range []types.AutonomyLevel{"L9", "bogus", "l1", " L1"} {
		_, content, ok := ForAgent("claude-code", level)
		if !ok {
			t.Errorf("ForAgent(claude-code, %q) ok=false: an undefined level must fail closed, not unmanaged", level)
			continue
		}
		if want := goldenFor(t, types.AutonomyL0); string(content) != string(want) {
			t.Errorf("ForAgent(claude-code, %q) content = %q, want the L0 document %q", level, content, want)
		}
	}
}
