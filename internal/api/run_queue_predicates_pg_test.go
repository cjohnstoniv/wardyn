// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Which later runs a held run holds, when a pass's starts go, and where a promoted run sent back to wait goes.

// A run held because the one runner that enforces its barrier is full holds only itself: an older Wall run
// waiting for a leaves its owner's newer Fence run free to start on b once b has room.
func TestPG_WaitingRunHeldForItsOwnRunnerHoldsNoLaterRunOfItsOwner(t *testing.T) {
	e := newQueueEnv(t)
	e.srv.cfg.DefaultPolicy.MinConfinementClass = types.CC1 // the Fence run below asks for less than the environment's Wall floor
	laptops, _, b := wallAndFence(e)
	e.maxRuns(b, e.personP, 1)
	laptops = e.reread(laptops)
	occupyRunner(e, laptops, b)
	wall := e.waitsFor(runBody(fmt.Sprintf(`"confinement_class":"CC2","runner_pool_id":%q`, laptops.ID)), "a is the only runner that enforces Wall")
	fence := e.waitsFor(runBody(fmt.Sprintf(`"confinement_class":"CC1","runner_pool_id":%q`, laptops.ID)), "both runners are full")
	e.endOccupant(b.String())
	if got := e.settles(fence.ID); got.State != types.RunRunning || got.RunnerID == nil || *got.RunnerID != b {
		t.Fatalf("once b has room the Fence run is %s on %v (%s); want it RUNNING on b", got.State, got.RunnerID, got.StatusDetail)
	}
	if got := e.run(wall.ID); got.State != types.RunQueued {
		t.Errorf("the Wall run is %s; it keeps waiting for a", got.State)
	}
}

// A pass starts the runs it promoted before it records the waits of the runs it held: that write is
// advisory, so one stuck behind a row lock neither keeps a promoted run from starting nor outlasts the
// pass's bound.
func TestPG_QueuePromotedRunStartsConcurrentlyWithAStuckWaitWrite(t *testing.T) {
	ctx := context.Background()
	e := newQueueEnv(t)
	p, a, b := cappedLaptops(e, 1)
	occupyRunner(e, p, a)
	occupyRunner(e, p, b)
	held := e.queuedLaunch(e.person, p, `,"placement":"local","runner_id":"`+a.String()+`"`)
	next := e.queuedLaunch(e.person, p, `,"placement":"local"`)
	// The held run still says the pool's cap, so the pass's wait for it (a is full) is a write.
	e.exec(`UPDATE agent_runs SET status_detail = $2 WHERE id = $1`, held.ID, statusDetailQueuePrefix+string(runnerpool.WaitAtCapacity))
	lock, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(ctx) }()
	if _, err := lock.Exec(ctx, `SELECT 1 FROM agent_runs WHERE id = $1 FOR UPDATE`, held.ID); err != nil {
		t.Fatal(err)
	}
	// b's occupant ends with no kick, so the pass below is the only one.
	var occupant uuid.UUID
	if err := e.pool.QueryRow(ctx, `SELECT id FROM agent_runs WHERE state = 'RUNNING' AND runner_id = $1`, b).Scan(&occupant); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.pg.UpdateRunStateIf(ctx, occupant, types.RunRunning, types.RunCompleted); err != nil || !ok {
		t.Fatalf("end the occupant: %v %v", ok, err)
	}
	// The first pass's bound is past the 20 seconds eventually gives a start, so however slow the start is,
	// the wait write is still behind the held run's row when the promoted run is seen started.
	done := make(chan error, 1)
	go func() { done <- e.srv.promoteQueue(ctx, p.ID, time.Minute) }()
	e.waitStarted(next.ID)
	select {
	case err := <-done:
		t.Fatalf("the pass ended (%v) before its promoted run started", err)
	default:
	}
	if err := lock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the pass, once the held run's row is released: %v", err)
	}
	// The write is held to the pass's bound, not to the caller's context, which has none here: behind a
	// held pool row it gives up when that bound is spent.
	poolLock, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = poolLock.Rollback(ctx) }()
	if _, err := poolLock.Exec(ctx, `SELECT 1 FROM runner_pools WHERE id = $1 FOR UPDATE`, p.ID); err != nil {
		t.Fatal(err)
	}
	go func() { done <- e.srv.promoteQueue(ctx, p.ID, queueEnqueueBound) }()
	select {
	case err := <-done:
		if err == nil {
			t.Errorf("the pass wrote a wait through a held pool row; want the write to give up at the pass's bound")
		}
	case <-time.After(10 * queueEnqueueBound):
		t.Errorf("the wait write outlasted the pass's bound")
	}
}

// A promoted run sent back to wait for its owner's sign-in after its pool was deleted has no queue to go
// back to: it is ended runner_pool_removed, and no queue row is left in the deleted pool.
func TestPG_PromotedRunOfADeletedPoolIsEndedNotRequeued(t *testing.T) {
	ctx := context.Background()
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	waiting, occupant := e.waitingPerson(pool, "")
	if ok, err := e.pg.UpdateRunStateIf(ctx, occupant.ID, types.RunRunning, types.RunCompleted); err != nil || !ok {
		t.Fatalf("end the occupant: %v %v", ok, err)
	}
	promoted, _, err := e.pg.PromoteRunQueue(ctx, store.PromotePass{PoolID: pool.ID, WatcherOwner: watcherOwner},
		func(context.Context, types.RunnerPool, store.RunQueueEntry) (store.QueueVerdict, error) {
			return store.QueueVerdict{Admit: true}, nil
		})
	if err != nil || len(promoted) != 1 || promoted[0].Run.ID != waiting.ID {
		t.Fatalf("promote: %v %v", promoted, err)
	}
	if _, err := e.pg.DeleteRunnerPool(ctx, pool.ID); err != nil {
		t.Fatal(err)
	}
	st, err := newQueuedStart(ctx, ctx, promoted[0])
	if err != nil {
		t.Fatal(err)
	}
	e.srv.endPromoted(st, &queueEnd{wait: runnerpool.WaitOwnerSignIn})
	got := e.run(waiting.ID)
	if got.State != types.RunFailed || got.StatusDetail != statusDetailQueuePrefix+string(types.RunEndRunnerPoolRemoved) ||
		got.FailureHint != types.RunEndRunnerPoolRemoved.Sentence() || e.queueRows(waiting.ID) != 0 {
		t.Errorf("the run is %s (%q, %q) with %d queue rows; want it FAILED runner_pool_removed with none",
			got.State, got.StatusDetail, got.FailureHint, e.queueRows(waiting.ID))
	}
}
