// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package agentpolicy

import "github.com/cjohnstoniv/wardyn/internal/types"

// InnerWalls is the harness's own walls a run launches with (#2822), read off the launch facts that
// choose them: hold is the run's hold lane, seedSkips an interactive run whose boot seed skips the
// harness's approvals (seed_auto_tools), and sandbox the Codex sandbox mode the run's managed
// requirements bind (CodexRequirements), "" for none. ok=false for anything but the two catalog
// harnesses, whose launch Wardyn writes: a custom image's agent, and an exec run, have no walls Wardyn
// knows.
//
// Claude Code's own sandbox is never on: its image ships no bubblewrap. Without sandbox, an
// interactive Codex session runs under Codex's own default sandbox, which Wardyn does not set, so its
// SandboxMode is empty. A seeded Codex session that skips approvals launches with the bypass flag,
// which the requirements refuse, so sandbox never applies to it.
func InnerWalls(harness string, interactive, hold, seedSkips bool, sandbox string) (types.HarnessInnerWalls, bool) {
	w := types.HarnessInnerWalls{SandboxMode: types.InnerSandboxOff, Approvals: types.InnerApprovalsNone}
	switch harness {
	case "claude-code":
		switch {
		case interactive && !seedSkips:
			w.Approvals = types.InnerApprovalsHarness
		case !interactive && hold:
			w.Approvals = types.InnerApprovalsWardyn
		}
	case "codex-cli":
		switch {
		case interactive && !seedSkips:
			w = types.HarnessInnerWalls{SandboxMode: sandbox, Approvals: types.InnerApprovalsHarness}
		case !interactive && sandbox != "":
			w.SandboxMode = sandbox
		}
	default:
		return types.HarnessInnerWalls{}, false
	}
	return w, true
}

// CodexSandboxStarts reports whether Codex's own sandbox (bubblewrap, which needs user namespaces)
// starts in mode at class, as measured under Wardyn's hardening on codex-cli 0.149.1: Docker's
// default seccomp refuses user namespaces at CC1; gVisor (CC2) admits them, but bubblewrap cannot
// raise a new network namespace's loopback there, so only a background run's workspace-write starts
// (agent-run keeps the sandbox's network, which Wardyn's egress walls); a microVM (CC3) starts both.
func CodexSandboxStarts(mode string, class types.ConfinementClass, interactive bool) bool {
	switch class {
	case types.CC3:
		return mode == types.InnerSandboxReadOnly || mode == types.InnerSandboxWorkspaceWrite
	case types.CC2:
		return mode == types.InnerSandboxWorkspaceWrite && !interactive
	}
	return false
}

// CodexRequirementsPath is where Codex reads its managed requirements on Linux: a file the agent
// cannot rewrite (runner.CodexManagedFileDir), which binds every codex the sandbox starts.
const CodexRequirementsPath = "/etc/codex/requirements.toml"

// The requirements, one per sandbox mode, byte for byte the files exercised against the pinned CLI
// (CODEX_VERSION 0.149.1; testdata/ holds the same bytes). Codex refuses a mode outside
// allowed_sandbox_modes, the bypass flag's danger-full-access included (exit 1), and requires
// read-only in the set; a session asking for a mode the set lacks falls back to read-only.
const (
	codexReadOnly       = "allowed_sandbox_modes = [\"read-only\"]\n"
	codexWorkspaceWrite = "allowed_sandbox_modes = [\"read-only\", \"workspace-write\"]\n"
)

// CodexRequirements returns the requirements file that binds Codex to sandbox mode, or ok=false for
// a mode that is not one of the harness's own sandbox modes.
func CodexRequirements(mode string) (path string, content []byte, ok bool) {
	switch mode {
	case types.InnerSandboxReadOnly:
		return CodexRequirementsPath, []byte(codexReadOnly), true
	case types.InnerSandboxWorkspaceWrite:
		return CodexRequirementsPath, []byte(codexWorkspaceWrite), true
	}
	return "", nil, false
}

// InnerWallsMiss names the first axis of a policy's choice (want) that a run's walls (got) do not
// meet, or "" when they meet every axis want sets. looser reports a choice that would loosen the run
// below what it launches with: no approvals where the run asks, or no sandbox where the harness keeps
// its own. Such a choice is refused, never built.
func InnerWallsMiss(want *types.HarnessInnerWalls, got types.HarnessInnerWalls) (axis string, looser bool) {
	if want == nil {
		return "", false
	}
	if want.SandboxMode != "" && want.SandboxMode != got.SandboxMode {
		return "sandbox_mode", want.SandboxMode == types.InnerSandboxOff
	}
	if want.Approvals != "" && want.Approvals != got.Approvals {
		return "approvals", want.Approvals == types.InnerApprovalsNone
	}
	return "", false
}
