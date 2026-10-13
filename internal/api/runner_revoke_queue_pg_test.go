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
