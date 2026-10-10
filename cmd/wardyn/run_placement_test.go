// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestRunPlacementFlagsAreSentUnchanged(t *testing.T) {
	runnerID := uuid.New()
	for _, args := range [][]string{
		{"run", "--agent", "claude-code", "--placement", "local", "--runner", runnerID.String()},
		{"run", "--agent", "claude-code", "--placement", "local", "--runner", runnerID.String(), "--dry-run"},
	} {
		srv := newCmdServer(t, http.StatusUnprocessableEntity, map[string]string{"error": "x", "reason": "invalid_request"})
		if err := execCmd(t, append(args, "--url", srv.URL, "--token", "t")...); err == nil {
			t.Fatalf("%v: the server's refusal was swallowed", args)
		}
		var body sdk.CreateRunRequest
		if err := json.Unmarshal(srv.last().body, &body); err != nil || body.Placement != "local" || body.RunnerID != runnerID.String() {
			t.Fatalf("%v: body = %s, want placement=local runner_id=%s", args, srv.last().body, runnerID)
		}
		if strings.Contains(args[0], "dry-run") {
			got := srv.last()
			if got.path != "/api/v1/runs/preflight" {
				t.Errorf("%v: path = %s, want /api/v1/runs/preflight", args, got.path)
			}
		}
	}

	// Invalid values: CLI does not validate, passes them through; server refuses.
	srv := newCmdServer(t, http.StatusBadRequest, map[string]string{"error": "x", "reason": "invalid_request"})
	if err := execCmd(t, "run", "--agent", "claude-code", "--placement", "nowhere", "--runner", "not-a-uuid", "--url", srv.URL, "--token", "t"); err == nil {
		t.Fatal("invalid placement/runner should return an error from the server")
	}
	var body sdk.CreateRunRequest
	if err := json.Unmarshal(srv.last().body, &body); err != nil || body.Placement != "nowhere" || body.RunnerID != "not-a-uuid" {
		t.Fatalf("body = %s, want placement=nowhere runner_id=not-a-uuid", srv.last().body)
	}

	// Neither flag set: no placement/runner_id keys in JSON (omitempty).
	srv = newCmdServer(t, http.StatusOK, types.AgentRun{ID: uuid.New(), State: types.RunPending})
	if err := execCmd(t, "run", "--agent", "claude-code", "--url", srv.URL, "--token", "t"); err != nil {
		t.Fatalf("run without placement/runner returned error: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(srv.last().body, &raw); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if _, ok := raw["placement"]; ok {
		t.Errorf("placement key present in body when unset: %v", raw)
	}
	if _, ok := raw["runner_id"]; ok {
		t.Errorf("runner_id key present in body when unset: %v", raw)
	}
}