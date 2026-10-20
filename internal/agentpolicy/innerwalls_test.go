// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package agentpolicy

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestInnerWallsAreWhatTheLaunchGives pins each harness's own walls per lane to what agent-run
// launches today: claude -p --dangerously-skip-permissions (none) or the toolgate hold lane
// (wardyn), codex exec --dangerously-bypass-approvals-and-sandbox (off, none), an interactive session
// asking at its prompt (harness) unless seed_auto_tools skips it, and Codex's own default sandbox in
// an interactive session ("").
func TestInnerWallsAreWhatTheLaunchGives(t *testing.T) {
	off, none, harness, wardyn := types.InnerSandboxOff, types.InnerApprovalsNone, types.InnerApprovalsHarness, types.InnerApprovalsWardyn
	for _, tc := range []struct {
		harness                      string
		interactive, hold, seedSkips bool
		want                         types.HarnessInnerWalls
	}{
		{"claude-code", false, false, false, types.HarnessInnerWalls{SandboxMode: off, Approvals: none}},
		{"claude-code", false, true, false, types.HarnessInnerWalls{SandboxMode: off, Approvals: wardyn}},
		{"claude-code", true, false, false, types.HarnessInnerWalls{SandboxMode: off, Approvals: harness}},
		{"claude-code", true, true, false, types.HarnessInnerWalls{SandboxMode: off, Approvals: harness}},
		{"claude-code", true, false, true, types.HarnessInnerWalls{SandboxMode: off, Approvals: none}},
		{"codex-cli", false, false, false, types.HarnessInnerWalls{SandboxMode: off, Approvals: none}},
		{"codex-cli", true, false, false, types.HarnessInnerWalls{Approvals: harness}},
		{"codex-cli", true, false, true, types.HarnessInnerWalls{SandboxMode: off, Approvals: none}},
	} {
		got, ok := InnerWalls(tc.harness, tc.interactive, tc.hold, tc.seedSkips)
		if !ok || got != tc.want {
			t.Errorf("InnerWalls(%s, interactive=%v, hold=%v, seed=%v) = %+v %v, want %+v", tc.harness, tc.interactive, tc.hold, tc.seedSkips, got, ok, tc.want)
		}
	}
	for _, agent := range []string{"", "my-agent", "aider"} {
		if got, ok := InnerWalls(agent, false, false, false); ok {
			t.Errorf("InnerWalls(%q) = %+v, want none: Wardyn does not write that harness's launch", agent, got)
		}
	}
}

// TestInnerWallsMissNamesTheAxisAndWhetherItLoosens: a choice the walls meet is "", a looser one
// (no approvals where the run asks, no sandbox where the harness keeps its own) says so, and a
// narrower one the lane cannot express is a plain miss.
func TestInnerWallsMissNamesTheAxisAndWhetherItLoosens(t *testing.T) {
	asks := types.HarnessInnerWalls{SandboxMode: types.InnerSandboxOff, Approvals: types.InnerApprovalsHarness}
	codexTUI := types.HarnessInnerWalls{Approvals: types.InnerApprovalsHarness}
	for _, tc := range []struct {
		want   *types.HarnessInnerWalls
		got    types.HarnessInnerWalls
		axis   string
		looser bool
	}{
		{nil, asks, "", false},
		{&types.HarnessInnerWalls{}, asks, "", false},
		{&types.HarnessInnerWalls{SandboxMode: types.InnerSandboxOff, Approvals: types.InnerApprovalsHarness}, asks, "", false},
		{&types.HarnessInnerWalls{Approvals: types.InnerApprovalsNone}, asks, "approvals", true},
		{&types.HarnessInnerWalls{Approvals: types.InnerApprovalsWardyn}, asks, "approvals", false},
		{&types.HarnessInnerWalls{SandboxMode: types.InnerSandboxOff}, codexTUI, "sandbox_mode", true},
		{&types.HarnessInnerWalls{Approvals: types.InnerApprovalsHarness}, codexTUI, "", false},
	} {
		axis, looser := InnerWallsMiss(tc.want, tc.got)
		if axis != tc.axis || looser != tc.looser {
			t.Errorf("InnerWallsMiss(%+v, %+v) = %q %v, want %q %v", tc.want, tc.got, axis, looser, tc.axis, tc.looser)
		}
	}
}
