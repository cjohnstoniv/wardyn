// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestTerminalDoorRefusesABackgroundRun: every way into a Background run's
// terminal (the ticket, the socket, the take-over) answers 409
// run_background_only, from the stored experience, and no ticket is minted.
func TestTerminalDoorRefusesABackgroundRun(t *testing.T) {
	ast := newAuthzStore()
	cfg := baseTestConfig(newHarness(t), ast)
	cfg.Runner = &fakeRunner{}
	srv := New(cfg)
	run := uuid.New()
	ast.runs[run] = types.AgentRun{ID: run, CreatedBy: "alice", OperatorOwned: true, State: types.RunRunning, SandboxRef: "sbx-1",
		Experience: types.ExperienceBackground}
	for _, door := range []struct{ method, path string }{
		{http.MethodPost, "/attach-ticket"},
		{http.MethodPost, "/attach/ticket"},
		{http.MethodGet, "/attach"},
		{http.MethodPost, "/attach/takeover"},
	} {
		w := do(t, srv, door.method, "/api/v1/runs/"+run.String()+door.path, adminToken, "")
		if w.Code != http.StatusConflict || reasonOf(t, w) != reasonRunBackgroundOnly {
			t.Errorf("%s %s = %d %s, want 409 %s", door.method, door.path, w.Code, w.Body.String(), reasonRunBackgroundOnly)
		}
	}
	if len(ast.tickets) != 0 {
		t.Fatalf("%d tickets minted for a background run", len(ast.tickets))
	}
}
