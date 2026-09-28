// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package agentpolicy generates the AGENT-SIDE half of a run's autonomy level
// (0.8 #95): the managed-settings document an agent CLI reads before it will
// honour — or refuse — its own launch flags. The control-plane half only
// constrains what POST /runs will ACCEPT (internal/api/runs_autonomy.go);
// this package is the agent-side expression of a level, and is PURE — no I/O,
// no server state.
//
// The documents are FROZEN LITERALS rather than a struct marshalled per call:
// each is byte for byte the file exercised against the pinned CLI
// (CLAUDE_CODE_VERSION 2.1.231, deploy/images/claude-code/Dockerfile), and
// testdata/ holds the same bytes, so the golden test compares against evidence
// rather than this file's own opinion. A marshalled struct would carry an
// error path whose only answer fails OPEN; frozen text has none. Re-run the
// checks in the issue's spike on every CLI version bump — these keys are a
// contract with one vendor's parser, not a standard.
package agentpolicy

import (
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ClaudeCodeManagedSettingsPath is where Claude Code reads its managed
// (enterprise-policy) settings on Linux. Two directories deep by necessity: a
// managed file's parent is the mount point on the Kubernetes substrate, so a
// file directly under /etc would mount over the image's own /etc.
const ClaudeCodeManagedSettingsPath = "/etc/claude-code/managed-settings.json"

// The three documents, one per rung that gets one — byte for byte the files
// the pinned-CLI check exercised (testdata/ holds the same bytes). Each rung's
// document is spelled out in full rather than composed from shared fragments,
// so a key never arrives at a rung nobody chose it for.
const (
	// L0 "attended": interactive only, so the one thing left to enforce
	// agent-side is that the agent may not hand itself the bypass flag.
	// allowManagedHooksOnly and allowManagedPermissionRulesOnly close the
	// routes by which a checked-out repo's own hooks or permission rules
	// otherwise resolve a prompt before it renders (measured on the pinned
	// CLI, #334); defaultMode: default and disableAutoMode hold the rest shut.
	claudeL0 = `{
  "permissions": {
    "defaultMode": "default",
    "disableBypassPermissionsMode": "disable",
    "disableAutoMode": "disable"
  },
  "allowManagedHooksOnly": true,
  "allowManagedPermissionRulesOnly": true
}
`
	// L1 "gated": permits an unattended run but routes every gated tool call
	// through wardyn-toolgate (agent-run's hold branch). allowManagedHooksOnly
	// and allowManagedPermissionRulesOnly are what make that gate load-bearing
	// — without them a workspace-writable hook or repo permission rule
	// resolves the call before the gate's permission-prompt tool ever runs.
	claudeL1 = `{
  "permissions": {
    "defaultMode": "default",
    "disableBypassPermissionsMode": "disable",
    "disableAutoMode": "disable"
  },
  "allowManagedHooksOnly": true,
  "allowManagedPermissionRulesOnly": true
}
`
	// L2 "unattended": auto-approval is PERMITTED at this rung via
	// --dangerously-skip-permissions, so this document must NOT disable the
	// bypass mode — that would refuse the rung's own launch flag. Not
	// allowManagedPermissionRulesOnly either: the bypass already resolves
	// every call, so restricting rules would only drop a person's own allow
	// rules at an interactive attach, for no gain.
	claudeL2 = `{
  "permissions": {
    "defaultMode": "acceptEdits",
    "disableAutoMode": "disable"
  }
}
`
)

// ForAgent returns the managed-settings file a run's agent should be launched
// under at level, or ok=false when this run gets no agent-side layer at all.
// content is the exact file body, trailing newline included. hold is whether
// the run launches on agent-run's hold lane (WARDYN_TOOL_APPROVALS=hold).
// ok=false is the ORDINARY answer for any agent but claude-code, and for the
// two rungs that constrain nothing — not an error.
func ForAgent(agent string, level types.AutonomyLevel, hold bool) (path string, content []byte, ok bool) {
	// Allowlist of one: a BYOA image or custom agent has no managed-settings
	// parser, so a denylist would generate a document nothing enforces.
	if agent != "claude-code" {
		return "", nil, false
	}
	var doc string
	switch {
	case hold && HoldTakesOver(level):
		// A hold run whose level brings no gate-protecting document (#358):
		// a repo/user permissions.allow rule can resolve a call before the
		// hold lane's gate is consulted, so L1's document is used instead.
		doc = claudeL1
	case level == "":
		// No profile, or a rubric that caps nothing at this posture, off the
		// hold lane — the run this deployment launched before the feature
		// existed.
		return "", nil, false
	case level == types.AutonomyL3:
		// The top rung permits task_mode=exec, which routes around every
		// other gate; a document that constrains nothing would only be
		// mistaken for a ceiling.
		return "", nil, false
	case level == types.AutonomyL2:
		doc = claudeL2
	case level == types.AutonomyL1:
		doc = claudeL1
	default:
		// L0, and every non-empty value no AutonomyLevel defines — fails
		// closed to the strictest document rather than letting an unrecognized
		// level shed the ceiling.
		doc = claudeL0
	}
	return ClaudeCodeManagedSettingsPath, []byte(doc), true
}

// HoldTakesOver reports whether a hold run at level gets the hold lane's
// document (L1's) instead of what its level brings: no level, and the two
// rungs whose own answer leaves the gate unprotected (L3 no file, L2 no
// allowManagedPermissionRulesOnly).
func HoldTakesOver(level types.AutonomyLevel) bool {
	return level == "" || level == types.AutonomyL2 || level == types.AutonomyL3
}
