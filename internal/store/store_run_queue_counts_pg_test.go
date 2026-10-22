// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunnerPoolSlotCounts counts slot-holders and waiting runs per pool in one read; a waiting run is
// never a slot, a finished run is neither, and an idle pool answers zeros.
func TestPG_RunnerPoolSlotCounts(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	busy, idle := queuePool(t, pg), queuePool(t, pg)
	for _, st := range []types.RunState{types.RunRunning, types.RunStarting, types.RunPending, types.RunCompleted} {
		if _, err := pg.CreateRun(ctx, poolRun(busy, st, "alice")); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	for i := range 3 {
		if _, err := enqueue(t, pg, busy, "bob", now.Add(time.Duration(i)*time.Second), 0); err != nil {
			t.Fatal(err)
		}
	}

	got, err := pg.RunnerPoolSlotCounts(ctx, []uuid.UUID{busy.ID, idle.ID})
	if err != nil {
		t.Fatal(err)
	}
	if want := (store.PoolCounts{Used: 3, Waiting: 3}); got[busy.ID] != want {
		t.Errorf("busy pool counts = %+v, want %+v (three slot-holders; the three waiting runs and the finished one are not slots)", got[busy.ID], want)
	}
	if c, ok := got[idle.ID]; !ok || c != (store.PoolCounts{}) {
		t.Errorf("idle pool counts = %+v present=%v, want zeros", c, ok)
	}
	if none, err := pg.RunnerPoolSlotCounts(ctx, nil); err != nil || len(none) != 0 {
		t.Errorf("no pools asked = %v, %v", none, err)
	}
}

// A run booked for later is not waiting yet: RunnerPoolSlotCounts leaves it out until its start time,
// so a pool with free slots is not said to make a launch wait for runs booked days ahead.
func TestPG_RunnerPoolSlotCountsLeavesOutRunsBookedForLater(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := queuePool(t, pg)
	now := time.Now().UTC()
	var ids []uuid.UUID
	for i := range 3 {
		r, err := enqueue(t, pg, p, "bob", now.Add(time.Duration(i)*time.Second), 0)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	for _, set := range []struct {
		id    uuid.UUID
		start string
	}{{ids[1], "now() + interval '2 days'"}, {ids[2], "now() - interval '1 second'"}} {
		if _, err := pool.Exec(ctx, `UPDATE run_queue SET start_at = `+set.start+` WHERE run_id = $1`, set.id); err != nil {
			t.Fatal(err)
		}
	}
	got, err := pg.RunnerPoolSlotCounts(ctx, []uuid.UUID{p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if want := (store.PoolCounts{Waiting: 2}); got[p.ID] != want {
		t.Errorf("pool counts = %+v, want %+v (the run booked two days ahead is not waiting yet; the due one is)", got[p.ID], want)
	}
}

// RunQueueFacts ranks each waiting run in its own pool by (queued_at, run_id), in one read, and leaves
// a run that is not waiting out.
func TestPG_RunQueueFacts(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	a, b := queuePool(t, pg), queuePool(t, pg)
	now := time.Now().UTC()
	a0, _ := enqueue(t, pg, a, "alice", now, 0)
	b0, _ := enqueue(t, pg, b, "alice", now.Add(time.Second), 0)
	a1, _ := enqueue(t, pg, a, "bob", now.Add(2*time.Second), 0)
	a2, _ := enqueue(t, pg, a, "bob", now.Add(3*time.Second), 0)
	running, err := pg.CreateRun(ctx, poolRun(a, types.RunRunning, "alice"))
	if err != nil {
		t.Fatal(err)
	}

	got, err := pg.RunQueueFacts(ctx, []uuid.UUID{a2.ID, a1.ID, b0.ID, running.ID})
	if err != nil {
		t.Fatal(err)
	}
	for id, wantAhead := range map[uuid.UUID]int{a1.ID: 1, a2.ID: 2, b0.ID: 0} {
		f, ok := got[id]
		if !ok || f.Ahead != wantAhead {
			t.Errorf("run %s = %+v present=%v, want %d ahead", id, f, ok, wantAhead)
		}
	}
	if f := got[a2.ID]; f.PoolID != a.ID || !f.DeadlineAt.After(f.QueuedAt) {
		t.Errorf("a2 = %+v, want pool %s and a deadline after it joined", f, a.ID)
	}
	if _, ok := got[running.ID]; ok || len(got) != 3 {
		t.Errorf("facts = %v, want only the three waiting runs asked for", got)
	}
	if _, ok := got[a0.ID]; ok {
		t.Errorf("a0 was not asked for but is present")
	}
}
