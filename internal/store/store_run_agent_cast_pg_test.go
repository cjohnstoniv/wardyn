// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_RunAgentCastObligation: a run owes its agent cast once it is set; a decline marks only a run
// that owes one, again while it owes it; clearing with a cast stored clears runner_declined too; and the sweep lists a run only
// once it is terminal and still owes it.
func TestPG_RunAgentCastObligation(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	run, err := pg.CreateRun(ctx, newRun(types.RunStarting))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := pg.DeclineRunAgentCast(ctx, run.ID); err != nil || ok {
		t.Fatalf("decline before any obligation = %v, %v; want nothing marked", ok, err)
	}
	if err := pg.SetRunAgentCastOwed(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	listed := func() bool {
		ids, _, err := pg.ListRunsAgentCastOwed(ctx, 0, nil, 100)
		if err != nil {
			t.Fatal(err)
		}
		return slices.Contains(ids, run.ID)
	}
	if listed() {
		t.Fatal("a STARTING run was listed for the sweep")
	}
	if ok, err := pg.UpdateRunStateIf(ctx, run.ID, types.RunStarting, types.RunCompleted); err != nil || !ok {
		t.Fatalf("complete the run: %v %v", ok, err)
	}
	if !listed() {
		t.Fatal("a terminal run that owes its cast was not listed")
	}
	if ok, err := pg.DeclineRunAgentCast(ctx, run.ID); err != nil || !ok {
		t.Fatalf("decline = %v, %v; want marked", ok, err)
	}
	if got, _ := pg.GetRun(ctx, run.ID); got.NotRecorded != types.NotRecordedRunnerDeclined {
		t.Fatalf("not_recorded = %q, want runner_declined", got.NotRecorded)
	}
	if ok, err := pg.DeclineRunAgentCast(ctx, run.ID); err != nil || !ok {
		t.Fatalf("a second decline = %v, %v; want the run, already marked, still owing its cast", ok, err)
	}
	if err := pg.ClearRunAgentCastOwed(ctx, run.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := pg.GetRun(ctx, run.ID); got.NotRecorded != "" || listed() {
		t.Fatalf("after a stored cast: not_recorded %q, listed %v; want recorded and owing nothing", got.NotRecorded, listed())
	}
	if err := pg.SetRunNotRecorded(ctx, run.ID, types.NotRecordedPolicyOff); err != nil {
		t.Fatal(err)
	}
	if err := pg.ClearRunAgentCastOwed(ctx, run.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := pg.GetRun(ctx, run.ID); got.NotRecorded != types.NotRecordedPolicyOff {
		t.Fatalf("a stored cast cleared %q; only runner_declined clears", got.NotRecorded)
	}
}

// More than a page of runs that keep owing their cast, as runs whose sweep fails on every pass do: a cursor
// walk reaches every one, oldest obligation first with the id breaking a tie, leaves out a run that is not
// terminal, and ends on a short page with a nil cursor, so the next walk starts again from the oldest.
func TestPG_ListRunsAgentCastOwedCursor(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	owe := func(id uuid.UUID, hours int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET agent_cast_owed_at = now() - make_interval(hours => $2),
			updated_at = now() - interval '1 hour' WHERE id = $1`, id, hours); err != nil {
			t.Fatal(err)
		}
	}
	var want []uuid.UUID
	for _, hours := range []int{10, 8, 8, 6, 4} {
		id := persistRun(t, ctx, pool, newRun(types.RunCompleted)).ID
		owe(id, hours)
		want = append(want, id)
	}
	// One statement sets the tie: now() is per statement, so two owe() calls never give an equal stamp.
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET agent_cast_owed_at = now() - interval '8 hours' WHERE id IN ($1, $2)`, want[1], want[2]); err != nil {
		t.Fatal(err)
	}
	if bytes.Compare(want[2][:], want[1][:]) < 0 { // the two 8-hour stamps: the id orders them
		want[1], want[2] = want[2], want[1]
	}
	owe(persistRun(t, ctx, pool, newRun(types.RunRunning)).ID, 20) // not terminal: never listed
	persistRun(t, ctx, pool, newRun(types.RunCompleted))           // owes nothing

	var walked []uuid.UUID
	var after *store.SweepCursor
	for i := 0; i < 5; i++ {
		page, next, err := pg.ListRunsAgentCastOwed(ctx, 0, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		walked = append(walked, page...)
		if next == nil {
			break
		}
		after = next
	}
	if !slices.Equal(walked, want) {
		t.Fatalf("a cursor walk of pages of 2 = %v, want %v", walked, want)
	}
	if first, _, err := pg.ListRunsAgentCastOwed(ctx, 0, nil, 2); err != nil || len(first) != 2 || first[0] != want[0] {
		t.Fatalf("a walk from no cursor = %v, %v; want it to start again at the oldest", first, err)
	}
}
