// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// A terminal run whose queued end the runner refused (a record it keeps only for containment takes
// no end) is not sent the same end at every control-plane boot: the next boot queues a kill, which
// such a record takes and which stops its agent. An end or a kill the runner applied clears the ref;
// a kill it refused is not queued again.
func TestPG_OrphanSweep_RefusedEndBecomesAKillNotAnotherEnd(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	f.offline(t)
	stopped, refused, ended := f.terminalRunnerRun(t), f.terminalRunnerRun(t), f.terminalRunnerRun(t)
	sweep := func() {
		t.Helper()
		if err := f.srv.reconcileOrphanedSandbox(f.ctx); err != nil {
			t.Fatal(err)
		}
	}

	sweep()
	for _, run := range []types.AgentRun{stopped, refused} {
		f.answer(t, run, types.RunnerActionEnd, types.RunnerActionRefused)
	}
	f.answer(t, ended, types.RunnerActionEnd, types.RunnerActionSuccess)
	sweep()
	sweep() // the kill is still open: nothing new is queued
	f.teardowns(t, ended, 1, 0)
	if got := f.sandboxRef(t, ended); got != "" {
		t.Fatalf("a run whose end the runner applied keeps its ref %q", got)
	}
	for _, run := range []types.AgentRun{stopped, refused} {
		f.teardowns(t, run, 1, 1)
	}

	f.answer(t, stopped, types.RunnerActionKill, types.RunnerActionSuccess)
	f.answer(t, refused, types.RunnerActionKill, types.RunnerActionRefused)
	sweep()
	sweep()
	for _, run := range []types.AgentRun{stopped, refused} {
		f.teardowns(t, run, 1, 1)
	}
	if got := f.sandboxRef(t, stopped); got != "" {
		t.Fatalf("a run whose kill the runner applied keeps its ref %q", got)
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
