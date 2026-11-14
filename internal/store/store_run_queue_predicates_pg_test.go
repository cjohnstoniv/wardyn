// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Which waiting runs a pass judges and lets go first, and what a revive and a requeue count as waiting.

// An owner-wide wait passes over only the later runs its hold key names: the same owner's run queued
// under another key (another authority snapshot or unit) is still judged and promoted.
func TestPG_PromoteQueueHoldsOnlyTheRunsOfItsHoldKey(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	p := cappedPool(t, pg, 1)
	now := time.Now().UTC()
	var ids []uuid.UUID
	for i := range 3 {
		r, err := enqueue(t, pg, p, "bob", now.Add(time.Duration(i)*time.Second), 0)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	held, same, other := ids[0], ids[1], ids[2]
	keys := map[uuid.UUID]string{held: "bob/a", same: "bob/a", other: "bob/b"}
	asked := map[uuid.UUID]int{}
	verdict := func(_ context.Context, _ types.RunnerPool, e store.RunQueueEntry) (store.QueueVerdict, error) {
		asked[e.RunID]++
		if e.RunID == held {
			return store.QueueVerdict{Wait: string(runnerpool.WaitNoRunnerFree), Owner: true}, nil
		}
		return store.QueueVerdict{Admit: true}, nil
	}
	scan := pass(p)
	scan.HoldKey = func(e store.RunQueueEntry) string { return keys[e.RunID] }
	got, _, err := pg.PromoteRunQueue(ctx, scan, verdict)
	if err != nil || len(got) != 1 || got[0].Run.ID != other {
		t.Fatalf("promoted %v, %v; want bob's run under the other key", got, err)
	}
	if asked[same] != 0 {
		t.Errorf("the run under the held run's key was judged %d times; it waits with that run this pass", asked[same])
	}
}

// A run admitted with runners but no size for them (a launch queued before its unbound size was recorded)
// does not spend the pass's room: when the promotion refuses it at the size it joined with, the next run,
// which fits, is promoted in the same pass, and the refused run keeps its place.
func TestPG_PromoteQueueJudgesPastARunItCouldNotSize(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	p := cappedPool(t, pg, 1)
	const small, roomy = "l61-small", "l61-roomy"
	remoteRunner(t, pg, small, sizedLimits(3, 1))
	remoteRunner(t, pg, roomy, sizedLimits(3, 3))
	now := time.Now().UTC()
	legacy := poolRun(p, types.RunQueued, "alice")
	legacy.Reserved = oneRun()
	legacy.Reserved.DiskMiB *= 2 // more than the small runner takes a run
	head, err := pg.CreateQueuedRun(ctx, legacy, store.RunQueueEntry{QueuedAt: now, DeadlineAt: now.Add(time.Hour)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	next, err := enqueue(t, pg, p, "bob", now.Add(time.Second), 0)
	if err != nil {
		t.Fatal(err)
	}
	verdict := func(_ context.Context, _ types.RunnerPool, e store.RunQueueEntry) (store.QueueVerdict, error) {
		if e.RunID == head.ID {
			return store.QueueVerdict{Admit: true, Remotes: []string{small}}, nil
		}
		return store.QueueVerdict{Admit: true, Remotes: []string{roomy}, Needs: map[string]types.RunnerAmounts{roomy: oneRun()}}, nil
	}
	got, _, err := pg.PromoteRunQueue(ctx, pass(p), verdict)
	if err != nil || len(got) != 1 || got[0].Run.ID != next.ID {
		t.Fatalf("promoted %v, %v; want the run behind the one its runner could not take", got, err)
	}
	if r := runState(t, pg, head.ID); r.State != types.RunQueued {
		t.Errorf("the run its runner could not take is %s; it keeps its place", r.State)
	}
}

// A scheduled run whose start time is still ahead is not in line yet, so it does not refuse a revive into
// a pool with a free slot; once its time has come it does, as a run waiting from its create does.
func TestPG_ReviveIsRefusedOnlyByARunThatIsDue(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := cappedPool(t, pg, 2)
	kept, err := pg.CreateRun(ctx, poolRun(p, types.RunRunning, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	keep := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at=now(), lost_reason='reboot' WHERE id=$1`, kept.ID); err != nil {
			t.Fatal(err)
		}
	}
	booked, err := enqueue(t, pg, p, "bob", time.Now().UTC(), 0)
	if err != nil {
		t.Fatal(err)
	}
	startAt := func(at string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE run_queue SET start_at = `+at+` WHERE run_id = $1`, booked.ID); err != nil {
			t.Fatal(err)
		}
	}
	keep()
	startAt(`now() + interval '1 hour'`)
	if claimed, err := pg.MarkRunRevived(ctx, kept.ID, types.LostReboot, nil, 0, true); err != nil || !claimed {
		t.Fatalf("revive with only a run booked for later = %v, %v; want claimed", claimed, err)
	}
	keep()
	startAt(`now() - interval '1 minute'`)
	if _, err := pg.MarkRunRevived(ctx, kept.ID, types.LostReboot, nil, 0, true); !errors.Is(err, store.ErrRunnerPoolFull) {
		t.Fatalf("revive with a booked run now due = %v; want ErrRunnerPoolFull", err)
	}
}

// A promoted run goes back to its pool's queue only while the pool exists: once the pool is deleted the
// requeue is refused with ErrNotFound and leaves the run PENDING with no queue row (its start then ends
// it). A disabled pool takes it back, and a delete after a requeue ends it with the runs it found waiting.
func TestPG_RequeueIntoADeletedPoolIsRefused(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	promoted := func(t *testing.T) (types.RunnerPool, store.PromotedRun) {
		t.Helper()
		p := cappedPool(t, pg, 1)
		if _, err := enqueue(t, pg, p, "alice", time.Now().UTC(), 0); err != nil {
			t.Fatal(err)
		}
		got, _, err := pg.PromoteRunQueue(ctx, pass(p), admitAll)
		if err != nil || len(got) != 1 {
			t.Fatalf("promote: %v %v", got, err)
		}
		return p, got[0]
	}
	t.Run("deleted before the requeue", func(t *testing.T) {
		p, run := promoted(t)
		if _, err := pg.DeleteRunnerPool(ctx, p.ID); err != nil {
			t.Fatal(err)
		}
		moved, err := pg.RequeuePromotedRun(ctx, run.Entry, "queue: owner_sign_in")
		if moved || !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("requeue into a deleted pool = %v, %v; want ErrNotFound", moved, err)
		}
		if r := runState(t, pg, run.Run.ID); r.State != types.RunPending || queueRows(t, pool, run.Run.ID) != 0 {
			t.Errorf("the run is %s with %d queue rows; want it PENDING with none", r.State, queueRows(t, pool, run.Run.ID))
		}
	})
	t.Run("disabled", func(t *testing.T) {
		p, run := promoted(t)
		off := types.RunnerPoolDisabled
		if _, err := pg.UpdateRunnerPool(ctx, p.ID, p.Revision, nil, &off, nil, nil); err != nil {
			t.Fatal(err)
		}
		if moved, err := pg.RequeuePromotedRun(ctx, run.Entry, "queue: owner_sign_in"); err != nil || !moved {
			t.Fatalf("requeue into a disabled pool = %v, %v; want it back in the queue", moved, err)
		}
		if r := runState(t, pg, run.Run.ID); r.State != types.RunQueued || queueRows(t, pool, run.Run.ID) != 1 {
			t.Errorf("the run is %s with %d queue rows; want it QUEUED with one", r.State, queueRows(t, pool, run.Run.ID))
		}
	})
	t.Run("deleted after the requeue", func(t *testing.T) {
		p, run := promoted(t)
		if moved, err := pg.RequeuePromotedRun(ctx, run.Entry, "queue: owner_sign_in"); err != nil || !moved {
			t.Fatalf("requeue = %v, %v", moved, err)
		}
		if _, err := pg.DeleteRunnerPool(ctx, p.ID); err != nil {
			t.Fatal(err)
		}
		if r := runState(t, pg, run.Run.ID); r.State != types.RunKilled || r.StatusDetail != "queue: "+string(types.RunEndRunnerPoolRemoved) || queueRows(t, pool, run.Run.ID) != 0 {
			t.Errorf("the run is %s (%q) with %d queue rows; want it KILLED runner_pool_removed with none", r.State, r.StatusDetail, queueRows(t, pool, run.Run.ID))
		}
	})
	t.Run("waits for the pool row", func(t *testing.T) {
		p, run := promoted(t)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SELECT 1 FROM runner_pools WHERE id = $1 FOR NO KEY UPDATE`, p.ID); err != nil {
			t.Fatal(err)
		}
		short, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		if moved, err := pg.RequeuePromotedRun(short, run.Entry, "queue: owner_sign_in"); moved || err == nil {
			t.Fatalf("requeue while the pool row is held = %v, %v; want it to wait for the row", moved, err)
		}
		if r := runState(t, pg, run.Run.ID); r.State != types.RunPending || queueRows(t, pool, run.Run.ID) != 0 {
			t.Errorf("the run is %s with %d queue rows; want it PENDING with none", r.State, queueRows(t, pool, run.Run.ID))
		}
	})
}
