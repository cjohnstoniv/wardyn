// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/lifecycle"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_MaxAgeStopsAnOldRunThroughTheRealAdapter: against Postgres, a RUNNING run created past
// WARDYN_RUN_MAX_AGE is stopped and torn down even though it was touched a moment ago and its
// auto-stop is off; a young run and a kept run (lost_at set) beside it are left alone; and the
// stop writes a run.max_age.expire audit row. Guarded by WARDYN_TEST_PG.
func TestPG_MaxAgeStopsAnOldRunThroughTheRealAdapter(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	pg := store.PG{Pool: pool}
	mk := func(createdAgo time.Duration) uuid.UUID {
		id := uuid.New()
		now := time.Now().UTC()
		if _, err := pg.CreateRun(ctx, types.AgentRun{
			ID: id, CreatedAt: now.Add(-createdAgo), UpdatedAt: now,
			CreatedBy: "op@example.com", Agent: "claude-code", Task: "max-age probe",
			ConfinementClass: types.CC2, State: types.RunRunning,
			SPIFFEID: "spiffe://wardyn.test/agent-run/" + id.String(), RunnerTarget: "docker",
			SandboxRef: "wardyn-agent-" + id.String(),
		}); err != nil {
			t.Fatalf("create run: %v", err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, id) })
		return id
	}
	old, young, kept := mk(3*time.Hour), mk(10*time.Minute), mk(3*time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at = now(), lost_reason = 'ended' WHERE id=$1`, kept); err != nil {
		t.Fatalf("mark the kept run: %v", err)
	}

	rn := &countingRunner{}
	rec := &fakeAuditRecorder{}
	reaper := lifecycle.New(lifecycleStore{pool: pool}, lifecycleStopper{pool: pool, runner: rn}, rec,
		lifecycle.Config{MaxAge: time.Hour})
	reaper.Tick(ctx)

	for id, want := range map[uuid.UUID]types.RunState{old: types.RunStopped, young: types.RunRunning, kept: types.RunRunning} {
		if got, err := pg.GetRun(ctx, id); err != nil || got.State != want {
			t.Errorf("run %s state = %v (err %v), want %s", id, got.State, err, want)
		}
	}
	if got, _ := pg.GetRun(ctx, old); got.EndedAt == nil {
		t.Error("the max-age stop left ended_at unset")
	}
	if rn.stops != 1 {
		t.Errorf("teardowns = %d, want 1 (only the old run)", rn.stops)
	}
	if rec.calls != 1 || rec.last.Action != "run.max_age.expire" || rec.last.RunID == nil || *rec.last.RunID != old {
		t.Errorf("audit = %d rows, last %+v; want one run.max_age.expire for %s", rec.calls, rec.last, old)
	}
}
