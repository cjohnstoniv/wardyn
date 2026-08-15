// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestIsModelRun_ExcludesExecTaskMode is the W5-S1-5 regression: a
// task_mode=exec run (the BYOA/CI plain-command lane) execs a bare shell
// command and never invokes the agent CLI, so it must NOT be treated as a
// model run — same as an existing non-interactive scan run. (The signature
// once also took a verifyPlan []byte for a "workspace verify" case — that
// pipeline is retired, superseded by "workspace record" + confined=true; see
// runs_create.go's reservedRunTasks doc comment. isModelRun's real
// discriminators are task_mode and workspace/source-linked-and-non-interactive.)
func TestIsModelRun_ExcludesExecTaskMode(t *testing.T) {
	cases := []struct {
		name        string
		workspaceID *uuid.UUID
		sourceID    *uuid.UUID
		interactive bool
		taskMode    string
		want        bool
	}{
		{"plain agent run", nil, nil, false, "", true},
		{"interactive agent run", nil, nil, true, "", true},
		{"task_mode=exec direct run", nil, nil, false, "exec", false},
		{"task_mode=exec interactive", nil, nil, true, "exec", false},
		{"scan run (workspace, non-interactive)", ptr(uuid.New()), nil, false, "", false},
		{"scan run (source, non-interactive)", nil, ptr(uuid.New()), false, "", false},
		{"interactive workspace run (record) IS a model run", ptr(uuid.New()), nil, true, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isModelRun(c.taskMode, c.workspaceID, c.sourceID, c.interactive)
			if got != c.want {
				t.Errorf("isModelRun(taskMode=%q, workspaceID=%v, sourceID=%v, interactive=%v) = %v, want %v",
					c.taskMode, c.workspaceID, c.sourceID, c.interactive, got, c.want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// TestBuildRunMounts_DropsResidentClaudeCredsOnNonModelRun is the other half
// of the W5-S1-5 regression: even when the POLICY declares a workspace_mount
// onto claudeCredTarget/claudeCredJSONTarget (the normal way an operator
// stages a host ~/.claude subscription), buildRunMounts must drop it for a
// non-model-run dispatch — the resident host OAuth session has no business in
// a sandbox that never invokes the model (e.g. a member's task_mode=exec run
// under a policy an admin authored for agentic use).
func TestBuildRunMounts_DropsResidentClaudeCredsOnNonModelRun(t *testing.T) {
	policy := types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{
		{Source: "/host/.claude", Target: claudeCredTarget},
		{Source: "/host/.claude.json", Target: claudeCredJSONTarget},
		{Source: "/host/repo", Target: "/work/repo"},
	}}

	// Non-model run (e.g. task_mode=exec, or a verify/scan run): the two
	// resident-credential mounts must be dropped; the unrelated repo mount stays.
	got := buildRunMounts(policy, llmTransport{modelRun: false})
	if len(got) != 1 || got[0].Target != "/work/repo" {
		t.Errorf("non-model run mounts = %+v, want only the /work/repo mount (claudeCredTarget/claudeCredJSONTarget dropped)", got)
	}

	// Model run: all three mounts pass through unchanged.
	got = buildRunMounts(policy, llmTransport{modelRun: true})
	if len(got) != 3 {
		t.Errorf("model run mounts = %+v, want all 3 policy mounts to pass through", got)
	}
}
