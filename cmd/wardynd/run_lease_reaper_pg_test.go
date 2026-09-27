// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/lifecycle"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_TheIdleReaperSkipsAKeptRun: a run its lease ended is RUNNING with its
// agent stopped and its files kept for the ended-run grace. It is not idle, and
// the idle reaper's stop would tear down the files the grace keeps, so the scan
// must not list it. Guarded by WARDYN_TEST_PG.
func TestPG_TheIdleReaperSkipsAKeptRun(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	run := skewedRun(t, store.PG{Pool: pool}, 60)
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET updated_at = now() - interval '2 hours',
		lost_at = now(), lost_reason = 'ended' WHERE id=$1`, run.ID); err != nil {
		t.Fatalf("mark the run kept: %v", err)
	}
	stopper := &recordingStopper{}
	reaper := lifecycle.New(lifecycleStore{pool: pool}, stopper, &fakeAuditRecorder{}, lifecycle.Config{})
	reaper.Tick(ctx)
	if slices.Contains(stopper.stopped, run.ID) {
		t.Fatal("the idle reaper stopped a kept run; the ended-run grace decides when its files go")
	}

	// Negative control: the same run, no longer kept, is reaped.
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at = NULL, lost_reason = '' WHERE id=$1`, run.ID); err != nil {
		t.Fatalf("unmark the run: %v", err)
	}
	reaper.Tick(ctx)
	if !slices.Contains(stopper.stopped, run.ID) {
		t.Error("an idle run past its auto-stop was not reaped once it was no longer kept")
	}
}

// countingRunner counts StopSandbox (the Docker teardown that deletes the
// agent's writable layer); every other Runner method is unreachable here.
type countingRunner struct {
	runner.Runner
	stops int
}

func (r *countingRunner) StopSandbox(context.Context, string) error { r.stops++; return nil }

// TestReviewIdleCASSkipsARunKeptAfterTheScan is F04's idle-CAS variant, from
// the 0.8 independent review (verify-080 lifecycle.md, finding F04): the idle
// scan lists a live run, then the lease/loss path marks it kept, then the idle
// CAS from the stale scan resumes. Before UpdateRunStateIfIdle gained its
// `lost_at IS NULL` clause, the final CAS had no way to see that mark — it
// compared only state and updated_at — so a reaper tick straddling the mark
// tore the kept run down anyway. Committed next to
// TestPG_TheIdleReaperSkipsAKeptRun, which only pins the scan's OWN filter and
// cannot see this gap: the CAS here runs on a snapshot taken BEFORE the mark
// landed, exactly the window between a reaper's scan and its stop.
func TestReviewIdleCASSkipsARunKeptAfterTheScan(t *testing.T) {
	for _, tc := range []struct {
		name string
		mark func(ctx context.Context, pg store.PG, id uuid.UUID) (bool, error)
	}{
		{"lease-end", func(ctx context.Context, pg store.PG, id uuid.UUID) (bool, error) {
			return pg.MarkRunEnded(ctx, id, time.Now().UTC())
		}},
		{"outage-loss", func(ctx context.Context, pg store.PG, id uuid.UUID) (bool, error) {
			return pg.MarkRunLost(ctx, id, types.LostOutage, time.Now().UTC(), 0)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := revocationPool(t)
			ctx := context.Background()
			pg := store.PG{Pool: pool}
			id := uuid.New()
			now := time.Now().UTC()
			if _, err := pg.CreateRun(ctx, types.AgentRun{
				ID: id, CreatedAt: now, UpdatedAt: now,
				CreatedBy: "op@example.com", Agent: "claude-code", Task: "idle-cas probe",
				ConfinementClass: types.CC2, State: types.RunRunning,
				SPIFFEID: "spiffe://wardyn.test/agent-run/" + id.String(), RunnerTarget: "docker",
				SandboxRef: "wardyn-agent-" + id.String(), AutoStopAfterSec: 60,
			}); err != nil {
				t.Fatalf("create run: %v", err)
			}
			t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, id) })
			if _, err := pool.Exec(ctx, `UPDATE agent_runs SET updated_at = now() - interval '2 hours',
				ends_at = now() - interval '1 minute' WHERE id=$1`, id); err != nil {
				t.Fatalf("make idle and due: %v", err)
			}

			// 1. The idle scan lists the run (lost_at IS NULL at this instant).
			sums, _, err := lifecycleStore{pool: pool}.ListRunningWithPolicy(ctx)
			if err != nil {
				t.Fatalf("idle scan: %v", err)
			}
			var snap time.Time
			for _, s := range sums {
				if s.ID == id {
					snap = s.UpdatedAt
				}
			}
			if snap.IsZero() {
				t.Fatal("idle scan did not list the live run")
			}

			// 2. The lease end / outage loss marks it kept.
			if ok, err := tc.mark(ctx, pg, id); err != nil || !ok {
				t.Fatalf("mark kept: applied=%v err=%v", ok, err)
			}

			// 3. The idle stop from the stale scan resumes.
			rn := &countingRunner{}
			out, err := lifecycleStopper{pool: pool, runner: rn}.StopRun(ctx, id, snap)
			if err != nil {
				t.Fatalf("stop: %v", err)
			}
			got, _ := pg.GetRun(ctx, id)
			if out.Applied || rn.stops != 0 {
				t.Fatalf("idle CAS tore down a kept run: applied=%v teardown=%d state=%s lost_reason=%q, want no transition and no teardown",
					out.Applied, rn.stops, got.State, got.LostReason)
			}
		})
	}
}
