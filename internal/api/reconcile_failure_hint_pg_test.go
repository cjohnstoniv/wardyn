// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// hintRunner reads every agent as failed, with a driver's own message and, when hint is set, a sentence
// for the run's person.
type hintRunner struct {
	*fakeRunner
	hint string
}

func (r hintRunner) AgentStatus(context.Context, string, string) (runner.Status, error) {
	return runner.Status{State: types.RunFailed, Message: "driver detail not for the run page", FailureHint: r.hint}, nil
}

// A run the watcher or the stale-watcher sweep finalizes from a status that carries a failure hint shows
// that hint on its page; one with none keeps the reconcile reason, and a driver's message never shows.
func TestPG_ReconciledRunRecordsItsStatusFailureHint(t *testing.T) {
	const replaced = "This run ended: its pod was replaced outside Wardyn, and Wardyn does not start a replacement. Files on your drives are kept. Start a new run."
	e := newGovEnvOn(t, throwawayBarePGPool(t))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prev := reconcileWatchIntervalNS.Swap(int64(time.Millisecond))
	defer reconcileWatchIntervalNS.Store(prev)
	stranded := func() uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := e.pg.CreateRun(ctx, types.AgentRun{ID: id, CreatedBy: "sub-hint", Agent: "claude-code", ConfinementClass: types.CC2,
			State: types.RunRunning, RunnerTarget: "docker", Task: "t"}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.pool.Exec(ctx, `UPDATE agent_runs SET sandbox_ref = $2, agent_exec_id = 'exec-1',
			watcher_heartbeat = now() - interval '10 minutes' WHERE id = $1`, id, "wardyn-agent-"+id.String()); err != nil {
			t.Fatal(err)
		}
		return id
	}
	hintOf := func(id uuid.UUID) (types.RunState, string) {
		t.Helper()
		got, err := e.pg.GetRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return got.State, got.FailureHint
	}
	for _, tc := range []struct{ name, hint, wantWatch, wantSweep string }{
		{"with a hint", replaced, replaced, replaced},
		{"without one", "", "reconciled exit", "reconciled after watcher lease expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.srv.cfg.Runner = hintRunner{fakeRunner: &fakeRunner{}, hint: tc.hint}
			watched := stranded()
			if e.srv.reconcileWatch(ctx, watched, "wardyn-agent-"+watched.String(), "exec-1"); ctx.Err() != nil {
				t.Fatal("the watcher never finalized the run")
			}
			if state, hint := hintOf(watched); state != types.RunFailed || hint != tc.wantWatch {
				t.Errorf("watched run is %s with failure hint %q, want FAILED with %q", state, hint, tc.wantWatch)
			}
			swept := stranded()
			if err := e.srv.sweepRunWatchers(ctx); err != nil {
				t.Fatal(err)
			}
			if state, hint := hintOf(swept); state != types.RunFailed || hint != tc.wantSweep {
				t.Errorf("swept run is %s with failure hint %q, want FAILED with %q", state, hint, tc.wantSweep)
			}
		})
	}
}
