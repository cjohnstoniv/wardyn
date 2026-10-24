// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The run shapes whose harness walls #2822's triage maps: each catalog harness, background and
// interactive, with and without the launch switches that change its approvals.
var (
	innerClaudeAuto   = createRunRequest{Agent: "claude-code", Task: "t"}
	innerClaudeHold   = createRunRequest{Agent: "claude-code", Task: "t", ToolApprovals: "hold"}
	innerClaudeExec   = createRunRequest{Agent: "claude-code", Task: "t", TaskMode: "exec"}
	innerClaudeAttach = createRunRequest{Agent: "claude-code", Interactive: true}
	innerClaudeSeeded = createRunRequest{Agent: "claude-code", Interactive: true, Task: "t", InteractiveStart: "agent", SeedAutoTools: true}
	innerCodexAuto    = createRunRequest{Agent: "codex-cli", Task: "t"}
	innerCodexAttach  = createRunRequest{Agent: "codex-cli", Interactive: true}
	innerCodexSeeded  = createRunRequest{Agent: "codex-cli", Interactive: true, Task: "t", InteractiveStart: "agent", SeedAutoTools: true}
	innerCustom       = createRunRequest{Agent: "my-agent", Task: "t"}
)

func innerWalls(sandbox, approvals string) *types.HarnessInnerWalls {
	return &types.HarnessInnerWalls{SandboxMode: sandbox, Approvals: approvals}
}

// TestInnerWalls_AbsentKeepsEveryRunAsToday: with no harness_inner_walls nothing is derived and
// nothing refused, and the walls recorded are the launch's own.
func TestInnerWalls_AbsentKeepsEveryRunAsToday(t *testing.T) {
	off, none, harness, wardyn := types.InnerSandboxOff, types.InnerApprovalsNone, types.InnerApprovalsHarness, types.InnerApprovalsWardyn
	for name, tc := range map[string]struct {
		req  createRunRequest
		want *types.HarnessInnerWalls
	}{
		"claude background":      {innerClaudeAuto, innerWalls(off, none)},
		"claude hold":            {innerClaudeHold, innerWalls(off, wardyn)},
		"claude exec":            {innerClaudeExec, nil},
		"claude interactive":     {innerClaudeAttach, innerWalls(off, harness)},
		"claude seed_auto_tools": {innerClaudeSeeded, innerWalls(off, none)},
		"codex background":       {innerCodexAuto, innerWalls(off, none)},
		"codex interactive":      {innerCodexAttach, innerWalls("", harness)},
		"codex seed_auto_tools":  {innerCodexSeeded, innerWalls(off, none)},
		"a custom image's agent": {innerCustom, nil},
	} {
		for _, spec := range []types.RunPolicySpec{{}, {HarnessInnerWalls: &types.HarnessInnerWalls{}}} {
			req := tc.req
			if warn := applyInnerWallsHold(&req, spec); warn != "" || req.ToolApprovals != tc.req.ToolApprovals {
				t.Errorf("%s: derived %q (%q) with no choice", name, req.ToolApprovals, warn)
			}
			if warn := applyInnerWallsSeed(&req, spec); warn != "" || req.SeedAutoTools != tc.req.SeedAutoTools {
				t.Errorf("%s: derived seed_auto_tools %v (%q) with no choice", name, req.SeedAutoTools, warn)
			}
			got, refusal := innerWallsFor(req, spec, nil, types.CC1)
			if refusal != nil {
				t.Errorf("%s: refused with no choice: %+v", name, refusal.body)
			}
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Errorf("%s: walls %+v, want %+v", name, got, tc.want)
			}
		}
	}
}

// TestInnerWalls_EveryPairMeetsOrIsRefused is the triage table as code: each (sandbox, approvals)
// choice a policy may store, against each run shape. A choice the run already launches with stands;
// approvals in Wardyn put a background Claude Code run on the hold lane; a looser choice is refused
// as looser and never built; a narrower one this lane cannot express is refused as unmet.
func TestInnerWalls_EveryPairMeetsOrIsRefused(t *testing.T) {
	const ok, looser, unmet = "", reasonHarnessInnerWallsLooser, reasonHarnessInnerWallsUnmet
	off, none, harness, wardyn := types.InnerSandboxOff, types.InnerApprovalsNone, types.InnerApprovalsHarness, types.InnerApprovalsWardyn
	for _, tc := range []struct {
		name   string
		req    createRunRequest
		want   *types.HarnessInnerWalls
		reason string
		hold   bool // the choice put the run on the hold lane
	}{
		{"off/none on a claude background run", innerClaudeAuto, innerWalls(off, none), ok, false},
		{"off/wardyn on a claude background run", innerClaudeAuto, innerWalls(off, wardyn), ok, true},
		{"wardyn on a claude hold run", innerClaudeHold, innerWalls("", wardyn), ok, false},
		{"none on a claude hold run", innerClaudeHold, innerWalls("", none), looser, false},
		{"harness on a claude background run", innerClaudeAuto, innerWalls("", harness), unmet, false},
		{"anything on a claude exec run", innerClaudeExec, innerWalls(off, ""), unmet, false},
		{"off/harness on a claude interactive run", innerClaudeAttach, innerWalls(off, harness), ok, false},
		{"none on a claude interactive run", innerClaudeAttach, innerWalls("", none), looser, false},
		{"wardyn on a claude interactive run", innerClaudeAttach, innerWalls("", wardyn), unmet, false},
		{"read-only on a claude background run", innerClaudeAuto, innerWalls(types.InnerSandboxReadOnly, ""), unmet, false},
		{"read-only on a codex background run at CC1", innerCodexAuto, innerWalls(types.InnerSandboxReadOnly, ""), reasonHarnessSandboxUnavailable, false},
		{"harness on a seed_auto_tools run", innerClaudeSeeded, innerWalls("", harness), ok, false},
		{"harness on a codex seed_auto_tools run", innerCodexSeeded, innerWalls("", harness), ok, false},
		{"none on a seed_auto_tools run", innerClaudeSeeded, innerWalls(off, none), ok, false},
		{"off/none on a codex background run", innerCodexAuto, innerWalls(off, none), ok, false},
		{"wardyn on a codex background run", innerCodexAuto, innerWalls("", wardyn), unmet, false},
		{"harness on a codex background run", innerCodexAuto, innerWalls("", harness), unmet, false},
		{"harness on a codex interactive run", innerCodexAttach, innerWalls("", harness), ok, false},
		{"off on a codex interactive run", innerCodexAttach, innerWalls(off, ""), looser, false},
		{"none on a codex interactive run", innerCodexAttach, innerWalls("", none), looser, false},
		{"anything on a custom image's agent", innerCustom, innerWalls("", none), unmet, false},
	} {
		req, spec := tc.req, types.RunPolicySpec{HarnessInnerWalls: tc.want}
		applyInnerWallsHold(&req, spec)
		applyInnerWallsSeed(&req, spec)
		if got := req.ToolApprovals == "hold" && tc.req.ToolApprovals != "hold"; got != tc.hold {
			t.Errorf("%s: derived hold %v, want %v", tc.name, got, tc.hold)
		}
		walls, refusal := innerWallsFor(req, spec, nil, types.CC1)
		switch {
		case tc.reason == ok && refusal != nil:
			t.Errorf("%s: refused %+v", tc.name, refusal.body)
		case tc.reason != ok && (refusal == nil || refusal.body.Reason != tc.reason):
			t.Errorf("%s: refusal %+v, want %s", tc.name, refusal, tc.reason)
		case tc.reason == ok && walls == nil:
			t.Errorf("%s: no walls recorded", tc.name)
		case tc.reason == ok && tc.want.Approvals != "" && walls.Approvals != tc.want.Approvals:
			t.Errorf("%s: walls %+v, want approvals %s", tc.name, walls, tc.want.Approvals)
		}
	}
}

// TestInnerWalls_ExtraHarnessMustMeetTheChoiceToo: an interactive run carrying Codex beside Claude
// Code is refused a sandbox choice Codex's own default does not meet.
func TestInnerWalls_ExtraHarnessMustMeetTheChoiceToo(t *testing.T) {
	spec := types.RunPolicySpec{HarnessInnerWalls: innerWalls(types.InnerSandboxOff, "")}
	set := []types.RunHarness{{Harness: "claude-code", Starts: true}, {Harness: "codex-cli"}}
	if _, refusal := innerWallsFor(innerClaudeAttach, spec, set, types.CC1); refusal == nil || refusal.body.Reason != reasonHarnessInnerWallsLooser {
		t.Fatalf("refusal %+v, want %s for Codex's own sandbox", refusal, reasonHarnessInnerWallsLooser)
	}
	if _, refusal := innerWallsFor(innerClaudeAttach, spec, set[:1], types.CC1); refusal != nil {
		t.Fatalf("Claude Code alone refused: %+v", refusal.body)
	}
}

// TestInnerWalls_WardynApprovalsLaunchUnderTheGatedDocument: the hold a policy's approvals=wardyn
// derives is the hold lane dispatch already serves, so the run gets the frozen L1 document (the
// testdata golden the pinned CLI was exercised with) and the hold env, at every level that allows a
// background run.
func TestInnerWalls_WardynApprovalsLaunchUnderTheGatedDocument(t *testing.T) {
	golden, err := os.ReadFile("../agentpolicy/testdata/L1-managed-settings.json")
	if err != nil {
		t.Fatal(err)
	}
	req := innerClaudeAuto
	applyInnerWallsHold(&req, types.RunPolicySpec{HarnessInnerWalls: innerWalls("", types.InnerApprovalsWardyn)})
	for _, level := range []types.AutonomyLevel{"", types.AutonomyL2, types.AutonomyL3} {
		spec, _, _ := agentPolicyDispatchParams(t, &fakeRunner{}, req.Agent, level, "", dispatchParams{ToolApprovals: req.ToolApprovals})
		if spec.Env["WARDYN_TOOL_APPROVALS"] != "hold" || len(spec.ManagedFiles) != 1 || !bytes.Equal(spec.ManagedFiles[0].Content, golden) {
			t.Errorf("level %q: env %q files %+v, want the hold lane under the L1 document", level, spec.Env["WARDYN_TOOL_APPROVALS"], spec.ManagedFiles)
		}
	}
}

// TestInnerWalls_PolicyValues: the closed values store, the harness's own sandbox modes included
// (the create door decides whether a run can start one), and anything else is a plain invalid policy.
func TestInnerWalls_PolicyValues(t *testing.T) {
	spec := func(w *types.HarnessInnerWalls) types.RunPolicySpec {
		return types.RunPolicySpec{MinConfinementClass: types.CC2, HarnessInnerWalls: w}
	}
	for _, w := range []*types.HarnessInnerWalls{nil, {}, innerWalls(types.InnerSandboxOff, types.InnerApprovalsHarness),
		innerWalls("", types.InnerApprovalsWardyn), innerWalls("", types.InnerApprovalsNone),
		innerWalls(types.InnerSandboxWorkspaceWrite, ""), innerWalls(types.InnerSandboxReadOnly, types.InnerApprovalsNone)} {
		if err := validatePolicySpec(spec(w)); err != nil {
			t.Errorf("%+v refused: %v", w, err)
		}
	}
	for _, w := range []*types.HarnessInnerWalls{innerWalls("on", ""), innerWalls("", "always")} {
		if err := validatePolicySpec(spec(w)); err == nil || specRefusalReason(err, "bucket") != "bucket" {
			t.Errorf("%+v: %v, want a plain invalid policy", w, err)
		}
	}
}

// TestInnerWalls_HarnessApprovalsTurnASeedsAutoToolsOff: approvals in the harness on a seeded
// interactive run turn seed_auto_tools off and say so; the run then launches asking at its prompt.
func TestInnerWalls_HarnessApprovalsTurnASeedsAutoToolsOff(t *testing.T) {
	spec := types.RunPolicySpec{HarnessInnerWalls: innerWalls("", types.InnerApprovalsHarness)}
	for _, seeded := range []createRunRequest{innerClaudeSeeded, innerCodexSeeded} {
		req := seeded
		warn := applyInnerWallsSeed(&req, spec)
		if req.SeedAutoTools || !strings.Contains(warn, "seed_auto_tools was turned off") {
			t.Errorf("%s: seed_auto_tools %v, warn %q, want it turned off and said", seeded.Agent, req.SeedAutoTools, warn)
		}
		if walls, refusal := innerWallsFor(req, spec, nil, types.CC1); refusal != nil || walls.Approvals != types.InnerApprovalsHarness {
			t.Errorf("%s: walls %+v refusal %+v, want approvals in the harness", seeded.Agent, walls, refusal)
		}
	}
	for _, other := range []createRunRequest{innerClaudeAttach, innerClaudeAuto} {
		req := other
		if warn := applyInnerWallsSeed(&req, spec); warn != "" || req.SeedAutoTools != other.SeedAutoTools {
			t.Errorf("%+v: derived %q on a run with no seed that skips approvals", other, warn)
		}
	}
}

// TestInnerWalls_CodexSandboxStartsOnlyWhereMeasured: a Codex sandbox choice launches bound to the
// mode where the class starts it, on a background run or an interactive one at CC3, and is refused
// everywhere else: harness_sandbox_unavailable for a Codex run at a class that cannot start it,
// unmet for a harness or launch Wardyn does not bind.
func TestInnerWalls_CodexSandboxStartsOnlyWhereMeasured(t *testing.T) {
	ro, ww := types.InnerSandboxReadOnly, types.InnerSandboxWorkspaceWrite
	none, harness := types.InnerApprovalsNone, types.InnerApprovalsHarness
	unavailable, unmet := reasonHarnessSandboxUnavailable, reasonHarnessInnerWallsUnmet
	codexExec := createRunRequest{Agent: "codex-cli", Task: "t", TaskMode: "exec"}
	for _, tc := range []struct {
		name   string
		req    createRunRequest
		mode   string
		class  types.ConfinementClass
		set    []types.RunHarness
		want   *types.HarnessInnerWalls
		reason string
	}{
		{"read-only, background, CC3", innerCodexAuto, ro, types.CC3, nil, innerWalls(ro, none), ""},
		{"workspace-write, background, CC3", innerCodexAuto, ww, types.CC3, nil, innerWalls(ww, none), ""},
		{"workspace-write, background, CC2", innerCodexAuto, ww, types.CC2, nil, innerWalls(ww, none), ""},
		{"read-only, interactive, CC3", innerCodexAttach, ro, types.CC3, nil, innerWalls(ro, harness), ""},
		{"read-only, background, CC2", innerCodexAuto, ro, types.CC2, nil, nil, unavailable},
		{"workspace-write, interactive, CC2", innerCodexAttach, ww, types.CC2, nil, nil, unavailable},
		{"read-only, background, CC1", innerCodexAuto, ro, types.CC1, nil, nil, unavailable},
		{"workspace-write, background, CC1", innerCodexAuto, ww, types.CC1, nil, nil, unavailable},
		{"read-only, seed_auto_tools, CC3", innerCodexSeeded, ro, types.CC3, nil, nil, unmet},
		{"read-only, exec run, CC3", codexExec, ro, types.CC3, nil, nil, unmet},
		{"read-only, Claude Code, CC3", innerClaudeAuto, ro, types.CC3, nil, nil, unmet},
		{"read-only, Codex beside Claude Code, CC3", innerCodexAuto, ro, types.CC3,
			[]types.RunHarness{{Harness: "codex-cli", Starts: true}, {Harness: "claude-code", Task: "t"}}, nil, unmet},
	} {
		spec := types.RunPolicySpec{HarnessInnerWalls: innerWalls(tc.mode, "")}
		walls, refusal := innerWallsFor(tc.req, spec, tc.set, tc.class)
		switch {
		case tc.reason == "" && (refusal != nil || walls == nil || *walls != *tc.want):
			t.Errorf("%s: walls %+v refusal %+v, want %+v", tc.name, walls, refusal, tc.want)
		case tc.reason != "" && (refusal == nil || refusal.body.Reason != tc.reason):
			t.Errorf("%s: walls %+v refusal %+v, want %s", tc.name, walls, refusal, tc.reason)
		}
	}
}
