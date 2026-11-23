// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_SnapshotHoldRestrictsRowDelete: a snapshot a fork holds cannot be deleted, by the store's delete or by a
// raw DELETE (the foreign key restricts it); a second snapshot of the same run cannot be made while one is being
// made; and once the fork's hold is released the snapshot's delete goes through.
func TestPG_SnapshotHoldRestrictsRowDelete(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	rn, err := pg.CreateRunner(ctx, newRunner("alice"))
	if err != nil {
		t.Fatal(err)
	}
	src := persistRun(t, ctx, pool, newRun(types.RunRunning))
	expires := time.Now().Add(14 * 24 * time.Hour).UTC()
	snap := types.MachineSnapshot{ID: uuid.New(), SourceRunID: &src.ID, OwnerSubject: src.CreatedBy, RunnerID: rn.ID,
		MachineKind: types.MachineLinuxVM, ImageRef: "ghcr.io/acme/dev:1", ImageDigest: "sha256:" + strings.Repeat("ab", 32),
		ImageID: "img-1", LayerName: "snap-1", ExpiresAt: &expires}
	if _, err := pg.CreateMachineSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	second := snap
	second.ID, second.LayerName = uuid.New(), "snap-2"
	if _, err := pg.CreateMachineSnapshot(ctx, second); !errors.Is(err, store.ErrMachineSnapshotBusy) {
		t.Fatalf("a second snapshot while one is made: %v, want ErrMachineSnapshotBusy", err)
	}
	fork := persistRun(t, ctx, pool, newRun(types.RunStarting))
	if err := pg.ClaimMachineSnapshotFork(ctx, snap.ID, fork.ID); !errors.Is(err, store.ErrMachineSnapshotUnavailable) {
		t.Fatalf("a fork of a snapshot still being made: %v, want ErrMachineSnapshotUnavailable", err)
	}
	if ok, err := pg.MarkMachineSnapshotReady(ctx, snap.ID, "abc", 1<<20, "stamp"); err != nil || !ok {
		t.Fatalf("mark ready: %v, %v", ok, err)
	}
	if err := pg.ClaimMachineSnapshotFork(ctx, snap.ID, fork.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.BeginMachineSnapshotDelete(ctx, snap.ID, src.CreatedBy); !errors.Is(err, store.ErrMachineSnapshotInUse) {
		t.Fatalf("a delete while a fork holds it: %v, want ErrMachineSnapshotInUse", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM machine_snapshots WHERE id = $1`, snap.ID); err == nil {
		t.Fatal("a raw DELETE of a held snapshot went through")
	}
	if err := pg.DeleteMachineSnapshotRow(ctx, snap.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := pg.GetMachineSnapshot(ctx, snap.ID); err != nil || got.State != types.MachineSnapshotReady {
		t.Fatalf("the row delete of a ready, held snapshot: %+v, %v; want it kept, ready", got, err)
	}
	if err := pg.ReleaseMachineSnapshotHold(ctx, snap.ID, fork.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.BeginMachineSnapshotDelete(ctx, snap.ID, src.CreatedBy); err != nil {
		t.Fatalf("a delete once the hold is released: %v", err)
	}
	if err := pg.DeleteMachineSnapshotRow(ctx, snap.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.GetMachineSnapshot(ctx, snap.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the deleted snapshot: %v, want ErrNotFound", err)
	}
}

// TestPG_SnapshotForkClaimRefusals: a fork claim is refused for a snapshot whose source run is gone and for one
// whose source run let a support helper in, and leaves no hold either way; a delete names the snapshot's owner,
// and an empty owner names any owner.
func TestPG_SnapshotForkClaimRefusals(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	rn, err := pg.CreateRunner(ctx, newRunner("alice"))
	if err != nil {
		t.Fatal(err)
	}
	ready := func(src *types.AgentRun) types.MachineSnapshot {
		t.Helper()
		snap := types.MachineSnapshot{ID: uuid.New(), OwnerSubject: "tester@example.com", RunnerID: rn.ID,
			MachineKind: types.MachineLinuxVM, ImageRef: "ghcr.io/acme/dev:1", ImageDigest: "sha256:" + strings.Repeat("cd", 32),
			ImageID: "img-1", LayerName: "snap-" + uuid.NewString()}
		if src != nil {
			snap.SourceRunID = &src.ID
		}
		if _, err := pg.CreateMachineSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		if ok, err := pg.MarkMachineSnapshotReady(ctx, snap.ID, "abc", 1, "stamp"); err != nil || !ok {
			t.Fatalf("mark ready: %v, %v", ok, err)
		}
		return snap
	}
	holds := func(snap types.MachineSnapshot) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM machine_snapshot_holds WHERE snapshot_id = $1`, snap.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	orphan := ready(nil)
	fork := persistRun(t, ctx, pool, newRun(types.RunStarting))
	if err := pg.ClaimMachineSnapshotFork(ctx, orphan.ID, fork.ID); !errors.Is(err, store.ErrForkSupportSession) || holds(orphan) != 0 {
		t.Errorf("a fork of a snapshot whose source run is gone: %v, %d holds; want ErrForkSupportSession and none", err, holds(orphan))
	}

	src := persistRun(t, ctx, pool, newRun(types.RunRunning))
	helped := ready(&src)
	g, err := pg.RequestSupportGrant(ctx, store.SupportGrant{RunID: src.ID, Helper: "helper", HelperEmail: "helper@example.test",
		Reason: "r", DoorsAsked: []string{"terminal"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.ClaimMachineSnapshotFork(ctx, helped.ID, fork.ID); err != nil {
		t.Fatalf("a fork while a support grant is only asked for: %v", err)
	}
	if err := pg.ReleaseMachineSnapshotHold(ctx, helped.ID, fork.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.AcceptSupportGrant(ctx, src.ID, g.ID, src.CreatedBy, src.CreatedBy, []string{"terminal"}, 30, 0); err != nil {
		t.Fatal(err)
	}
	other := persistRun(t, ctx, pool, newRun(types.RunStarting))
	if err := pg.ClaimMachineSnapshotFork(ctx, helped.ID, other.ID); !errors.Is(err, store.ErrForkSupportSession) || holds(helped) != 0 {
		t.Errorf("a fork after a support grant was accepted: %v, %d holds; want ErrForkSupportSession and none", err, holds(helped))
	}

	if _, err := pg.BeginMachineSnapshotDelete(ctx, orphan.ID, "someone-else"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a delete naming another owner: %v, want ErrNotFound", err)
	}
	if _, err := pg.BeginMachineSnapshotDelete(ctx, orphan.ID, ""); err != nil {
		t.Errorf("a delete naming no owner: %v, want it to go through", err)
	}
}
