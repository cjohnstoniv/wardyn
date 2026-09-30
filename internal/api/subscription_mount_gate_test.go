// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/google/uuid"
)

// TestIsModelRun_ExcludesExecTaskMode: a task_mode=exec run (the BYOA/CI
// plain-command lane) execs a bare shell command and never invokes the agent
// CLI, so it must not be treated as a model run — same as an existing
// non-interactive scan run. isModelRun's discriminators are task_mode and
// workspace/source-linked-and-non-interactive; verification is "workspace
// record" + confined=true (see runs_create.go's reservedRunTasks doc comment).
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
