// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// #1197 L1a: migration 0091's ended_at column — the three state writers that
// stamp it, and the historical backfill. Guarded by WARDYN_TEST_PG, same as
// every other store_*_pg_test.go file.
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_UpdateRunStateIf_StampsEndedAtOnTerminalOnly is the F1197-L1a pin:
// UpdateRunStateIf sets ended_at on a transition INTO a terminal state, and
// leaves it nil on a transition between two non-terminal ones. Proven red by
// reverting the CASE WHEN $n THEN now() clause to a bare no-op (equivalent to
// dropping the ended_at write entirely) — see the lane's revert-proof note.
func TestPG_UpdateRunStateIf_StampsEndedAtOnTerminalOnly(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	t.Run("terminal_transition_stamps_it", func(t *testing.T) {
		r := persistRun(t, ctx, pool, newRun(types.RunRunning))
		ok, err := pg.UpdateRunStateIf(ctx, r.ID, types.RunRunning, types.RunCompleted)
		if err != nil || !ok {
			t.Fatalf("UpdateRunStateIf = %v, %v; want applied", ok, err)
		}
		got, err := pg.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if got.EndedAt == nil {
			t.Fatal("ended_at is nil after a RUNNING -> COMPLETED transition")
		}
		if time.Since(*got.EndedAt) > time.Minute {
			t.Errorf("ended_at = %v, want close to now", got.EndedAt)
		}
	})

	t.Run("non_terminal_transition_leaves_it_nil", func(t *testing.T) {
		r := persistRun(t, ctx, pool, newRun(types.RunPending))
		ok, err := pg.UpdateRunStateIf(ctx, r.ID, types.RunPending, types.RunStarting)
		if err != nil || !ok {
			t.Fatalf("UpdateRunStateIf = %v, %v; want applied", ok, err)
		}
		got, err := pg.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if got.EndedAt != nil {
			t.Errorf("ended_at = %v after a PENDING -> STARTING (non-terminal) transition, want nil", got.EndedAt)
		}
	})
}

// TestPG_UpdateRunStateIfIdle_StampsEndedAt is the idle-reaper's writer: the
// same terminal-only stamp, through the idleness-guarded CAS.
func TestPG_UpdateRunStateIfIdle_StampsEndedAt(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	r := persistRun(t, ctx, pool, newRun(types.RunRunning))
	notAfter := time.Now().UTC().Add(time.Hour) // updated_at (just set by persistRun) is well before this
	ok, err := pg.UpdateRunStateIfIdle(ctx, r.ID, types.RunRunning, types.RunStopped, notAfter)
	if err != nil || !ok {
		t.Fatalf("UpdateRunStateIfIdle = %v, %v; want applied", ok, err)
	}
	got, err := pg.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.EndedAt == nil {
		t.Fatal("ended_at is nil after an idle RUNNING -> STOPPED transition")
	}
}

// TestPG_StopKeptRunIf_StampsEndedAt is the ended-run grace's writer: a kept
// run (lost_at set, RUNNING) that stops terminal gets ended_at, same as any
// other terminal writer.
func TestPG_StopKeptRunIf_StampsEndedAt(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	lostAt := time.Now().UTC().Add(-time.Hour)
	endsAt := time.Now().UTC().Add(-time.Minute)
	r := keepRun(t, ctx, pool, &lostAt, types.LostOutage, &endsAt)
	ok, err := pg.StopKeptRunIf(ctx, r.ID, types.RunStopped, &lostAt, types.LostOutage, &endsAt)
	if err != nil || !ok {
		t.Fatalf("StopKeptRunIf = %v, %v; want applied", ok, err)
	}
	got, err := pg.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.EndedAt == nil {
		t.Fatal("ended_at is nil after StopKeptRunIf's RUNNING -> STOPPED transition")
	}
}

// TestPG_Migration0091_BackfillsEndedAt replays 0091's backfill UPDATE (the
// exact statement the migration itself runs, verbatim) against a row this
// test puts into the PRE-0091 shape it describes — ended_at NULL, a terminal
// state, a real updated_at — and asserts the same outcome the migration's own
// one-time run already gave every such row in this pool: ended_at =
// updated_at. Re-running the live migration file itself is not possible here
// (runsPGPool already applied every migration, including 0091, before this
// test starts — db.Migrate is all-or-nothing), so this proves the backfill
// STATEMENT is correct rather than proving migration ORDERING; ordering is
// covered by the plain fact that runsPGPool's own Migrate call succeeds.
func TestPG_Migration0091_BackfillsEndedAt(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	r := persistRun(t, ctx, pool, newRun(types.RunCompleted))
	backdated := time.Now().UTC().Add(-72 * time.Hour)
	if _, err := pool.Exec(ctx,
		`UPDATE agent_runs SET ended_at = NULL, updated_at = $2 WHERE id = $1`, r.ID, backdated); err != nil {
		t.Fatalf("reset to pre-0091 shape: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE agent_runs SET ended_at = updated_at
		 WHERE ended_at IS NULL AND state IN ('COMPLETED', 'FAILED', 'KILLED', 'STOPPED', 'ARCHIVED')`); err != nil {
		t.Fatalf("replay 0091 backfill: %v", err)
	}

	got, err := store.NewPG(pool).GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	// timestamptz round-trips at microsecond precision (Go's time.Time carries
	// nanoseconds), so this compares the instant within a millisecond rather
	// than with Equal.
	if got.EndedAt == nil || got.EndedAt.Sub(backdated).Abs() > time.Millisecond {
		t.Errorf("ended_at = %v, want backfilled updated_at %v", got.EndedAt, backdated)
	}
}
