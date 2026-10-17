// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Revoking a runner ends the runs waiting in a pool's queue for it alone, as it ends the ones running on
// it: the waiting run is KILLED with reason runner_revoked, its queue row is gone, and the revoke counts it.
func TestPG_RevokingARunnerEndsTheRunsWaitingForIt(t *testing.T) {
	e := newQueueEnv(t)
	p, a, _ := cappedLaptops(e, 1)
	occupyRunner(e, p, a)
	waiting := e.queuedLaunch(e.person, p, `,"placement":"local","runner_id":"`+a.String()+`"`)

	w := e.call(e.person, http.MethodDelete, "/api/v1/me/runners/"+a.String(), "")
	if w.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", w.Code, w.Body)
	}
	var body struct {
		RunsKilled int `json:"runs_killed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.RunsKilled != 2 {
		t.Errorf("revoke answered runs_killed %d (%v), want 2: the run on it and the run waiting for it", body.RunsKilled, err)
	}
	if got := e.run(waiting.ID); got.State != types.RunKilled {
		t.Fatalf("the run waiting for the revoked runner is %s, want KILLED", got.State)
	}
	if n := e.queueRows(waiting.ID); n != 0 {
		t.Errorf("the ended run still has %d queue rows", n)
	}
	kills := e.audits(waiting.ID, "run.kill")
	if len(kills) != 1 {
		t.Fatalf("%d run.kill rows for the waiting run, want 1", len(kills))
	}
	var data map[string]any
	if json.Unmarshal(kills[0].Data, &data) != nil || data["reason"] != queueCancelRunnerRevoked {
		t.Errorf("run.kill data %s, want reason %s", kills[0].Data, queueCancelRunnerRevoked)
	}
}

// A waiting run that cannot be ended does not keep the next one waiting for the revoked runner: the
// revoke goes on to it, and answers that it did not complete.
func TestPG_RevokingARunnerEndsEachWaitingRunDespiteAFailedOne(t *testing.T) {
	e := newQueueEnv(t)
	p, a, _ := cappedLaptops(e, 1)
	occupyRunner(e, p, a)
	pinned := `,"placement":"local","runner_id":"` + a.String() + `"`
	stuck, next := e.queuedLaunch(e.person, p, pinned), e.queuedLaunch(e.person, p, pinned)
	e.exec(`CREATE FUNCTION refuse_end() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'refused'; END $$`)
	e.exec(`CREATE TRIGGER refuse_end BEFORE UPDATE ON agent_runs FOR EACH ROW WHEN (OLD.id = '` + stuck.ID.String() + `') EXECUTE FUNCTION refuse_end()`)

	w := e.call(e.person, http.MethodDelete, "/api/v1/me/runners/"+a.String(), "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("revoke with a run that cannot be ended = %d %s, want 500", w.Code, w.Body)
	}
	if got := e.run(stuck.ID); got.State != types.RunQueued {
		t.Fatalf("the run whose end was refused is %s, want it still QUEUED", got.State)
	}
	if got := e.run(next.ID); got.State != types.RunKilled {
		t.Errorf("the run queued behind a failed end is %s, want KILLED", got.State)
	}
}

// endedForRevokedRunner asserts the waiting run was ended for its revoked runner: KILLED, out of the
// queue, with one run.kill row whose reason is runner_revoked.
func (e *queueEnv) endedForRevokedRunner(id uuid.UUID) {
	e.t.Helper()
	if got := e.run(id); got.State != types.RunKilled {
		e.t.Fatalf("the run waiting for a revoked runner is %s after the sweep, want KILLED", got.State)
	}
	if n := e.queueRows(id); n != 0 {
		e.t.Errorf("the ended run still has %d queue rows", n)
	}
	kills := e.audits(id, "run.kill")
	var data map[string]any
	if len(kills) != 1 || json.Unmarshal(kills[0].Data, &data) != nil || data["reason"] != queueCancelRunnerRevoked {
		e.t.Errorf("run.kill rows %+v, want one with reason %s", kills, queueCancelRunnerRevoked)
	}
}

// A waiting run the revoke could not end is not left to wait out its deadline: the revoke answers 500
// and says the queue sweep ends it, and the sweep does, once the run can be ended and its retry wait
// (revokedEndRetry) is over, with nobody revoking again.
func TestPG_QueueSweepEndsTheWaitingRunARevokeCouldNotEnd(t *testing.T) {
	e := newQueueEnv(t)
	p, a, _ := cappedLaptops(e, 1)
	occupyRunner(e, p, a)
	stuck := e.queuedLaunch(e.person, p, `,"placement":"local","runner_id":"`+a.String()+`"`)
	e.exec(`CREATE FUNCTION refuse_end() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'refused'; END $$`)
	e.exec(`CREATE TRIGGER refuse_end BEFORE UPDATE ON agent_runs FOR EACH ROW WHEN (OLD.id = '` + stuck.ID.String() + `') EXECUTE FUNCTION refuse_end()`)

	w := e.call(e.person, http.MethodDelete, "/api/v1/me/runners/"+a.String(), "")
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "the queue sweep ends a run still waiting for it") {
		t.Fatalf("revoke with a waiting run that cannot be ended = %d %s, want 500 naming the queue sweep", w.Code, w.Body)
	}
	e.sweep()
	if got := e.run(stuck.ID); got.State != types.RunQueued {
		t.Fatalf("the run whose end is still refused is %s, want it QUEUED for the next tick", got.State)
	}
	e.exec(`DROP TRIGGER refuse_end ON agent_runs`)
	later := time.Now().Add(revokedEndFirstWait)
	e.srv.cfg.Now = func() time.Time { return later }
	e.sweep()
	e.endedForRevokedRunner(stuck.ID)
}

// A revoke that committed in a server that stopped before it ended the waiting runs: the sweep ends the
// run waiting for the revoked runner, from the stored state alone, and leaves one waiting for a runner
// that is not revoked.
func TestPG_QueueSweepEndsTheWaitingRunOfARevokeThatNeverFinished(t *testing.T) {
	e := newQueueEnv(t)
	p, a, b := cappedLaptops(e, 1)
	occupyRunner(e, p, a)
	occupyRunner(e, p, b)
	orphan := e.queuedLaunch(e.person, p, `,"placement":"local","runner_id":"`+a.String()+`"`)
	keeps := e.queuedLaunch(e.person, p, `,"placement":"local","runner_id":"`+b.String()+`"`)
	if _, err := e.pg.RevokeRunner(context.Background(), a, time.Now().UTC(), e.personP, ""); err != nil {
		t.Fatal(err)
	}
	if got := e.run(orphan.ID); got.State != types.RunQueued {
		t.Fatalf("the waiting run is %s after the bare revoke, want QUEUED", got.State)
	}
	e.sweep()
	e.endedForRevokedRunner(orphan.ID)
	if got := e.run(keeps.ID); got.State != types.RunQueued {
		t.Errorf("the run waiting for a runner that is not revoked is %s, want QUEUED", got.State)
	}
}

// Several sweeps at once and the revoke's own pass end each waiting run exactly once: one run.kill row,
// no queue row. A run another exit already ended is left as it is: one its deadline ended stays FAILED.
func TestPG_ARunWaitingForARevokedRunnerIsEndedOnce(t *testing.T) {
	e := newQueueEnv(t)
	p, a, _ := cappedLaptops(e, 1)
	occupyRunner(e, p, a)
	pinned := `,"placement":"local","runner_id":"` + a.String() + `"`
	var ids []uuid.UUID
	for range 4 {
		ids = append(ids, e.queuedLaunch(e.person, p, pinned).ID)
	}
	expired := e.queuedLaunch(e.person, p, pinned).ID
	ctx := context.Background()
	if _, err := e.pg.RevokeRunner(ctx, a, time.Now().UTC(), e.personP, ""); err != nil {
		t.Fatal(err)
	}
	if !e.srv.exitQueued(ctx, expired, types.RunFailed, statusDetailQueuePrefix+string(types.RunEndQueueTimeout), "its wait ran out") {
		t.Fatal("could not end the run as its deadline step does")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { _ = e.srv.endQueuedRunsOfRevokedRunners(ctx) })
	}
	wg.Go(func() { _, _ = e.srv.endQueuedRunsPinnedTo(ctx, types.Runner{Owner: e.personP}, a) })
	wg.Wait()
	if applied, err := e.srv.endRunQueuedForRevokedRunner(ctx, expired); applied || err != nil || e.run(expired).State != types.RunFailed {
		t.Errorf("the run its deadline ended was ended again (applied %v, %v): now %s", applied, err, e.run(expired).State)
	}
	for _, id := range ids {
		if got, kills := e.run(id), e.audits(id, "run.kill"); got.State != types.RunKilled || len(kills) != 1 || e.queueRows(id) != 0 {
			t.Errorf("run %s: %s with %d run.kill rows and %d queue rows, want KILLED, one row, none", id, got.State, len(kills), e.queueRows(id))
		}
	}
}
