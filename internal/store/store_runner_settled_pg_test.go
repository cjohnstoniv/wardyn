// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type settledFixture struct {
	pool *pgxpool.Pool
	st   store.PG
	rn   types.Runner
	run  types.AgentRun
	row  runnerspool.Record
	exit runnerwire.AgentExit
}

// newSettledFixture binds an execution generation to the spool fixture's run.
func newSettledFixture(t *testing.T) settledFixture {
	t.Helper()
	pool, st, rn, run, row := spoolPGFixture(t)
	ctx := context.Background()
	gen := uuid.New()
	ref := "runner:" + rn.ID.String() + "/agent"
	if err := st.SetSandboxRef(ctx, run.ID, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginRunnerExecution(ctx, rn.ID, run.ID, uuid.Nil, gen, runnerwire.MethodExec); err != nil {
		t.Fatal(err)
	}
	if err := st.BindRunnerExecution(ctx, rn.ID, run.ID, gen, "exec"); err != nil {
		t.Fatal(err)
	}
	exit := runnerwire.AgentExit{Generation: gen, ExecID: "exec", Ref: ref, ObservedAt: time.Now()}
	return settledFixture{pool: pool, st: st, rn: rn, run: run, row: row, exit: exit}
}

func (f settledFixture) exec(t *testing.T, query string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), query, f.run.ID); err != nil {
		t.Fatal(err)
	}
}

func (f settledFixture) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), query, f.run.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f settledFixture) runJSON(t *testing.T) string {
	t.Helper()
	run, err := f.st.GetRun(context.Background(), f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(run)
	return string(b)
}

func claimedRunner(t *testing.T, st store.PG, owner string) types.Runner {
	t.Helper()
	ctx := context.Background()
	rn, err := st.CreateRunner(ctx, newRunner(owner))
	if err != nil {
		t.Fatal(err)
	}
	rn, err = st.ClaimRunner(ctx, rn.ID, rn.Owner, rn.KeyFingerprint, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return rn
}

const (
	stagedExitsSQL  = `SELECT count(*) FROM runner_execution_exits WHERE run_id=$1`
	spooledAuditSQL = `SELECT count(*) FROM audit_events WHERE run_id=$1 AND data->>'spooled'='true'`
)

// Characterization: runner, owner, generation and lifecycle-phase refusals
// hold for a settled run exactly as for a running one.
func TestPG_RunnerSettledRecordsStillRefuseForeignBinding(t *testing.T) {
	for _, state := range []string{"RUNNING", "COMPLETED", "KILLED"} {
		t.Run(state, func(t *testing.T) {
			f := newSettledFixture(t)
			ctx := context.Background()
			f.exec(t, `UPDATE agent_runs SET state='`+state+`' WHERE id=$1`)
			other := claimedRunner(t, f.st, f.rn.Owner)
			if err := f.st.StageRunnerExit(ctx, other.ID, f.run.ID, f.exit); !errors.Is(err, store.ErrRunnerExecutionRefused) {
				t.Fatalf("exit for another runner's run accepted: %v", err)
			}
			if _, err := f.st.IngestRunnerDecision(ctx, other.ID, f.row, prepareSpoolTest); !errors.Is(err, store.ErrRunnerSpoolRefused) {
				t.Fatalf("spool for another runner's run accepted: %v", err)
			}
			wrongGen := f.exit
			wrongGen.Generation = uuid.New()
			if err := f.st.StageRunnerExit(ctx, f.rn.ID, f.run.ID, wrongGen); !errors.Is(err, store.ErrRunnerExecutionRefused) {
				t.Fatalf("exit for an unissued generation accepted: %v", err)
			}
			wrongRef := f.exit
			wrongRef.Ref = "runner:" + other.ID.String() + "/agent"
			if err := f.st.StageRunnerExit(ctx, f.rn.ID, f.run.ID, wrongRef); !errors.Is(err, store.ErrRunnerExecutionRefused) {
				t.Fatalf("exit for another sandbox accepted: %v", err)
			}
			f.exec(t, `UPDATE agent_runs SET created_by='other' WHERE id=$1`)
			if err := f.st.StageRunnerExit(ctx, f.rn.ID, f.run.ID, f.exit); !errors.Is(err, store.ErrRunnerExecutionRefused) {
				t.Fatalf("exit for another owner's run accepted: %v", err)
			}
			if _, err := f.st.IngestRunnerDecision(ctx, f.rn.ID, f.row, prepareSpoolTest); !errors.Is(err, store.ErrRunnerSpoolRefused) {
				t.Fatalf("spool for another owner's run accepted: %v", err)
			}
			if _, err := f.pool.Exec(ctx, `UPDATE agent_runs SET created_by=$2 WHERE id=$1`, f.run.ID, f.rn.Owner); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(ctx, `UPDATE runners SET state='revoked',revoked_at=clock_timestamp() WHERE id=$1`, f.rn.ID); err != nil {
				t.Fatal(err)
			}
			if err := f.st.StageRunnerExit(ctx, f.rn.ID, f.run.ID, f.exit); !errors.Is(err, store.ErrRunnerExecutionRefused) {
				t.Fatalf("exit from a revoked runner accepted: %v", err)
			}
			if _, err := f.st.IngestRunnerDecision(ctx, f.rn.ID, f.row, prepareSpoolTest); !errors.Is(err, store.ErrRunnerSpoolRefused) {
				t.Fatalf("spool from a revoked runner accepted: %v", err)
			}
			if f.count(t, stagedExitsSQL) != 0 || f.count(t, spooledAuditSQL) != 0 {
				t.Fatal("refused record left a receipt or audit")
			}
		})
	}
}

// Characterization: a run that has not started yet takes no agent exit (none
// exists before RUNNING), and an interactive run kept through an outage takes
// its agent exit as a receipt only (DR-0436): acknowledged, never exit work.
func TestPG_RunnerUnstartedExitRefusedAndKeptInteractiveExitReceiptOnly(t *testing.T) {
	f := newSettledFixture(t)
	ctx := context.Background()
	f.exec(t, `UPDATE agent_runs SET state='STARTING' WHERE id=$1`)
	if err := f.st.StageRunnerExit(ctx, f.rn.ID, f.run.ID, f.exit); !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.Fatalf("exit for an unstarted run accepted: %v", err)
	}
	if f.count(t, stagedExitsSQL) != 0 {
		t.Fatal("refused exit left a receipt")
	}
	f.exec(t, `UPDATE agent_runs SET state='RUNNING',interactive=true,lost_at=now(),lost_reason='outage' WHERE id=$1`)
	if err := f.st.StageRunnerExit(ctx, f.rn.ID, f.run.ID, f.exit); err != nil {
		t.Fatalf("exit for a kept interactive run = %v, want acknowledged", err)
	}
	if n := f.count(t, stagedExitsSQL+` AND receipt_only AND tail_complete AND accepted_at IS NULL`); n != 1 || f.count(t, stagedExitsSQL) != 1 {
		t.Fatalf("kept interactive exit left %d receipt-only rows, want 1", n)
	}
	if work, err := f.st.ListRunnerExitWork(ctx, uuid.Nil); err != nil || len(work) != 0 {
		t.Fatalf("kept interactive exit became exit work: %+v %v", work, err)
	}
}

// spoolRecords appends one decision of each kind given to a fresh spool of
// runID, as the run's proxy does, and returns its records in order.
func spoolRecords(t *testing.T, runID uuid.UUID, decisions ...egress.Decision) []runnerspool.Record {
	t.Helper()
	ctx := context.Background()
	spool, err := runnerspool.Open(filepath.Join(t.TempDir(), "spool"), runID)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	for _, d := range decisions {
		if err := spool.Append(ctx, egress.DecisionLog{Request: egress.Request{RunID: runID, Host: "allowed.test", Time: time.Now().UTC().Truncate(time.Microsecond)}, Decision: d, RuleSource: "policy:allowed"}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := spool.After(ctx, 0, len(decisions))
	if err != nil || len(rows) != len(decisions) {
		t.Fatalf("spool rows %d %v, want %d", len(rows), err, len(decisions))
	}
	return rows
}

// X1 (DR-0436): the exit of a run kept through a reboot is a receipt, not a
// refusal: acknowledged, stored receipt-only and tail-complete, never exit
// work, and it does not block the revive's next generation. Replayed at every
// step, it is acknowledged against the one row; the run's later spool records
// and the next generation's real exit are taken as before.
func TestPG_RunnerKeptRebootExitIsReceiptReviveBegins(t *testing.T) {
	f := newSettledFixture(t)
	ctx := context.Background()
	if ok, err := f.st.MarkRunLost(ctx, f.run.ID, types.LostReboot, time.Now(), 0); err != nil || !ok {
		t.Fatalf("mark lost %v %v", ok, err)
	}
	stage := func(step string) {
		t.Helper()
		if err := f.st.StageRunnerExit(ctx, f.rn.ID, f.run.ID, f.exit); err != nil {
			t.Fatalf("%s: kept run's exit = %v, want acknowledged", step, err)
		}
		var all, receipts int
		if err := f.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE receipt_only AND tail_complete AND accepted_at IS NULL AND parked_at IS NULL)
			FROM runner_execution_exits WHERE run_id=$1 AND generation=$2`, f.run.ID, f.exit.Generation).Scan(&all, &receipts); err != nil {
			t.Fatal(err)
		}
		if all != 1 || receipts != 1 || f.count(t, stagedExitsSQL) != 1 {
			t.Fatalf("%s: %d rows for the generation, %d receipt-only and tail-complete, want one", step, all, receipts)
		}
		if work, err := f.st.ListRunnerExitWork(ctx, uuid.Nil); err != nil || len(work) != 0 {
			t.Fatalf("%s: kept exit became exit work: %+v %v", step, work, err)
		}
	}
	stage("lost")
	if ok, err := f.st.MarkRunRevived(ctx, f.run.ID, types.LostReboot, nil, 0, false); err != nil || !ok {
		t.Fatalf("revive %v %v", ok, err)
	}
	stage("revived")
	next := uuid.New()
	if _, err := f.st.BeginRunnerExecution(ctx, f.rn.ID, f.run.ID, f.exit.Generation, next, runnerwire.MethodExec); err != nil {
		t.Fatalf("revive's next generation refused after the receipt: %v", err)
	}
	stage("next generation")
	if ok, err := f.st.IngestRunnerDecision(ctx, f.rn.ID, f.row, prepareSpoolTest); err != nil || !ok {
		t.Fatalf("spool after the receipt: first=%v err=%v, want accepted", ok, err)
	}
	if err := f.st.BindRunnerExecution(ctx, f.rn.ID, f.run.ID, next, "exec-2"); err != nil {
		t.Fatal(err)
	}
	nextExit := runnerwire.AgentExit{Generation: next, ExecID: "exec-2", Ref: f.exit.Ref, ObservedAt: time.Now()}
	if err := f.st.StageRunnerExit(ctx, f.rn.ID, f.run.ID, nextExit); err != nil {
		t.Fatalf("next generation's exit: %v", err)
	}
	if work, err := f.st.ListRunnerExitWork(ctx, uuid.Nil); err != nil || len(work) != 1 || work[0].Exit.Generation != next {
		t.Fatalf("exit work = %+v %v, want the next generation's exit", work, err)
	}
	if ok, err := f.st.AcceptRunnerExit(ctx, f.rn.ID, f.run.ID, nextExit, prepareSpoolTest); err != nil || !ok {
		t.Fatalf("next generation's exit accepted=%v err=%v", ok, err)
	}
}

// X2 (DR-0436): a run kept through a reboot whose exit was received and that is
// then killed names that exit in its retirement receipt, so the runner's
// retirement proof matches the exit it holds.
func TestPG_RunnerKeptExitNamedByRetirementReceipt(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	run := f.run(t)
	gen := f.issue(t, run)
	if _, err := f.pool.Exec(ctx, `UPDATE runner_executions SET exec_id = 'agent' WHERE run_id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.st.InsertRunnerOutputWatermark(ctx, run.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO runner_spool_cursors (run_id, runner_id, spool_id) VALUES ($1, $2, $3)`, run.ID, f.rn.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.st.MarkRunLost(ctx, run.ID, types.LostReboot, time.Now(), 0); err != nil || !ok {
		t.Fatalf("mark lost %v %v", ok, err)
	}
	exit := runnerwire.AgentExit{Generation: gen, ExecID: "agent", Ref: run.SandboxRef, Code: 0, ObservedAt: time.Now().UTC().Truncate(time.Microsecond)}
	if err := f.st.StageRunnerExit(ctx, f.rn.ID, run.ID, exit); err != nil {
		t.Errorf("kept run's exit = %v, want acknowledged", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE agent_runs SET state = 'KILLED' WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	f.advance(t, run.ID, store.RetirementKill)
	if err := f.result(t, f.tip(t, run.ID), types.RunnerActionSuccess); err != nil {
		t.Fatal(err)
	}
	f.advance(t, run.ID, store.RetirementRetire)
	receipt := f.tip(t, run.ID).Retirement
	b, err := json.Marshal(exit)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if want := hex.EncodeToString(sum[:]); receipt.ExitHash != want || receipt.Generation != gen || receipt.ExecID != "agent" {
		t.Fatalf("receipt = %+v, want the kept exit's hash %s", receipt, want)
	}
}

// P1 (DR-0436): a run's spool is taken from STARTING, when its proxy starts,
// and stays one stream through RUNNING, a loss and a revive: no record is
// refused and none leaves a gap.
func TestPG_RunnerSpoolFromStartingThroughReviveHasNoGap(t *testing.T) {
	f := newSettledFixture(t)
	ctx := context.Background()
	rows := spoolRecords(t, f.run.ID, egress.Allow, egress.Allow, egress.Allow)
	ingest := func(i int) {
		t.Helper()
		if ok, err := f.st.IngestRunnerDecision(ctx, f.rn.ID, rows[i], prepareSpoolTest); err != nil || !ok {
			t.Fatalf("spool seq %d: first=%v err=%v, want accepted", rows[i].Seq, ok, err)
		}
	}
	f.exec(t, `UPDATE agent_runs SET state='STARTING' WHERE id=$1`)
	ingest(0)
	if ok, err := f.st.UpdateRunStateIf(ctx, f.run.ID, types.RunStarting, types.RunRunning); err != nil || !ok {
		t.Fatalf("STARTING to RUNNING %v %v", ok, err)
	}
	ingest(1)
	if ok, err := f.st.MarkRunLost(ctx, f.run.ID, types.LostOutage, time.Now(), 0); err != nil || !ok {
		t.Fatalf("mark lost %v %v", ok, err)
	}
	if ok, err := f.st.MarkRunRevived(ctx, f.run.ID, types.LostOutage, nil, 0, false); err != nil || !ok {
		t.Fatalf("revive %v %v", ok, err)
	}
	ingest(2)
	if seq := f.count(t, `SELECT last_seq FROM runner_spool_cursors WHERE run_id=$1`); seq != 3 {
		t.Fatalf("spool cursor at %d, want 3", seq)
	}
	if n := f.count(t, spooledAuditSQL); n != 3 {
		t.Fatalf("spooled audit rows %d, want 3", n)
	}
}

// A spool record arriving after the org accepted the run's exit is audit-only
// history: acknowledged, audited once, and the run is untouched.
func TestPG_RunnerSpoolAfterAcceptedExitIsAuditedHistory(t *testing.T) {
	f := newSettledFixture(t)
	ctx := context.Background()
	if ok, err := f.st.AcceptRunnerExit(ctx, f.rn.ID, f.run.ID, f.exit, prepareSpoolTest); err != nil || !ok {
		t.Fatalf("accept exit %v %v", ok, err)
	}
	before := f.runJSON(t)
	if ok, err := f.st.IngestRunnerDecision(ctx, f.rn.ID, f.row, prepareSpoolTest); err != nil || !ok {
		t.Fatalf("spool after accepted exit refused: %v %v", ok, err)
	}
	if ok, err := f.st.IngestRunnerDecision(ctx, f.rn.ID, f.row, prepareSpoolTest); err != nil || ok {
		t.Fatalf("duplicate spool record: %v %v", ok, err)
	}
	if after := f.runJSON(t); after != before {
		t.Fatalf("spool history changed the settled run:\nbefore%s\nafter%s", before, after)
	}
	if n := f.count(t, spooledAuditSQL); n != 1 {
		t.Fatalf("spooled audit rows %d, want 1", n)
	}
	for _, state := range []string{"FAILED", "STOPPED", "ARCHIVED"} {
		g := newSettledFixture(t)
		g.exec(t, `UPDATE agent_runs SET state='`+state+`' WHERE id=$1`)
		if ok, err := g.st.IngestRunnerDecision(ctx, g.rn.ID, g.row, prepareSpoolTest); err != nil || !ok {
			t.Fatalf("spool for a %s run refused: %v %v", state, ok, err)
		}
	}
}

// An agent exit for a run the org already settled is acknowledged with no
// transition, and nothing is left for the exit sweeper.
func TestPG_RunnerExitAfterSettlementAcknowledgedWithoutTransition(t *testing.T) {
	for _, scenario := range []string{"lease-ended", "killed", "completed", "stopped", "failed-ref-cleared"} {
		t.Run(scenario, func(t *testing.T) {
			f := newSettledFixture(t)
			ctx := context.Background()
			switch scenario {
			case "failed-ref-cleared":
				// A finalize cleared the ref; the runner's exit still names it.
				f.exec(t, `UPDATE agent_runs SET state='FAILED',sandbox_ref='' WHERE id=$1`)
			case "lease-ended":
				f.exec(t, `UPDATE agent_runs SET ends_at=now()-interval '1 minute' WHERE id=$1`)
				if ok, err := f.st.MarkRunEnded(ctx, f.run.ID, time.Now()); err != nil || !ok {
					t.Fatalf("mark ended %v %v", ok, err)
				}
			case "killed":
				f.exec(t, `UPDATE agent_runs SET state='KILLED' WHERE id=$1`)
			case "completed":
				f.exec(t, `UPDATE agent_runs SET state='COMPLETED',ended_at=now() WHERE id=$1`)
			case "stopped":
				f.exec(t, `UPDATE agent_runs SET state='STOPPED',ended_at=now() WHERE id=$1`)
			}
			before := f.runJSON(t)
			if err := f.st.StageRunnerExit(ctx, f.rn.ID, f.run.ID, f.exit); err != nil {
				t.Fatalf("exit for a settled run refused: %v", err)
			}
			if err := f.st.StageRunnerExit(ctx, f.rn.ID, f.run.ID, f.exit); err != nil {
				t.Fatalf("repeated exit for a settled run refused: %v", err)
			}
			if after := f.runJSON(t); after != before {
				t.Fatalf("exit changed the settled run:\nbefore%s\nafter%s", before, after)
			}
			// A settled run keeps the exit as a receipt-only row (its
			// retirement receipt names it); it is never work or accepted. One
			// ended at its lease keeps it tail-complete too (DR-0436).
			if n := f.count(t, stagedExitsSQL+` AND receipt_only AND accepted_at IS NULL`); n != 1 || f.count(t, stagedExitsSQL) != 1 {
				t.Fatalf("settled exit left %d receipt-only rows, want 1", n)
			}
			if scenario == "lease-ended" && f.count(t, stagedExitsSQL+` AND tail_complete`) != 1 {
				t.Fatal("lease-ended exit receipt is not tail-complete")
			}
			if work, err := f.st.ListRunnerExitWork(ctx, uuid.Nil); err != nil || len(work) != 0 {
				t.Fatalf("settled exit became exit work: %+v %v", work, err)
			}
			if n := f.count(t, `SELECT count(*) FROM audit_events WHERE run_id=$1`); n != 0 {
				t.Fatalf("settled exit authored %d audit rows", n)
			}
		})
	}
}

// AcceptRunnerExit stamps ended_at at the terminal transition, so the run is
// listed under the default ended-within window.
func TestPG_RunnerAcceptedExitStampsEndedAt(t *testing.T) {
	f := newSettledFixture(t)
	ctx := context.Background()
	if ok, err := f.st.AcceptRunnerExit(ctx, f.rn.ID, f.run.ID, f.exit, prepareSpoolTest); err != nil || !ok {
		t.Fatalf("accept exit %v %v", ok, err)
	}
	got, err := f.st.GetRun(ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.RunCompleted || got.EndedAt == nil || time.Since(*got.EndedAt) > time.Minute {
		t.Fatalf("accepted exit state=%s ended_at=%v", got.State, got.EndedAt)
	}
	listed, err := f.st.ListRunsFiltered(ctx, store.RunFilter{Owner: f.run.CreatedBy, EndedWithin: 7 * 24 * time.Hour}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != f.run.ID {
		t.Fatalf("ended_within=7d lists %d runs, want the accepted run", len(listed))
	}
}

// TouchRunnerActivity bounds its row-lock wait like the other read-loop
// callbacks instead of holding the runner's read loop until its deadline.
func TestPG_RunnerCurrentActivityBoundsLockWait(t *testing.T) {
	f := newSettledFixture(t)
	ctx := context.Background()
	// The hold is on a pool of its own: the touch asks f.pool for a connection on this goroutine (WARDYN_TEST_POOL_GUARD).
	holder, err := pgxpool.New(ctx, f.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // test cleanup
	if _, err := tx.Exec(ctx, `SELECT 1 FROM agent_runs WHERE id=$1 FOR UPDATE`, f.run.ID); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = f.st.TouchRunnerActivity(bounded, f.rn.ID, f.run.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("activity lock wait not bounded by lock_timeout: %v", err)
	}
}
