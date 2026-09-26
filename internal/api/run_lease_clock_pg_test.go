// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_FailClosedStopUsesTheMarkItWrote is M4's pin (Opus review of #1080,
// F04 follow-up): loseRun and endRun set run.LostAt/run.LostReason from the
// SAME now they just wrote via MarkRunLost/MarkRunEnded, so stopKeptRun's
// StopKeptRunIf compare matches the row it just marked. Reading the clock
// AGAIN between the mark write and that compare — `again := s.cfg.Now()` in
// place of reusing `now` — would make the compare parameter a few
// microseconds LATER than what is actually stored, so the CAS's
// `lost_at IS NOT DISTINCT FROM $3` never matches and every fail-closed stop
// silently becomes a no-op. Every api-level fixture (run_lease_test.go's
// leaseFixture) uses a FIXED clock, where two Now() calls return the
// identical frozen instant — hiding exactly this regression. This test uses
// a REAL Postgres store and the REAL wall clock (newHarness's Config.Now
// defaults to time.Now, never overridden here) so the mark actually written
// and the compare parameter are two independent clock reads whenever the
// code takes one, the same way production is.
func TestPG_FailClosedStopUsesTheMarkItWrote(t *testing.T) {
	newRunnerAndHarness := func(t *testing.T) (*harness, *leaseRunner, store.PG) {
		t.Helper()
		pool := throwawayPGPool(t)
		st := store.NewPG(pool)
		h := newHarness(t)
		h.srv.cfg.Store = st
		rn := &leaseRunner{
			finalizeTailRunner: &finalizeTailRunner{fakeRunner: &fakeRunner{}},
			endErr:             runner.ErrEndUnsupported,
		}
		h.srv.cfg.Runner = rn
		h.srv.cfg.EndedRunGrace = 7 * 24 * time.Hour
		return h, rn, st
	}

	t.Run("loseRun", func(t *testing.T) {
		h, rn, st := newRunnerAndHarness(t)
		ctx := context.Background()
		run, err := st.CreateRun(ctx, types.AgentRun{
			ID: uuid.New(), CreatedAt: time.Now(), UpdatedAt: time.Now(),
			CreatedBy: "op@example.com", Agent: "claude-code", Task: "m4 loseRun probe",
			ConfinementClass: types.CC2, State: types.RunRunning, RunnerTarget: "docker",
			Interactive: true, SandboxRef: "wardyn-agent-m4-loserun",
		})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}

		if !h.srv.loseRun(ctx, st, st, run, types.LostReboot, types.RunFailed, 0) {
			t.Fatal("loseRun returned false, want true (handled: kept, torn down, or retried)")
		}
		got, err := st.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if got.State != types.RunFailed || rn.stopCount() != 1 {
			t.Fatalf("state=%s stopCount=%d, want FAILED and 1 (the fail-closed teardown, ErrEndUnsupported)",
				got.State, rn.stopCount())
		}
	})

	t.Run("endRun via leaseRun", func(t *testing.T) {
		h, rn, st := newRunnerAndHarness(t)
		ctx := context.Background()
		pastEnd := time.Now().Add(-time.Minute)
		run, err := st.CreateRun(ctx, types.AgentRun{
			ID: uuid.New(), CreatedAt: time.Now(), UpdatedAt: time.Now(),
			CreatedBy: "op@example.com", Agent: "claude-code", Task: "m4 endRun probe",
			ConfinementClass: types.CC2, State: types.RunRunning, RunnerTarget: "docker",
			SandboxRef: "wardyn-agent-m4-endrun", EndsAt: &pastEnd,
		})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}

		h.srv.leaseRun(ctx, st, run)
		got, err := st.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if got.State != types.RunStopped || rn.stopCount() != 1 {
			t.Fatalf("state=%s stopCount=%d, want STOPPED and 1 (the fail-closed teardown, ErrEndUnsupported)",
				got.State, rn.stopCount())
		}
	})
}
