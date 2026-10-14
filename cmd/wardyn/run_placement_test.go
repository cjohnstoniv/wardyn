// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestRunPlacementFlagsAreSentUnchanged(t *testing.T) {
	runnerID := uuid.New()

	t.Run("placement local with runner", func(t *testing.T) {
		for _, c := range []struct {
			name, path string
			extra      []string
		}{
			{"create", "/api/v1/runs", nil},
			{"dry-run", "/api/v1/runs/preflight", []string{"--dry-run"}},
		} {
			t.Run(c.name, func(t *testing.T) {
				args := append([]string{"run", "--agent", "claude-code", "--placement", "local", "--runner", runnerID.String()}, c.extra...)
				srv := newCmdServer(t, http.StatusUnprocessableEntity, map[string]string{"error": "x", "reason": "invalid_request"})
				if err := execCmd(t, append(args, "--url", srv.URL, "--token", "t")...); err == nil {
					t.Fatal("the server's refusal was swallowed")
				}
				if got := srv.last().path; got != c.path {
					t.Errorf("path = %s, want %s", got, c.path)
				}
				var body sdk.CreateRunRequest
				if err := json.Unmarshal(srv.last().body, &body); err != nil || body.Placement != "local" || body.RunnerID != runnerID.String() {
					t.Fatalf("body = %s, want placement=local runner_id=%s", srv.last().body, runnerID)
				}
			})
		}
	})

	t.Run("invalid values pass through", func(t *testing.T) {
		srv := newCmdServer(t, http.StatusBadRequest, map[string]string{"error": "x", "reason": "invalid_request"})
		if err := execCmd(t, "run", "--agent", "claude-code", "--placement", "nowhere", "--runner", "not-a-uuid", "--url", srv.URL, "--token", "t"); err == nil {
			t.Fatal("invalid placement/runner should return an error from the server")
		}
		var body sdk.CreateRunRequest
		if err := json.Unmarshal(srv.last().body, &body); err != nil || body.Placement != "nowhere" || body.RunnerID != "not-a-uuid" {
			t.Fatalf("body = %s, want placement=nowhere runner_id=not-a-uuid", srv.last().body)
		}
	})

	t.Run("runner without placement local passes through", func(t *testing.T) {
		srv := newCmdServer(t, http.StatusUnprocessableEntity, map[string]string{"error": "x", "reason": "invalid_request"})
		if err := execCmd(t, "run", "--agent", "claude-code", "--runner", runnerID.String(), "--url", srv.URL, "--token", "t"); err == nil {
			t.Fatal("the server's refusal was swallowed")
		}
		var body sdk.CreateRunRequest
		if err := json.Unmarshal(srv.last().body, &body); err != nil || body.RunnerID != runnerID.String() || body.Placement != "" {
			t.Fatalf("body = %s, want runner_id=%s and no placement", srv.last().body, runnerID)
		}
	})

	t.Run("neither flag leaves the keys out", func(t *testing.T) {
		srv := newCmdServer(t, http.StatusOK, types.AgentRun{ID: uuid.New(), State: types.RunPending})
		if err := execCmd(t, "run", "--agent", "claude-code", "--url", srv.URL, "--token", "t"); err != nil {
			t.Fatalf("run without placement/runner returned error: %v", err)
		}
		var raw map[string]any
		if err := json.Unmarshal(srv.last().body, &raw); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		for _, key := range []string{"placement", "runner_id"} {
			if _, ok := raw[key]; ok {
				t.Errorf("%s key present in body when unset: %v", key, raw)
			}
		}
	})
}
