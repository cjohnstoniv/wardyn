// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// A run records the kept machine it kept or resumed, and keeps the id after the machine's row is gone.
func TestRunKeptMachineOutlivesTheMachine(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	runner := claimedRunnerOf(t, pg, "sub-a")
	run := erasureRun(t, pg, "sub-a", "keep")
	if id, mode, err := pg.RunKeptMachine(ctx, run); err != nil || id != nil || mode != "" {
		t.Fatalf("a run that keeps none: %v %q %v", id, mode, err)
	}
	m, err := pg.CreateKeptMachine(ctx, keptMachineFor("sub-a", runner, run, "web"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.SetRunKeptMachine(ctx, run, &m.ID, "new"); err != nil {
		t.Fatal(err)
	}
	if err := pg.DropUnstartedKeptMachine(ctx, m.ID, run); err != nil {
		t.Fatal(err)
	}
	if id, mode, err := pg.RunKeptMachine(ctx, run); err != nil || id == nil || *id != m.ID || mode != "new" {
		t.Fatalf("after the machine's row went: %v %q %v", id, mode, err)
	}
	if err := pg.SetRunKeptMachine(ctx, run, nil, "new"); err != nil {
		t.Fatal(err)
	}
	if id, mode, err := pg.RunKeptMachine(ctx, run); err != nil || id != nil || mode != "" {
		t.Fatalf("cleared: %v %q %v", id, mode, err)
	}
	if err := pg.SetRunKeptMachine(ctx, uuid.New(), &m.ID, "new"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a missing run: %v", err)
	}
	if _, _, err := pg.RunKeptMachine(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a missing run: %v", err)
	}
}

// A fresh start records its new base only while its run holds the machine.
func TestResetKeptMachineBaseOnlyForTheHoldingRun(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	runner := claimedRunnerOf(t, pg, "sub-a")
	m := keptMachineOf(t, pg, "sub-a", runner, "web")
	run := erasureRun(t, pg, "sub-a", "resume")
	at := time.Now().UTC().Truncate(time.Second)
	if err := pg.ResetKeptMachineBase(ctx, m.ID, run, "Standard", at); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a kept machine no run holds: %v", err)
	}
	if _, err := pg.BeginKeptMachineUse(ctx, m.ID, "sub-a", run); err != nil {
		t.Fatal(err)
	}
	if err := pg.ResetKeptMachineBase(ctx, m.ID, run, "Standard", at); err != nil {
		t.Fatal(err)
	}
	got, err := pg.GetKeptMachine(ctx, m.ID)
	if err != nil || got.Base != "Standard" || !got.BasePreparedAt.Equal(at) {
		t.Fatalf("after a fresh start: %+v, %v", got, err)
	}
}
