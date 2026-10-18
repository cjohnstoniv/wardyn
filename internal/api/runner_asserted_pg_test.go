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

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// erasedRow is a STATE row for a run the org has no row for.
func erasedRow(runnerID uuid.UUID) runnerwire.RunState {
	id := uuid.New()
	return runnerwire.RunState{RunID: id, Ref: "runner:" + runnerID.String() + "/fake-" + id.String(), Generation: uuid.New(), SandboxPresent: true}
}

func applyAll(t *testing.T, srv *Server, runnerID uuid.UUID, pending []runnerwire.PendingAction, outcome func(runnerwire.PendingAction) string) {
	t.Helper()
	for _, a := range pending {
		runID := a.RunID
		result := runnerwire.ActionResult{ID: a.ID, Outcome: outcome(a), ObservedAt: time.Now().Add(-time.Minute)}
		if err := srv.EventFromRunner(t.Context(), runnerID, runnerwire.Event{Kind: runnerwire.EventActionResult, RunID: &runID, Data: mustJSON(result)}); err != nil {
			t.Fatalf("result for %s: %v", a.Kind, err)
		}
	}
}

// One run the org has no row for (erased, or restored over) must not refuse
// the runner's inventory: it is contained by a kill and an asserted
// quarantine, and once the runner applied them its unrelated run is READY.
func TestRunnerAssertedPG_ErasedRunIsContainedAndUnrelatedRunReachesReady(t *testing.T) {
	_, cfg, rn, run, exit, _ := runnerExitAPIFixture(t)
	ctx := t.Context()
	srv := New(cfg)
	healthy := runnerwire.RunState{RunID: run.ID, SandboxPresent: true,
		Execution: &runnerwire.ExecutionResult{Generation: exit.Generation, Operation: runnerwire.MethodExec, Attempted: true}}
	erased := erasedRow(rn.ID)
	if err := finalStatePage(t, srv, rn.ID, healthy, erased); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
		t.Fatalf("an erased run in STATE = %v, want only reconciliation required", err)
	}
	pending, err := srv.PendingFromRunner(ctx, rn.ID)
	if err != nil || len(pending) != 2 {
		t.Fatalf("PENDING = %+v, %v", pending, err)
	}
	kill, quarantine := pending[0], pending[1]
	if kill.Kind != types.RunnerActionKill || kill.RunID != erased.RunID || kill.Ref != erased.Ref || kill.Quarantine != nil {
		t.Fatalf("first asserted action = %+v", kill)
	}
	if quarantine.Kind != types.RunnerActionQuarantine || quarantine.RunID != erased.RunID || quarantine.Ref != erased.Ref ||
		quarantine.Quarantine == nil || !quarantine.Quarantine.Asserted || quarantine.Quarantine.Generation != erased.Generation {
		t.Fatalf("second asserted action = %+v", quarantine)
	}
	applyAll(t, srv, rn.ID, pending, func(runnerwire.PendingAction) string { return types.RunnerActionSuccess })
	// The runner's quarantined run leaves its inventory.
	if err := finalStatePage(t, srv, rn.ID, healthy); err != nil {
		t.Fatalf("unrelated run after containment = %v, want READY", err)
	}
}

// Past the cap of asserted subjects nothing more is queued, the runner is held
// not-ready and runner.inventory.hold is audited.
func TestRunnerAssertedPG_CapHoldsTheRunnerAndAudits(t *testing.T) {
	pool, cfg, rn, _, _, _ := runnerExitAPIFixture(t)
	ctx := t.Context()
	srv := New(cfg)
	rows := make([]runnerwire.RunState, types.RunnerAssertedSubjectCap+1)
	for i := range rows {
		rows[i] = erasedRow(rn.ID)
	}
	slices.SortFunc(rows, func(a, b runnerwire.RunState) int { return bytes.Compare(a.RunID[:], b.RunID[:]) })
	session, inventory := uuid.NewString(), uuid.New()
	if err := cfg.Store.(store.PG).BeginRunnerInventory(ctx, rn.ID, session); err != nil {
		t.Fatal(err)
	}
	first := runnerwire.StatePage{InventoryID: inventory, Runs: rows[:types.RunnerAssertedSubjectCap]}
	if err := srv.StatePageFromRunner(ctx, rn.ID, session, first); err != nil {
		t.Fatalf("first page = %v", err)
	}
	last := rows[types.RunnerAssertedSubjectCap]
	final := runnerwire.StatePage{InventoryID: inventory, After: rows[types.RunnerAssertedSubjectCap-1].RunID, Runs: []runnerwire.RunState{last}, Final: true}
	if err := srv.StatePageFromRunner(ctx, rn.ID, session, final); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
		t.Fatalf("over-cap Final = %v, want held", err)
	}
	var subjects, overCap int
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT asserted_run_id), count(*) FILTER (WHERE asserted_run_id=$2) FROM runner_pending_actions WHERE runner_id=$1 AND subject='asserted'`,
		rn.ID, last.RunID).Scan(&subjects, &overCap); err != nil || subjects != types.RunnerAssertedSubjectCap || overCap != 0 {
		t.Fatalf("asserted subjects=%d over-cap rows=%d err=%v", subjects, overCap, err)
	}
	audit := cfg.Audit.(*syncAudit)
	audit.mu.Lock()
	defer audit.mu.Unlock()
	held := 0
	for _, ev := range audit.events {
		if ev.Action == placement.ActionRunnerInventoryHold && ev.Target == rn.ID.String() && ev.Outcome == "denied" {
			held++
		}
	}
	if held != 1 {
		t.Fatalf("%d runner.inventory.hold events, want one", held)
	}
}

// A deferred asserted quarantine continues in exactly one successor, carried
// by the next session's PENDING after a completed inventory; the successor's
// success settles the chain.
func TestRunnerAssertedPG_DeferredQuarantineContinuesInTheNextSession(t *testing.T) {
	pool, cfg, rn, _, _, _ := runnerExitAPIFixture(t)
	ctx := t.Context()
	srv := New(cfg)
	erased := erasedRow(rn.ID)
	if err := finalStatePage(t, srv, rn.ID, erased); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
		t.Fatalf("erased-run Final = %v", err)
	}
	pending, err := srv.PendingFromRunner(ctx, rn.ID)
	if err != nil || len(pending) != 2 {
		t.Fatalf("PENDING = %+v, %v", pending, err)
	}
	applyAll(t, srv, rn.ID, pending, func(a runnerwire.PendingAction) string {
		if a.Kind == types.RunnerActionQuarantine {
			return types.RunnerActionDeferred
		}
		return types.RunnerActionSuccess
	})
	if err := srv.BeginInventoryFromRunner(ctx, rn.ID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	next, err := srv.PendingFromRunner(ctx, rn.ID)
	if err != nil || len(next) != 1 || next[0].Kind != types.RunnerActionQuarantine || next[0].ID == pending[1].ID ||
		next[0].Quarantine == nil || *next[0].Quarantine != *pending[1].Quarantine || next[0].Ref != pending[1].Ref {
		t.Fatalf("successor PENDING = %+v, %v", next, err)
	}
	applyAll(t, srv, rn.ID, next, func(runnerwire.PendingAction) string { return types.RunnerActionSuccess })
	var settlement string
	var by *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT settlement, settled_by FROM runner_pending_actions WHERE id=$1`, pending[1].ID).Scan(&settlement, &by); err != nil ||
		settlement != "chain" || by == nil || *by != next[0].ID {
		t.Fatalf("deferred predecessor settlement=%q by=%v err=%v", settlement, by, err)
	}
}

// A run's own deferred kill continues the same way: once the session that deferred it completed its
// inventory, the next session's PENDING carries one successor with the identical binding.
func TestRunnerAssertedPG_DeferredRunKillContinuesInTheNextSession(t *testing.T) {
	pool, cfg, rn, run, _, _ := runnerExitAPIFixture(t)
	ctx := t.Context()
	srv := New(cfg)
	var gen uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT generation FROM runner_executions WHERE run_id=$1`, run.ID).Scan(&gen); err != nil {
		t.Fatal(err)
	}
	ref := "runner:" + rn.ID.String() + "/sandbox"
	if _, _, err := cfg.Store.(store.PG).QueueRunnerAction(ctx, types.RunnerPendingAction{ID: uuid.New(), RunnerID: rn.ID, RunID: run.ID,
		Kind: types.RunnerActionKill, Ref: ref, Generation: gen}); err != nil {
		t.Fatal(err)
	}
	pending, err := srv.PendingFromRunner(ctx, rn.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("PENDING = %+v, %v", pending, err)
	}
	applyAll(t, srv, rn.ID, pending, func(runnerwire.PendingAction) string { return types.RunnerActionDeferred })
	// The page omits the run, which the org still holds: reconciliation is required, and the inventory completes.
	if err := finalStatePage(t, srv, rn.ID); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
		t.Fatalf("final inventory: %v", err)
	}
	if err := srv.BeginInventoryFromRunner(ctx, rn.ID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	next, err := srv.PendingFromRunner(ctx, rn.ID)
	if err != nil || len(next) != 1 || next[0].Kind != types.RunnerActionKill || next[0].ID == pending[0].ID || next[0].Ref != ref {
		t.Fatalf("successor PENDING = %+v, %v", next, err)
	}
}
