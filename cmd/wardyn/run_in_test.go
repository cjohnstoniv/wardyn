// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRunCmd_SendsRunIn: --run-in sends the unit as run_in; without it the body names none.
func TestRunCmd_SendsRunIn(t *testing.T) {
	srv := newCmdServer(t, http.StatusCreated, types.AgentRun{ID: uuid.New(), State: types.RunPending})
	base := []string{"run", "--url", srv.URL, "--token", "tok", "--agent", "claude-code", "--task", "t"}
	for _, tc := range []struct {
		args []string
		want string
	}{{[]string{"--run-in", "platform"}, "platform"}, {nil, ""}} {
		if err := execCmd(t, append(base, tc.args...)...); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		var body map[string]any
		if err := json.Unmarshal(srv.last().body, &body); err != nil {
			t.Fatal(err)
		}
		if got, _ := body["run_in"].(string); got != tc.want {
			t.Errorf("%v: run_in = %q, want %q", tc.args, got, tc.want)
		}
	}
}
