// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A queued kill for a sandbox the runner holds no record of, and cannot prove
// it created, is refused: the action closes, the refusal is audited, nothing
// is killed and the runner's next session still comes up.
func TestPG_RunnerAction_UnprovableKillIsRefusedAndTheRunnerComesOnline(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	run := f.localRun(t, 0)
	run.SandboxRef = placement.RunnerRefPrefix(f.rn.ID.String()) + "fake-" + run.ID.String()
	if err := f.st.SetSandboxRef(f.ctx, run.ID, run.SandboxRef); err != nil {
		t.Fatal(err)
	}
	f.disconnect()
	for deadline := time.Now().Add(5 * time.Second); f.srv.runnerEndpoint(f.rn.ID).link.Online() && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	a, queued, err := f.st.QueueRunnerTeardown(f.ctx, f.rn.ID, types.RunnerActionKill, run.SandboxRef)
	if err != nil || !queued {
		t.Fatalf("queue kill: queued=%v err=%v", queued, err)
	}
	f.disconnect = f.connect(t)

	var outcome, reason string
	if err := f.st.Pool.QueryRow(f.ctx, `SELECT a.outcome, e.data->>'reason' FROM runner_pending_actions a
		JOIN audit_events e ON e.action = 'runner.action.refuse' AND e.target = a.id::text AND e.run_id = a.run_id
		WHERE a.id = $1 AND a.applied_at IS NOT NULL`, a.ID).Scan(&outcome, &reason); err != nil {
		t.Fatalf("no closed, audited refusal: %v", err)
	}
	if outcome != types.RunnerActionRefused || reason == "" {
		t.Fatalf("outcome %q, reason %q", outcome, reason)
	}
	if f.fake.Called("kill", "fake-"+run.ID.String()) {
		t.Fatal("the runner killed a container it could not prove it created")
	}
	if open, err := f.st.RunHasPendingRunnerAction(f.ctx, run.ID); err != nil || open {
		t.Fatalf("the refused action is still pending: %v %v", open, err)
	}
}
