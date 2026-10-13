// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_RunnerActions_RefusedClosesTheActionAndAudits(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	run := f.run(t)
	a := f.mustQueue(t, f.action(run, types.RunnerActionKill, uuid.Nil))
	observed := time.Now().Add(-time.Minute)
	const reason = "pending action names a container this runner cannot prove it created"
	for name, c := range map[string]struct{ outcome, reason string }{
		"refused without a reason": {types.RunnerActionRefused, ""},
		"success with a reason":    {types.RunnerActionSuccess, reason},
		"reason over the bound":    {types.RunnerActionRefused, strings.Repeat("x", 1025)},
	} {
		if _, _, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, a.ID, c.outcome, c.reason, observed, prepareSpoolTest); !errors.Is(err, store.ErrRunnerActionRefused) {
			t.Fatalf("%s: %v, want ErrRunnerActionRefused", name, err)
		}
	}
	// The run moved on since: a refusal changed nothing on the runner, so it still closes the action.
	if _, err := f.pool.Exec(ctx, `UPDATE agent_runs SET sandbox_ref = sandbox_ref || '-moved' WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	done, first, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, a.ID, types.RunnerActionRefused, reason, observed, prepareSpoolTest)
	if err != nil || !first || done.Outcome != types.RunnerActionRefused || done.AppliedAt == nil {
		t.Fatalf("refused result = %+v first=%v err=%v", done, first, err)
	}
	if open, err := f.st.RunHasPendingRunnerAction(ctx, run.ID); err != nil || open {
		t.Fatalf("a refused action is still pending: %v %v", open, err)
	}
	if _, first, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, a.ID, types.RunnerActionRefused, reason, observed, prepareSpoolTest); err != nil || first {
		t.Fatalf("an identical repeat: first=%v err=%v", first, err)
	}
	if n := f.count(t, `SELECT count(*) FROM audit_events WHERE action = 'runner.action.refuse' AND run_id = $1 AND target = $2
		AND data->>'action' = 'kill' AND data->>'run_id' = $1::text AND data->>'reason' = $3`, run.ID, a.ID.String(), reason); n != 1 {
		t.Fatalf("%d runner.action.refuse rows, want one", n)
	}
	if n := f.count(t, `SELECT count(*) FROM agent_runs WHERE id = $1 AND state = $2`, run.ID, string(run.State)); n != 1 {
		t.Fatal("a refused action changed the run")
	}

	// The widened CHECK still rejects any outcome outside the closed set.
	other := f.mustQueue(t, f.action(f.run(t), types.RunnerActionKill, uuid.Nil))
	_, err = f.pool.Exec(ctx, `UPDATE runner_pending_actions SET applied_at = now(), observed_at = now(), outcome = 'ignored' WHERE id = $1`, other.ID)
	if err == nil || !strings.Contains(err.Error(), "runner_pending_actions_binding") {
		t.Fatalf("an outcome outside the closed set = %v, want the binding CHECK", err)
	}
}
