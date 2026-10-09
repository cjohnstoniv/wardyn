// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package agentpolicy generates the AGENT-SIDE half of a run's autonomy level: the managed-settings
// document an agent CLI reads before it will honour — or refuse — its own launch flags. The control-plane
// half only constrains what POST /runs will ACCEPT; this package is the agent-side expression of a level,
// and is PURE — no I/O, no server state.
//
// SECURITY invariant: documents are FROZEN LITERALS, not a struct marshalled per call — each is byte for
// byte the file exercised against the pinned CLI (CLAUDE_CODE_VERSION 2.1.231), and testdata/ holds the
// same bytes so the golden test compares against evidence, not this file's own opinion. A marshalled
// struct would carry an error path that fails OPEN; frozen text has none. Re-run the pinned-CLI checks on
// every CLI version bump — these keys are a contract with one vendor's parser, not a standard.
package agentpolicy

import (
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ClaudeCodeManagedSettingsPath is where Claude Code reads its managed (enterprise-policy) settings on
// Linux. Two directories deep by necessity: the parent is the k8s mount point, so a file directly under
// /etc would mount over the image's own /etc.
const ClaudeCodeManagedSettingsPath = "/etc/claude-code/managed-settings.json"

// The documents, one per rung that gets one (L2 has a locked variant too) — byte for byte the files
// the pinned-CLI check exercised. Each is spelled out in full rather than composed from shared
// fragments, so a key never arrives at a rung nobody chose it for.
const (
	// L0 "attended": interactive only, so the one thing left to enforce agent-side is that the agent may
	// not hand itself the bypass flag. allowManagedHooksOnly/allowManagedPermissionRulesOnly close the
	// routes by which a repo's own hooks or permission rules otherwise resolve a prompt before it renders;
	// defaultMode:default and disableAutoMode hold the rest shut.
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
	// L1 "gated": permits an unattended run but routes every gated tool call through wardyn-toolgate.
	// SECURITY: allowManagedHooksOnly/allowManagedPermissionRulesOnly are what make that gate load-bearing
	// — without them a workspace-writable hook or repo permission rule resolves the call before the gate's
	// permission-prompt tool ever runs.
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
	// L2 "unattended": auto-approval is PERMITTED here via --dangerously-skip-permissions, so this document
	// must NOT disable bypass mode — that would refuse the rung's own launch flag. Not
	// allowManagedPermissionRulesOnly either: bypass already resolves every call, so restricting rules
	// would only drop a person's own allow rules at an interactive attach, for no gain.
	claudeL2 = `{
  "permissions": {
    "defaultMode": "acceptEdits",
    "disableAutoMode": "disable"
  }
}
`
	// L2 locked (governance term agent_guardrail_locks): L2's document plus exactly the two keys that
	// stop a workspace-writable hook from running and a hook or repository permission rule from
	// answering a tool call (on the hold lane, before the gate). Off the hold lane L2 has no gate —
	// bypass answers every call — so there the measured effect is that repository and user hooks do
	// not run. Still no disableBypassPermissionsMode: it would refuse the rung's own
	// --dangerously-skip-permissions.
	claudeL2Locked = `{
  "permissions": {
    "defaultMode": "acceptEdits",
    "disableAutoMode": "disable"
  },
  "allowManagedHooksOnly": true,
  "allowManagedPermissionRulesOnly": true
}
`
)

// ForAgent returns the managed-settings file a run's agent should launch under at level, or ok=false when
// this run gets no agent-side layer. content is the exact file body, trailing newline included. hold is
// whether the run is on agent-run's hold lane. locked is the governance rubric's agent_guardrail_locks
// term; it selects the locked document at L2 and is ignored at every other level. ok=false is ORDINARY
// for any agent but claude-code, and for the two rungs that constrain nothing — not an error.
func ForAgent(agent string, level types.AutonomyLevel, hold, locked bool) (path string, content []byte, ok bool) {
	// Allowlist of one: a BYOA/custom agent has no managed-settings parser, so a denylist would generate a
	// document nothing enforces.
	if agent != "claude-code" {
		return "", nil, false
	}
	var doc string
	switch {
	case hold && HoldTakesOver(level, locked):
		// A hold run whose level brings no gate-protecting document: a repo/user permissions.allow rule
		// can resolve a call before the hold lane's gate is consulted, so L1's document is used instead.
		doc = claudeL1
	case level == "":
		return "", nil, false // no profile, or a rubric capping nothing here, off the hold lane
	case level == types.AutonomyL3:
		return "", nil, false // top rung permits task_mode=exec, routing around every other gate
	case lockedL2(level, locked):
		doc = claudeL2Locked
	case level == types.AutonomyL2:
		doc = claudeL2
	case level == types.AutonomyL1:
		doc = claudeL1
	default:
		// L0, and every value no AutonomyLevel defines: fails closed to the strictest document.
		doc = claudeL0
	}
	return ClaudeCodeManagedSettingsPath, []byte(doc), true
}

func lockedL2(level types.AutonomyLevel, locked bool) bool {
	return locked && level == types.AutonomyL2
}

// IsLocked reports whether content is the locked L2 document, so callers name the variant from the
// document ForAgent actually chose rather than re-deciding the selection.
func IsLocked(content []byte) bool { return string(content) == claudeL2Locked }

// HoldTakesOver reports whether a hold run at level gets L1's document instead of what its level brings:
// no level, and the rungs whose own answer leaves the gate unprotected (L3 no file, L2 no
// allowManagedPermissionRulesOnly — which the locked L2 document carries).
func HoldTakesOver(level types.AutonomyLevel, locked bool) bool {
	return level == "" || level == types.AutonomyL3 || (level == types.AutonomyL2 && !locked)
}
