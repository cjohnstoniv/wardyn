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
