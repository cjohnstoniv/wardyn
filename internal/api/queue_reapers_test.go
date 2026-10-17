// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A QUEUED run waits past the undispatched grace by design and is never failed by it; a run promoted
// out of the queue is aged from admitted_at, so a long wait does not fail it the moment it starts.
func TestReconcileOnBoot_QueuedRunSurvivesAndPromotedRunAgesFromAdmission(t *testing.T) {
	now := time.Now().UTC()
	longAgo := now.Add(-(undispatchedGrace + 3*time.Hour))
	for _, tc := range []struct {
		name     string
		state    types.RunState
		admitted *time.Time
		finalize bool
	}{
		{"queued past the grace", types.RunQueued, nil, false},
		{"promoted a minute ago after a long wait", types.RunPending, ptr(now.Add(-time.Minute)), false},
		{"promoted past the grace", types.RunPending, ptr(now.Add(-(undispatchedGrace + time.Minute))), true},
		{"never queued, past the grace", types.RunPending, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			run := execRun(t, "")
			run.SandboxRef = ""
			run.State = tc.state
			run.CreatedAt = longAgo
			run.AdmittedAt = tc.admitted
			fake := &bootReconcileStore{run: run}
			cfg := baseTestConfig(h, fake)
			cfg.Runner = &fakeRunner{}
			cfg.Broker = h.broker
			srv := testServer(t, cfg)

			if err := srv.ReconcileOnBoot(context.Background()); err != nil {
				t.Fatalf("reconcile on boot: %v", err)
			}
			if to, got := fake.finalTransition(); got != tc.finalize {
				t.Fatalf("%s: finalized=%v (to=%q), want %v", tc.name, got, to, tc.finalize)
			}
		})
	}
}

// FAILED keeps a "queue: <token>" end reason and KILLED keeps only runner_pool_removed; a QUEUED run
// shows its wait reason; every other state, and a FAILED run with no queue detail, blanks as before.
func TestProjectStatusDetail_QueueReasons(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      types.RunState
		detail     string
		wantDetail string
		wantReason string
	}{
		{"failed on timeout", types.RunFailed, "queue: queue_timeout", "queue: queue_timeout", "queue_timeout"},
		{"failed on a changed profile", types.RunFailed, "queue: queue_profile_changed", "queue: queue_profile_changed", "queue_profile_changed"},
		{"failed on a changed policy", types.RunFailed, "queue: queue_policy_changed", "queue: queue_policy_changed", "queue_policy_changed"},
		{"failed on a revoked portal", types.RunFailed, "queue: queue_portal_revoked", "queue: queue_portal_revoked", "queue_portal_revoked"},
		{"failed on a deactivated owner", types.RunFailed, "queue: identity_deactivated", "queue: identity_deactivated", "identity_deactivated"},
		{"killed with its pool removed", types.RunKilled, "queue: runner_pool_removed", "queue: runner_pool_removed", "runner_pool_removed"},
		{"killed for another queue reason", types.RunKilled, "queue: queue_timeout", "", ""},
		{"queued shows its wait", types.RunQueued, "queue: runner_pool_at_capacity", "queue: runner_pool_at_capacity", "runner_pool_at_capacity"},
		{"promoted run drops the wait", types.RunPending, "queue: runner_pool_at_capacity", "", ""},
		{"running run drops it", types.RunRunning, "queue: queue_timeout", "", ""},
		{"failed after a queue wait", types.RunFailed, "queue: runner_pool_at_capacity", "", ""},
		{"failed with an ordinary wait", types.RunFailed, "agent: ContainerCreating", "", ""},
		{"failed with a malformed token", types.RunFailed, "queue: Not A Token", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs := []types.AgentRun{{State: tc.state, StatusDetail: tc.detail}}
			projectStatusDetail(runs)
			if runs[0].StatusDetail != tc.wantDetail || runs[0].StatusReason != tc.wantReason {
				t.Errorf("%s %q projected to detail %q reason %q, want %q and %q",
					tc.state, tc.detail, runs[0].StatusDetail, runs[0].StatusReason, tc.wantDetail, tc.wantReason)
			}
		})
	}
}
