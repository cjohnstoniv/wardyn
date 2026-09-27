// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package agentpolicy generates the AGENT-SIDE half of a run's autonomy level
// (0.8 #95): the managed-settings document an agent CLI reads before it will
// honour — or refuse — its own launch flags.
//
// The control-plane half only constrains what POST /runs will ACCEPT
// (internal/api/runs_autonomy.go). Inside the sandbox the agent still runs with
// whatever its harness defaults to, so a level that is never expressed
// agent-side is a level the agent itself never hears about. This package is
// that expression and nothing else: PURE — no I/O, no server state. Delivering
// the file root-owned is the runner contract's job (runner.ManagedFile, #94)
// and recording what was generated is dispatch's
// (internal/api/runs_dispatch_agentpolicy.go).
//
// The documents are FROZEN LITERALS rather than a struct marshalled per call,
// and that is a security property rather than a shortcut. Each is byte for byte
// the file exercised against the CLI this tree pins — CLAUDE_CODE_VERSION
// 2.1.231, deploy/images/claude-code/Dockerfile — and testdata/ holds the same
// bytes as they came out of that check, so the golden test is a comparison
// against evidence rather than against this file's own opinion. A struct
// re-derived per call would carry a marshal error path whose only possible
// answer is "this run gets no managed layer": a ceiling that fails OPEN. Frozen
// text has no such path, and a reviewer reads the exact bytes the sandbox gets
// in the source of this package.
//
// Re-run the checks in the issue's spike on every CLI version bump. These keys
// are a contract with one vendor's parser, not a standard.
package agentpolicy

import (
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ClaudeCodeManagedSettingsPath is where Claude Code reads its managed
// (enterprise-policy) settings on Linux. Two directories deep by necessity, not
// by taste: a managed file's PARENT is the mount point on the Kubernetes
// substrate, so a file directly under /etc would mount over the image's own
// /etc (runner.ValidateManagedFiles).
const ClaudeCodeManagedSettingsPath = "/etc/claude-code/managed-settings.json"

// The three documents, one per rung that gets one. Byte for byte the files the
// pinned-CLI check exercised; testdata/ holds the same bytes.
//
// What is ABSENT from each is as load-bearing as what is present, so each is
// spelled out in full rather than composed from shared fragments: a composed
// document is one where a key can arrive at a rung nobody chose it for.
const (
	// L0 "attended" — interactive only; the gate refuses this rung an
	// unattended run, seeded auto tools and task_mode=exec alike, so the one
	// thing left to say agent-side is that the agent may not hand ITSELF the
	// bypass flag. `disableBypassPermissionsMode` was verified to refuse
	// --dangerously-skip-permissions outright rather than warn about it.
	//
	// `allowManagedHooksOnly` here too, for the reason #334 measured on the
	// pinned CLI: with only the bypass key, a repository-scoped PreToolUse hook
	// answering "allow" resolved the CLI's own permission prompt before it
	// rendered, so a checked-out repo could answer the question this rung
	// exists to ask a person. With the key, the same hook never ran and the
	// prompt rendered.
	//
	// The remaining keys close the same route through the other settings a
	// repository can carry, each measured on the pinned CLI:
	//   - `allowManagedPermissionRulesOnly`: without it a repo `permissions.allow`
	//     rule resolved the call and no prompt rendered. It also drops rules
	//     from user settings and --allowedTools, which only ever widen here.
	//   - `defaultMode: default`: without it a repo `defaultMode: acceptEdits`
	//     brought the session up accepting edits, and Write ran unasked.
	//   - `disableAutoMode`: auto mode is unreachable on this pin, so this one
	//     holds the door for a CLI bump rather than closing a measured route.
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
	// L1 "gated" — the rung that permits an unattended run but derives its
	// tool approvals to `hold`, which routes every gated tool call through
	// wardyn-toolgate (agent-run's hold branch).
	//
	// `allowManagedHooksOnly` is what makes that gate worth installing. Without
	// it a repository- or user-scoped PreToolUse hook — inside the workspace
	// the agent itself can write — runs BEFORE the permission prompt tool is
	// consulted and can resolve the call, so the run would show a gate in its
	// launch flags and obey a hook instead. The check that mattered most in the
	// spike is the other half of it: the literal hold-branch invocation
	// (--mcp-config … --strict-mcp-config --permission-prompt-tool
	// mcp__gate__approve) still starts under this key, so the key does not buy
	// the hooks ceiling at the cost of the gate.
	//
	// `allowManagedPermissionRulesOnly` for the same reason one layer over: a
	// repo `permissions.allow` rule resolves a call before the permission
	// prompt tool too, and without the key the hold lane's approve tool was
	// never called and the tool ran. With it, the gate was called for the same
	// repo file. `defaultMode` and `disableAutoMode` as at L0, for an
	// interactive attach to an L1 run (the hold branch passes its own
	// --permission-mode).
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
	// L2 "unattended" — auto-approval and seeded auto tools are PERMITTED at
	// this rung, and agent-run's autonomous branch launches them with
	// --dangerously-skip-permissions.
	//
	// So this document must NOT disable the bypass mode. Adding the L0/L1 key
	// here would kill the very lane the rung exists to permit, at its first
	// tool call, with a refusal that reads as a CLI bug rather than as a policy
	// — a file that looks stricter and does the opposite of what it claims.
	// `defaultMode: acceptEdits` is the honest agent-side statement of the same
	// posture for the interactive attach, where no launch flag is passed at all.
	//
	// `disableAutoMode` as on every rung. NOT `allowManagedPermissionRulesOnly`:
	// this rung permits --dangerously-skip-permissions, which resolves every
	// call without consulting a rule, so restricting whose rules count would
	// constrain nothing the rung does not already allow — and would drop a
	// person's own allow rules at an interactive attach for no gain.
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
//
// content is the exact file body, trailing newline included; the delivery
// contract adds nothing to it.
//
// hold is whether the run launches on agent-run's hold lane
// (WARDYN_TOOL_APPROVALS=hold), whatever level, or no level, it resolved to.
//
// ok=false is the ORDINARY answer, not an error: every agent but claude-code
// has no managed-settings mechanism to express a level through, and the two
// rungs that constrain nothing are better served by no file than by an empty
// one. A caller that gets ok=false has nothing to deliver and nothing to fix.
func ForAgent(agent string, level types.AutonomyLevel, hold bool) (path string, content []byte, ok bool) {
	// An allowlist of one, for agentHasHoldLane's reason
	// (internal/api/runs_autonomy.go): a BYOA image or a custom agent has no
	// Wardyn launcher and no managed-settings parser, so a denylist would
	// generate a document for an agent that silently ignores it — a ceiling
	// recorded in the audit trail and enforced nowhere.
	if agent != "claude-code" {
		return "", nil, false
	}
	var doc string
	switch {
	case hold && HoldTakesOver(level):
		// A hold run whose level brings no gate-protecting document (#358).
		// The hold lane's gate is a gate only if nothing answers a tool call
		// before it, and a repository or user `permissions.allow` rule does:
		// the CLI resolves it before consulting the permission prompt tool,
		// and the agent can write both files. The L1 document is the one
		// written and verified for exactly this lane (its comment above).
		// Its bypass refusal costs a hold run nothing: the hold branch never
		// passes --dangerously-skip-permissions, and must not.
		doc = claudeL1
	case level == "":
		// No profile, no rubric, or a rubric that caps nothing at this
		// posture, off the hold lane. Byte for byte the run this deployment
		// launched before the feature existed — the absent-row rule every
		// governance limit follows.
		return "", nil, false
	case level == types.AutonomyL3:
		// The top rung permits task_mode=exec, the door that routes around
		// every other gate. A document that constrains nothing is worse than
		// no document: it puts a managed-policy file on disk for a reader to
		// mistake for a ceiling.
		return "", nil, false
	case level == types.AutonomyL2:
		doc = claudeL2
	case level == types.AutonomyL1:
		doc = claudeL1
	default:
		// L0, and every non-empty value no AutonomyLevel defines. A stored
		// level nobody authored ranks BELOW L0 (types.AutonomyLevel.Rank), so
		// the gate refuses it everything; the agent-side half fails closed the
		// same way rather than letting a corrupted column be the one route to
		// shedding the ceiling. A rung added to the type and forgotten here
		// lands here too, and is over-restricted rather than unmanaged.
		doc = claudeL0
	}
	return ClaudeCodeManagedSettingsPath, []byte(doc), true
}

// HoldTakesOver reports whether a hold run at level gets the hold lane's
// document (L1's) instead of what its level brings: no level, and the two
// rungs whose own answer leaves the gate unprotected (L3 no file, L2 no
// allowManagedPermissionRulesOnly). The one spelling of that set, for ForAgent
// and for a caller naming why a run got its file.
func HoldTakesOver(level types.AutonomyLevel) bool {
	return level == "" || level == types.AutonomyL2 || level == types.AutonomyL3
}
