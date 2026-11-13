// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A promoted start's copy of its queue row against the owner's run_tasks erasure, on a real Postgres.
// Guarded by WARDYN_TEST_PG.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// promoteByHand frees the occupant's slot and promotes the waiting run with no start behind it: the
// returned entry is the copy of the queue row a promoted start holds in memory.
func promoteByHand(e *queueEnv, pool types.RunnerPool, occupant types.AgentRun, want uuid.UUID) store.PromotedRun {
	e.t.Helper()
	e.exec(`DELETE FROM agent_runs WHERE id = $1`, occupant.ID)
	promoted, _, err := e.pg.PromoteRunQueue(context.Background(), store.PromotePass{PoolID: pool.ID, ScanLimit: 10, WatcherOwner: "paused-start"},
		func(context.Context, types.RunnerPool, store.RunQueueEntry) (store.QueueVerdict, error) {
			return store.QueueVerdict{Admit: true}, nil
		})
	if err != nil || len(promoted) != 1 || promoted[0].Run.ID != want {
		e.t.Fatalf("promote by hand: %v %v", promoted, err)
	}
	return promoted[0]
}

const signInWait = statusDetailQueuePrefix + string(runnerpool.WaitOwnerSignIn)

// A promoted start that has to wait for its owner's sign-in after the owner's run_tasks erasure does not
// put its run back in the queue: its copy of the queue row was taken before the erasure and holds the
// erased task text. Catches a requeue that writes that copy back into run_queue after the erasure returned.
func TestPG_QueueRequeueAfterTheOwnersTaskErasureIsRefused(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 2)
	first, second := e.occupy(pool), e.occupy(pool)
	waiting := e.queuedLaunch(e.person, pool, "")
	others := e.queuedLaunch(e.other, pool, "")
	promoted := promoteByHand(e, pool, first, waiting.ID)
	othersPromoted := promoteByHand(e, pool, second, others.ID)

	if _, err := e.srv.eraseRunTasks(context.Background(), e.personP); err != nil {
		t.Fatal(err)
	}
	if moved, err := e.pg.RequeuePromotedRun(context.Background(), promoted.Entry, signInWait); err != nil || moved {
		t.Errorf("requeue of the start's pre-erasure copy = %v, %v; want refused", moved, err)
	}
	// Another person's promoted run is not marked by this erasure: its start still puts it back.
	if moved, err := e.pg.RequeuePromotedRun(context.Background(), othersPromoted.Entry, signInWait); err != nil || !moved {
		t.Errorf("requeue of another person's promoted run = %v, %v; want moved", moved, err)
	}
	if n := e.queueRows(others.ID); n != 1 {
		t.Errorf("run_queue holds %d rows for another person's run; want 1", n)
	}
	if n := e.queueRows(waiting.ID); n != 0 {
		t.Errorf("run_queue holds %d rows for the erased person's run; want none", n)
	}
	if got := e.run(waiting.ID); got.State != types.RunPending || got.Task != "" {
		t.Errorf("the run after the refused requeue = %s task %q; want it still PENDING (the start ends it) and blanked", got.State, got.Task)
	}
}

// A promoted start that puts its run back in the queue while the erasure runs, after its first cancel
// of the queue and before the blank, is cancelled by the erasure too. Catches an erasure that cancels the
// queue only before the blank, which leaves that run waiting with the erased task in its queue row.
func TestPG_QueueErasureCancelsARunPutBackWhileItRuns(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	occupant := e.occupy(pool)
	waiting := e.queuedLaunch(e.person, pool, "")
	promoted := promoteByHand(e, pool, occupant, waiting.ID)

	reached := false
	e.srv.queueRace = func(point string) {
		if point != queueRaceEraseAfterCancel {
			return
		}
		reached = true
		if moved, err := e.pg.RequeuePromotedRun(context.Background(), promoted.Entry, signInWait); err != nil || !moved {
			t.Errorf("requeue before the blank = %v, %v; want moved", moved, err)
		}
	}
	out, err := e.srv.eraseRunTasks(context.Background(), e.personP)
	e.srv.queueRace = nil
	if err != nil || !reached {
		t.Fatalf("erase = %v (race point reached %v)", err, reached)
	}
	if got := out.(map[string]any)["queued_cancelled"]; got != 1 {
		t.Errorf("erase reported %v cancelled waiting runs, want 1", got)
	}
	if e.run(waiting.ID).State != types.RunKilled || e.queueRows(waiting.ID) != 0 {
		t.Error("the run put back during the erasure kept its queue row")
	}
}

// A promoted start that is past its checks when its owner's run_tasks erasure runs does not start the run
// from its pre-erasure copy: the run ends FAILED as a refused start, with its run.queue.start failure row,
// and the slot it held goes to another person's run, which starts. Catches a start write that ignores the
// erasure's mark, and a refused start that leaves the run PENDING.
func TestPG_QueueStartAfterTheOwnersTaskErasureEndsTheRun(t *testing.T) {
	e := newQueueEnv(t)
	erased := make(chan error, 1)
	var fired atomic.Bool // the first start to pass its checks is the erased person's: the pool has one slot
	e.srv.queueRace = func(point string) {
		if point == queueRaceStartAfterPrecheck && fired.CompareAndSwap(false, true) {
			_, err := e.srv.eraseRunTasks(context.Background(), e.personP)
			erased <- err
		}
	}
	pool := e.cappedPool("Busy", 1)
	occupant := e.occupy(pool)
	waiting := e.queuedLaunch(e.person, pool, "")
	others := e.queuedLaunch(e.other, pool, "")
	e.free(occupant)
	select {
	case err := <-erased:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the erasure never ran inside the promoted start")
	}
	got := e.waitFailed(waiting.ID)
	if got.StatusDetail != "queue: person_erased" || got.FailureHint != queueStartFailedHint || got.SPIFFEID != "" || got.SandboxRef != "" || got.Task != "" {
		t.Errorf("the erased person's run = %q %q, spiffe %q, sandbox %q, task %q; want person_erased, nothing started or launched, blanked",
			got.StatusDetail, got.FailureHint, got.SPIFFEID, got.SandboxRef, got.Task)
	}
	// One row, naming the erasure: not a start_lost row from a run left PENDING for the stale-start sweep.
	if rows := e.waitAudit(got.ID, "run.queue.start"); len(rows) != 1 || rows[0].Outcome != "failure" || queueAuditData(t, rows[0])["reason"] != "person_erased" {
		t.Errorf("run.queue.start rows = %+v; want one failure naming person_erased", rows)
	}
	if n := e.queueRows(waiting.ID); n != 0 {
		t.Errorf("run_queue holds %d rows for the erased person's run; want none", n)
	}
	e.waitStarted(others.ID)
}
