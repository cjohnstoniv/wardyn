// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func TestPG_RunnerExitAtomicReceiptAndGenerationBarrier(t *testing.T) {
	pool, st, rn, run, _ := spoolPGFixture(t)
	ctx := context.Background()
	gen := uuid.New()
	ref := "runner:" + rn.ID.String() + "/agent"
	if err := st.SetSandboxRef(ctx, run.ID, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginRunnerExecution(ctx, rn.ID, run.ID, uuid.Nil, gen, runnerwire.MethodExec); err != nil {
		t.Fatal(err)
	}
	exit := runnerwire.AgentExit{Generation: gen, ExecID: "exec-1", Ref: ref, Code: 0, ObservedAt: time.Now().Add(-time.Hour)}
	if _, err := st.AcceptRunnerExit(ctx, rn.ID, run.ID, exit, prepareSpoolTest); !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.Fatalf("unbound handle accepted: %v", err)
	}
	if err := st.BindRunnerExecution(ctx, rn.ID, run.ID, gen, "exec-1"); err != nil {
		t.Fatal(err)
	}
	fail := errors.New("seal unavailable")
	if _, err := st.AcceptRunnerExit(ctx, rn.ID, run.ID, exit, func(context.Context, types.AuditEvent) (types.AuditEvent, error) { return types.AuditEvent{}, fail }); !errors.Is(err, fail) {
		t.Fatal(err)
	}
	got, err := st.GetRun(ctx, run.ID)
	if err != nil || got.State != types.RunRunning {
		t.Fatalf("failed seal transitioned: %s %v", got.State, err)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := st.AcceptRunnerExit(ctx, rn.ID, run.ID, exit, prepareSpoolTest)
			if err != nil {
				t.Error(err)
			}
			if ok {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted %d", accepted.Load())
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE run_id=$1 AND action='run.complete'`, run.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit count %d %v", count, err)
	}
	calls := 0
	tail := func(context.Context, types.AgentRun, runnerwire.AgentExit) error { calls++; return fail }
	if err := st.RetryRunnerExitTail(ctx, rn.ID, run.ID, gen, tail); !errors.Is(err, fail) {
		t.Fatal(err)
	}
	// Even a separate lifecycle writer moving the run back to RUNNING cannot
	// issue a new generation while accepted old teardown is outstanding.
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET state='RUNNING' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	next := uuid.New()
	if _, err := st.BeginRunnerExecution(ctx, rn.ID, run.ID, gen, next, runnerwire.MethodStart); !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.Fatalf("incomplete tail bypassed: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET state='COMPLETED' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.RetryRunnerExitTail(ctx, rn.ID, run.ID, gen, func(context.Context, types.AgentRun, runnerwire.AgentExit) error { calls++; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET state='RUNNING' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginRunnerExecution(ctx, rn.ID, run.ID, gen, next, runnerwire.MethodStart); err != nil {
		t.Fatal(err)
	}
	if err := st.RetryRunnerExitTail(ctx, rn.ID, run.ID, gen, tail); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("old receipt tore down new incarnation")
	}
	if ok, err := st.AcceptRunnerExit(ctx, rn.ID, run.ID, exit, prepareSpoolTest); err != nil || ok {
		t.Fatalf("old receipt replay %v %v", ok, err)
	}
	got, err = st.GetRun(ctx, run.ID)
	if err != nil || got.State != types.RunRunning {
		t.Fatalf("old exit finalized new generation %s %v", got.State, err)
	}
}

func TestPG_RunnerExitOwnershipStateAndReceiptIntegrity(t *testing.T) {
	for _, scenario := range []string{"wrong-owner", "revoked", "killed", "interactive-outage", "other-lost", "headless-outage"} {
		t.Run(scenario, func(t *testing.T) {
			pool, st, rn, run, _ := spoolPGFixture(t)
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
			query := ""
			switch scenario {
			case "wrong-owner":
				query = `UPDATE agent_runs SET created_by='other' WHERE id=$1`
			case "revoked":
				if _, err := pool.Exec(ctx, `UPDATE runners SET revoked_at=now(),state='revoked' WHERE id=$1`, rn.ID); err != nil {
					t.Fatal(err)
				}
			case "killed":
				query = `UPDATE agent_runs SET state='KILLED' WHERE id=$1`
			case "interactive-outage":
				query = `UPDATE agent_runs SET interactive=true,lost_at=now(),lost_reason='outage' WHERE id=$1`
			case "other-lost":
				query = `UPDATE agent_runs SET lost_at=now(),lost_reason='reboot' WHERE id=$1`
			case "headless-outage":
				query = `UPDATE agent_runs SET interactive=false,lost_at=now(),lost_reason='outage' WHERE id=$1`
			}
			if query != "" {
				if _, err := pool.Exec(ctx, query, run.ID); err != nil {
					t.Fatal(err)
				}
			}
			exit := runnerwire.AgentExit{Generation: gen, ExecID: "exec", Ref: ref, Code: 7, ObservedAt: time.Now()}
			accepted, err := st.AcceptRunnerExit(ctx, rn.ID, run.ID, exit, prepareSpoolTest)
			if scenario != "headless-outage" {
				if accepted || !errors.Is(err, store.ErrRunnerExecutionRefused) {
					t.Fatalf("forbidden exit %v %v", accepted, err)
				}
				return
			}
			if !accepted || err != nil {
				t.Fatalf("kept headless exit %v %v", accepted, err)
			}
			exit.Code = 0
			if _, err := st.AcceptRunnerExit(ctx, rn.ID, run.ID, exit, prepareSpoolTest); !errors.Is(err, store.ErrRunnerExecutionRefused) {
				t.Fatalf("receipt collision accepted: %v", err)
			}
		})
	}
}

func TestPG_RunnerExitStagesBeforeBindingWithoutLifecycleEffects(t *testing.T) {
	pool, st, rn, run, _ := spoolPGFixture(t)
	ctx := context.Background()
	gen := uuid.New()
	ref := "runner:" + rn.ID.String() + "/agent"
	if err := st.SetSandboxRef(ctx, run.ID, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginRunnerExecution(ctx, rn.ID, run.ID, uuid.Nil, gen, runnerwire.MethodExec); err != nil {
		t.Fatal(err)
	}
	exit := runnerwire.AgentExit{Generation: gen, ExecID: "exec-early", Ref: ref, Code: 0, ObservedAt: time.Now()}
	if err := st.StageRunnerExit(ctx, rn.ID, run.ID, exit); err != nil {
		t.Fatal(err)
	}
	if err := st.StageRunnerExit(ctx, rn.ID, run.ID, exit); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetRun(ctx, run.ID)
	if err != nil || got.State != types.RunRunning {
		t.Fatal("stage changed lifecycle", err)
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE run_id=$1`, run.ID).Scan(&auditCount); err != nil || auditCount != 0 {
		t.Fatal("stage authored audit", err)
	}
	if rows, err := st.ListRunnerExitWork(ctx, uuid.Nil); err != nil || len(rows) != 0 {
		t.Fatal("unbound exit ready for finalization", err)
	}
	if _, err := st.BeginRunnerExecution(ctx, rn.ID, run.ID, gen, uuid.New(), runnerwire.MethodExec); !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.Fatal("pending candidate allowed new incarnation", err)
	}
	if err := st.BindRunnerExecution(ctx, rn.ID, run.ID, gen, "exec-early"); err != nil {
		t.Fatal(err)
	}
	if rows, err := st.ListRunnerExitWork(ctx, uuid.Nil); err != nil || len(rows) != 1 {
		t.Fatalf("bound candidate lost: %d %v", len(rows), err)
	}
	if accepted, err := st.AcceptRunnerExit(ctx, rn.ID, run.ID, exit, prepareSpoolTest); err != nil || !accepted {
		t.Fatalf("bound candidate not accepted %v %v", accepted, err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	tailDone := make(chan error, 1)
	go func() {
		tailDone <- st.RetryRunnerExitTail(ctx, rn.ID, run.ID, gen, func(context.Context, types.AgentRun, runnerwire.AgentExit) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	staged := make(chan error, 1)
	go func() { staged <- st.StageRunnerExit(ctx, rn.ID, run.ID, exit) }()
	select {
	case err := <-staged:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("read-loop stage blocked behind tail operation lock")
	}
	close(release)
	if err := <-tailDone; err != nil {
		t.Fatal(err)
	}
}
