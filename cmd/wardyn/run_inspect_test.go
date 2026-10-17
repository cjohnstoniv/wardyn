// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// `run get` on a waiting run says where it stands and when its wait ends, and prints nothing of the
// kind for a run that is not waiting.
func TestRunGetCmd_QueuedShowsPositionAndDeadline(t *testing.T) {
	runID, poolID := uuid.New(), uuid.New()
	queuedAt := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		queue *types.RunQueue
		want  []string
	}{
		{"waiting", &types.RunQueue{PoolID: poolID, QueuedAt: queuedAt, DeadlineAt: queuedAt.Add(24 * time.Hour), Position: 2, Reason: "runner_pool_at_capacity"},
			[]string{"QUEUED", "waiting: 2 ahead", poolID.String(), queuedAt.Add(24 * time.Hour).Format(time.RFC3339), "runner_pool_at_capacity"}},
		{"not waiting", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := types.RunRunning
			if tc.queue != nil {
				state = types.RunQueued
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(types.AgentRun{ID: runID, Agent: "claude-code", State: state, Queue: tc.queue, CreatedAt: queuedAt})
			}))
			defer srv.Close()
			out := &strings.Builder{}
			root := rootCmd()
			root.SetArgs([]string{"run", "get", runID.String(), "--url", srv.URL, "--token", "tok"})
			root.SetOut(out)
			root.SetErr(&strings.Builder{})
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("run get output lacks %q:\n%s", w, out)
				}
			}
			if tc.queue == nil && strings.Contains(out.String(), "waiting:") {
				t.Errorf("a run that is not waiting printed a wait line:\n%s", out)
			}
		})
	}
}

// T18: `run get` prints the Azure DevOps decline when the run has a
// run.ado_lane.skip row, and nothing of the kind otherwise.
func TestRunGetCmd_PrintsTheADOLaneDecline(t *testing.T) {
	runID := uuid.New()
	const detail = "This run was not given an Azure DevOps credential: its repositories span more than one Azure DevOps organisation."
	for _, withRow := range []bool{true, false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/audit" {
				var rows []types.AuditEvent
				if withRow && r.URL.Query().Get("action_prefix") == "run.ado_lane.skip" {
					data, _ := json.Marshal(map[string]string{"reason": "cross_repo_conflict", "detail": detail})
					rows = append(rows, types.AuditEvent{RunID: &runID, Action: "run.ado_lane.skip", Outcome: "denied", Data: data})
				}
				_ = json.NewEncoder(w).Encode(rows)
				return
			}
			_ = json.NewEncoder(w).Encode(types.AgentRun{ID: runID, Agent: "claude-code", State: types.RunRunning})
		}))
		out := &strings.Builder{}
		root := rootCmd()
		root.SetArgs([]string{"run", "get", runID.String(), "--url", srv.URL, "--token", "tok"})
		root.SetOut(out)
		root.SetErr(&strings.Builder{})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		srv.Close()
		if got := strings.Contains(out.String(), "azure devops: "+detail); got != withRow {
			t.Errorf("row=%v: output carries the decline = %v:\n%s", withRow, got, out)
		}
	}
}
