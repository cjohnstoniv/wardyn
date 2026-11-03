// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"slices"
	"testing"

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
		ids, err := pg.ListRunsAgentCastOwed(ctx, 0, 100)
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
