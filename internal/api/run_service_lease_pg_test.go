// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newServiceLeasePG is a server on a throwaway Postgres store with a fixed clock (*clock) and one
// RUNNING service run started at *clock whose captured lease is leaseSec and whose end is endsIn away.
func newServiceLeasePG(t *testing.T, clock *time.Time, leaseSec int, endsIn time.Duration) (*Server, store.PG, types.AgentRun) {
	t.Helper()
	st := store.NewPG(throwawayPGPool(t))
	h := newHarness(t)
	h.srv.cfg.Store = st
	h.srv.cfg.Runner = &leaseRunner{finalizeTailRunner: &finalizeTailRunner{fakeRunner: &fakeRunner{}}}
	h.srv.cfg.Now = func() time.Time { return *clock }
	end := clock.Add(endsIn)
	run, err := st.CreateRun(context.Background(), types.AgentRun{
		ID: uuid.New(), CreatedAt: *clock, UpdatedAt: *clock,
		CreatedBy: "op@example.com", OperatorOwned: true, Agent: "claude-code", Task: "service lease probe",
		ConfinementClass: types.CC2, State: types.RunRunning, RunnerTarget: "docker",
		SandboxRef: "wardyn-agent-" + uuid.NewString(), Experience: types.ExperienceBackground,
		EndsAt: &end, RunLimits: types.RunLimits{MaxEndAheadSec: leaseSec},
		Service: &types.RunService{Restart: types.ServiceRestartOnFailure},
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return h.srv, st, run
}

func pgRun(t *testing.T, st store.PG, id uuid.UUID) types.AgentRun {
	t.Helper()
	run, err := st.GetRun(context.Background(), id)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	return run
}

func TestPG_AServiceRenewalMovesTheStoredEnd(t *testing.T) {
	clock := time.Now().UTC().Truncate(time.Second)
	srv, st, run := newServiceLeasePG(t, &clock, 7200, 30*time.Minute)
	if err := srv.sweepRunLeases(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got := pgRun(t, st, run.ID)
	if want := clock.Add(2 * time.Hour); got.EndsAt == nil || !got.EndsAt.Equal(want) {
		t.Fatalf("ends_at = %v, want %v", got.EndsAt, want)
	}
}

// A re-clamp cuts the end and stamps end_tightened_at; the next renewal uses the tightened unit,
// never the captured one, and its moved end clears the stamp (SetRunEndAndWait).
func TestPG_ARenewalAfterAReclampUsesTheTightenedUnit(t *testing.T) {
	clock := time.Now().UTC().Truncate(time.Second)
	srv, st, run := newServiceLeasePG(t, &clock, 7200, 50*time.Minute)
	ctx := context.Background()
	tight := run.RunLimits
	tight.MaxEndAheadSec = 1800
	cut := clock.Add(30 * time.Minute)
	stamped := clock
	if ok, err := st.ReclampRunLimits(ctx, run, tight, &cut, run.WaitBudgetSec, &stamped); err != nil || !ok {
		t.Fatalf("reclamp: applied=%v err=%v", ok, err)
	}
	if got := pgRun(t, st, run.ID); got.EndTightenedAt == nil {
		t.Fatal("end_tightened_at not stamped by the re-clamp")
	}

	// Inside the tightened half-life (15 minutes), with a fresh observation.
	clock = clock.Add(20 * time.Minute)
	if ok, err := st.SetRunServiceStatus(ctx, run.ID, types.RunServiceStatus{Health: types.ServiceHealthHealthy, ObservedAt: clock}); err != nil || !ok {
		t.Fatalf("set service status: applied=%v err=%v", ok, err)
	}
	if err := srv.sweepRunLeases(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got := pgRun(t, st, run.ID)
	if want := clock.Add(30 * time.Minute); got.EndsAt == nil || !got.EndsAt.Equal(want) {
		t.Fatalf("ends_at = %v, want the tightened unit's %v", got.EndsAt, want)
	}
	if got.EndTightenedAt != nil {
		t.Errorf("end_tightened_at = %v after the renewal moved the end, want cleared", got.EndTightenedAt)
	}
	if got.RunLimits.MaxEndAheadSec != 1800 {
		t.Errorf("max_end_ahead_sec = %d, want the re-clamped 1800", got.RunLimits.MaxEndAheadSec)
	}
}
