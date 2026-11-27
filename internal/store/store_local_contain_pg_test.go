// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// latchRun records that a local operator contained runID on runnerID, as a STATE page would.
func latchRun(t *testing.T, pool *pgxpool.Pool, runID, runnerID uuid.UUID) {
	t.Helper()
	srExec(t, pool, `INSERT INTO run_local_containment (run_id, runner_id, contained_at, scope, operator_uid) VALUES ($1, $2, now(), 'run', 1000)`,
		runID, runnerID)
}

// A run held for recovery (its proxy pauses, an open window covers its lapsed token) is no longer held once a
// local operator contained it on its runner.
func TestPG_RecoveryHold_LocalContainedRunIsNotHeld(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	srExec(t, pool, `DELETE FROM outage_windows`)
	srExec(t, pool, `UPDATE cp_heartbeat SET beat_at = now()`)
	outageInsert(t, pool, store.OutageKindServer, nil, outageDBNow(t, pool).Add(-time.Hour), nil)
	run := srRun(t, pool, 70*time.Minute, "")
	twin := srRun(t, pool, 70*time.Minute, "")
	for _, id := range []uuid.UUID{run.ID, twin.ID} {
		if err := pg.SetRunProxyPauses(ctx, id, true); err != nil {
			t.Fatal(err)
		}
		if got, err := pg.RunRecoveryCheck(ctx, id, false); err != nil || !got.Held {
			t.Fatalf("before the latch: %+v, %v; want held", got, err)
		}
	}
	latchRun(t, pool, run.ID, uuid.New())
	if got, err := pg.RunRecoveryCheck(ctx, run.ID, false); err != nil || got.Held || !got.Covered {
		t.Fatalf("after the latch: %+v, %v; want covered and not held", got, err)
	}
	if got, err := pg.RunRecoveryCheck(ctx, twin.ID, false); err != nil || !got.Held {
		t.Fatalf("the unlatched twin: %+v, %v; want still held", got, err)
	}
}

// A local containment makes RunHeldContainedOnRunner true, so the lease refusal and recovery check 6 read it.
func TestPG_RunHeldContainedOnRunner_LocalContain(t *testing.T) {
	f := newActionFixture(t)
	run, twin := f.run(t), f.run(t)
	for _, id := range []uuid.UUID{run.ID, twin.ID} {
		if held, err := f.st.RunHeldContainedOnRunner(t.Context(), id); err != nil || held {
			t.Fatalf("before the latch: held=%v, %v", held, err)
		}
	}
	latchRun(t, f.pool, run.ID, f.rn.ID)
	if held, err := f.st.RunHeldContainedOnRunner(t.Context(), run.ID); err != nil || !held {
		t.Fatalf("after the latch: held=%v, %v; want held", held, err)
	}
	if held, err := f.st.RunHeldContainedOnRunner(t.Context(), twin.ID); err != nil || held {
		t.Fatalf("the unlatched twin: held=%v, %v; want not held", held, err)
	}
}

// RunLocallyContained reads the latch row and nothing else: a queued kill is not a local containment.
func TestPG_RunLocallyContained(t *testing.T) {
	f := newActionFixture(t)
	run, killed := f.run(t), f.run(t)
	f.mustQueue(t, f.action(killed, "kill", f.issue(t, killed)))
	for _, id := range []uuid.UUID{run.ID, killed.ID, uuid.New()} {
		if got, err := f.st.RunLocallyContained(t.Context(), id); err != nil || got {
			t.Fatalf("before the latch: %v, %v; want false", got, err)
		}
	}
	latchRun(t, f.pool, run.ID, f.rn.ID)
	if got, err := f.st.RunLocallyContained(t.Context(), run.ID); err != nil || !got {
		t.Fatalf("after the latch: %v, %v; want true", got, err)
	}
	if got, err := f.st.RunLocallyContained(t.Context(), killed.ID); err != nil || got {
		t.Fatalf("a killed run: %v, %v; want false", got, err)
	}
}
