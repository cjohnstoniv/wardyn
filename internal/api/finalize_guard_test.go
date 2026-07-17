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

// Models the real store's fence (see importStateFake): applies only while the
// import-step slot still holds expectedActive.
func (s *finalizeGuardStore) SetWorkspaceImportState(_ context.Context, _ uuid.UUID, status types.WorkspaceStatus, active *uuid.UUID, expectedActive *uuid.UUID, _ json.RawMessage, _ string, _ *time.Time) (types.Workspace, bool, error) {
	if !samePtrUUID(s.ws.ActiveRunID, expectedActive) {
		return s.ws, false, nil
	}
	s.ws.Status = status
	s.ws.ActiveRunID = active
	return s.ws, true, nil
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

// TestFinalizeWorkspace_FencedAgainstRunClaimedAfterGuard is the counterfactual
// for the crown's "SetWorkspaceImportState is the only unfenced workspace
// writer" HIGH. C001 added an active-run guard to handleFinalizeWorkspace, but a
// guard is only a READ: it proves no run was live at read time. A verify/record
// run that claims the import-step slot in the window between that read and the
// write would be silently clobbered — finalize would zero active_run_id and mark
// the workspace ready while the run is live. The store write is now fenced on
// the slot the guard observed, so the late finalize is refused (409) instead.
func TestFinalizeWorkspace_FencedAgainstRunClaimedAfterGuard(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	claimed := uuid.New() // the run that claims the slot after the guard read

	fake := &finalizeGuardStore{
		ws: types.Workspace{
			ID: wsID, Kind: types.WorkspaceKindLocalDir, Source: "/w",
			Status: types.WorkspaceScanned,
			// The guard read saw an EMPTY slot (nil); by write time a run owns it.
			ActiveRunID: &claimed,
		},
	}
	srv := New(baseTestConfig(h, fake))

	// Finalize decided from a snapshot with no active run (expectedActive=nil),
	// which is exactly what the guard permits; the fence must catch the drift.
	_, applied, err := srv.cfg.Store.SetWorkspaceImportState(context.Background(), wsID,
		types.WorkspaceReady, nil, nil, nil, "", nil)
	if err != nil {
		t.Fatalf("SetWorkspaceImportState: %v", err)
	}
	if applied {
		t.Fatal("a finalize decided from a stale (empty-slot) read was applied while a run owns the import step — it would drop the live run's result")
	}
	if fake.ws.Status == types.WorkspaceReady || fake.ws.ActiveRunID == nil {
		t.Errorf("the refused write still mutated the row: status=%s active=%v", fake.ws.Status, fake.ws.ActiveRunID)
	}

	// Control: the same write fenced on the CORRECT observed slot applies.
	_, applied, err = srv.cfg.Store.SetWorkspaceImportState(context.Background(), wsID,
		types.WorkspaceReady, nil, &claimed, nil, "", nil)
	if err != nil {
		t.Fatalf("SetWorkspaceImportState: %v", err)
	}
	if !applied {
		t.Error("a write fenced on the actual current slot must apply — the fence over-reached")
	}
}
