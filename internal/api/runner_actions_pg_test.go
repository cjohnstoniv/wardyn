// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// finalStatePage begins a fresh inventory session, as the Hub does before
// PENDING, and submits rows as its one Final page.
func finalStatePage(t *testing.T, srv *Server, runnerID uuid.UUID, rows ...runnerwire.RunState) error {
	t.Helper()
	session := uuid.NewString()
	if err := srv.cfg.Store.(store.PG).BeginRunnerInventory(t.Context(), runnerID, session); err != nil {
		t.Fatal(err)
	}
	rows = slices.Clone(rows)
	slices.SortFunc(rows, func(a, b runnerwire.RunState) int { return bytes.Compare(a.RunID[:], b.RunID[:]) })
	return srv.StatePageFromRunner(t.Context(), runnerID, session, runnerwire.StatePage{InventoryID: uuid.New(), Runs: rows, Final: true})
}

func TestRunnerActionsPG_StatePageQueuesContainmentAndOnlyItsExactResultSettlesIt(t *testing.T) {
	pool, cfg, rn, run, exit, _ := runnerExitAPIFixture(t)
	ctx := t.Context()
	st := cfg.Store.(store.PG)
	srv := New(cfg)
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at=now(), lost_reason='outage' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	row := runnerwire.RunState{RunID: run.ID, SandboxPresent: true,
		Execution: &runnerwire.ExecutionResult{Generation: exit.Generation, Operation: runnerwire.MethodExec, Attempted: true}}
	if err := finalStatePage(t, srv, rn.ID, row); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
		t.Fatalf("late containment Final = %v, want reconciliation required", err)
	}
	pending, err := srv.PendingFromRunner(ctx, rn.ID)
	if err != nil || len(pending) != 1 || pending[0].Kind != types.RunnerActionStopProxy || pending[0].RunID != run.ID || pending[0].Ref != exit.Ref {
		t.Fatalf("PENDING = %+v, %v", pending, err)
	}
	if err := finalStatePage(t, srv, rn.ID, row); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
		t.Fatalf("Final with an open action = %v, want reconciliation required", err)
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
		"unknown field":        {rn.ID, runnerwire.Event{Kind: runnerwire.EventActionResult, RunID: &run.ID, Data: mustJSON(map[string]any{"id": result.ID, "outcome": "success", "observed_at": result.ObservedAt, "extra": 1})}},
		"no run":               {rn.ID, runnerwire.Event{Kind: runnerwire.EventActionResult, Data: event.Data}},
		"posture has no store": {rn.ID, runnerwire.Event{Kind: runnerwire.EventPosture, RunID: &run.ID, Data: event.Data}},
		"another owned runner": {sibling.ID, event},
	} {
		if err := srv.EventFromRunner(ctx, c.runner, c.event); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := srv.EventFromRunner(ctx, rn.ID, event); err != nil {
		t.Fatal(err)
	}
	if err := srv.EventFromRunner(ctx, rn.ID, event); err != nil {
		t.Fatalf("an identical repeat was not acknowledged: %v", err)
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE run_id=$1 AND action='run.containment.reassert'`, run.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("%d late-containment audits, want one", audits)
	}
	stopped := time.Now()
	row.ProxyStoppedAt = &stopped
	if err := finalStatePage(t, srv, rn.ID, row); err != nil {
		t.Fatalf("converged Final = %v", err)
	}
}
