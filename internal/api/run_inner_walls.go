// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// validateHarnessInnerWalls holds harness_inner_walls to its closed values. Whether a run can
// launch a harness sandbox mode is the create door's question (innerWallsFor): it depends on the
// harness, the lane and the confinement class.
func validateHarnessInnerWalls(w *types.HarnessInnerWalls) error {
	if w == nil {
		return nil
	}
	switch w.SandboxMode {
	case "", types.InnerSandboxOff, types.InnerSandboxWorkspaceWrite, types.InnerSandboxReadOnly:
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
		!slices.ContainsFunc(taskAgents(*req), agentHasHoldLane) || req.ToolApprovals == "hold" {
		return ""
	}
	req.ToolApprovals = "hold"
	// Name the agents the policy holds: on a run with several, the one that took the lane
	// may not be the run's own agent.
	var held []string
	for _, a := range taskAgents(*req) {
		if agentHasHoldLane(a) {
			held = append(held, a+"'s")
		}
	}
	return "tool_approvals was set to hold: the run's policy keeps " + strings.Join(held, " and ") + " approvals in Wardyn (harness_inner_walls.approvals: wardyn)"
}

// applyInnerWallsSeed keeps an interactive run's approvals in the harness when its policy says so:
// a seed that would skip them (seed_auto_tools) is turned off, and said.
func applyInnerWallsSeed(req *createRunRequest, spec types.RunPolicySpec) string {
	w := spec.HarnessInnerWalls
	if w == nil || w.Approvals != types.InnerApprovalsHarness || !innerWallsSeedSkips(*req) {
		return ""
	}
	req.SeedAutoTools = false
	return "seed_auto_tools was turned off: the run's policy keeps " + req.Agent + "'s approvals in the harness (harness_inner_walls.approvals: harness)"
}

// innerWallsSandbox is the Codex sandbox mode a run's launch is bound to (agentpolicy.CodexRequirements):
// the policy's choice, on a Codex run that is not an exec run and whose class starts that mode.
// "" otherwise, which leaves innerWallsFor to refuse the choice.
func innerWallsSandbox(req createRunRequest, spec types.RunPolicySpec, class types.ConfinementClass) string {
	w := spec.HarnessInnerWalls
	if w == nil || req.Agent != codexHarness || req.TaskMode == "exec" ||
		!agentpolicy.CodexSandboxStarts(w.SandboxMode, class, requestIsInteractive(req)) {
		return ""
	}
	return w.SandboxMode
}

// innerWallsFor is the walls the run's started harness launches with, held to the policy's
// harness_inner_walls: every harness the run carries must meet each axis the policy sets. A choice
// looser than what the run launches with is refused, never built; so is one this lane cannot
// express. nil walls is a run with no catalog harness (a custom image, or an exec run).
func innerWallsFor(req createRunRequest, spec types.RunPolicySpec, harnesses []types.RunHarness, class types.ConfinementClass) (*types.HarnessInnerWalls, *runRefusal) {
	interactive := requestIsInteractive(req)
	hold := holdLane(interactive, req.ToolApprovals, req.TaskMode)
	started, ok := agentpolicy.InnerWalls(req.Agent, interactive, hold, innerWallsSeedSkips(req), innerWallsSandbox(req, spec, class))
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
		// An agent running its own task launches on the run's lane, as the run's agent does.
		if w, ok := agentpolicy.InnerWalls(h.Harness, interactive, hold && h.Task != "", false, ""); ok && h.Harness != req.Agent {
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
		if axis == "sandbox_mode" && c.harness == codexHarness && c.harness == req.Agent && req.TaskMode != "exec" && !innerWallsSeedSkips(req) {
			return nil, runError(http.StatusUnprocessableEntity, reasonHarnessSandboxUnavailable, fmt.Sprintf(
				"This run's policy sets harness_inner_walls.sandbox_mode to %s, and Codex's own sandbox cannot start at %s: it needs user namespaces, which CC1 refuses; CC2 starts workspace-write on a background run, and CC3 starts both modes. Launch at a class that starts it.",
				chose, cmp.Or(string(class), "this run's confinement class")))
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
	case axis == "sandbox_mode" && chose != types.InnerSandboxOff && innerWallsSeedSkips(req):
		return "seed_auto_tools starts the harness without its sandbox. Launch without seed_auto_tools."
	case axis == "sandbox_mode" && chose != types.InnerSandboxOff:
		return "Wardyn binds only Codex to its own sandbox; Claude Code's image ships none. Launch Codex alone, not as an exec run."
	}
	return "use a policy whose harness_inner_walls this run can meet."
}

// codexSandbox sets the sandbox mode a Codex run's walls froze at create (innerWallsFor) on env, and
// returns the managed requirements that bind every codex in the sandbox to it (agentpolicy.CodexRequirements):
// agent-run launches codex exec in that mode, and an image whose agent-run does not know it still
// cannot launch without a sandbox, since the requirements refuse the bypass flag. A runner that
// cannot deliver the file where the agent cannot rewrite it refuses the run (agentPolicyRefusal):
// a sandbox the run shows and nothing binds is a wall that is not there.
func (s *Server) codexSandbox(ctx context.Context, run types.AgentRun, env map[string]string) ([]runner.ManagedFile, error) {
	if run.InnerWalls == nil || run.Agent != codexHarness {
		return nil, nil
	}
	mode := run.InnerWalls.SandboxMode
	path, content, ok := agentpolicy.CodexRequirements(mode)
	if !ok {
		return nil, nil
	}
	refused := "This run was not launched: its policy binds Codex to its own sandbox (" + mode + ") through managed requirements, and "
	caps, err := s.runCapabilities(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agentPolicyRefusal(refused+"its runner's capabilities could not be confirmed"), err)
	}
	if reason, deliverable := managedFilesGap(caps, run.ConfinementClass); !deliverable || reason != "" {
		return nil, agentPolicyRefusal(refused + "this runner cannot vouch for them: " + reason)
	}
	env["WARDYN_HARNESS_SANDBOX"] = mode
	return []runner.ManagedFile{{Path: path, Content: content}}, nil
}
