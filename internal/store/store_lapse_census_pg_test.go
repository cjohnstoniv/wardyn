// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	censusLife   = time.Hour + 5*time.Minute
	censusWindow = 30 * time.Minute
)

// censusRun stores a run in state whose token was last renewed renewedAgo ago.
func censusRun(t *testing.T, pool *pgxpool.Pool, state types.RunState, renewedAgo time.Duration) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	r, err := store.NewPG(pool).CreateRun(ctx, newRun(state))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	censusExec(t, pool, `UPDATE agent_runs SET token_renewed_at = now() - $2::interval WHERE id = $1`, r.ID, renewedAgo.String())
	return r.ID
}

func censusExec(t *testing.T, pool *pgxpool.Pool, q string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func readCensus(t *testing.T, pool *pgxpool.Pool) store.LapseCensus {
	t.Helper()
	c, err := store.NewPG(pool).LapseCensus(context.Background(), censusLife, censusWindow)
	if err != nil {
		t.Fatalf("LapseCensus: %v", err)
	}
	return c
}

func TestPG_LapseCensus_CountsOnlyTheServersOwnRuns(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	rn, err := st.CreateRunner(ctx, newRunner("census-owner"))
	if err != nil {
		t.Fatal(err)
	}
	own := censusRun(t, pool, types.RunRunning, time.Minute)
	viaRunner := censusRun(t, pool, types.RunRunning, time.Minute)
	censusExec(t, pool, `UPDATE agent_runs SET runner_id = $2 WHERE id = $1`, viaRunner, rn.ID)
	local := censusRun(t, pool, types.RunRunning, time.Minute)
	censusExec(t, pool, `UPDATE agent_runs SET placement = 'local' WHERE id = $1`, local) // a deleted runner's laptop run: no runner_id

	if c := readCensus(t, pool); c.Running != 1 {
		t.Fatalf("Running = %d, want 1: only %s is the server's own (not %s with a runner, nor %s placed local)", c.Running, own, viaRunner, local)
	}
}

func TestPG_LapseCensus_CountsALapseLiveAtItsMoment(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	// Each run's token was last renewed 70 minutes ago, so its lapse moment was 5 minutes ago.
	renewed := 70 * time.Minute

	counted := censusRun(t, pool, types.RunRunning, renewed) // RUNNING and not lost: live at its moment
	lostAfter := censusRun(t, pool, types.RunRunning, renewed)
	censusExec(t, pool, `UPDATE agent_runs SET lost_at = now() - interval '1 minute', lost_reason = 'outage' WHERE id = $1`, lostAfter)
	endedAfter := censusRun(t, pool, types.RunCompleted, renewed)
	censusExec(t, pool, `UPDATE agent_runs SET ended_at = now() - interval '1 minute' WHERE id = $1`, endedAfter)

	endedBefore := censusRun(t, pool, types.RunCompleted, renewed)
	censusExec(t, pool, `UPDATE agent_runs SET ended_at = now() - interval '10 minutes' WHERE id = $1`, endedBefore)
	keptBefore := censusRun(t, pool, types.RunCompleted, renewed) // kept before its moment, ended after it
	censusExec(t, pool, `UPDATE agent_runs SET lost_at = now() - interval '10 minutes', lost_reason = 'reboot', ended_at = now() - interval '1 minute' WHERE id = $1`, keptBefore)
	tooOld := censusRun(t, pool, types.RunRunning, 120*time.Minute) // its moment, 55 minutes ago, is outside the 30 minute window

	c := readCensus(t, pool)
	if c.Lapsed != 3 {
		t.Errorf("Lapsed = %d, want 3 (%s, %s, %s; not %s, %s, %s)", c.Lapsed, counted, lostAfter, endedAfter, endedBefore, keptBefore, tooOld)
	}
	if c.Running != 2 {
		t.Errorf("Running = %d, want 2: the RUNNING runs that are not lost", c.Running)
	}
}

func TestPG_LapseCensus_NewestRenewAgeIsTheDatabasesClock(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	if c := readCensus(t, pool); c.Running != 0 || c.NewestRenewAge != 0 {
		t.Fatalf("an empty table gave %+v, want zero", c)
	}
	censusRun(t, pool, types.RunRunning, 10*time.Minute)
	censusRun(t, pool, types.RunRunning, 3*time.Minute)
	c := readCensus(t, pool)
	if c.Running != 2 || c.NewestRenewAge < 3*time.Minute || c.NewestRenewAge > 3*time.Minute+30*time.Second {
		t.Fatalf("census = %+v, want 2 running and a newest renewal about 3 minutes old", c)
	}
}
