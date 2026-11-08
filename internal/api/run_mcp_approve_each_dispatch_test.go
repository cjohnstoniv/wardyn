// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDispatchCarriesMCPApproveEach: a dispatched rule set with an approve_each rule puts the
// marker in the proxy config, so an older proxy refuses the config and the run does not start on
// it; a set without one leaves the key out, so an older proxy still starts every other run.
func TestDispatchCarriesMCPApproveEach(t *testing.T) {
	each := []types.HarnessToolRules{
		{Harness: "claude-code", Rules: []types.ToolRule{{Tool: "Read", Effect: types.ToolAllow}}},
		{Harness: "codex-cli", Rules: []types.ToolRule{{Tool: "mcp__linear__delete_issue", Effect: types.ToolApproveEach}}},
	}
	for _, tc := range []struct {
		name string
		sets []types.HarnessToolRules
		want bool
	}{{"approve_each", each, true}, {"none", twoHarnessSets, false}} {
		rn := &fakeRunner{}
		srv, _, _, run := dispatchTeardownFixture(t, rn, types.RunPending)
		run.Task = ""
		srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
			RunToken: "run-token", Image: "img", Interactive: true, HarnessToolRules: tc.sets,
			Harnesses: []types.RunHarness{{Harness: "claude-code", Starts: true}, {Harness: laneSecondHarness}},
		})
		if rn.createCalls != 1 {
			t.Fatalf("%s: the sandbox was not created", tc.name)
		}
		if got := rn.lastSpec.ProxyConfig.MCPApproveEach != nil; got != tc.want {
			t.Errorf("%s: proxy config carries mcp_approve_each = %v, want %v", tc.name, got, tc.want)
		}
	}
}
