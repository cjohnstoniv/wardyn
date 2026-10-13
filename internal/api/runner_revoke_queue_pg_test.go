// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

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
