// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// waitRow waits for q (a count over $1) to reach want.
func (f *runnerLeaseFixture) waitRow(t *testing.T, what, q string, arg any, want int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var n int
		if err := f.st.Pool.QueryRow(f.ctx, q, arg).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d rows, want %d", what, n, want)
		}
	}
}

// reconnect ends the runner's session and opens a fresh one, as a runnerd
// restart does: the fresh session receives the open pending actions.
func (f *runnerLeaseFixture) reconnect(t *testing.T) {
	t.Helper()
	f.disconnect()
	for deadline := time.Now().Add(5 * time.Second); f.srv.runnerEndpoint(f.rn.ID).link.Online(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the runner's session never ended")
		}
	}
	f.disconnect = f.connect(t)
}

// An ordinary online completion retires through the real org and runner: the
// org snapshots the binding as it clears the ref, the producer queues the
// containment kill, the next fresh session applies it, the producer then
// queues the retire_run, and the session after that settles it, sealing the
// run's spool. The run captured no output, so the receipt says no_output and
// the runner settles it at 0 (it holds no buffer and recorded no capture).
func TestPG_Retirement_OnlineCompletionRetires(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	f.sub.goneIsStopped.Store(true)
	run := f.localRun(t, 0)
	f.srv.recordRunnerOutputWatermark(f.ctx, run, false) // as dispatch does, before the create
	f.placed(t, &run)
	f.running(t, &run, nil)

	// One proxy decision, spooled on the runner and replayed to the org by
	// the next fresh session: the run's spool cursor.
	spool, err := runnerspool.Open(filepath.Join(f.dir, "spools", run.ID.String()), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.Append(f.ctx, egress.DecisionLog{Request: egress.Request{RunID: run.ID, Host: "example.test", Time: time.Now()}, Decision: egress.Allow}); err != nil {
		t.Fatal(err)
	}
	if err := spool.Close(); err != nil {
		t.Fatal(err)
	}
	f.reconnect(t)
	f.waitRow(t, "spool cursor", `SELECT count(*) FROM runner_spool_cursors WHERE run_id = $1 AND last_seq = 1`, run.ID, 1)

	// The run completes online: the org stops its sandbox through the runner
	// and clears its ref, snapshotting the binding first.
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE agent_runs SET state = 'COMPLETED', ended_at = now() WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	_ = f.srv.finalizeRunEffects(f.ctx, run.ID, run.SandboxRef, "run.complete", nil, false, false)
	f.waitRow(t, "binding snapshot", `SELECT count(*) FROM runner_retirements r JOIN agent_runs a ON a.id = r.run_id
		WHERE r.run_id = $1 AND r.state = 'kill' AND a.sandbox_ref = ''`, run.ID, 1)

	if err := f.srv.produceRunnerRetirements(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.waitRow(t, "queued kill", `SELECT count(*) FROM runner_pending_actions WHERE run_id = $1 AND kind = 'kill' AND applied_at IS NULL`, run.ID, 1)
	if err := f.srv.produceRunnerRetirements(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.waitRow(t, "no retire_run before the kill succeeds", `SELECT count(*) FROM runner_pending_actions WHERE run_id = $1 AND kind = 'retire_run'`, run.ID, 0)

	f.reconnect(t)
	f.waitRow(t, "kill applied", `SELECT count(*) FROM runner_pending_actions WHERE run_id = $1 AND kind = 'kill' AND outcome = 'success'`, run.ID, 1)
	if err := f.srv.produceRunnerRetirements(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.waitRow(t, "queued retire_run", `SELECT count(*) FROM runner_pending_actions WHERE run_id = $1 AND kind = 'retire_run' AND applied_at IS NULL
		AND (proof->>'no_output')::boolean AND (proof->'spool'->>'seq')::int = 1`, run.ID, 1)

	f.reconnect(t)
	f.waitRow(t, "settled retirement", `SELECT count(*) FROM runner_retirements WHERE run_id = $1 AND state = 'settled'`, run.ID, 1)
	f.waitRow(t, "runner.retire row", `SELECT count(*) FROM audit_events WHERE run_id = $1 AND action = 'runner.retire' AND outcome = 'success'`, run.ID, 1)
	if has, err := f.st.RunHasPendingRunnerAction(f.ctx, run.ID); err != nil || has {
		t.Fatalf("fence after retirement = %v %v", has, err)
	}

	// The runner sealed the run's spool: nothing more can be recorded there.
	spool, err = runnerspool.Open(filepath.Join(f.dir, "spools", run.ID.String()), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	if err := spool.Append(f.ctx, egress.DecisionLog{Request: egress.Request{RunID: run.ID, Host: "late.test", Time: time.Now()}, Decision: egress.Allow}); !errors.Is(err, runnerspool.ErrSealed) {
		t.Fatalf("append to a retired run's spool = %v, want ErrSealed", err)
	}

	// A retired run is never produced again.
	if err := f.srv.produceRunnerRetirements(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.waitRow(t, "no further actions", `SELECT count(*) FROM runner_pending_actions WHERE run_id = $1`, run.ID, 2)
	if run, err := f.st.GetRun(f.ctx, run.ID); err != nil || run.State != types.RunCompleted {
		t.Fatalf("run after retirement = %+v %v", run.State, err)
	}
}
