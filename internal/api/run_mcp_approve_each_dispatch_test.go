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
// it; a set without one leaves the key out, so an older proxy still starts every other run. A run
// with a correlation ID also sets the marker's send_once.
func TestDispatchCarriesMCPApproveEach(t *testing.T) {
	each := []types.HarnessToolRules{
		{Harness: "claude-code", Rules: []types.ToolRule{{Tool: "Read", Effect: types.ToolAllow}}},
		{Harness: "codex-cli", Rules: []types.ToolRule{{Tool: "mcp__linear__delete_issue", Effect: types.ToolApproveEach}}},
	}
	for _, tc := range []struct {
		name string
		sets []types.HarnessToolRules
		corr string // the run's correlation ID
		want bool
		once bool // the marker's send_once
	}{{"approve_each", each, "", true, false}, {"approve_each with a correlation ID", each, "job-7/step-2", true, true}, {"none", twoHarnessSets, "job-7/step-2", false, false}} {
		rn := &fakeRunner{}
		srv, _, _, run := dispatchTeardownFixture(t, rn, types.RunPending)
		run.Task, run.CorrelationID = "", tc.corr
		srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
			RunToken: "run-token", Image: "img", Interactive: true, HarnessToolRules: tc.sets,
			Harnesses: []types.RunHarness{{Harness: "claude-code", Starts: true}, {Harness: laneSecondHarness}},
		})
		if rn.createCalls != 1 {
			t.Fatalf("%s: the sandbox was not created", tc.name)
		}
		mae := rn.lastSpec.ProxyConfig.MCPApproveEach
		if got := mae != nil; got != tc.want {
			t.Errorf("%s: proxy config carries mcp_approve_each = %v, want %v", tc.name, got, tc.want)
		}
		if got := mae != nil && mae.SendOnce; got != tc.once {
			t.Errorf("%s: mcp_approve_each.send_once = %v, want %v", tc.name, got, tc.once)
		}
	}
}
