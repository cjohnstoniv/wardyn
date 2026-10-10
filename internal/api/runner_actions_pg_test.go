// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestRunnerActionsPG_StateQueuesContainmentAndOnlyItsExactResultSettlesIt(t *testing.T) {
	pool, cfg, rn, run, exit, _ := runnerExitAPIFixture(t)
	ctx := t.Context()
	st := cfg.Store.(store.PG)
	srv := New(cfg)
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at=now(), lost_reason='outage' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	row := runnerwire.RunState{RunID: run.ID, SandboxPresent: true,
		Execution: &runnerwire.ExecutionResult{Generation: exit.Generation, Operation: runnerwire.MethodExec, Attempted: true}}
	requests, err := srv.StateFromRunner(ctx, rn.ID, []runnerwire.RunState{row})
	if !errors.Is(err, store.ErrRunnerReconciliationRequired) || len(requests) != 1 {
		t.Fatalf("late containment STATE = %v, %v", requests, err)
	}
	pending, err := srv.PendingFromRunner(ctx, rn.ID)
	if err != nil || len(pending) != 1 || pending[0].Kind != types.RunnerActionStopProxy || pending[0].RunID != run.ID || pending[0].Ref != exit.Ref {
		t.Fatalf("PENDING = %+v, %v", pending, err)
	}
	if _, err := srv.StateFromRunner(ctx, rn.ID, []runnerwire.RunState{row}); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
		t.Fatalf("STATE with an open action = %v, want reconciliation required", err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runner_pending_actions WHERE run_id=$1`, run.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("%d actions queued, want one", rows)
	}

	result := runnerwire.ActionResult{ID: pending[0].ID, Outcome: types.RunnerActionSuccess, ObservedAt: time.Now().Add(-time.Minute)}
	event := runnerwire.Event{Kind: runnerwire.EventActionResult, RunID: &run.ID, Data: mustJSON(result)}
	sibling, err := st.CreateRunner(ctx, types.Runner{ID: uuid.New(), Owner: rn.Owner, Name: "sibling", PublicKey: make([]byte, 32), KeyFingerprint: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimRunner(ctx, sibling.ID, sibling.Owner, sibling.KeyFingerprint, time.Now()); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		runner uuid.UUID
		event  runnerwire.Event
	}{
		"unknown field":        {rn.ID, runnerwire.Event{Kind: runnerwire.EventActionResult, RunID: &run.ID, Data: []byte(`{"id":"` + result.ID.String() + `","outcome":"success","observed_at":"2026-01-01T00:00:00Z","extra":1}`)}},
		"no run":               {rn.ID, runnerwire.Event{Kind: runnerwire.EventActionResult, Data: event.Data}},
		"another kind":         {rn.ID, runnerwire.Event{Kind: runnerwire.EventSpool, RunID: &run.ID, Data: event.Data}},
		"another owned runner": {sibling.ID, event},
	} {
		if err := srv.ActionResultFromRunner(ctx, c.runner, c.event); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := srv.ActionResultFromRunner(ctx, rn.ID, event); err != nil {
		t.Fatal(err)
	}
	if err := srv.ActionResultFromRunner(ctx, rn.ID, event); err != nil {
		t.Fatalf("an identical repeat was not acknowledged: %v", err)
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE run_id=$1 AND action='run.containment.reassert'`, run.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("%d late-containment audits, want one", audits)
	}
	stopped := time.Now()
	row.ProxyStoppedAt = &stopped
	if _, err := srv.StateFromRunner(ctx, rn.ID, []runnerwire.RunState{row}); err != nil {
		t.Fatalf("converged STATE = %v", err)
	}
}
