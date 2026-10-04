// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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

			if ok, err := pg.MarkRunRevived(ctx, kept.ID, reason, ended, 1, true); ok || !errors.Is(err, store.ErrRunCapReached) {
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
			if ok, err := pg.MarkRunRevived(ctx, kept.ID, reason, ended, 1, true); err != nil || !ok {
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
	if ok, err := pg.MarkRunRevived(ctx, run.ID, "", nil, 1, false); err != nil || !ok {
		t.Fatalf("live restart at the cap = %v, %v; want true", ok, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at = now(), lost_reason = $2 WHERE id = $1`, run.ID, string(types.LostOutage)); err != nil {
		t.Fatalf("keep the run after an outage: %v", err)
	}
	if ok, err := pg.MarkRunRevived(ctx, run.ID, types.LostOutage, nil, 1, false); err != nil || !ok {
		t.Fatalf("outage revive at the cap = %v, %v; want true", ok, err)
	}
	if n, err := pg.CountNonTerminalRuns(ctx); err != nil || n != 1 {
		t.Fatalf("CountNonTerminalRuns = %d, %v; want 1 (the one run, counted once)", n, err)
	}
}

// TestPG_MarkRunRevived_ExtendedOutageRunTakesASlot pins that a revive starting an agent takes a
// slot even when the row already reads as counted: an outage-kept run past its end (its agent
// stopped by the lease sweep) frees its slot, a replacement takes it, and the owner then moves
// the end later, which makes the row count again though its agent is still stopped. Reviving it
// at cap 1 is refused and leaves it kept; once the replacement ends, the same revive is claimed.
func TestPG_MarkRunRevived_ExtendedOutageRunTakesASlot(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	a, err := pg.CreateRunUnderCap(ctx, newRun(types.RunRunning), 1)
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at = now(), lost_reason = $2, ends_at = now() - interval '1 minute' WHERE id = $1`, a.ID, string(types.LostOutage)); err != nil {
		t.Fatalf("keep A after an outage, past its end: %v", err)
	}
	if n, err := pg.CountNonTerminalRuns(ctx); err != nil || n != 0 {
		t.Fatalf("CountNonTerminalRuns with A past its end = %d, %v; want 0", n, err)
	}
	b, err := pg.CreateRunUnderCap(ctx, newRun(types.RunRunning), 1)
	if err != nil {
		t.Fatalf("create B in A's freed slot: %v", err)
	}
	cur, err := pg.GetRun(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(48 * time.Hour)
	if ok, err := pg.SetRunEndAndWait(ctx, a.ID, cur.RunLimits, cur.EndsAt, cur.WaitBudgetSec, &later, cur.WaitBudgetSec, nil); err != nil || !ok {
		t.Fatalf("extend A = %v, %v; want applied", ok, err)
	}
	if n, err := pg.CountNonTerminalRuns(ctx); err != nil || n != 2 {
		t.Fatalf("CountNonTerminalRuns after extending A = %d, %v; want 2 (A reads as counted again)", n, err)
	}

	if ok, err := pg.MarkRunRevived(ctx, a.ID, types.LostOutage, nil, 1, true); ok || !errors.Is(err, store.ErrRunCapReached) {
		t.Fatalf("revive starting A's agent at the cap = %v, %v; want false, ErrRunCapReached", ok, err)
	}
	if got, err := pg.GetRun(ctx, a.ID); err != nil || got.LostAt == nil || got.LostReason != types.LostOutage {
		t.Fatalf("refused A: lost %v %q, err %v; want still kept (outage)", got.LostAt, got.LostReason, err)
	}

	if ok, err := pg.UpdateRunStateIf(ctx, b.ID, types.RunRunning, types.RunFailed); err != nil || !ok {
		t.Fatalf("end B: ok=%v err=%v", ok, err)
	}
	if ok, err := pg.MarkRunRevived(ctx, a.ID, types.LostOutage, nil, 1, true); err != nil || !ok {
		t.Fatalf("revive starting A's agent under the cap = %v, %v; want true", ok, err)
	}
	if n, err := pg.CountNonTerminalRuns(ctx); err != nil || n != 1 {
		t.Fatalf("CountNonTerminalRuns = %d, %v; want 1 (A alone)", n, err)
	}
}

// afterCapCount is a pgx tracer that runs fire, once, just before the statement that follows
// the deployment cap's count on its connection: a deterministic point inside a revive's claim
// transaction, after it has counted the runs at the cap and before it decides or claims.
type afterCapCount struct {
	fire        func()
	armed, done atomic.Bool
}

func (a *afterCapCount) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if a.armed.Load() && a.done.CompareAndSwap(false, true) {
		a.fire()
	}
	if strings.Contains(d.SQL, "SELECT count(*) FROM agent_runs") {
		a.armed.Store(true)
	}
	return ctx
}

func (a *afterCapCount) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestPG_MarkRunRevived_ConcurrentExtendCannotSplitTheCapCount pins the admission of a revive
// that starts an agent against an extension of the SAME run landing inside its claim. At cap 1,
// with B live and A an outage-kept run whose end has passed (its agent stopped, holding no slot),
// the owner moves A's end later right after the claim has counted the runs at the cap. The
// claim must still be refused: A's agent would start beside B. Admission counts the OTHER runs
// in one statement, so a change to A's own row cannot be read half before and half after.
func TestPG_MarkRunRevived_ConcurrentExtendCannotSplitTheCapCount(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	a, err := pg.CreateRunUnderCap(ctx, newRun(types.RunRunning), 1)
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at = now(), lost_reason = $2, ends_at = now() - interval '1 minute' WHERE id = $1`, a.ID, string(types.LostOutage)); err != nil {
		t.Fatalf("keep A after an outage, its end passed: %v", err)
	}
	b, err := pg.CreateRunUnderCap(ctx, newRun(types.RunRunning), 1)
	if err != nil {
		t.Fatalf("create B in A's freed slot: %v", err)
	}

	cur, err := pg.GetRun(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(48 * time.Hour)
	var extendErr error
	extended := false
	hook := &afterCapCount{fire: func() {
		extended, extendErr = pg.SetRunEndAndWait(ctx, a.ID, cur.RunLimits, cur.EndsAt, cur.WaitBudgetSec, &later, cur.WaitBudgetSec, nil)
	}}
	cfg, err := pgxpool.ParseConfig(pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = hook
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)

	ok, err := store.NewPG(traced).MarkRunRevived(ctx, a.ID, types.LostOutage, nil, 1, true)
	if !hook.done.Load() || extendErr != nil || !extended {
		t.Fatalf("the extension inside the claim did not land: fired=%v applied=%v err=%v", hook.done.Load(), extended, extendErr)
	}
	if ok || !errors.Is(err, store.ErrRunCapReached) {
		t.Fatalf("revive starting A's agent at the cap, A extended mid-claim = %v, %v; want false, ErrRunCapReached", ok, err)
	}
	if got, err := pg.GetRun(ctx, a.ID); err != nil || got.LostAt == nil || got.LostReason != types.LostOutage {
		t.Fatalf("refused A: lost %v %q, err %v; want still kept (outage), its agent not started", got.LostAt, got.LostReason, err)
	}
	if got, err := pg.GetRun(ctx, b.ID); err != nil || got.State != types.RunRunning || got.LostAt != nil {
		t.Fatalf("B: state %s lost %v, err %v; want the one live run", got.State, got.LostAt, err)
	}
}
