// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// The boot pass queues a kill for a terminal run on a runner, never an end: a runner that cannot keep an
// ended sandbox (a Kubernetes runner) refuses an end, and a record kept only for containment refuses one
// too. An end another path queued and the runner refused is followed by a kill, not the same end again.
// A kill the runner applied clears the ref; a kill it refused is not queued again.
func TestPG_OrphanSweep_QueuesAKillNeverAnEnd(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	f.offline(t)
	applied, refused, endRefused := f.terminalRunnerRun(t), f.terminalRunnerRun(t), f.terminalRunnerRun(t)
	sweep := func() {
		t.Helper()
		if err := f.srv.reconcileOrphanedSandbox(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	// A finalize's stop while the runner is away is queued as an end (this runner keeps ended sandboxes).
	if err := f.srv.cfg.Runner.StopSandbox(f.ctx, endRefused.SandboxRef); !errors.Is(err, runner.ErrPendingOnRunner) {
		t.Fatalf("offline stop = %v, want it queued", err)
	}
	f.answer(t, endRefused, types.RunnerActionEnd, types.RunnerActionRefused)

	sweep()
	sweep() // each kill is still open: nothing new is queued
	f.teardowns(t, applied, 0, 1)
	f.teardowns(t, refused, 0, 1)
	f.teardowns(t, endRefused, 1, 1)

	f.answer(t, applied, types.RunnerActionKill, types.RunnerActionSuccess)
	f.answer(t, refused, types.RunnerActionKill, types.RunnerActionRefused)
	f.answer(t, endRefused, types.RunnerActionKill, types.RunnerActionSuccess)
	sweep()
	sweep()
	f.teardowns(t, applied, 0, 1)
	f.teardowns(t, refused, 0, 1)
	f.teardowns(t, endRefused, 1, 1)
	for _, run := range []types.AgentRun{applied, endRefused} {
		if got := f.sandboxRef(t, run); got != "" {
			t.Fatalf("a run whose kill the runner applied keeps its ref %q", got)
		}
	}
	if got := f.sandboxRef(t, refused); got != refused.SandboxRef {
		t.Fatalf("a run whose kill the runner refused lost its ref: %q", got)
	}
}

// terminalRunnerRun is a COMPLETED run on the fixture's runner that still carries its sandbox ref.
func (f *runnerLeaseFixture) terminalRunnerRun(t *testing.T) types.AgentRun {
	t.Helper()
	run := f.localRun(t, 0)
	run.SandboxRef = placement.RunnerRefPrefix(f.rn.ID.String()) + "fake-" + run.ID.String()
	if err := f.st.SetSandboxRef(f.ctx, run.ID, run.SandboxRef); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE agent_runs SET state = 'COMPLETED', ended_at = now() WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	run.State = types.RunCompleted
	return run
}

// answer records the runner's receipt for run's one open action of kind.
func (f *runnerLeaseFixture) answer(t *testing.T, run types.AgentRun, kind types.RunnerActionKind, outcome string) {
	t.Helper()
	var id uuid.UUID
	if err := f.st.Pool.QueryRow(f.ctx, `SELECT id FROM runner_pending_actions WHERE run_id = $1 AND kind = $2 AND applied_at IS NULL`,
		run.ID, string(kind)).Scan(&id); err != nil {
		t.Fatalf("no open %s for run %s: %v", kind, run.ID, err)
	}
	reason := ""
	if outcome == types.RunnerActionRefused {
		reason = "containment-only identity refuses this action"
	}
	prepare := func(_ context.Context, ev types.AuditEvent) (types.AuditEvent, error) { return ev, nil }
	if _, _, err := f.st.RecordRunnerActionResult(f.ctx, f.rn.ID, run.ID, id, outcome, reason, time.Now().UTC(), prepare); err != nil {
		t.Fatal(err)
	}
}

// teardowns checks how many ends and kills were ever queued for run.
func (f *runnerLeaseFixture) teardowns(t *testing.T, run types.AgentRun, ends, kills int) {
	t.Helper()
	var e, k int
	if err := f.st.Pool.QueryRow(f.ctx, `SELECT count(*) FILTER (WHERE kind = 'end'), count(*) FILTER (WHERE kind = 'kill')
		FROM runner_pending_actions WHERE run_id = $1`, run.ID).Scan(&e, &k); err != nil {
		t.Fatal(err)
	}
	if e != ends || k != kills {
		t.Fatalf("run %s: %d ends and %d kills queued, want %d and %d", run.ID, e, k, ends, kills)
	}
}

func (f *runnerLeaseFixture) sandboxRef(t *testing.T, run types.AgentRun) string {
	t.Helper()
	var ref string
	if err := f.st.Pool.QueryRow(f.ctx, `SELECT sandbox_ref FROM agent_runs WHERE id = $1`, run.ID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	return ref
}
