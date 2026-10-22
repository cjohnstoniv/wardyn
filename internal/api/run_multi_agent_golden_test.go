// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestGolden_SingleAgentRunModeUnchanged pins today's run-mode requests, the ones that
// carry no tools[].task, byte-for-byte: what the door refuses or projects, the harness
// set it records, the per-harness rules it compiles and the lane it settles. The golden
// was written from the tree before several agents in one background run existed, so any
// drift here is a change to a single-agent run.
func TestGolden_SingleAgentRunModeUnchanged(t *testing.T) {
	const (
		claude    = `{"id":"claude-code","kind":"harness"}`
		codex     = `{"id":"codex-cli","kind":"harness"}`
		claudeRul = `{"id":"claude-code","kind":"harness","model_provider":"anthropic-team","tool_rules":[{"tool":"Read","effect":"allow"},{"tool":"Bash","effect":"deny"}],"default_effect":"hold"}`
		codexRul  = `{"id":"codex-cli","kind":"harness","tool_rules":[{"tool":"mcp__gh__create_issue","effect":"deny"}]}`
		task      = `"workload":{"kind":"agent_task","agent":"claude-code","task":"fix the build"}`
		codexTask = `"workload":{"kind":"agent_task","agent":"codex-cli","task":"write the tests"}`
		bg        = `"experience":"background","no_repositories_or_drives":true,`
	)
	corpus := []struct{ name, body string }{
		{"background agent task, no tools", `{` + bg + task + `}`},
		{"background agent task, its own tool", `{` + bg + task + `,"tools":[` + claude + `]}`},
		{"background agent task, its tool with rules", `{` + bg + task + `,"tools":[` + claudeRul + `]}`},
		{"background agent task, a second harness", `{` + bg + task + `,"tools":[` + claude + `,` + codex + `]}`},
		{"background agent task, only another harness", `{` + bg + task + `,"tools":[` + codex + `]}`},
		{"background codex task with rules", `{` + bg + codexTask + `,"tools":[` + codexRul + `]}`},
		{"background command, two harnesses", `{` + bg + `"workload":{"kind":"command","command":"make test"},"tools":[` + claude + `,` + codex + `]}`},
		{"interactive, two harnesses", `{"experience":"interactive","no_repositories_or_drives":true,"tools":[` + claude + `,` + codexRul + `],"startup":{"kind":"harness","tool":"claude-code"}}`},
		{"interactive, claude's own rules beside codex", `{"experience":"interactive","no_repositories_or_drives":true,"tools":[` + claudeRul + `,` + codex + `],"startup":{"kind":"harness","tool":"claude-code"}}`},
		{"interactive, two harnesses, codex starts", `{"experience":"interactive","no_repositories_or_drives":true,"tools":[` + claude + `,` + codex + `],"startup":{"kind":"harness","tool":"codex-cli","prompt":"hi"}}`},
	}
	policy := types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "Write", Effect: types.ToolHold}}}
	type refusalView struct {
		Status int    `json:"status"`
		Reason string `json:"reason"`
		Error  string `json:"error"`
	}
	type outcome struct {
		Name          string                   `json:"name"`
		Refusal       *refusalView             `json:"refusal,omitempty"`
		Request       json.RawMessage          `json:"request,omitempty"`
		Harnesses     []types.RunHarness       `json:"harnesses,omitempty"`
		ToolRules     []types.ToolRule         `json:"tool_rules,omitempty"`
		HarnessRules  []types.HarnessToolRules `json:"harness_tool_rules,omitempty"`
		ToolApprovals string                   `json:"tool_approvals,omitempty"`
	}
	var got []outcome
	for _, c := range corpus {
		var req createRunRequest
		if err := json.Unmarshal([]byte(c.body), &req); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		o := outcome{Name: c.name}
		if f := applyRunMode(&req); f != nil {
			o.Refusal = &refusalView{f.status, f.body.Reason, f.body.Error}
			got = append(got, o)
			continue
		}
		o.Request, _ = json.Marshal(req)
		o.Harnesses = harnessSet(req)
		spec, sets := applyHarnessToolRules(policy, req.Tools, toolLaneDefault(req))
		(&Server{}).applyToolPosture(&req, &spec)
		o.ToolRules, o.HarnessRules, o.ToolApprovals = spec.ToolRules, sets, req.ToolApprovals
		got = append(got, o)
	}
	compareOrUpdateGolden(t, filepath.Join("testdata", "run_mode_single_agent_golden.json"), got)
}
