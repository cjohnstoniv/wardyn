// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
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

// offline ends the runner's session and waits until the org sees it gone.
func (f *runnerLeaseFixture) offline(t *testing.T) {
	t.Helper()
	f.disconnect()
	for deadline := time.Now().Add(5 * time.Second); f.srv.runnerEndpoint(f.rn.ID).link.Online(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the runner's session never ended")
		}
	}
}

// reconnect ends the runner's session and opens a fresh one, as a runnerd
// restart does: the fresh session receives the open pending actions, and
// connect returns only once the runner is READY.
func (f *runnerLeaseFixture) reconnect(t *testing.T) {
	t.Helper()
	f.offline(t)
	f.disconnect = f.connect(t)
}

func (f *runnerLeaseFixture) produce(t *testing.T) {
	t.Helper()
	if err := f.srv.produceRunnerRetirements(f.ctx); err != nil {
		t.Fatal(err)
	}
}

func (f *runnerLeaseFixture) actions(t *testing.T, run types.AgentRun, what string, want int) {
	t.Helper()
	f.waitRow(t, what, `SELECT count(*) FROM runner_pending_actions WHERE run_id = $1`, run.ID, want)
}

// spoolOne spools one proxy decision on the runner and reconnects, so the
// fresh session replays it and the org holds the run's spool cursor.
func (f *runnerLeaseFixture) spoolOne(t *testing.T, run types.AgentRun) {
	t.Helper()
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
}

// complete ends the run online: the org stops its sandbox through the runner
// and clears its ref, snapshotting the binding first.
func (f *runnerLeaseFixture) complete(t *testing.T, run types.AgentRun) {
	t.Helper()
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE agent_runs SET state = 'COMPLETED', ended_at = now() WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	_ = f.srv.finalizeRunEffects(f.ctx, run.ID, run.SandboxRef, "run.complete", nil, false, false)
	f.waitRow(t, "binding snapshot", `SELECT count(*) FROM runner_retirements r JOIN agent_runs a ON a.id = r.run_id
		WHERE r.run_id = $1 AND r.state = 'kill' AND a.sandbox_ref = ''`, run.ID, 1)
}

// retire drives a completed run's retirement through the real runner: the
// producer queues the kill, the next fresh session applies it, the producer
// queues the retire_run (whose proof must match receipt, a SQL condition),
// and the session after that settles it. It checks the runner sealed the
// run's spool.
func (f *runnerLeaseFixture) retire(t *testing.T, run types.AgentRun, receipt string) {
	t.Helper()
	f.produce(t)
	f.waitRow(t, "queued kill", `SELECT count(*) FROM runner_pending_actions WHERE run_id = $1 AND kind = 'kill' AND applied_at IS NULL`, run.ID, 1)
	f.produce(t)
	f.waitRow(t, "no retire_run before the kill succeeds", `SELECT count(*) FROM runner_pending_actions WHERE run_id = $1 AND kind = 'retire_run'`, run.ID, 0)

	f.reconnect(t)
	f.waitRow(t, "kill applied", `SELECT count(*) FROM runner_pending_actions WHERE run_id = $1 AND kind = 'kill' AND outcome = 'success'`, run.ID, 1)
	f.produce(t)
	f.waitRow(t, "queued retire_run", `SELECT count(*) FROM runner_pending_actions WHERE run_id = $1 AND kind = 'retire_run' AND applied_at IS NULL
		AND (proof->'spool'->>'seq')::int = 1 AND `+receipt, run.ID, 1)

	f.reconnect(t)
	f.waitRow(t, "settled retirement", `SELECT count(*) FROM runner_retirements WHERE run_id = $1 AND state = 'settled'`, run.ID, 1)
	f.waitRow(t, "runner.retire row", `SELECT count(*) FROM audit_events WHERE run_id = $1 AND action = 'runner.retire' AND outcome = 'success'`, run.ID, 1)
	if has, err := f.st.RunHasPendingRunnerAction(f.ctx, run.ID); err != nil || has {
		t.Fatalf("fence after retirement = %v %v", has, err)
	}
	spool, err := runnerspool.Open(filepath.Join(f.dir, "spools", run.ID.String()), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	if err := spool.Append(f.ctx, egress.DecisionLog{Request: egress.Request{RunID: run.ID, Host: "late.test", Time: time.Now()}, Decision: egress.Allow}); !errors.Is(err, runnerspool.ErrSealed) {
		t.Fatalf("append to a retired run's spool = %v, want ErrSealed", err)
	}
	// A retired run is never produced again.
	f.produce(t)
	f.actions(t, run, "no further actions", 2)
	if run, err := f.st.GetRun(f.ctx, run.ID); err != nil || run.State != types.RunCompleted {
		t.Fatalf("run after retirement = %+v %v", run.State, err)
	}
}

// An ordinary online completion retires through the real org and runner. The
// run captured no output, so the receipt says no_output and the runner
// settles it at 0 (it holds no buffer and recorded no capture).
func TestPG_Retirement_OnlineCompletionRetires(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	f.sub.goneIsStopped.Store(true)
	run := f.localRun(t, 0)
	f.srv.recordRunnerOutputWatermark(f.ctx, run, false) // as dispatch does, before the create
	f.placed(t, &run)
	f.running(t, &run, nil)
	f.spoolOne(t, run)
	f.complete(t, run)
	f.retire(t, run, `(proof->>'no_output')::boolean`)
}

// A run whose output was captured retires with the receipt's output_end equal
// to the bytes the runner wrote and the org committed, and the runner deletes
// the run's output directory once it settles the retire_run.
func TestPG_Retirement_CapturedOutputReleased(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	f.sub.goneIsStopped.Store(true)
	sink := &lockedBuffer{} // the org's output sink
	f.execOutput = sink
	run := f.localRun(t, 0)
	f.srv.recordRunnerOutputWatermark(f.ctx, run, true)
	f.placed(t, &run)
	f.running(t, &run, nil)

	// The fake substrate's agent writes its output on the runner at create,
	// through the runner's output buffer, as a driver's drain does.
	local := "fake-" + run.ID.String()
	data := "agent output of " + local + "\n"
	n := strconv.Itoa(len(data))
	f.waitRow(t, "committed watermark", `SELECT count(*) FROM runner_output_watermarks WHERE run_id = $1 AND capture AND accepted_end = `+n, run.ID, 1)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		at, settled, err := f.streams.OutputSettled(f.ctx, run.ID, local)
		if err == nil && settled && at == int64(len(data)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runner output never settled at %d: %d %v %v", len(data), at, settled, err)
		}
	}
	if got := sink.String(); got != data {
		t.Fatalf("org sink holds %q, want %q", got, data)
	}

	outDir := filepath.Join(f.dir, "output", run.ID.String())
	if _, err := os.Stat(outDir); err != nil {
		t.Fatalf("run output directory before retirement: %v", err)
	}

	f.spoolOne(t, run)
	f.complete(t, run)
	f.retire(t, run, `proof->'no_output' IS NULL AND (proof->>'output_end')::bigint = `+n)
	if _, err := os.Stat(outDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("run output directory after retirement: %v, want gone", err)
	}
}

// A runner whose caps omit retire (older than 0.9.3) is never sent a
// retire_run, or the kill that leads to one, and stays READY across fresh
// sessions; its run's output is kept. Once its caps say retire, the producer
// retires the run, but only while the runner is connected.
func TestPG_Retirement_OldRunnerNeverGetsRetireRun(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	f.sub.goneIsStopped.Store(true)
	f.oldCaps.Store(true)
	f.reconnect(t)
	run := f.localRun(t, 0)
	f.srv.recordRunnerOutputWatermark(f.ctx, run, false)
	f.placed(t, &run)
	f.running(t, &run, nil)
	f.spoolOne(t, run)
	f.complete(t, run)
	for range 3 {
		f.produce(t)
		f.reconnect(t) // READY each time
	}
	f.actions(t, run, "actions for an old runner", 0)

	// The runner upgrades: its caps say retire. While it is offline nothing
	// is queued; connected, the retirement starts.
	f.oldCaps.Store(false)
	f.reconnect(t)
	f.offline(t)
	f.produce(t)
	f.actions(t, run, "actions while the runner is offline", 0)
	f.disconnect = f.connect(t)
	f.produce(t)
	f.waitRow(t, "queued kill", `SELECT count(*) FROM runner_pending_actions WHERE run_id = $1 AND kind = 'kill' AND applied_at IS NULL`, run.ID, 1)
	f.reconnect(t)
	f.produce(t)
	f.waitRow(t, "queued retire_run", `SELECT count(*) FROM runner_pending_actions WHERE run_id = $1 AND kind = 'retire_run'`, run.ID, 1)
}
