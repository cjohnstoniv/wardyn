// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

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

// Characterization: a run that has not started yet takes no spool history or
// agent exit, and an interactive run kept through an outage still refuses an
// agent exit.
func TestPG_RunnerUnstartedSpoolAndKeptInteractiveExitStillRefused(t *testing.T) {
	f := newSettledFixture(t)
	ctx := context.Background()
	f.exec(t, `UPDATE agent_runs SET state='STARTING' WHERE id=$1`)
	if _, err := f.st.IngestRunnerDecision(ctx, f.rn.ID, f.row, prepareSpoolTest); !errors.Is(err, store.ErrRunnerSpoolRefused) {
		t.Fatalf("spool for an unstarted run accepted: %v", err)
	}
	if err := f.st.StageRunnerExit(ctx, f.rn.ID, f.run.ID, f.exit); !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.Fatalf("exit for an unstarted run accepted: %v", err)
	}
	f.exec(t, `UPDATE agent_runs SET state='RUNNING',interactive=true,lost_at=now(),lost_reason='outage' WHERE id=$1`)
	if err := f.st.StageRunnerExit(ctx, f.rn.ID, f.run.ID, f.exit); !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.Fatalf("exit for a kept interactive run accepted: %v", err)
	}
	if f.count(t, stagedExitsSQL) != 0 || f.count(t, spooledAuditSQL) != 0 {
		t.Fatal("refused record left a receipt or audit")
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
// receipt and no transition, so nothing is left for the exit sweeper.
func TestPG_RunnerExitAfterSettlementAcknowledgedWithoutTransition(t *testing.T) {
	for _, scenario := range []string{"lease-ended", "killed", "completed", "stopped"} {
		t.Run(scenario, func(t *testing.T) {
			f := newSettledFixture(t)
			ctx := context.Background()
			switch scenario {
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
			if n := f.count(t, stagedExitsSQL); n != 0 {
				t.Fatalf("settled exit left %d receipts", n)
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
