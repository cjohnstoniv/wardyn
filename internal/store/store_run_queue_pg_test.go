// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func queuePool(t *testing.T, pg store.PG) types.RunnerPool {
	t.Helper()
	p, err := pg.CreateRunnerPool(context.Background(), types.RunnerPool{ID: uuid.New(), Name: "Queue " + uuid.NewString()[:8], Scope: types.RunnerPoolOrganization}, "test")
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	return p
}

// poolRun is a run bound to p in state, owned by owner. A QUEUED run has no identity yet.
func poolRun(p types.RunnerPool, state types.RunState, owner string) types.AgentRun {
	r := newRun(state)
	r.CreatedBy = owner
	r.RunnerPoolID, r.RunnerPoolRevision, r.RunnerPoolSelection = &p.ID, p.Revision, types.RunnerPoolSelectedExplicit
	if state == types.RunQueued {
		r.SPIFFEID = ""
	}
	return r
}

func enqueue(t *testing.T, pg store.PG, p types.RunnerPool, owner string, at time.Time, maxPerOwner int) (types.AgentRun, error) {
	t.Helper()
	return pg.CreateQueuedRun(context.Background(), poolRun(p, types.RunQueued, owner),
		store.RunQueueEntry{QueuedAt: at, DeadlineAt: at.Add(time.Hour)}, maxPerOwner)
}

func queueRows(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM run_queue WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A waiting run holds no slot: the pool count, the deployment count and the fleet view leave it out,
// and the person's count and wardyn_runs_active's states include it.
func TestPG_QueuedRunHoldsNoSlot(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := queuePool(t, pg)
	if _, err := pg.CreateRun(ctx, poolRun(p, types.RunRunning, "alice")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := range 2 {
		if _, err := enqueue(t, pg, p, "alice", now.Add(time.Duration(i)*time.Second), 0); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	if n, err := pg.CountRunnerPoolSlotHolders(ctx, p.ID); err != nil || n != 1 {
		t.Errorf("pool slot holders = %d, %v; want 1 (the running run, not the two waiting)", n, err)
	}
	if n, err := pg.CountNonTerminalRuns(ctx, uuid.Nil); err != nil || n != 1 {
		t.Errorf("deployment slot holders = %d, %v; want 1", n, err)
	}
	// The deployment cap of 2 admits a second slot-holder past two waiting runs.
	if _, err := pg.CreateRunUnderCap(ctx, newRun(types.RunPending), 2); err != nil {
		t.Errorf("a create under cap 2 with one slot-holder and two waiting runs: %v", err)
	}
	if n, err := pg.CountActiveRunsBy(ctx, "alice"); err != nil || n != 3 {
		t.Errorf("alice's active runs = %d, %v; want 3 (the waiting runs count against the person)", n, err)
	}
	capacity, err := pg.RunCapacity(ctx, store.RunCapacityOpts{Now: time.Now(), CurrentKind: "docker"})
	if err != nil {
		t.Fatal(err)
	}
	if capacity.States[string(types.RunQueued)] != 2 {
		t.Errorf("States = %v, want the two QUEUED runs reported", capacity.States)
	}
	if capacity.Totals.Holding != 1 {
		t.Errorf("fleet Holding = %d, want 1 (the running run; a PENDING or QUEUED run holds no capacity yet)", capacity.Totals.Holding)
	}
	if len(capacity.ByOwner) != 1 || capacity.ByOwner[0].Holding != 1 {
		t.Errorf("ByOwner = %+v, want alice holding the one running run, not her two waiting ones", capacity.ByOwner)
	}
}

// Every exit from QUEUED removes the run_queue row in the same statement as the state change, and a
// second exit finds nothing to move. Kill, timeout, suspension and pool removal land the run terminal;
// promotion lands it PENDING and stamps admitted_at.
func TestPG_EveryExitFromQueuedDeletesTheRow(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := queuePool(t, pg)
	now := time.Now().UTC()
	admitted := now.Add(time.Minute).Truncate(time.Microsecond)

	for _, tc := range []struct {
		name string
		exit func(r types.AgentRun) (bool, error)
		want types.RunState
	}{
		{"kill", func(r types.AgentRun) (bool, error) {
			return pg.UpdateRunStateIf(ctx, r.ID, types.RunQueued, types.RunKilled)
		}, types.RunKilled},
		{"timeout", func(r types.AgentRun) (bool, error) {
			return pg.UpdateRunStateIf(ctx, r.ID, types.RunQueued, types.RunFailed)
		}, types.RunFailed},
		{"suspension", func(r types.AgentRun) (bool, error) {
			live, err := pg.ListNonTerminalRunsBy(ctx, r.CreatedBy)
			if err != nil || len(live) != 1 || live[0].State != types.RunQueued {
				return false, fmt.Errorf("the suspension sweep reads %v, %v; want the one waiting run", live, err)
			}
			return pg.UpdateRunStateIf(ctx, live[0].ID, live[0].State, types.RunKilled)
		}, types.RunKilled},
		{"pool removal", func(r types.AgentRun) (bool, error) { return pg.LeaveRunQueue(ctx, r.ID, types.RunKilled, nil) }, types.RunKilled},
		{"promotion", func(r types.AgentRun) (bool, error) { return pg.LeaveRunQueue(ctx, r.ID, types.RunPending, &admitted) }, types.RunPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := enqueue(t, pg, p, "owner-"+tc.name, now, 0)
			if err != nil {
				t.Fatal(err)
			}
			if queueRows(t, pool, r.ID) != 1 {
				t.Fatal("an enqueued run must have exactly one run_queue row")
			}
			if ok, err := tc.exit(r); err != nil || !ok {
				t.Fatalf("exit: ok=%v err=%v", ok, err)
			}
			if n := queueRows(t, pool, r.ID); n != 0 {
				t.Errorf("%s left %d run_queue rows behind", tc.name, n)
			}
			got, err := pg.GetRun(ctx, r.ID)
			if err != nil || got.State != tc.want {
				t.Fatalf("state after %s = %v, %v; want %s", tc.name, got.State, err, tc.want)
			}
			if tc.want == types.RunPending && (got.AdmittedAt == nil || !got.AdmittedAt.Equal(admitted)) {
				t.Errorf("promotion left admitted_at %v, want %v", got.AdmittedAt, admitted)
			}
			if tc.want != types.RunPending && got.AdmittedAt != nil {
				t.Errorf("%s stamped admitted_at %v; only promotion does", tc.name, got.AdmittedAt)
			}
			if ok, err := pg.UpdateRunStateIf(ctx, r.ID, types.RunQueued, types.RunKilled); err != nil || ok {
				t.Errorf("a second exit moved the run again: ok=%v err=%v", ok, err)
			}
		})
	}
}

// 100 waiting runs block a 101st launch for the same person and pool; cancelling them frees every
// place, because each cancel deleted its row. Another person, or another pool, is not blocked.
func TestPG_FloodLimitCountsOnlyWaitingRows(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p, other := queuePool(t, pg), queuePool(t, pg)
	now := time.Now().UTC()
	const limit = 100

	var ids []uuid.UUID
	for i := range limit {
		r, err := enqueue(t, pg, p, "flood", now.Add(time.Duration(i)*time.Millisecond), limit)
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		ids = append(ids, r.ID)
	}
	if _, err := enqueue(t, pg, p, "flood", now, limit); !errors.Is(err, store.ErrRunQueueFull) {
		t.Fatalf("the %dst waiting run = %v, want ErrRunQueueFull", limit+1, err)
	}
	if n, err := pg.CountQueuedRunsByOwner(ctx, p.ID, "flood"); err != nil || n != limit {
		t.Fatalf("waiting count = %d, %v; want %d", n, err, limit)
	}
	for _, who := range []struct {
		pool  types.RunnerPool
		owner string
	}{{p, "someone-else"}, {other, "flood"}} {
		if _, err := enqueue(t, pg, who.pool, who.owner, now, limit); err != nil {
			t.Errorf("%s in pool %s was blocked by another's queue: %v", who.owner, who.pool.Name, err)
		}
	}
	for _, id := range ids {
		if ok, err := pg.UpdateRunStateIf(ctx, id, types.RunQueued, types.RunKilled); err != nil || !ok {
			t.Fatalf("cancel %s: ok=%v err=%v", id, ok, err)
		}
	}
	if _, err := enqueue(t, pg, p, "flood", now, limit); err != nil {
		t.Fatalf("after cancelling all %d, a new launch is still blocked: %v", limit, err)
	}
}

// Position is the number of rows ahead (oldest first, run id breaking a tie); the promoter's scan
// is the same order, and a second scan passes over the rows the first holds.
func TestPG_QueueOrderPositionAndSkipLocked(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := queuePool(t, pg)
	base := time.Now().UTC()
	var ids []uuid.UUID
	for i, at := range []time.Time{base, base.Add(2 * time.Second), base.Add(time.Second)} { // the third is older than the second
		r, err := enqueue(t, pg, p, fmt.Sprintf("o%d", i), at, 0)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	for id, want := range map[uuid.UUID]int{ids[0]: 0, ids[2]: 1, ids[1]: 2} {
		if got, err := pg.RunQueuePosition(ctx, id); err != nil || got != want {
			t.Errorf("position of %s = %d, %v; want %d", id, got, err, want)
		}
	}
	if _, err := pg.RunQueuePosition(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("position of a run that is not waiting = %v, want ErrNotFound", err)
	}

	a, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Rollback(ctx) //nolint:errcheck
	b, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Rollback(ctx) //nolint:errcheck
	first, err := store.ListRunQueueForUpdate(ctx, a, p.ID, 1)
	if err != nil || len(first) != 1 || first[0].RunID != ids[0] {
		t.Fatalf("first scan = %+v, %v; want the oldest run %s", first, err, ids[0])
	}
	second, err := store.ListRunQueueForUpdate(ctx, b, p.ID, 0)
	if err != nil || len(second) != 2 || second[0].RunID != ids[2] || second[1].RunID != ids[1] {
		t.Fatalf("a second scan = %+v, %v; want to skip the locked head and see %s then %s", second, err, ids[2], ids[1])
	}
}

// run_queue.pool_id is RESTRICT: a pool with a waiting run cannot be hard-deleted past it. Once the
// run is cancelled its row is gone and the queue no longer stands in the way.
func TestPG_PoolDeleteRestrictedByWaitingRows(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := queuePool(t, pg)
	r, err := enqueue(t, pg, p, "waiting", time.Now().UTC(), 0)
	if err != nil {
		t.Fatal(err)
	}
	// The run's own reference to the pool (NO ACTION) would answer first; drop it so the queue's is what stops the delete.
	if _, err := pool.Exec(ctx, `ALTER TABLE agent_runs DROP CONSTRAINT agent_runs_runner_pool_id_fkey`); err != nil {
		t.Fatal(err)
	}
	constraint := func() string {
		_, err := pool.Exec(ctx, `DELETE FROM runner_pools WHERE id = $1`, p.ID)
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			return pe.ConstraintName
		}
		return fmt.Sprint(err)
	}
	if got := constraint(); got != "run_queue_pool_id_fkey" {
		t.Fatalf("deleting a pool with a waiting run was stopped by %q, want run_queue_pool_id_fkey", got)
	}
	if ok, err := pg.UpdateRunStateIf(ctx, r.ID, types.RunQueued, types.RunKilled); err != nil || !ok {
		t.Fatalf("cancel: ok=%v err=%v", ok, err)
	}
	if got := constraint(); got != "<nil>" {
		t.Errorf("deleting the pool after the run was cancelled = %q, want it to succeed", got)
	}
}

// A state CAS that is not LeaveRunQueue cannot take a run out of QUEUED and strand its row.
func TestPG_OtherStateCASesRefuseQueued(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	r, err := enqueue(t, pg, queuePool(t, pg), "owner", time.Now().UTC(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := pg.UpdateRunStateIfIdle(ctx, r.ID, types.RunQueued, types.RunStopped, time.Now().Add(time.Hour)); ok || err == nil {
		t.Errorf("UpdateRunStateIfIdle from QUEUED = %v, %v; want a refusal", ok, err)
	}
	if ok, err := pg.UpdateRunStateIfCreatedBefore(ctx, r.ID, types.RunQueued, types.RunStopped, time.Now().Add(time.Hour)); ok || err == nil {
		t.Errorf("UpdateRunStateIfCreatedBefore from QUEUED = %v, %v; want a refusal", ok, err)
	}
	if queueRows(t, pool, r.ID) != 1 {
		t.Error("a refused CAS must leave the run_queue row")
	}
}

// Two enqueues racing at a limit of one for the same person admit exactly one: the owner count and the
// insert share the pool row's lock.
func TestPG_ConcurrentEnqueueRespectsTheLimit(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := queuePool(t, pg)
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := enqueue(t, pg, p, "racer", time.Now().UTC(), 1); err == nil {
				mu.Lock()
				admitted++
				mu.Unlock()
			} else if !errors.Is(err, store.ErrRunQueueFull) {
				t.Errorf("enqueue: %v", err)
			}
		}()
	}
	wg.Wait()
	if admitted != 1 {
		t.Errorf("%d racing enqueues admitted, want exactly 1", admitted)
	}
}

// The max-age stop's cutoff counts a promoted run from admitted_at, so a long wait is not age.
func TestPG_MaxAgeCutoffCountsFromAdmission(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	now := time.Now().UTC()
	cutoff := now.Add(-24 * time.Hour)
	for _, tc := range []struct {
		name     string
		admitted *time.Time
		stopped  bool
	}{
		{"waited, admitted an hour ago", func() *time.Time { a := now.Add(-time.Hour); return &a }(), false},
		{"admitted past the cap", func() *time.Time { a := now.Add(-25 * time.Hour); return &a }(), true},
		{"never queued, created past the cap", nil, true},
	} {
		r := newRun(types.RunRunning)
		r.CreatedAt = now.Add(-30 * time.Hour)
		if _, err := pg.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		if tc.admitted != nil {
			if _, err := pool.Exec(ctx, `UPDATE agent_runs SET admitted_at = $2 WHERE id = $1`, r.ID, *tc.admitted); err != nil {
				t.Fatal(err)
			}
		}
		if ok, err := pg.UpdateRunStateIfCreatedBefore(ctx, r.ID, types.RunRunning, types.RunStopped, cutoff); err != nil || ok != tc.stopped {
			t.Errorf("%s: stopped=%v err=%v, want stopped=%v", tc.name, ok, err, tc.stopped)
		}
	}
}
