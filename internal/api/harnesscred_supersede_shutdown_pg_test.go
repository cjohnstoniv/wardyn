// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// TestPG_SupersedeTailAbandonedAtShutdown is #710's still-open case: a
// supersede's kill-teardown tail (supersedeOneLoginRun's goBackground call,
// harnesscred_supersede.go) runs detached from the request that claimed it, so
// a shutdown that hits WaitBackground's budget while the tail is still inside
// KillSandbox abandons it. The run is already KILLED — claimKillTransition's
// CAS wins synchronously, before the detach — but no run.kill row was ever
// written, exactly the state recoverAbandonedKillTail (runs_lifecycle.go)
// exists to find. A restarted process must recover it: SweepTerminalSandboxes
// re-runs the teardown and writes run.kill once the row is old enough that its
// own tail could not still be running (killTailRecoveryGrace).
//
// Real Postgres, so the "is this settled" read recoverAbandonedKillTail
// depends on (store.RunAuditMatcher) is the actual EXISTS query, not a fake's
// in-memory scan — and the "restart" is a second, independent Server built
// over the same rows, with no memory of the first one's still-gated goroutine.
package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_SupersedeTailAbandonedAtShutdown(t *testing.T) {
	pool := throwawayPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	h := newHarness(t)
	id := uuid.New()
	now := time.Now().UTC()
	run := types.AgentRun{
		ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: "person@example.com", Agent: "aws-sso-login",
		ConfinementClass: types.CC1, State: types.RunRunning, RunnerTarget: "docker",
		SandboxRef: "sbx-supersede-shutdown", SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(),
	}
	if _, err := pg.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// srv1 is the process that supersedes the run and starts tearing it down,
	// then is asked to shut down before the detached tail finishes. Its
	// KillSandbox is gated shut so the test can observe "still mid-tail".
	rnr := &gatedKillRunner{fakeRunner: &fakeRunner{}, gate: make(chan struct{}), entered: make(chan struct{})}
	cfg1 := baseTestConfig(h, pg)
	cfg1.Audit = &store.Recorder{Pool: pool}
	cfg1.Runner = rnr
	cfg1.Broker = h.broker
	srv1 := New(cfg1)
	srv1.bgWaitBudget = 50 * time.Millisecond
	t.Cleanup(func() {
		// Let the leaked goroutine finish (fakeRunner succeeds at once) before
		// the pool it writes to is closed by throwawayPGPool's own cleanup,
		// which — registered before this one — runs after it.
		rnr.openGate()
		srv1.bg.Wait()
	})

	srv1.supersedeOneLoginRun(ctx, run, "person@example.com", uuid.New())

	select {
	case <-rnr.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the superseded teardown never reached KillSandbox")
	}

	if got, err := pg.GetRun(ctx, id); err != nil {
		t.Fatalf("get run: %v", err)
	} else if got.State != types.RunKilled {
		t.Fatalf("run state once supersedeOneLoginRun returned = %s, want KILLED (the CAS is synchronous)", got.State)
	}

	// Shutdown hits its budget while the tail is still gated inside KillSandbox.
	waitDone := make(chan struct{})
	go func() {
		srv1.WaitBackground()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitBackground never returned — it must give up at its budget, not hang")
	}

	killRows := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM audit_events WHERE run_id=$1 AND action='run.kill'`, id).Scan(&n); err != nil {
			t.Fatalf("count run.kill rows: %v", err)
		}
		return n
	}
	if n := killRows(); n != 0 {
		t.Fatalf("run.kill rows right after the abandoned shutdown = %d, want 0 — the tail was still gated shut", n)
	}

	// "Restart": age the KILLED transition past killTailRecoveryGrace, on the
	// database's clock like every other updated_at writer, then hand the row
	// to a FRESH Server — a new process, its own Runner, no memory of srv1's
	// still-gated goroutine.
	age := int((killTailRecoveryGrace + time.Minute).Seconds())
	if _, err := pool.Exec(ctx,
		`UPDATE agent_runs SET updated_at = now() - $2 * interval '1 second' WHERE id=$1`, id, age); err != nil {
		t.Fatalf("age run: %v", err)
	}

	cfg2 := baseTestConfig(h, pg)
	cfg2.Audit = &store.Recorder{Pool: pool}
	cfg2.Runner = &fakeRunner{}
	cfg2.Broker = h.broker
	srv2 := New(cfg2)

	swept, err := srv2.SweepTerminalSandboxes(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept = %d, want 1 (the restarted process recovering the abandoned kill tail)", swept)
	}
	if n := killRows(); n != 1 {
		t.Fatalf("run.kill rows after the restart's sweep = %d, want 1", n)
	}
	var outcome string
	if err := pool.QueryRow(ctx,
		`SELECT outcome FROM audit_events WHERE run_id=$1 AND action='run.kill'`, id).Scan(&outcome); err != nil {
		t.Fatalf("read run.kill outcome: %v", err)
	}
	if outcome != "success" {
		t.Errorf("run.kill outcome = %q, want success", outcome)
	}
}
