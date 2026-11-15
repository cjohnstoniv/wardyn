// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A queued run's start under a namespace binding holds only while the binding exists: a delete still in
// flight is waited for, and once it commits the start writes nothing. Under a binding that stands it writes.
func TestPG_QueueStart_WaitsForABindingDeleteAndThenRefuses(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := t.Context()
	run := promotedRun(t, pg, time.Now().UTC())
	pid := orgPool(t, pg, "A")
	bind := func(ns string) store.WorkloadBinding {
		t.Helper()
		b, err := pg.CreateWorkloadBinding(ctx, store.WorkloadBinding{PoolID: pid, Namespace: ns, Service: "bot", Sponsor: "sub-a", CreatedBy: "secop"})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	b := bind("team-a")

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
		_, _, err := pg.StartQueuedRun(ctx, run.ID, store.QueueStart{SPIFFEID: "spiffe://x/bound", ConfinementClass: types.CC2, Binding: &b.ID})
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("start = %v while the binding's delete was in flight, want it to wait for the delete", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if !errors.Is(err, store.ErrWorkloadBindingGone) {
			t.Fatalf("start after the delete committed = %v, want ErrWorkloadBindingGone", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the start did not answer once the delete committed")
	}
	if r := runState(t, pg, run.ID); r.SPIFFEID != "" {
		t.Fatalf("the refused start wrote %q", r.SPIFFEID)
	}
	standing := bind("team-b")
	if started, ok, err := pg.StartQueuedRun(ctx, run.ID, store.QueueStart{SPIFFEID: "spiffe://x/bound", ConfinementClass: types.CC2, Binding: &standing.ID}); err != nil || !ok ||
		started.SPIFFEID != "spiffe://x/bound" {
		t.Fatalf("start under a standing binding = %+v, %v, %v; want it written", started, ok, err)
	}
}
