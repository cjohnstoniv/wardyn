// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// startingRun is a run of owner's being dispatched.
func startingRun(t *testing.T, pool *pgxpool.Pool, pg store.PG, owner string) uuid.UUID {
	t.Helper()
	run := erasureRun(t, pg, owner, "dispatch")
	if _, err := pool.Exec(context.Background(), `UPDATE agent_runs SET state = 'STARTING' WHERE id = $1`, run); err != nil {
		t.Fatal(err)
	}
	return run
}

// A run records the kept machine it kept, and keeps the id after the machine's row is gone.
func TestRunKeptMachineOutlivesTheMachine(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	runner := claimedRunnerOf(t, pg, "sub-a")
	run := startingRun(t, pool, pg, "sub-a")
	if id, mode, err := pg.RunKeptMachine(ctx, run); err != nil || id != nil || mode != "" {
		t.Fatalf("a run that keeps none: %v %q %v", id, mode, err)
	}
	m, err := pg.CreateKeptMachineForRun(ctx, keptMachineFor("sub-a", runner, run, "web"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if id, mode, err := pg.RunKeptMachine(ctx, run); err != nil || id == nil || *id != m.ID || mode != "new" {
		t.Fatalf("after the insert: %v %q %v", id, mode, err)
	}
	if err := pg.DropUnstartedKeptMachine(ctx, m.ID, run); err != nil {
		t.Fatal(err)
	}
	if id, _, err := pg.RunKeptMachine(ctx, run); err != nil || id == nil || *id != m.ID {
		t.Fatalf("after the machine's row went: %v %v", id, err)
	}
	if err := pg.SetRunKeptMachine(ctx, run, nil, "new"); err != nil {
		t.Fatal(err)
	}
	if id, mode, err := pg.RunKeptMachine(ctx, run); err != nil || id != nil || mode != "" {
		t.Fatalf("cleared: %v %q %v", id, mode, err)
	}
	if _, _, err := pg.RunKeptMachine(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a missing run: %v", err)
	}
}

// A kill that holds the run's row first wins: the claim waits for it, then makes and changes nothing.
func TestClaimKeptMachineForRunLosesToAKillThatCommitsFirst(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	runner := claimedRunnerOf(t, pg, "sub-a")
	m := keptMachineOf(t, pg, "sub-a", runner, "web")
	run := startingRun(t, pool, pg, "sub-a")
	kill, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = kill.Rollback(ctx) }()
	if _, err := kill.Exec(ctx, `UPDATE agent_runs SET state = 'KILLED' WHERE id = $1 AND state = 'STARTING'`, run); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := pg.ClaimKeptMachineForRun(ctx, m.ID, "sub-a", run, "resume")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the claim did not wait for the kill holding the run: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := kill.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, store.ErrRunNotDispatching) {
		t.Fatalf("claim after the kill: %v", err)
	}
	got, err := pg.GetKeptMachine(ctx, m.ID)
	if err != nil || got.State != types.KeptMachineKept || (got.RunID != nil && *got.RunID == run) {
		t.Fatalf("the killed run's claim changed the machine: %+v, %v", got, err)
	}
	if id, _, _ := pg.RunKeptMachine(ctx, run); id != nil {
		t.Fatal("the killed run records a machine")
	}
	if _, err := pg.CreateKeptMachineForRun(ctx, keptMachineFor("sub-a", runner, run, "new"), 4); !errors.Is(err, store.ErrRunNotDispatching) {
		t.Fatalf("insert for a killed run: %v", err)
	}
	if ms, _ := pg.ListKeptMachinesOf(ctx, "sub-a"); len(ms) != 1 {
		t.Fatalf("machines = %d, want only the first", len(ms))
	}
}

// A kill that lands after the claim finds the machine on the run, so its delete intent reaches it.
func TestKillAfterClaimKeptMachineForRunFindsTheMachine(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	runner := claimedRunnerOf(t, pg, "sub-a")
	m := keptMachineOf(t, pg, "sub-a", runner, "web")
	run := startingRun(t, pool, pg, "sub-a")
	if _, err := pg.ClaimKeptMachineForRun(ctx, m.ID, "sub-a", run, "resume"); err != nil {
		t.Fatal(err)
	}
	// The kill's own statements, in its order: the run's row, then the machine the run has.
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET state = 'KILLED' WHERE id = $1 AND state = 'STARTING'`, run); err != nil {
		t.Fatal(err)
	}
	tag, err := pool.Exec(ctx, `UPDATE kept_machines SET delete_intent_at = now(), delete_intent_by = 'admin'
		WHERE run_id = $1 AND state <> 'deleting' AND delete_intent_at IS NULL`, run)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("the kill's intent reached %d machines: %v", tag.RowsAffected(), err)
	}
}

// A fresh start's base is written only from its confirmation, for the run that marked it.
func TestKeptMachineResetMarkAndConfirmation(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	runner := claimedRunnerOf(t, pg, "sub-a")
	m := keptMachineOf(t, pg, "sub-a", runner, "web")
	run := startingRun(t, pool, pg, "sub-a")
	if _, err := pg.ClaimKeptMachineForRun(ctx, m.ID, "sub-a", run, "fresh"); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Second)
	if err := pg.ResetKeptMachineBase(ctx, m.ID, run, "Standard", at); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a base written with no mark: %v", err)
	}
	if err := pg.MarkKeptMachineResetting(ctx, m.ID, run); err != nil {
		t.Fatal(err)
	}
	if pending, err := pg.KeptMachineResetPending(ctx, m.ID); err != nil || !pending {
		t.Fatalf("pending = %v, %v", pending, err)
	}
	if err := pg.ResetKeptMachineBase(ctx, m.ID, run, "Standard", at); err != nil {
		t.Fatal(err)
	}
	got, err := pg.GetKeptMachine(ctx, m.ID)
	if err != nil || got.Base != "Standard" || !got.BasePreparedAt.Equal(at) {
		t.Fatalf("after a confirmed fresh start: %+v, %v", got, err)
	}
	if pending, _ := pg.KeptMachineResetPending(ctx, m.ID); pending {
		t.Fatal("the confirmation left the mark")
	}
	if err := pg.MarkKeptMachineResetting(ctx, m.ID, run); err != nil {
		t.Fatal(err)
	}
	if err := pg.ClearKeptMachineResetting(ctx, m.ID, run); err != nil {
		t.Fatal(err)
	}
	if pending, _ := pg.KeptMachineResetPending(ctx, m.ID); pending {
		t.Fatal("a fresh start never asked of the Mac left its mark")
	}
}
