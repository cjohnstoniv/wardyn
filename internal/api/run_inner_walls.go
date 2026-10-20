// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errHarnessSandboxUnavailable marks validatePolicySpec's refusal of a harness sandbox mode, so a
// policy write answers it with its own reason (specRefusalReason).
var errHarnessSandboxUnavailable = errors.New("cannot start inside Wardyn's sandbox")

// validateHarnessInnerWalls holds harness_inner_walls to its closed values. The harness's own
// sandbox modes are refused, not stored: Claude Code's and Codex's sandboxes need user namespaces,
// which Wardyn's sandbox does not grant, so a run under either would fail every command or claim a
// wall nothing enforces.
func validateHarnessInnerWalls(w *types.HarnessInnerWalls) error {
	if w == nil {
		return nil
	}
	switch w.SandboxMode {
	case "", types.InnerSandboxOff:
	case types.InnerSandboxWorkspaceWrite, types.InnerSandboxReadOnly:
		return fmt.Errorf("harness_inner_walls.sandbox_mode: %q %w: the harness's own sandbox needs user namespaces, which Wardyn's sandbox does not grant. Use off, or leave it unset", w.SandboxMode, errHarnessSandboxUnavailable)
	default:
		return fmt.Errorf("harness_inner_walls.sandbox_mode: %q is not off, workspace-write or read-only", w.SandboxMode)
	}
	switch w.Approvals {
	case "", types.InnerApprovalsHarness, types.InnerApprovalsWardyn, types.InnerApprovalsNone:
		return nil
	}
	return fmt.Errorf("harness_inner_walls.approvals: %q is not harness, wardyn or none", w.Approvals)
}

// innerWallsSeedSkips reports an interactive run whose boot seed starts the harness skipping its
// approvals (seed_auto_tools), as agent-run's --boot-seed branch does.
func innerWallsSeedSkips(req createRunRequest) bool {
	return requestIsInteractive(req) && req.SeedAutoTools && req.InteractiveStart == "agent" && interactiveBootSeed(true, req.Task) != ""
}

// applyInnerWallsHold puts a background Claude Code run on the hold lane when its policy keeps the
// harness's approvals in Wardyn, as autonomy level L1 does, and says so. Every other shape is left
// for innerWallsFor to answer.
func applyInnerWallsHold(req *createRunRequest, spec types.RunPolicySpec) string {
	w := spec.HarnessInnerWalls
	if w == nil || w.Approvals != types.InnerApprovalsWardyn || requestIsInteractive(*req) || req.TaskMode == "exec" ||
		!agentHasHoldLane(req.Agent) || req.ToolApprovals == "hold" {
		return ""
	}
	req.ToolApprovals = "hold"
	return "tool_approvals was set to hold: the run's policy keeps " + req.Agent + "'s approvals in Wardyn (harness_inner_walls.approvals: wardyn)"
}

// innerWallsFor is the walls the run's started harness launches with, held to the policy's
// harness_inner_walls: every harness the run carries must meet each axis the policy sets. A choice
// looser than what the run launches with is refused, never built; so is one this lane cannot
// express. nil walls is a run with no catalog harness (a custom image, or an exec run).
func innerWallsFor(req createRunRequest, spec types.RunPolicySpec, harnesses []types.RunHarness) (*types.HarnessInnerWalls, *runRefusal) {
	interactive := requestIsInteractive(req)
	hold := holdLane(interactive, req.ToolApprovals, req.TaskMode)
	started, ok := agentpolicy.InnerWalls(req.Agent, interactive, hold, innerWallsSeedSkips(req))
	if req.TaskMode == "exec" {
		ok = false
	}
	want := spec.HarnessInnerWalls
	if want == nil || (want.SandboxMode == "" && want.Approvals == "") {
		if !ok {
			return nil, nil
		}
		return &started, nil
	}
	if !ok {
		return nil, runError(http.StatusUnprocessableEntity, reasonHarnessInnerWallsUnmet,
			"This run's policy sets harness_inner_walls, and this run launches no Claude Code or Codex harness whose walls Wardyn sets. Launch one of them, or use a policy without harness_inner_walls.")
	}
	check := []harnessWalls{{req.Agent, started}}
	for _, h := range harnesses {
		if w, ok := agentpolicy.InnerWalls(h.Harness, interactive, false, false); ok && h.Harness != req.Agent {
			check = append(check, harnessWalls{h.Harness, w})
		}
	}
	for _, c := range check {
		axis, looser := agentpolicy.InnerWallsMiss(want, c.walls)
		if axis == "" {
			continue
		}
		chose, got := want.Approvals, c.walls.Approvals
		if axis == "sandbox_mode" {
			chose, got = want.SandboxMode, c.walls.SandboxMode
		}
		if got == "" {
			got = "the harness's own default"
		}
		if looser {
			return nil, runError(http.StatusUnprocessableEntity, reasonHarnessInnerWallsLooser, fmt.Sprintf(
				"This run's policy sets harness_inner_walls.%s to %s, which is looser than this run's %s (%s): a policy may only add inner walls. Leave harness_inner_walls.%s unset.",
				axis, chose, c.harness, got, axis))
		}
		return nil, runError(http.StatusUnprocessableEntity, reasonHarnessInnerWallsUnmet, fmt.Sprintf(
			"This run's policy sets harness_inner_walls.%s to %s, and this run's %s launches with %s: %s",
			axis, chose, c.harness, got, innerWallsHint(req, axis, chose)))
	}
	return &started, nil
}

// harnessWalls is one harness of a run and the walls it launches with.
type harnessWalls struct {
	harness string
	walls   types.HarnessInnerWalls
}

// innerWallsHint is what to change when a run cannot meet its policy's approvals choice.
func innerWallsHint(req createRunRequest, axis, chose string) string {
	switch {
	case axis == "approvals" && chose == types.InnerApprovalsHarness && requestIsInteractive(req):
		return "launch without seed_auto_tools, so the harness asks before each gated tool."
	case axis == "approvals" && chose == types.InnerApprovalsHarness:
		return "approvals in the harness need a person at its prompt. Launch interactively."
	case axis == "approvals" && chose == types.InnerApprovalsWardyn && requestIsInteractive(req):
		return "an interactive run's approvals happen in the harness. Launch it as a background run."
	case axis == "approvals" && chose == types.InnerApprovalsWardyn:
		return "only Claude Code has a Wardyn approval lane. Launch Claude Code."
	}
	return "use a policy whose harness_inner_walls this run can meet."
}
