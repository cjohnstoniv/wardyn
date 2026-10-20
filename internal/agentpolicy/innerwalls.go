// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package agentpolicy

import "github.com/cjohnstoniv/wardyn/internal/types"

// InnerWalls is the harness's own walls a run launches with (#2822), read off the launch facts that
// choose them: hold is the run's hold lane, and seedSkips an interactive run whose boot seed skips the
// harness's approvals (seed_auto_tools). ok=false for anything but the two catalog harnesses, whose
// launch Wardyn writes: a custom image's agent, and an exec run, have no walls Wardyn knows.
//
// Neither harness's own sandbox is turned on by Wardyn, and neither can start inside Wardyn's sandbox
// (it needs user namespaces). An interactive Codex session runs under Codex's own default sandbox, which
// Wardyn does not set, so its SandboxMode is empty.
func InnerWalls(harness string, interactive, hold, seedSkips bool) (types.HarnessInnerWalls, bool) {
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
		if interactive && !seedSkips {
			w = types.HarnessInnerWalls{Approvals: types.InnerApprovalsHarness}
		}
	default:
		return types.HarnessInnerWalls{}, false
	}
	return w, true
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
