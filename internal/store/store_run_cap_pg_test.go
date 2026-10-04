// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_CreateRunUnderCap_TwoInstancesAdmitExactlyTheCap races creators over two
// stores that share one database but not a connection pool, as two replicas do.
// Count-then-insert without the advisory lock admits more than the cap here.
func TestPG_CreateRunUnderCap_TwoInstancesAdmitExactlyTheCap(t *testing.T) {
	poolA := runsPGPoolIsolated(t)
	poolB, err := db.Connect(context.Background(), poolA.Config().ConnString())
	if err != nil {
		t.Fatalf("second instance connect: %v", err)
	}
	t.Cleanup(poolB.Close)
	instances := []store.PG{store.NewPG(poolA), store.NewPG(poolB)}

	const limit, creators = 3, 24
	var admitted, refused atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < creators; i++ {
		wg.Add(1)
		go func(pg store.PG) {
			defer wg.Done()
			_, err := pg.CreateRunUnderCap(context.Background(), newRun(types.RunPending), limit)
			switch {
			case err == nil:
				admitted.Add(1)
			case errors.Is(err, store.ErrRunCapReached):
				refused.Add(1)
			default:
				t.Errorf("create: %v", err)
			}
		}(instances[i%2])
	}
	wg.Wait()
	if admitted.Load() != limit || refused.Load() != creators-limit {
		t.Fatalf("admitted %d, refused %d; want exactly %d admitted and %d refused", admitted.Load(), refused.Load(), limit, creators-limit)
	}
}

// TestPG_CreateRunUnderCap_CountsRowsAndFreesOnTerminal pins that the cap counts
// non-terminal rows (a finished run frees a slot) and that 0 is unlimited.
func TestPG_CreateRunUnderCap_CountsRowsAndFreesOnTerminal(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	first, err := pg.CreateRunUnderCap(ctx, newRun(types.RunPending), 1)
	if err != nil {
		t.Fatalf("first create under cap 1: %v", err)
	}
	if _, err := pg.CreateRunUnderCap(ctx, newRun(types.RunPending), 1); !errors.Is(err, store.ErrRunCapReached) {
		t.Fatalf("second create under cap 1 = %v, want ErrRunCapReached", err)
	}
	if _, err := pg.CreateRunUnderCap(ctx, newRun(types.RunPending), 0); err != nil {
		t.Fatalf("cap 0 must be unlimited: %v", err)
	}
	if ok, err := pg.UpdateRunStateIf(ctx, first.ID, types.RunPending, types.RunFailed); err != nil || !ok {
		t.Fatalf("fail the first run: ok=%v err=%v", ok, err)
	}
	if n, err := pg.CountNonTerminalRuns(ctx); err != nil || n != 1 {
		t.Fatalf("CountNonTerminalRuns = %d, %v; want 1 (the failed run is terminal)", n, err)
	}
	// One PENDING row is still held (the cap-0 insert), so cap 1 refuses and cap 2 admits.
	if _, err := pg.CreateRunUnderCap(ctx, newRun(types.RunPending), 1); !errors.Is(err, store.ErrRunCapReached) {
		t.Fatalf("create at cap 1 with one live row = %v, want ErrRunCapReached", err)
	}
	if _, err := pg.CreateRunUnderCap(ctx, newRun(types.RunPending), 2); err != nil {
		t.Fatalf("create at cap 2 with one live row (the other is terminal): %v", err)
	}
}

// TestPG_CreateRunUnderCap_KeptRunHoldsNoSlot pins that an ended run kept for its
// grace (RUNNING with lost_at set, no running agent) is not counted: it holds no
// sandbox, so it must not hold a slot for the whole grace. A run kept after an
// outage still runs its agent, so it keeps its slot.
func TestPG_CreateRunUnderCap_KeptRunHoldsNoSlot(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	kept, err := pg.CreateRunUnderCap(ctx, newRun(types.RunRunning), 1)
	if err != nil {
		t.Fatalf("create the run: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at = now(), lost_reason = $2 WHERE id = $1`, kept.ID, string(types.LostEnded)); err != nil {
		t.Fatalf("keep the run: %v", err)
	}
	if n, err := pg.CountNonTerminalRuns(ctx); err != nil || n != 0 {
		t.Fatalf("CountNonTerminalRuns = %d, %v; want 0 (the kept run holds no sandbox)", n, err)
	}
	if _, err := pg.CreateRunUnderCap(ctx, newRun(types.RunPending), 1); err != nil {
		t.Fatalf("create at cap 1 with only a kept run: %v", err)
	}
}

// TestPG_CreateRunUnderCap_OutageKeptRunHoldsSlot pins that a run kept after a
// control-plane outage still runs its agent, so it still counts against the cap.
func TestPG_CreateRunUnderCap_OutageKeptRunHoldsSlot(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	kept, err := pg.CreateRunUnderCap(ctx, newRun(types.RunRunning), 1)
	if err != nil {
		t.Fatalf("create the run: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at = now(), lost_reason = $2 WHERE id = $1`, kept.ID, string(types.LostOutage)); err != nil {
		t.Fatalf("keep the run: %v", err)
	}
	if n, err := pg.CountNonTerminalRuns(ctx); err != nil || n != 1 {
		t.Fatalf("CountNonTerminalRuns = %d, %v; want 1 (the outage-kept run's agent still runs)", n, err)
	}
	if _, err := pg.CreateRunUnderCap(ctx, newRun(types.RunPending), 1); !errors.Is(err, store.ErrRunCapReached) {
		t.Fatalf("create at cap 1 with an outage-kept run = %v; want ErrRunCapReached", err)
	}
	// Past its end the lease sweep stops its agent too, so it holds no slot.
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET ends_at = now() - interval '1 minute' WHERE id = $1`, kept.ID); err != nil {
		t.Fatalf("pass the run's end: %v", err)
	}
	if _, err := pg.CreateRunUnderCap(ctx, newRun(types.RunPending), 1); err != nil {
		t.Fatalf("create at cap 1 with an outage-kept run past its end: %v", err)
	}
}

// TestPG_MarkRunRevived_KeptRunTakesASlotUnderTheCap pins that reviving a run kept
// after a reboot or its end, which holds no slot, takes one like a create does: at
// cap 1, with a replacement admitted in its slot, the revive is refused with
// ErrRunCapReached and the run stays kept; once the slot is free, it is claimed.
func TestPG_MarkRunRevived_KeptRunTakesASlotUnderTheCap(t *testing.T) {
	for _, reason := range []types.LostReason{types.LostReboot, types.LostEnded} {
		t.Run(string(reason), func(t *testing.T) {
			ctx := context.Background()
			pool := runsPGPoolIsolated(t)
			pg := store.NewPG(pool)
			kept, err := pg.CreateRunUnderCap(ctx, newRun(types.RunRunning), 1)
			if err != nil {
				t.Fatalf("create the run: %v", err)
			}
			var lostAt time.Time
			if err := pool.QueryRow(ctx, `UPDATE agent_runs SET lost_at = now(), lost_reason = $2 WHERE id = $1 RETURNING lost_at`, kept.ID, string(reason)).Scan(&lostAt); err != nil {
				t.Fatalf("keep the run: %v", err)
			}
			var ended *store.EndedKept
			if reason == types.LostEnded {
				ended = keptAt(lostAt, time.Now())
			}
			replacement, err := pg.CreateRunUnderCap(ctx, newRun(types.RunRunning), 1)
			if err != nil {
				t.Fatalf("create the replacement in the kept run's slot: %v", err)
			}

			if ok, err := pg.MarkRunRevived(ctx, kept.ID, reason, ended, 1); ok || !errors.Is(err, store.ErrRunCapReached) {
				t.Fatalf("revive at the cap = %v, %v; want false, ErrRunCapReached", ok, err)
			}
			if n, err := pg.CountNonTerminalRuns(ctx); err != nil || n != 1 {
				t.Fatalf("CountNonTerminalRuns = %d, %v; want 1 (the refused revive took no slot)", n, err)
			}
			if got, err := pg.GetRun(ctx, kept.ID); err != nil || got.LostAt == nil || got.LostReason != reason {
				t.Fatalf("refused run: lost %v %q, err %v; want still kept (%s)", got.LostAt, got.LostReason, err, reason)
			}

			if ok, err := pg.UpdateRunStateIf(ctx, replacement.ID, types.RunRunning, types.RunFailed); err != nil || !ok {
				t.Fatalf("end the replacement: ok=%v err=%v", ok, err)
			}
			if ok, err := pg.MarkRunRevived(ctx, kept.ID, reason, ended, 1); err != nil || !ok {
				t.Fatalf("revive under the cap = %v, %v; want true", ok, err)
			}
			if n, err := pg.CountNonTerminalRuns(ctx); err != nil || n != 1 {
				t.Fatalf("CountNonTerminalRuns = %d, %v; want 1 (the revived run)", n, err)
			}
			if got, err := pg.GetRun(ctx, kept.ID); err != nil || got.LostAt != nil {
				t.Fatalf("revived run: lost %v, err %v; want live", got.LostAt, err)
			}
		})
	}
}

// TestPG_MarkRunRevived_CountedRunTakesNoExtraSlot pins that restarting a run the
// cap already counts (live, or kept after an outage with its agent still running)
// is never refused at the cap and adds nothing to the count.
func TestPG_MarkRunRevived_CountedRunTakesNoExtraSlot(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	run, err := pg.CreateRunUnderCap(ctx, newRun(types.RunRunning), 1)
	if err != nil {
		t.Fatalf("create the run: %v", err)
	}
	if ok, err := pg.MarkRunRevived(ctx, run.ID, "", nil, 1); err != nil || !ok {
		t.Fatalf("live restart at the cap = %v, %v; want true", ok, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at = now(), lost_reason = $2 WHERE id = $1`, run.ID, string(types.LostOutage)); err != nil {
		t.Fatalf("keep the run after an outage: %v", err)
	}
	if ok, err := pg.MarkRunRevived(ctx, run.ID, types.LostOutage, nil, 1); err != nil || !ok {
		t.Fatalf("outage revive at the cap = %v, %v; want true", ok, err)
	}
	if n, err := pg.CountNonTerminalRuns(ctx); err != nil || n != 1 {
		t.Fatalf("CountNonTerminalRuns = %d, %v; want 1 (the one run, counted once)", n, err)
	}
}
