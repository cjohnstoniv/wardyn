// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// A PATCH reads the row, edits it and writes the whole row back. A run claim
// that lands between the read and the write must survive it, and the edit must
// say it was refused rather than revert the claim.
func TestPG_UpdateWorkspace_SnapshotOlderThanAClaimIsRefused(t *testing.T) {
	pg := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	ws := imageBuildWorkspace(t, pg, "")
	snapshot, err := pg.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	runID := uuid.New()
	if _, applied, cerr := pg.ClaimWorkspaceActiveRun(ctx, ws.ID, runID, nil); cerr != nil || !applied {
		t.Fatalf("claim: applied=%v err=%v", applied, cerr)
	}

	snapshot.Name += "-renamed"
	if _, err := pg.UpdateWorkspace(ctx, ws.ID, snapshot, false); !errors.Is(err, store.ErrWorkspaceStale) {
		t.Fatalf("UpdateWorkspace on a stale snapshot = %v, want ErrWorkspaceStale", err)
	}
	got, err := pg.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ActiveRunID == nil || *got.ActiveRunID != runID {
		t.Errorf("active_run_id = %v after the refused edit, want %s", got.ActiveRunID, runID)
	}
	if got.Name != ws.Name {
		t.Errorf("name = %q, the refused edit must change nothing", got.Name)
	}

	// The edit built from a fresh read lands, and keeps the claim.
	fresh := got
	fresh.Name += "-renamed"
	updated, err := pg.UpdateWorkspace(ctx, ws.ID, fresh, false)
	if err != nil {
		t.Fatalf("UpdateWorkspace on a fresh read: %v", err)
	}
	if updated.Name != fresh.Name || updated.ActiveRunID == nil || *updated.ActiveRunID != runID {
		t.Errorf("updated = name %q active_run_id %v", updated.Name, updated.ActiveRunID)
	}
}

// A missing row stays ErrNotFound: the guard must not turn it into a conflict.
func TestPG_UpdateWorkspace_UnknownIDIsStillNotFound(t *testing.T) {
	pg := store.NewPG(runsPGPool(t))
	ws := imageBuildWorkspace(t, pg, "")
	if _, err := pg.UpdateWorkspace(context.Background(), uuid.New(), ws, false); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateWorkspace(unknown id) = %v, want ErrNotFound", err)
	}
}

// workspaces.name is still globally UNIQUE; a taken name is a conflict the
// caller can answer with a 409, not an opaque store error.
func TestPG_Workspace_TakenNameIsErrConflict(t *testing.T) {
	pg := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	a := imageBuildWorkspace(t, pg, "")
	dup := a
	dup.ID = uuid.New()
	if _, err := pg.CreateWorkspace(ctx, dup); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("CreateWorkspace(taken name) = %v, want ErrConflict", err)
	}
	b := imageBuildWorkspace(t, pg, "")
	b.Name = a.Name
	_, err := pg.UpdateWorkspace(ctx, b.ID, b, false)
	if !errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrWorkspaceStale) {
		t.Fatalf("UpdateWorkspace(rename onto a taken name) = %v, want ErrConflict and not ErrWorkspaceStale", err)
	}
}
