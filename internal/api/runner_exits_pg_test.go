// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type runnerExitTailProbe struct {
	*fakeRunner
	calls   atomic.Int32
	fail    atomic.Bool
	pending atomic.Bool
}

func (r *runnerExitTailProbe) StopSandbox(context.Context, string) error {
	r.calls.Add(1)
	if r.pending.Load() {
		return runner.ErrPendingOnRunner
	}
	if r.fail.Load() {
		return errors.New("runner temporarily disconnected")
	}
	return nil
}

func runnerExitAPIFixture(t *testing.T) (*pgxpool.Pool, Config, types.Runner, types.AgentRun, runnerwire.AgentExit, *runnerExitTailProbe) {
	t.Helper()
	pool := throwawayPGPool(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	id := uuid.New()
	rn, err := st.CreateRunner(ctx, types.Runner{ID: id, Owner: "exit-owner", Name: "exit-test", PublicKey: make([]byte, 32), KeyFingerprint: id.String(), Version: "0.9.0", Limits: roomyRunnerLimits()})
	if err != nil {
		t.Fatal(err)
	}
	rn, err = st.ClaimRunner(ctx, rn.ID, rn.Owner, rn.KeyFingerprint, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	run := newFinalizeRun()
	run.CreatedBy, run.Placement, run.RunnerID = rn.Owner, types.PlacementLocal, &rn.ID
	run, err = st.CreateRun(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	ref := "runner:" + rn.ID.String() + "/sandbox"
	if err := st.SetSandboxRef(ctx, run.ID, ref); err != nil {
		t.Fatal(err)
	}
	gen := uuid.New()
	if _, err := st.BeginRunnerExecution(ctx, rn.ID, run.ID, uuid.Nil, gen, runnerwire.MethodExec); err != nil {
		t.Fatal(err)
	}
	probe := &runnerExitTailProbe{fakeRunner: &fakeRunner{}}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.Runner = probe
	cfg.Audit = &syncAudit{}
	cfg.RunnerAuditPrepare = func(_ context.Context, ev types.AuditEvent) (types.AuditEvent, error) { return ev, nil }
	exit := runnerwire.AgentExit{Generation: gen, ExecID: "exec-1", Ref: ref, ObservedAt: time.Now().Add(-time.Hour)}
	return pool, cfg, rn, run, exit, probe
}

func TestRunnerExitPG_StageBeforeBindAndRetryTailAfterRestart(t *testing.T) {
	pool, cfg, rn, run, exit, probe := runnerExitAPIFixture(t)
	ctx := t.Context()
	st := cfg.Store.(store.PG)
	event := runnerwire.Event{Kind: runnerwire.EventAgentExit, RunID: &run.ID, Data: mustJSON(exit)}
	first := New(cfg)
	if err := first.ExitFromRunner(ctx, rn.ID, event); err != nil {
		t.Fatal(err)
	}
	if err := first.SweepRunnerExits(ctx); err != nil {
		t.Fatal(err)
	}
	assertRunnerExitPhase(t, pool, run.ID, false, false, 0)
	if got, _ := st.GetRun(ctx, run.ID); got.State != types.RunRunning || probe.calls.Load() != 0 {
		t.Fatal("unbound staged exit caused lifecycle/RPC effects")
	}
	if err := st.BindRunnerExecution(ctx, rn.ID, run.ID, exit.Generation, exit.ExecID); err != nil {
		t.Fatal(err)
	}
	probe.fail.Store(true)
	if err := first.SweepRunnerExits(ctx); err == nil {
		t.Fatal("failed teardown acknowledged complete")
	}
	assertRunnerExitPhase(t, pool, run.ID, true, false, 1)
	if got, _ := st.GetRun(ctx, run.ID); got.State != types.RunCompleted || got.SandboxRef == "" {
		t.Fatal("accepted exit lost pending teardown")
	}
	if _, err := st.BeginRunnerExecution(ctx, rn.ID, run.ID, exit.Generation, uuid.New(), runnerwire.MethodStart); err == nil {
		t.Fatal("new incarnation bypassed pending tail")
	}
	probe.fail.Store(false)
	restarted := New(cfg)
	if err := restarted.SweepRunnerExits(ctx); err != nil {
		t.Fatal(err)
	}
	assertRunnerExitPhase(t, pool, run.ID, true, true, 1)
	if got, _ := st.GetRun(ctx, run.ID); got.SandboxRef != "" {
		t.Fatal("successful shared tail did not clear sandbox ref")
	}
	if err := restarted.ExitFromRunner(ctx, rn.ID, event); err != nil {
		t.Fatal(err)
	}
	if err := restarted.SweepRunnerExits(ctx); err != nil {
		t.Fatal(err)
	}
	if probe.calls.Load() != 2 {
		t.Fatalf("completed receipt repeated teardown: %d", probe.calls.Load())
	}
	assertRunnerExitPhase(t, pool, run.ID, true, true, 1)
	first.WaitBackground()
	restarted.WaitBackground()
}

func assertRunnerExitPhase(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, accepted, done bool, audits int) {
	t.Helper()
	var gotAccepted, gotDone bool
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT accepted_at IS NOT NULL,tail_complete FROM runner_execution_exits WHERE run_id=$1`, id).Scan(&gotAccepted, &gotDone); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE run_id=$1 AND action='run.complete'`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if gotAccepted != accepted || gotDone != done || count != audits {
		t.Fatalf("receipt accepted=%v done=%v audits=%d; want %v %v %d", gotAccepted, gotDone, count, accepted, done, audits)
	}
}

func TestRunnerExitPG_RevokedAfterStageCannotApply(t *testing.T) {
	pool, cfg, rn, run, exit, probe := runnerExitAPIFixture(t)
	ctx := t.Context()
	st := cfg.Store.(store.PG)
	srv := New(cfg)
	event := runnerwire.Event{Kind: runnerwire.EventAgentExit, RunID: &run.ID, Data: mustJSON(exit)}
	if err := srv.ExitFromRunner(ctx, rn.ID, event); err != nil {
		t.Fatal(err)
	}
	if err := st.BindRunnerExecution(ctx, rn.ID, run.ID, exit.Generation, exit.ExecID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE runners SET state='revoked',revoked_at=clock_timestamp() WHERE id=$1`, rn.ID); err != nil {
		t.Fatal(err)
	}
	if err := srv.SweepRunnerExits(ctx); err == nil {
		t.Fatal("revoked runner candidate applied")
	}
	assertRunnerExitPhase(t, pool, run.ID, false, false, 0)
	if got, _ := st.GetRun(ctx, run.ID); got.State != types.RunRunning || probe.calls.Load() != 0 {
		t.Fatal("revocation allowed candidate lifecycle effects")
	}
	if err := srv.ExitFromRunner(ctx, rn.ID, event); err == nil {
		t.Fatal("revoked runner duplicate ACKed")
	}
}

// A teardown queued on the offline runner is handed to its pending-action
// queue: the tail audits it once and completes instead of failing every sweep.
func TestRunnerExitPG_TeardownQueuedOnOfflineRunnerAuditsOnce(t *testing.T) {
	pool, cfg, rn, run, exit, probe := runnerExitAPIFixture(t)
	ctx := t.Context()
	st := cfg.Store.(store.PG)
	srv := New(cfg)
	event := runnerwire.Event{Kind: runnerwire.EventAgentExit, RunID: &run.ID, Data: mustJSON(exit)}
	if err := srv.ExitFromRunner(ctx, rn.ID, event); err != nil {
		t.Fatal(err)
	}
	if err := st.BindRunnerExecution(ctx, rn.ID, run.ID, exit.Generation, exit.ExecID); err != nil {
		t.Fatal(err)
	}
	probe.pending.Store(true)
	for range 3 {
		if err := srv.SweepRunnerExits(ctx); err != nil {
			t.Fatalf("queued teardown failed the sweep: %v", err)
		}
	}
	assertRunnerExitPhase(t, pool, run.ID, true, true, 1)
	if n := len(cfg.Audit.(*syncAudit).eventsFor(run.ID, "run.complete")); n != 1 || !cfg.Audit.(*syncAudit).has(run.ID, "run.complete", "failure") {
		t.Fatalf("queued teardown audited %d times, want once", n)
	}
	if probe.calls.Load() != 1 {
		t.Fatalf("queued teardown retried %d times", probe.calls.Load())
	}
	if got, _ := st.GetRun(ctx, run.ID); got.State != types.RunCompleted || got.SandboxRef == "" {
		t.Fatal("queued teardown cleared the sandbox ref as if confirmed")
	}
	srv.WaitBackground()
}
