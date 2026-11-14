// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// seedRunIn stores a run of the env's member in state and returns it.
func seedRunIn(t *testing.T, e svcEnv, state types.RunState) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if _, err := e.st.CreateRun(ctx, types.AgentRun{ID: id, CreatedBy: "mem", Agent: "claude-code", ConfinementClass: types.CC1, State: state,
		RunnerTarget: "docker", Task: "t", SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String()}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE agent_runs SET state = $2 WHERE id = $1`, id, state); err != nil {
		t.Fatal(err)
	}
	return id
}

// seedOwedEnd stores a running run owed an end that waits on after (nil for none), recorded two hours ago,
// so before every end the test ages to one hour.
func seedOwedEnd(t *testing.T, e svcEnv, after *uuid.UUID) uuid.UUID {
	t.Helper()
	id := seedRunIn(t, e, types.RunRunning)
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO workload_run_ends (run_id, owner, name, generation, reason, after_run_id, created_at)
		VALUES ($1, 'mem', 'other', 1, 'workload_replaced', $2, now() - interval '2 hours')`, id, after); err != nil {
		t.Fatal(err)
	}
	return id
}

// deletedWorkloadRun applies the member's workload "web", deletes it while its run cannot be read, so the
// run is left owed its end as a crash leaves it, and ages that end to one hour. It returns the run.
func deletedWorkloadRun(t *testing.T, e svcEnv) uuid.UUID {
	t.Helper()
	pol := e.plainPolicy(t, "wl-pol")
	run := launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t", "")), http.StatusCreated))
	e.h.srv.cfg.Store = cutShort{PG: e.st, cancel: func() {}, failReads: new(atomic.Bool)}
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/workloads/web", e.mem, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s, want 204", w.Code, w.Body.String())
	}
	e.h.srv.cfg.Store = e.st
	wantRunLive(t, e, run, "the deleted workload's run before the sweep")
	if _, err := e.pool.Exec(context.Background(), `UPDATE workload_run_ends SET created_at = now() - interval '1 hour' WHERE run_id = $1`, run); err != nil {
		t.Fatal(err)
	}
	return run
}

// countOf runs a count(*) query over the runs ids.
func countOf(t *testing.T, e svcEnv, q string, ids []uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), q, ids).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A full batch of older owed ends that wait on queued replacements (QUEUED, PENDING, STARTING) does not hold
// back a newer end that is due: one sweep ends the deleted workload's run, and every waiting end, and its
// running run, stays as it was.
func TestPG_Workload_ADueOwedEndBehindAFullBatchOfWaitingOnesIsEnded(t *testing.T) {
	e := newSvcEnv(t)
	run := deletedWorkloadRun(t, e)
	on := []uuid.UUID{seedRunIn(t, e, types.RunQueued), seedRunIn(t, e, types.RunPending), seedRunIn(t, e, types.RunStarting)}
	waiting := make([]uuid.UUID, 0, workloadRunEndBatch)
	for i := range workloadRunEndBatch {
		waiting = append(waiting, seedOwedEnd(t, e, &on[i%len(on)]))
	}
	if err := e.h.srv.sweepWorkloadRunEnds(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantKilledFor(t, e, run, workloadKillDeleted)
	if n := countOf(t, e, `SELECT count(*) FROM workload_run_ends WHERE run_id = ANY($1) AND after_run_id IS NOT NULL`, waiting); n != len(waiting) {
		t.Fatalf("waiting ends left = %d, want all %d", n, len(waiting))
	}
	if n := countOf(t, e, `SELECT count(*) FROM agent_runs WHERE id = ANY($1) AND state = 'RUNNING'`, waiting); n != len(waiting) {
		t.Fatalf("running runs whose ends wait = %d, want all %d still running", n, len(waiting))
	}
}

// holdsUnreadable is the store whose read of whether a workload holds a run fails for the runs in fail, so
// the sweep's end of each of them fails on every pass.
type holdsUnreadable struct {
	store.PG
	fail map[uuid.UUID]bool
}

func (s holdsUnreadable) DropWorkloadRunEndIfHeld(ctx context.Context, owner, name string, runID uuid.UUID) (bool, error) {
	if s.fail[runID] {
		return false, errors.New("connection refused")
	}
	return s.PG.DropWorkloadRunEndIfHeld(ctx, owner, name, runID)
}

// A full batch of older owed ends that fail on every pass does not hold back a newer one that is due: the
// next pass starts after them and ends the deleted workload's run, and the failing ends stay owed.
func TestPG_Workload_OwedEndsThatKeepFailingDoNotHoldBackTheNext(t *testing.T) {
	e := newSvcEnv(t)
	run := deletedWorkloadRun(t, e)
	failing := make([]uuid.UUID, 0, workloadRunEndBatch)
	fail := map[uuid.UUID]bool{}
	for range workloadRunEndBatch {
		id := seedOwedEnd(t, e, nil)
		failing, fail[id] = append(failing, id), true
	}
	e.h.srv.cfg.Store = holdsUnreadable{PG: e.st, fail: fail}
	for range 2 {
		if err := e.h.srv.sweepWorkloadRunEnds(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	e.h.srv.cfg.Store = e.st
	wantKilledFor(t, e, run, workloadKillDeleted)
	if n := countOf(t, e, `SELECT count(*) FROM workload_run_ends WHERE run_id = ANY($1)`, failing); n != len(failing) {
		t.Fatalf("failing ends left = %d, want all %d still owed", n, len(failing))
	}
}
