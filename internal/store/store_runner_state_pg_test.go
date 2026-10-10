// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestPG_RunnerStateLapseAndContainment(t *testing.T) {
	pool, st, rn, run, _ := spoolPGFixture(t)
	ctx := t.Context()
	ref := "runner:" + rn.ID.String() + "/sandbox"
	if err := st.SetSandboxRef(ctx, run.ID, ref); err != nil {
		t.Fatal(err)
	}
	gen := uuid.New()
	if _, err := st.BeginRunnerExecution(ctx, rn.ID, run.ID, uuid.Nil, gen, runnerwire.MethodExec); err != nil {
		t.Fatal(err)
	}
	row := runnerwire.RunState{RunID: run.ID, SandboxPresent: true, Execution: &runnerwire.ExecutionResult{Generation: gen, Operation: runnerwire.MethodExec, Attempted: true}}
	if _, err := st.ReconcileRunnerState(ctx, rn.ID, []runnerwire.RunState{row}); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.MarkRunnerRunLost(ctx, rn.ID, run.ID, ref, time.Now()); err != nil || changed {
		t.Fatalf("fresh drop lost run: %v %v", changed, err)
	}
	if _, err := st.ReconcileRunnerState(ctx, rn.ID, nil); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
		t.Fatalf("omitted live run: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET token_renewed_at=now()-interval '2 hours' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if requests, err := st.ReconcileRunnerState(ctx, rn.ID, []runnerwire.RunState{row}); !errors.Is(err, store.ErrRunnerReconciliationRequired) || len(requests) != 0 {
		t.Fatalf("lapsed state: %v %v", requests, err)
	}
	got, _ := st.GetRun(ctx, run.ID)
	if got.LostAt != nil {
		t.Fatal("checked STATE wrote lifecycle")
	}
	if changed, err := st.MarkRunnerRunLost(ctx, rn.ID, run.ID, "stale-ref", time.Now()); changed || !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.Fatalf("stale reference: %v %v", changed, err)
	}
	if changed, err := st.MarkRunnerRunLost(ctx, rn.ID, run.ID, ref, time.Now()); err != nil || !changed {
		t.Fatalf("loss claim: %v %v", changed, err)
	}
	if _, err := st.ReconcileRunnerState(ctx, rn.ID, nil); err != nil {
		t.Fatalf("omitted kept run trapped reconnect: %v", err)
	}
	requests, err := st.ReconcileRunnerState(ctx, rn.ID, []runnerwire.RunState{row})
	if !errors.Is(err, store.ErrRunnerReconciliationRequired) || len(requests) != 1 || requests[0].Generation != gen || requests[0].Ref != ref || requests[0].LostAt.IsZero() {
		t.Fatalf("late containment: %v %v", requests, err)
	}
	observed := time.Now()
	row.ProxyStoppedAt = &observed
	if _, err := st.ReconcileRunnerState(ctx, rn.ID, []runnerwire.RunState{row}); err != nil {
		t.Fatal(err)
	}
	row.Execution.Generation = uuid.New()
	if _, err := st.ReconcileRunnerState(ctx, rn.ID, []runnerwire.RunState{row}); !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.Fatalf("stale generation: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE runners SET state='revoked',revoked_at=now() WHERE id=$1`, rn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileRunnerState(ctx, rn.ID, nil); !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.Fatalf("revoked empty STATE: %v", err)
	}
}

func TestPG_RunnerLossClaimRechecksAuthority(t *testing.T) {
	for _, scenario := range []string{"renewed", "revoked", "foreign-owner"} {
		t.Run(scenario, func(t *testing.T) {
			pool, st, rn, run, _ := spoolPGFixture(t)
			ctx := t.Context()
			ref := "runner:" + rn.ID.String() + "/sandbox"
			if err := st.SetSandboxRef(ctx, run.ID, ref); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE agent_runs SET token_renewed_at=now()-interval '2 hours' WHERE id=$1`, run.ID); err != nil {
				t.Fatal(err)
			}
			candidates, err := st.ListRunnerLapsedRuns(ctx, rn.ID, uuid.Nil)
			if err != nil || len(candidates) != 1 {
				t.Fatalf("candidates: %d %v", len(candidates), err)
			}
			switch scenario {
			case "renewed":
				_, err = st.StampRunTokenRenewed(ctx, run.ID)
			case "revoked":
				_, err = pool.Exec(ctx, `UPDATE runners SET state='revoked',revoked_at=now() WHERE id=$1`, rn.ID)
			case "foreign-owner":
				_, err = pool.Exec(ctx, `UPDATE agent_runs SET created_by='another-owner' WHERE id=$1`, run.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			changed, err := st.MarkRunnerRunLost(ctx, rn.ID, run.ID, ref, time.Now())
			if changed || (scenario != "renewed" && !errors.Is(err, store.ErrRunnerExecutionRefused)) {
				t.Fatalf("authority race accepted: %v %v", changed, err)
			}
			got, _ := st.GetRun(ctx, run.ID)
			if got.LostAt != nil {
				t.Fatal("lost mark written")
			}
		})
	}
}

func TestPG_RunnerLateContainmentAtomicProof(t *testing.T) {
	pool, st, rn, run, _ := spoolPGFixture(t)
	ctx := t.Context()
	gen := uuid.New()
	ref := "runner:" + rn.ID.String() + "/sandbox"
	if err := st.SetSandboxRef(ctx, run.ID, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginRunnerExecution(ctx, rn.ID, run.ID, uuid.Nil, gen, runnerwire.MethodExec); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at=now(),lost_reason='outage' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := st.QueueRunnerAction(ctx, types.RunnerPendingAction{ID: uuid.New(), RunnerID: rn.ID, RunID: run.ID, Kind: types.RunnerActionStopProxy, Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	observed := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	proof := store.RunnerContainmentProof{RunnerContainment: store.RunnerContainment{RunnerID: rn.ID, RunID: run.ID, Generation: gen, Ref: ref, LostAt: *got.LostAt}, ActionID: a.ID, ObservedAt: observed}
	apply := func(p store.RunnerContainmentProof, prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (bool, error) {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return false, err
		}
		defer tx.Rollback(ctx) //nolint:errcheck // closed after commit
		tag, err := tx.Exec(ctx, `UPDATE runner_pending_actions SET applied_at=clock_timestamp(),outcome='success',observed_at=$2 WHERE id=$1 AND applied_at IS NULL`, a.ID, observed)
		if err != nil {
			return false, err
		}
		if tag.RowsAffected() == 0 {
			return false, tx.Commit(ctx)
		}
		if err := st.RecordRunnerContainmentTx(ctx, tx, p, prepare); err != nil {
			return false, err
		}
		return true, tx.Commit(ctx)
	}
	for _, scenario := range []string{"old-lost-mark", "old-generation", "wrong-action", "seal-failure"} {
		t.Run(scenario, func(t *testing.T) {
			p := proof
			prepare := prepareSpoolTest
			switch scenario {
			case "old-lost-mark":
				p.LostAt = p.LostAt.Add(-time.Second)
			case "old-generation":
				p.Generation = uuid.New()
			case "wrong-action":
				p.ActionID = uuid.New()
			case "seal-failure":
				prepare = func(context.Context, types.AuditEvent) (types.AuditEvent, error) {
					return types.AuditEvent{}, errors.New("seal unavailable")
				}
			}
			if changed, err := apply(p, prepare); err == nil || changed {
				t.Fatalf("invalid proof accepted: %v %v", changed, err)
			}
			var applied bool
			var count int
			if err := pool.QueryRow(ctx, `SELECT applied_at IS NOT NULL FROM runner_pending_actions WHERE id=$1`, a.ID).Scan(&applied); err != nil || applied {
				t.Fatalf("action CAS escaped rollback: %v %v", applied, err)
			}
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE run_id=$1 AND action='run.containment.reassert'`, run.ID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("audit escaped rollback: %d %v", count, err)
			}
		})
	}
	if changed, err := apply(proof, prepareSpoolTest); err != nil || !changed {
		t.Fatalf("exact proof: %v %v", changed, err)
	}
	if changed, err := apply(proof, prepareSpoolTest); err != nil || changed {
		t.Fatalf("duplicate action: %v %v", changed, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE run_id=$1 AND action='run.containment.reassert' AND time>$2`, run.ID, observed).Scan(&count); err != nil || count != 1 {
		t.Fatalf("arrival-clock audit: %d %v", count, err)
	}
	final, _ := st.GetRun(ctx, run.ID)
	if final.State != types.RunRunning || final.LostAt == nil || !final.LostAt.Equal(*got.LostAt) {
		t.Fatal("late proof restored lifecycle")
	}
}
