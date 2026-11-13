// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_ReleaseStrandedRunClaims_FreesOnlyAnOldClaimWithNoRun: a claim naming a run that was never
// written is released once its row has gone unwritten past the grace, the source back to error with its
// profile and the workspace's scanning status to error. A claim younger than the grace (a launch still
// between its claim and its run insert) and a claim whose run exists (a live claimant) are kept.
// Guarded by WARDYN_TEST_PG through hydrateStore.
func TestPG_ReleaseStrandedRunClaims_FreesOnlyAnOldClaimWithNoRun(t *testing.T) {
	s := hydrateStore(t)
	ctx := context.Background()
	age := func(table string, id uuid.UUID) {
		t.Helper()
		if _, err := s.Pool.Exec(ctx, `UPDATE `+table+` SET updated_at = now() - interval '2 hours' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	liveRun := func() uuid.UUID {
		r := newRun(types.RunPending)
		persistRun(t, ctx, s.Pool, r)
		return r.ID
	}
	claimSource := func(locator string, runID uuid.UUID) types.Source {
		t.Helper()
		src := mkSource(t, s, types.SourceRepo, locator, nil)
		if _, err := s.SetSourceScanResultUnfenced(ctx, src.ID, []byte(`{"confidence":"high"}`), types.WorkspaceScanned, nil); err != nil {
			t.Fatal(err)
		}
		if err := s.ClaimSourceActiveRun(ctx, src.ID, runID); err != nil {
			t.Fatal(err)
		}
		return src
	}
	claimWorkspace := func(name string, runID uuid.UUID) types.Workspace {
		t.Helper()
		ws, err := s.CreateWorkspace(ctx, types.Workspace{
			ID: uuid.New(), Name: name, Status: types.WorkspacePendingScan,
			Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral}},
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, err := s.SetWorkspaceImportState(ctx, ws.ID, types.WorkspaceScanning, &runID, nil); err != nil || !ok {
			t.Fatalf("claim workspace %s: ok=%v err=%v", name, ok, err)
		}
		return ws
	}

	strandedSrc := claimSource("acme/stranded", uuid.New())
	age("sources", strandedSrc.ID)
	freshSrc := claimSource("acme/fresh", uuid.New())
	liveSrc := claimSource("acme/live", liveRun())
	age("sources", liveSrc.ID)
	strandedWS := claimWorkspace("ws-stranded", uuid.New())
	age("workspaces", strandedWS.ID)
	liveWS := claimWorkspace("ws-live", liveRun())
	age("workspaces", liveWS.ID)
	// A record step claims without moving the status (ClaimWorkspaceActiveRun): the release keeps it.
	recordRun := uuid.New()
	recordWS := claimWorkspace("ws-record", recordRun)
	if _, ok, err := s.SetWorkspaceImportState(ctx, recordWS.ID, types.WorkspaceScanned, &recordRun, &recordRun); err != nil || !ok {
		t.Fatalf("stage the record claim: ok=%v err=%v", ok, err)
	}
	age("workspaces", recordWS.ID)

	released, err := s.ReleaseStrandedRunClaims(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]string{}
	for _, c := range released {
		got[c.ID] = c.Kind
	}
	if len(released) != 3 || got[strandedSrc.ID] != "source" || got[strandedWS.ID] != "workspace" || got[recordWS.ID] != "workspace" {
		t.Fatalf("released = %+v, want exactly the stranded source and the two stranded workspaces", released)
	}

	src, err := s.GetSource(ctx, strandedSrc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if src.ActiveRunID != nil || src.Status != types.WorkspaceError || string(src.Profile) == "" {
		t.Errorf("released source: active_run_id=%v status=%s profile=%s, want none, error, the profile kept", src.ActiveRunID, src.Status, src.Profile)
	}
	for _, id := range []uuid.UUID{freshSrc.ID, liveSrc.ID} {
		kept, err := s.GetSource(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if kept.ActiveRunID == nil || kept.Status != types.WorkspaceScanning {
			t.Errorf("source %s lost its claim: active_run_id=%v status=%s", kept.Locator, kept.ActiveRunID, kept.Status)
		}
	}
	ws, err := s.GetWorkspace(ctx, strandedWS.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ws.ActiveRunID != nil || ws.Status != types.WorkspaceError {
		t.Errorf("released workspace: active_run_id=%v status=%s, want none and error", ws.ActiveRunID, ws.Status)
	}
	if rec, err := s.GetWorkspace(ctx, recordWS.ID); err != nil || rec.ActiveRunID != nil || rec.Status != types.WorkspaceScanned {
		t.Errorf("released record claim: active_run_id=%v status=%s err=%v, want none and scanned", rec.ActiveRunID, rec.Status, err)
	}
	if kept, err := s.GetWorkspace(ctx, liveWS.ID); err != nil || kept.ActiveRunID == nil {
		t.Errorf("the live claimant's workspace lost its claim: %+v %v", kept.ActiveRunID, err)
	}
}
