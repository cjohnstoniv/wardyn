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
			[]string{"QUEUED", "waiting: 2 ahead", poolID.String(), "2026-10-13T09:00:00Z", "runner_pool_at_capacity"}},
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
