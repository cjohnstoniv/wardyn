// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The run-history retention sweep (WARDYN_RUN_HISTORY_RETENTION_DAYS). Guarded by WARDYN_TEST_PG like the rest of
// this package's Postgres tests; each runs in its own database, because the sweep deletes across the whole table.
package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// retentionWindow is the window every test here sweeps with: a run that ended 40 days ago is past it, one that
// ended 10 days ago is not.
const retentionWindow = 30 * 24 * time.Hour

func retentionExec(t *testing.T, pool *pgxpool.Pool, q string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// endRun sets the run's state and its end time daysAgo days back, and records its sandbox as gone.
func endRun(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, state types.RunState, daysAgo int) {
	t.Helper()
	retentionExec(t, pool, `UPDATE agent_runs SET state = $2, ended_at = now() - make_interval(days => $3), sandbox_ref = '' WHERE id = $1`,
		id, string(state), daysAgo)
}

func runExists(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM agent_runs WHERE id = $1)`, id).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func tombstoneCount(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM run_history_tombstones WHERE run_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func deleteExpired(t *testing.T, st store.PG, limit int) int {
	t.Helper()
	n, err := st.DeleteExpiredRuns(context.Background(), retentionWindow, limit)
	if err != nil {
		t.Fatalf("DeleteExpiredRuns: %v", err)
	}
	return n
}

// An ended run past the window goes with its approval and its model usage, and leaves a tombstone with its unit,
// creation time and end time; a run that ended inside the window stays.
func TestDeleteExpiredRunsCascadesPG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	unit := uuid.New()
	old := newRun(types.RunRunning)
	old.UnitID = &unit
	old = persistRun(t, ctx, pool, old)
	endRun(t, pool, old.ID, types.RunCompleted, 40)
	recent := persistRun(t, ctx, pool, newRun(types.RunRunning))
	endRun(t, pool, recent.ID, types.RunCompleted, 10)

	approvalID, callID := uuid.New(), uuid.New()
	retentionExec(t, pool, `INSERT INTO approvals (id, run_id, kind, requested_scope) VALUES ($1, $2, 'egress_domain', '{}')`, approvalID, old.ID)
	retentionExec(t, pool, `INSERT INTO run_model_usage (call_id, run_id, at, vendor, host, state) VALUES ($1, $2, now(), 'anthropic', 'api.anthropic.com', 'ok')`, callID, old.ID)

	var createdAt, endedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT created_at, ended_at FROM agent_runs WHERE id = $1`, old.ID).Scan(&createdAt, &endedAt); err != nil {
		t.Fatal(err)
	}

	if n := deleteExpired(t, st, 100); n != 1 {
		t.Fatalf("deleted %d runs, want 1", n)
	}
	if runExists(t, pool, old.ID) {
		t.Error("the run 40 days past its end is still there")
	}
	if !runExists(t, pool, recent.ID) {
		t.Error("the run 10 days past its end was deleted")
	}
	var approvals, usage int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM approvals WHERE id = $1), (SELECT count(*) FROM run_model_usage WHERE call_id = $2)`,
		approvalID, callID).Scan(&approvals, &usage); err != nil {
		t.Fatal(err)
	}
	if approvals != 0 || usage != 0 {
		t.Errorf("approvals = %d, usage rows = %d after the delete, want 0 and 0", approvals, usage)
	}

	var gotUnit *uuid.UUID
	var gotCreated, gotEnded time.Time
	if err := pool.QueryRow(ctx, `SELECT unit_id, created_at, ended_at FROM run_history_tombstones WHERE run_id = $1`, old.ID).
		Scan(&gotUnit, &gotCreated, &gotEnded); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	if gotUnit == nil || *gotUnit != unit || !gotCreated.Equal(createdAt) || !gotEnded.Equal(endedAt) {
		t.Errorf("tombstone = unit %v, created %v, ended %v; want %v, %v, %v", gotUnit, gotCreated, gotEnded, unit, createdAt, endedAt)
	}
	if tombstoneCount(t, pool, recent.ID) != 0 {
		t.Error("the kept run has a tombstone")
	}
}

// Nothing live and nothing held is deleted, however old: the function returns 0 and every run stays.
func TestDeleteExpiredRunsKeepsLiveAndHeldPG(t *testing.T) {
	f := newActionFixture(t)
	pool := f.pool
	var kept []uuid.UUID
	ended := func(state types.RunState) types.AgentRun {
		t.Helper()
		run := f.run(t)
		endRun(t, pool, run.ID, state, 40)
		kept = append(kept, run.ID)
		return run
	}

	// A lease-ended run is still RUNNING: its end time is lost_at, but it is not terminal.
	lost := f.run(t)
	retentionExec(t, pool, `UPDATE agent_runs SET lost_reason = 'ended', lost_at = now() - interval '40 days', sandbox_ref = '' WHERE id = $1`, lost.ID)
	kept = append(kept, lost.ID)
	ended(types.RunQueued)
	failed := f.run(t)
	retentionExec(t, pool, `UPDATE agent_runs SET state = 'FAILED', ended_at = NULL, sandbox_ref = '' WHERE id = $1`, failed.ID)
	kept = append(kept, failed.ID)

	pushRun := ended(types.RunCompleted)
	pushApproval := uuid.New()
	retentionExec(t, pool, `INSERT INTO approvals (id, run_id, kind, requested_scope) VALUES ($1, $2, 'push_content', '{}')`, pushApproval, pushRun.ID)
	retentionExec(t, pool, `INSERT INTO push_content_paths (approval_id, paths, truncated) VALUES ($1, '{README.md}', false)`, pushApproval)

	// Each mcp_call_sends arm on its own run: one send names the run but rides a live run's approval; the other
	// rides the run's approval but names the live run.
	mcpLive := f.run(t)
	mcpSend := func(approvalRun, sendRun uuid.UUID) {
		t.Helper()
		approval := uuid.New()
		retentionExec(t, pool, `INSERT INTO approvals (id, run_id, kind, requested_scope) VALUES ($1, $2, 'tool_call', '{}')`, approval, approvalRun)
		retentionExec(t, pool, `INSERT INTO mcp_call_sends (owner, operation_scope, operation_key, approval_id, run_id, call_sha256, sent_at)
			VALUES ('owner', 'scope', $1, $2, $3, 'sha', now())`, uuid.NewString(), approval, sendRun)
	}
	mcpNamed := ended(types.RunCompleted)
	mcpSend(mcpLive.ID, mcpNamed.ID)
	mcpApproved := ended(types.RunCompleted)
	mcpSend(mcpApproved.ID, mcpLive.ID)

	retired := ended(types.RunCompleted)
	retentionExec(t, pool, `INSERT INTO runner_retirements (run_id, runner_id, obligation_id, ref, state, settled_at)
		VALUES ($1, $2, $3, $4, 'settled', now())`, retired.ID, f.rn.ID, uuid.New(), retired.SandboxRef)
	watermarked := ended(types.RunCompleted)
	retentionExec(t, pool, `INSERT INTO runner_output_watermarks (run_id, capture) VALUES ($1, true)`, watermarked.ID)
	resident := ended(types.RunCompleted)
	retentionExec(t, pool, `INSERT INTO runner_resident_runs (run_id, runner_id, not_after) VALUES ($1, $2, now() + interval '1 day')`, resident.ID, f.rn.ID)

	keptMachine := ended(types.RunStopped)
	retentionExec(t, pool, `INSERT INTO kept_machines (id, name, owner_subject, runner_id, base, base_prepared_at, state, run_id, expires_at)
		VALUES ($1, 'machine-a', 'owner', $2, 'base', now(), 'kept', $3, now() + interval '1 day')`, uuid.New(), f.rn.ID, keptMachine.ID)
	freshStart := ended(types.RunStopped)
	retentionExec(t, pool, `INSERT INTO kept_machines (id, name, owner_subject, runner_id, base, base_prepared_at, state, reset_run_id, expires_at)
		VALUES ($1, 'machine-b', 'owner', $2, 'base', now(), 'kept', $3, now() + interval '1 day')`, uuid.New(), f.rn.ID, freshStart.ID)
	workloadRun := ended(types.RunKilled)
	retentionExec(t, pool, `INSERT INTO workloads (owner, name, source, intent, digest, created_by, current_run_id)
		VALUES ('owner', 'workload-a', 'api', '{}', 'digest', 'owner', $1)`, workloadRun.ID)
	heldTeardown := ended(types.RunKilled)
	retentionExec(t, pool, `INSERT INTO run_teardown_holds (run_id, runner_id, kind, since) VALUES ($1, $2, 'kill', now())`, heldTeardown.ID, f.rn.ID)
	// A sandbox never confirmed gone: the terminal sandbox sweep still finds it by this row.
	sandboxLeft := ended(types.RunCompleted)
	retentionExec(t, pool, `UPDATE agent_runs SET sandbox_ref = $2 WHERE id = $1`, sandboxLeft.ID, sandboxLeft.SandboxRef)
	// An owed end still waits for this run, the replacement, to be running (the row is dropped once processed).
	replacement := ended(types.RunFailed)
	ownedEnd := f.run(t)
	retentionExec(t, pool, `INSERT INTO workload_run_ends (run_id, owner, name, generation, reason, after_run_id)
		VALUES ($1, 'owner', 'workload-b', 1, 'workload_replaced', $2)`, ownedEnd.ID, replacement.ID)
	containment := ended(types.RunKilled)
	retentionExec(t, pool, `INSERT INTO run_containment_pending (run_id, runner_id, kind, since) VALUES ($1, $2, 'agent_only', now())`,
		containment.ID, f.rn.ID)

	// An asserted subject's action (no run) supersedes one of this run's.
	superseded := ended(types.RunFailed)
	actionA := uuid.New()
	retentionExec(t, pool, `INSERT INTO runner_pending_actions (id, runner_id, run_id, kind, ref) VALUES ($1, $2, $3, 'kill', $4)`,
		actionA, f.rn.ID, superseded.ID, superseded.SandboxRef)
	retentionExec(t, pool, `INSERT INTO runner_pending_actions (id, runner_id, subject, asserted_run_id, kind, ref, binding_version, owner, supersedes)
		VALUES ($1, $2, 'asserted', $3, 'kill', $4, 1, '', $5)`, uuid.New(), f.rn.ID, uuid.New(), "runner:"+f.rn.ID.String()+"/local", actionA)

	// Another, live run's action settles one of this run's.
	settled := ended(types.RunFailed)
	live := f.run(t)
	actionS := uuid.New()
	retentionExec(t, pool, `INSERT INTO runner_pending_actions (id, runner_id, run_id, kind, ref) VALUES ($1, $2, $3, 'kill', $4)`,
		actionS, f.rn.ID, settled.ID, settled.SandboxRef)
	retentionExec(t, pool, `INSERT INTO runner_pending_actions (id, runner_id, run_id, kind, ref, binding_version, owner, settlement, settled_at, settled_by)
		VALUES ($1, $2, $3, 'kill', $4, 1, $5, 'chain', now(), $6)`, uuid.New(), f.rn.ID, live.ID, live.SandboxRef, f.rn.Owner, actionS)

	if n := deleteExpired(t, f.st, 500); n != 0 {
		t.Fatalf("deleted %d runs, want 0", n)
	}
	for _, id := range kept {
		if !runExists(t, pool, id) {
			t.Errorf("run %s was deleted", id)
		}
		if tombstoneCount(t, pool, id) != 0 {
			t.Errorf("run %s has a tombstone", id)
		}
	}
}

// A batch that holds a run whose action another, live run's action supersedes keeps that run and deletes the
// others, with no foreign-key error. Without the runner_pending_actions arm of the held predicate this test fails
// with SQLSTATE 23503 (the supersedes reference), and the whole batch with it.
func TestDeleteExpiredRunsCrossRunActionDoesNotAbortTheBatchPG(t *testing.T) {
	f := newActionFixture(t)
	pool := f.pool
	runs := []types.AgentRun{f.run(t), f.run(t), f.run(t)}
	for _, r := range runs {
		endRun(t, pool, r.ID, types.RunCompleted, 40)
	}
	slices.SortFunc(runs, func(a, b types.AgentRun) int { return bytes.Compare(a.ID[:], b.ID[:]) })
	held := runs[1]
	live := f.run(t)
	actionA := uuid.New()
	retentionExec(t, pool, `INSERT INTO runner_pending_actions (id, runner_id, run_id, kind, ref) VALUES ($1, $2, $3, 'kill', $4)`,
		actionA, f.rn.ID, held.ID, held.SandboxRef)
	retentionExec(t, pool, `INSERT INTO runner_pending_actions (id, runner_id, run_id, kind, ref, binding_version, owner, supersedes)
		VALUES ($1, $2, $3, 'kill', $4, 1, $5, $6)`, uuid.New(), f.rn.ID, live.ID, live.SandboxRef, f.rn.Owner, actionA)

	n, err := f.st.DeleteExpiredRuns(context.Background(), retentionWindow, 500)
	if err != nil {
		t.Fatalf("DeleteExpiredRuns: %v (a 23503 here is a reference the held predicate misses)", err)
	}
	if n != 2 {
		t.Fatalf("deleted %d runs, want 2", n)
	}
	for _, r := range []types.AgentRun{runs[0], runs[2]} {
		if runExists(t, pool, r.ID) || tombstoneCount(t, pool, r.ID) != 1 {
			t.Errorf("run %s: still there, or no single tombstone", r.ID)
		}
	}
	if !runExists(t, pool, held.ID) || tombstoneCount(t, pool, held.ID) != 0 {
		t.Errorf("the held run %s was deleted or has a tombstone", held.ID)
	}
	if n := deleteExpired(t, f.st, 500); n != 0 {
		t.Fatalf("second call deleted %d runs, want 0", n)
	}
}

// The limit bounds each call: seven expired runs with a limit of 3 go 3, 3, 1, then 0.
func TestDeleteExpiredRunsLimitPG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	for range 7 {
		r := persistRun(t, ctx, pool, newRun(types.RunRunning))
		endRun(t, pool, r.ID, types.RunStopped, 40)
	}
	var got []int
	for range 4 {
		got = append(got, deleteExpired(t, st, 3))
	}
	if !slices.Equal(got, []int{3, 3, 1, 0}) {
		t.Fatalf("deleted %v, want [3 3 1 0]", got)
	}
}

// A unit admin still reads a deleted run's audit rows through its tombstone, and never another unit's.
func TestQueryAuditEventsForUnitsReadsATombstonedRunPG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	u1, u2 := uuid.New(), uuid.New()
	runIn := func(unit uuid.UUID) uuid.UUID {
		r := newRun(types.RunRunning)
		r.UnitID = &unit
		id := persistRun(t, ctx, pool, r).ID
		endRun(t, pool, id, types.RunCompleted, 40)
		return id
	}
	r1, r2 := runIn(u1), runIn(u2)
	want := map[uuid.UUID]bool{}
	for _, run := range []uuid.UUID{r1, r2} {
		ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: &run, ActorType: types.ActorHuman,
			Actor: "alice@corp.example", Action: "unit.probe", Outcome: "success"}
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if run == r1 {
			want[ev.ID] = true
		}
	}
	if n := deleteExpired(t, st, 100); n != 2 {
		t.Fatalf("deleted %d runs, want 2", n)
	}
	got, err := st.QueryAuditEventsForUnits(ctx, []uuid.UUID{u1}, store.AuditFilter{}, store.Page{Limit: 100})
	if err != nil {
		t.Fatalf("unit read: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("unit read returned %d rows, want the %d of the deleted run of the unit", len(got), len(want))
	}
	for _, ev := range got {
		if !want[ev.ID] {
			t.Errorf("unit read returned a row of run %v, outside the unit", ev.RunID)
		}
	}
}

// A device may not file a row under a run this organisation deleted: the tombstone still names it.
func TestRefuseOrgRunsRefusesATombstonedRunPG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	endRun(t, pool, run.ID, types.RunCompleted, 40)
	if n := deleteExpired(t, st, 100); n != 1 {
		t.Fatalf("deleted %d runs, want 1", n)
	}
	d, err := st.CreateDevice(ctx, types.Device{ID: uuid.New(), Name: "laptop"}, "wdd_"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	ev := types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC().Truncate(time.Microsecond), RunID: &run.ID,
		ActorType: types.ActorHuman, Actor: "ceo@corp.example", Action: "approval.decide",
		Target: "approval:" + uuid.NewString(), Outcome: "success", SourceIP: "10.0.0.1",
		Data: json.RawMessage(`{"decision":"APPROVED"}`),
	}
	ev.RowHash = auditRowHash(t, pool, "", ev)
	_, err = st.IngestDeviceAudit(ctx, d.ID, testPeer, []types.FederatedAuditEvent{{AuditEvent: ev, Seq: 1}})
	if !errors.Is(err, store.ErrFederatedOrgRun) {
		t.Fatalf("ingest of a row under deleted run %s: err = %v, want ErrFederatedOrgRun", run.ID, err)
	}
}

// A batch statement runs under its statement timeout: one that waits past it on a lock is cancelled (SQLSTATE
// 57014) instead of holding the sweep, and the run stays for the next tick.
func TestDeleteExpiredRunsBatchHasAStatementTimeoutPG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	endRun(t, pool, run.ID, types.RunCompleted, 40)
	store.SetRunRetentionStatementTimeout(t, "300ms")

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `LOCK TABLE approvals IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = st.DeleteExpiredRuns(ctx, retentionWindow, 10)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("DeleteExpiredRuns under a held lock = %v, want a statement timeout (57014)", err)
	}
	if waited := time.Since(start); waited > 30*time.Second {
		t.Fatalf("the cancelled batch took %v", waited)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if !runExists(t, pool, run.ID) || tombstoneCount(t, pool, run.ID) != 0 {
		t.Fatal("the cancelled batch deleted the run or left a tombstone")
	}
}
