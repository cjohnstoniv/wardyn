// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// finalizeGuardStore serves one workspace + its active run to the finalize handler.
type finalizeGuardStore struct {
	store.Store
	ws  types.Workspace
	run types.AgentRun
}

func (s *finalizeGuardStore) GetWorkspace(context.Context, uuid.UUID) (types.Workspace, error) {
	return s.ws, nil
}
func (s *finalizeGuardStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return s.run, nil
}
func (s *finalizeGuardStore) SetWorkspaceImportState(_ context.Context, _ uuid.UUID, status types.WorkspaceStatus, active *uuid.UUID, _ json.RawMessage, _ string, _ *time.Time) (types.Workspace, error) {
	s.ws.Status = status
	s.ws.ActiveRunID = active
	return s.ws, nil
}

// TestFinalizeWorkspace_RefusesWhileRunLive is the C001 regression (crown record-
// verify): finalizing a workspace while its verify/record run is still RUNNING would
// zero active_run_id + mark it ready, silently dropping the live run's real result
// (its later upload then 409s on the cleared pointer). Finalize must 409 while a run
// is live — matching the guard verify/record already have — and proceed once terminal.
func TestFinalizeWorkspace_RefusesWhileRunLive(t *testing.T) {
	h := newHarness(t)
	wsID, runID := uuid.New(), uuid.New()
	fake := &finalizeGuardStore{
		ws:  types.Workspace{ID: wsID, Status: types.WorkspaceVerifying, ActiveRunID: &runID},
		run: types.AgentRun{ID: runID, State: types.RunRunning},
	}
	srv := New(baseTestConfig(h, fake))

	w := do(t, srv, http.MethodPost, "/api/v1/workspaces/"+wsID.String()+"/finalize", adminToken, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("finalize while the active run is RUNNING must 409 (C001); got %d: %s", w.Code, w.Body.String())
	}

	// A terminal active run (a stale pointer) must NOT block finalize.
	fake.run.State = types.RunCompleted
	w = do(t, srv, http.MethodPost, "/api/v1/workspaces/"+wsID.String()+"/finalize", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("finalize with a terminal active run must succeed; got %d: %s", w.Code, w.Body.String())
	}
}
