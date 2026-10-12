// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// How a promotion pass judges the queue (pool-queue.md sections 7 and 8): with no lock held, past heads it
// holds, and without letting one run it cannot judge stop the rest.

// A verdict that cannot be given (the store would not answer for that owner) leaves that run in its place
// and the pass promotes the next: one run never holds back the pool.
func TestPG_PromoteQueueSkipsARunItCannotJudge(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	p := cappedPool(t, pg, 1)
	now := time.Now().UTC()
	head, err := enqueue(t, pg, p, "bob", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	next, err := enqueue(t, pg, p, "alice", now.Add(time.Second), 0)
	if err != nil {
		t.Fatal(err)
	}
	poisoned := func(_ context.Context, _ types.RunnerPool, e store.RunQueueEntry) (store.QueueVerdict, error) {
		if e.RunID == head.ID {
			return store.QueueVerdict{}, errors.New("read the owner's pools: connection reset")
		}
		return store.QueueVerdict{Admit: true}, nil
	}
	got, held, err := pg.PromoteRunQueue(ctx, pass(p), poisoned)
	if err != nil || len(got) != 1 || got[0].Run.ID != next.ID {
		t.Fatalf("promoted %v, %v; want the next run, whatever the head's verdict did", got, err)
	}
	if len(held) != 1 || held[0].RunID != head.ID || held[0].Wait != "" {
		t.Errorf("held = %+v; want the head, with no wait reason to record", held)
	}
	if st := runState(t, pg, head.ID).State; st != types.RunQueued {
		t.Errorf("the head is %s; it keeps its place", st)
	}
}

// Once a run is held because its owner's runners are all busy, the owner's later runs are passed over, and
// the pass reads on past a full scan window of held heads, so a run further back is still judged and
// promoted while the pool has room.
func TestPG_PromoteQueueJudgesPastHeldHeads(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	p := cappedPool(t, pg, 1)
	now := time.Now().UTC()
	for i := range 7 {
		if _, err := enqueue(t, pg, p, "bob", now.Add(time.Duration(i)*time.Second), 0); err != nil {
			t.Fatal(err)
		}
	}
	behind, err := enqueue(t, pg, p, "alice", now.Add(time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}
	asked := map[string]int{}
	busyBob := func(_ context.Context, _ types.RunnerPool, e store.RunQueueEntry) (store.QueueVerdict, error) {
		asked[e.Owner]++
		if e.Owner == "bob" {
			return store.QueueVerdict{Wait: string(runnerpool.WaitNoRunnerFree), Owner: true}, nil
		}
		return store.QueueVerdict{Admit: true}, nil
	}
	scan := pass(p)
	scan.ScanLimit = 3
	got, _, err := pg.PromoteRunQueue(ctx, scan, busyBob)
	if err != nil || len(got) != 1 || got[0].Run.ID != behind.ID {
		t.Fatalf("promoted %v, %v; want the run behind seven held heads", got, err)
	}
	if asked["bob"] != 1 {
		t.Errorf("bob's runs were judged %d times in one pass; once his head is held his later runs wait with it", asked["bob"])
	}
}

// The verdicts, which ask the substrate, are given with no lock held: neither the pool row nor the
// deployment cap's advisory lock is taken until the verdicts are in.
func TestPG_PromoteQueueJudgesWithoutALock(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := cappedPool(t, pg, 1)
	r, err := enqueue(t, pg, p, "alice", time.Now().UTC(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var poolFree, capFree bool
	var probeErr error
	probe := func(context.Context, types.RunnerPool, store.RunQueueEntry) (store.QueueVerdict, error) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			probeErr = err
			return store.QueueVerdict{}, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var id uuid.UUID
		poolFree = tx.QueryRow(ctx, `SELECT id FROM runner_pools WHERE id = $1 FOR UPDATE NOWAIT`, p.ID).Scan(&id) == nil
		probeErr = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1, 0)`, db.RunCapLockClass).Scan(&capFree)
		return store.QueueVerdict{Admit: true}, nil
	}
	withCap := pass(p)
	withCap.DeploymentCap = 10
	got, _, err := pg.PromoteRunQueue(ctx, withCap, probe)
	if err != nil || probeErr != nil || len(got) != 1 || got[0].Run.ID != r.ID {
		t.Fatalf("promote = %v, %v (probe %v); want the run promoted", got, err, probeErr)
	}
	if !poolFree || !capFree {
		t.Errorf("while the verdict was asked the pool row was free %v and the deployment lock free %v; both must be", poolFree, capFree)
	}
}

// A run held for a reason of its own (it is pinned to one runner) holds only itself: the same owner's
// next run is still judged and promoted in the same pass.
func TestPG_PromoteQueueHoldsAPinnedRunAlone(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	p := cappedPool(t, pg, 1)
	now := time.Now().UTC()
	pinned, err := enqueue(t, pg, p, "bob", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	next, err := enqueue(t, pg, p, "bob", now.Add(time.Second), 0)
	if err != nil {
		t.Fatal(err)
	}
	pinHeld := func(_ context.Context, _ types.RunnerPool, e store.RunQueueEntry) (store.QueueVerdict, error) {
		if e.RunID == pinned.ID {
			return store.QueueVerdict{Wait: string(runnerpool.WaitNoRunnerFree)}, nil
		}
		return store.QueueVerdict{Admit: true}, nil
	}
	got, _, err := pg.PromoteRunQueue(ctx, pass(p), pinHeld)
	if err != nil || len(got) != 1 || got[0].Run.ID != next.ID {
		t.Fatalf("promoted %v, %v; want bob's next run, which no pin holds", got, err)
	}
}

// A judging cut short by its deadline still promotes the runs it had already admitted.
func TestPG_PromoteQueueKeepsWhatItAdmittedWhenCutShort(t *testing.T) {
	pg := store.NewPG(runsPGPoolIsolated(t))
	p := cappedPool(t, pg, 3)
	now := time.Now().UTC()
	var ids []uuid.UUID
	for i := range 3 {
		r, err := enqueue(t, pg, p, "alice", now.Add(time.Duration(i)*time.Second), 0)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	slow := func(ctx context.Context, _ types.RunnerPool, e store.RunQueueEntry) (store.QueueVerdict, error) {
		if e.RunID == ids[0] {
			return store.QueueVerdict{Admit: true}, nil
		}
		<-ctx.Done() // the judging half of the pass is spent on this run
		return store.QueueVerdict{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	scan := pass(p)
	scan.ScanLimit = 1
	got, _, err := pg.PromoteRunQueue(ctx, scan, slow)
	if len(got) != 1 || got[0].Run.ID != ids[0] {
		t.Fatalf("promoted %v (%v); want the run admitted before the judging ran out of time", got, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v; the cut-short judging's error comes back beside what it promoted", err)
	}
}
