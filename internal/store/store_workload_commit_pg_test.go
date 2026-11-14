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
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A read-back of a workload's current run made while a record of the run is still committing waits for that
// record and reads its outcome, never the row as it was before a commit that then lands. No such workload, or
// another run, is not held.
func TestPG_Workload_HoldsRunWaitsForARecordStillCommitting(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	run := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
	w := mustPutWorkload(t, st, workloadPut("sub-a", "svc", "d1"))

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	// The record's own row lock and write, not committed yet.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM workloads WHERE id = $1 FOR UPDATE`, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE workloads SET current_run_id = $2, current_generation = 1 WHERE id = $1`, w.ID, run); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		held bool
		err  error
	}
	got := make(chan answer, 1)
	go func() {
		held, _, err := st.WorkloadHoldsRun(ctx, "sub-a", "svc", run)
		got <- answer{held, err}
	}()
	select {
	case a := <-got:
		t.Fatalf("read-back = %+v while the record was still committing, want it to wait for the record", a)
	case <-time.After(200 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-got:
		if a.err != nil || !a.held {
			t.Fatalf("read-back after the commit = %+v, want the run held", a)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the read-back did not answer once the record committed")
	}
	if held, deferred, err := st.WorkloadHoldsRun(ctx, "sub-a", "svc", uuid.New()); err != nil || held || deferred {
		t.Fatalf("another run held = %v, deferred = %v (%v), want neither", held, deferred, err)
	}
	if held, deferred, err := st.WorkloadHoldsRun(ctx, "sub-a", "gone", run); err != nil || held || deferred {
		t.Fatalf("no such workload held = %v, deferred = %v (%v), want neither", held, deferred, err)
	}
}

// A custom resource's record holds only while the binding its launch was authorised under exists: a delete of
// the binding still in flight is waited for, and once it commits the record writes nothing and the claim
// still holds. Under a binding that stands the record is SetWorkloadRun's.
func TestPG_Workload_BoundRecordWaitsForABindingDeleteAndThenRefuses(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	pid := orgPool(t, st, "A")
	bind := func(ns string) store.WorkloadBinding {
		t.Helper()
		b, err := st.CreateWorkloadBinding(ctx, store.WorkloadBinding{PoolID: pid, Namespace: ns, Service: "bot", Sponsor: "sub-a", CreatedBy: "secop"})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	b := bind("team-a")
	w := mustPutWorkload(t, st, workloadPut("service:bot", "web", "d1"))
	claim := mustClaim(t, st, w.ID)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	if _, err := tx.Exec(ctx, `DELETE FROM workload_bindings WHERE id = $1`, b.ID); err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		_, err := st.SetBoundWorkloadRun(ctx, w.ID, claim, 1, run, b.ID)
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("record = %v while the binding's delete was in flight, want it to wait for the delete", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if !errors.Is(err, store.ErrWorkloadBindingGone) {
			t.Fatalf("record after the delete committed = %v, want ErrWorkloadBindingGone", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the record did not answer once the delete committed")
	}
	v, err := st.GetWorkload(ctx, "service:bot", "web")
	if err != nil || v.CurrentRunID != nil || v.CurrentGeneration != 0 || v.LaunchClaimUntil == nil {
		t.Fatalf("workload after the refused record = %+v (%v), want no run and the claim still held", v.Workload, err)
	}
	if prev, err := st.SetBoundWorkloadRun(ctx, w.ID, claim, 1, run, bind("team-b").ID); err != nil || prev != nil {
		t.Fatalf("record under a standing binding = %v, %v; want it recorded with no previous run", prev, err)
	}
	if v, err := st.GetWorkload(ctx, "service:bot", "web"); err != nil || v.CurrentRunID == nil || *v.CurrentRunID != run || v.CurrentGeneration != 1 {
		t.Fatalf("workload after the record = %+v (%v), want run %s at generation 1", v.Workload, err, run)
	}
}

// owedEndsByRun is every owed end, by run.
func owedEndsByRun(t *testing.T, st store.PG) map[uuid.UUID]store.WorkloadRunEnd {
	t.Helper()
	ends, err := st.ListWorkloadRunEnds(t.Context(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	out := map[uuid.UUID]store.WorkloadRunEnd{}
	for _, e := range ends {
		out[e.RunID] = e
	}
	return out
}

// An owed end of a workload's current run that waits on nothing is what a record of unknown outcome leaves.
// The next record that replaces that run owes its end afresh: when the replacement waits in its queue, the
// end waits on it, never the stale end that would end the run while its replacement has not started. A
// read-back of the replaced run then reads it as deferred.
func TestPG_Workload_ARecordReplacesAStaleOwedEndOfItsCurrentRun(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	running := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
	queued := persistRun(t, ctx, pool, newRun(types.RunQueued)).ID
	w := mustPutWorkload(t, st, workloadPut("sub-a", "svc", "d1"))
	if _, err := st.SetWorkloadRun(ctx, w.ID, mustClaim(t, st, w.ID), 1, running); err != nil {
		t.Fatal(err)
	}
	if err := st.OweWorkloadRunEnd(ctx, store.WorkloadRunEnd{RunID: running, Owner: "sub-a", Name: "svc", Generation: 1, Reason: store.WorkloadEndReplaced}); err != nil {
		t.Fatal(err)
	}
	mustPutWorkload(t, st, workloadPut("sub-a", "svc", "d2"))
	if prev, err := st.SetWorkloadRun(ctx, w.ID, mustClaim(t, st, w.ID), 2, queued); err != nil || prev != nil {
		t.Fatalf("queued replacement recorded = %v, %v; want no run handed back to end", prev, err)
	}
	if e, ok := owedEndsByRun(t, st)[running]; !ok || e.AfterRunID == nil || *e.AfterRunID != queued {
		t.Fatalf("running run's owed end = %+v (%v), want it waiting on its queued replacement %s", e, ok, queued)
	}
	// A read-back of the replaced run reads it as the workload's once, now waiting on its replacement.
	if held, deferred, err := st.WorkloadHoldsRun(ctx, "sub-a", "svc", running); err != nil || held || !deferred {
		t.Fatalf("read-back of the replaced run = held %v, deferred %v (%v); want deferred", held, deferred, err)
	}
}

// The sweep's drop of a held run's owed end reads the workload under its row lock in the drop's own
// transaction: a record still committing that replaces the run is waited for, and the run it let go keeps
// its owed end. A held run's end that waits on nothing is dropped.
func TestPG_Workload_DropIfHeldWaitsForARecordStillCommitting(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	run := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
	next := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
	w := mustPutWorkload(t, st, workloadPut("sub-a", "svc", "d1"))
	if _, err := st.SetWorkloadRun(ctx, w.ID, mustClaim(t, st, w.ID), 1, run); err != nil {
		t.Fatal(err)
	}
	owe := func() {
		t.Helper()
		if err := st.OweWorkloadRunEnd(ctx, store.WorkloadRunEnd{RunID: run, Owner: "sub-a", Name: "svc", Generation: 1, Reason: store.WorkloadEndReplaced}); err != nil {
			t.Fatal(err)
		}
	}
	owe()
	if held, err := st.DropWorkloadRunEndIfHeld(ctx, "sub-a", "svc", run); err != nil || !held {
		t.Fatalf("drop of the held run's end = %v (%v), want held", held, err)
	}
	if _, ok := owedEndsByRun(t, st)[run]; ok {
		t.Fatal("the held run is still owed its end after the drop")
	}
	owe()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	// A record that replaces run, not committed yet: its row lock and write.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM workloads WHERE id = $1 FOR UPDATE`, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE workloads SET current_run_id = $2 WHERE id = $1`, w.ID, next); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		held bool
		err  error
	}
	got := make(chan answer, 1)
	go func() {
		held, err := st.DropWorkloadRunEndIfHeld(ctx, "sub-a", "svc", run)
		got <- answer{held, err}
	}()
	select {
	case a := <-got:
		t.Fatalf("drop = %+v while the record was still committing, want it to wait for the record", a)
	case <-time.After(200 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-got:
		if a.err != nil || a.held {
			t.Fatalf("drop after the record committed = %+v, want not held", a)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the drop did not answer once the record committed")
	}
	if _, ok := owedEndsByRun(t, st)[run]; !ok {
		t.Fatal("the run the record let go lost its owed end")
	}
}

// A read-back that waits for a record replacing its run with a queued run reads, once that record commits,
// that the run is owed an end waiting on the replacement (deferred): the second read runs under the lock, after
// the wait, so it sees the owed end that record wrote.
func TestPG_Workload_HoldsRunReadsAReplacementCommittedDuringItsWait(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	run := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
	queued := persistRun(t, ctx, pool, newRun(types.RunQueued)).ID
	w := mustPutWorkload(t, st, workloadPut("sub-a", "svc", "d1"))
	if _, err := st.SetWorkloadRun(ctx, w.ID, mustClaim(t, st, w.ID), 1, run); err != nil {
		t.Fatal(err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	// A later record, not committed yet: its row lock, its write and the end it owes run, waiting on queued.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM workloads WHERE id = $1 FOR UPDATE`, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE workloads SET current_run_id = $2, current_generation = 2 WHERE id = $1`, w.ID, queued); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workload_run_ends (run_id, owner, name, generation, reason, after_run_id)
		VALUES ($1, 'sub-a', 'svc', 2, 'workload_replaced', $2)`, run, queued); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		held, deferred bool
		err            error
	}
	got := make(chan answer, 1)
	go func() {
		held, deferred, err := st.WorkloadHoldsRun(ctx, "sub-a", "svc", run)
		got <- answer{held, deferred, err}
	}()
	select {
	case a := <-got:
		t.Fatalf("read-back = %+v while the later record was still committing, want it to wait for the record", a)
	case <-time.After(200 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-got:
		if a.err != nil || a.held || !a.deferred {
			t.Fatalf("read-back after the later record committed = %+v, want not held and deferred", a)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the read-back did not answer once the later record committed")
	}
}
