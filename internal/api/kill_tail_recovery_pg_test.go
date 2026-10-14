// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// unroutedKillRunner answers a sandbox kill as the orchestrator does for a
// runner this replica has no route to.
type unroutedKillRunner struct {
	*fakeRunner
	kills atomic.Int32
}

func (r *unroutedKillRunner) KillSandbox(_ context.Context, ref string) error {
	r.kills.Add(1)
	return fmt.Errorf("%w: no registered substrate for ref %q", runner.ErrRunnerOffline, ref)
}

// runnerReadFails is the Postgres store whose runner read fails.
type runnerReadFails struct{ store.PG }

func (runnerReadFails) GetRunner(context.Context, uuid.UUID) (types.Runner, error) {
	return types.Runner{}, errors.New("store: get runner: connection reset")
}

// TestPG_KillTailRecovery_OnARunner: the sweep's recovery of a KILLED run on a
// self-hosted runner settles in the two cases where no stop can reach the
// sandbox now. An offline runner's stop is queued for its return, and a runner
// whose own row reads revoked is sent none; either row says which, and neither
// says the sandbox is gone. A runner that merely has no route here, or whose
// row cannot be read, is not taken for revoked: its stop is tried and the
// failure recorded for the next pass. Any other step that fails still fails
// the row.
func TestPG_KillTailRecovery_OnARunner(t *testing.T) {
	for _, tc := range []struct {
		name        string
		revoked     bool  // the run's runner is revoked
		routed      bool  // the runner is registered here, and offline
		unreadable  bool  // the runner's row cannot be read
		brokerErr   error // the broker revocation fails
		wantOutcome string
		wantStop    any    // the row's sandbox_stop
		wantError   string // the step the row names as failed
		wantKills   int32  // stops sent for the unrouted runner
	}{
		{name: "an offline runner's stop is queued", routed: true, wantOutcome: "success", wantStop: "queued"},
		{name: "a revoked runner is sent no stop", revoked: true, wantOutcome: "success", wantStop: "runner_revoked"},
		{name: "a runner with no route is not taken for revoked", wantOutcome: "failure", wantError: "runner_error", wantKills: 1},
		{name: "a runner whose row cannot be read is not taken for revoked", revoked: true, unreadable: true,
			wantOutcome: "failure", wantError: "runner_error", wantKills: 1},
		{name: "a failed revocation still fails a revoked runner's run", revoked: true, brokerErr: errors.New("broker unreachable"),
			wantOutcome: "failure", wantStop: "runner_revoked", wantError: "broker_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdmissionFixture(t)
			audit := &syncAudit{}
			unrouted := &unroutedKillRunner{fakeRunner: &fakeRunner{}}
			f.srv.cfg.Audit, f.srv.cfg.Runner = audit, unrouted
			f.srv.cfg.Broker = &killTailBroker{fakeBroker: &fakeBroker{}, revokeErr: tc.brokerErr}
			if tc.routed {
				f.srv.cfg.Runner = &routedKillRunner{fakeRunner: &fakeRunner{}, sub: f.srv.runnerEndpoint(f.rn.ID).sub}
			}
			if tc.revoked {
				if _, err := f.st.RevokeRunner(f.ctx, f.rn.ID, time.Now().UTC(), "admin@example.com", ""); err != nil {
					t.Fatalf("revoke the runner: %v", err)
				}
			}
			if tc.unreadable {
				f.srv.cfg.Store = runnerReadFails{f.st}
			}
			// KILLED, with no run.kill row, longer ago than a live tail could still be running.
			if _, err := f.pool.Exec(f.ctx, `UPDATE agent_runs SET state = 'KILLED', ended_at = now(),
				updated_at = now() - $2 * interval '1 second' WHERE id = $1`, f.run.ID, int((killTailRecoveryGrace + time.Minute).Seconds())); err != nil {
				t.Fatalf("age the killed run: %v", err)
			}

			if swept, err := f.srv.SweepTerminalSandboxes(f.ctx); err != nil || swept != 1 {
				t.Fatalf("sweep = %d, %v; want the one abandoned kill tail recovered", swept, err)
			}
			kills := audit.eventsFor(f.run.ID, "run.kill")
			if len(kills) != 1 || kills[0].Outcome != tc.wantOutcome {
				t.Fatalf("run.kill rows = %+v, want one %s", kills, tc.wantOutcome)
			}
			data := leaseAuditData(t, kills[0])
			if data["sandbox_stop"] != tc.wantStop || data["reason"] != "sweep_recovered_kill_tail" {
				t.Errorf("run.kill data = %v, want sandbox_stop %v on a sweep recovery", data, tc.wantStop)
			}
			for _, step := range []string{"runner_error", "broker_error", "identity_error"} {
				if (data[step] != nil) != (step == tc.wantError) {
					t.Errorf("run.kill data = %v, want %q as its only failed step", data, tc.wantError)
				}
			}
			if got := unrouted.kills.Load(); got != tc.wantKills {
				t.Errorf("stops sent for the unrouted runner = %d, want %d", got, tc.wantKills)
			}
			page, _, err := f.st.ListPendingRunnerActions(f.ctx, f.rn.ID)
			if queued := len(page) == 1 && page[0].Kind == types.RunnerActionKill; err != nil || queued != tc.routed {
				t.Errorf("pending actions = %+v, %v; want a queued kill exactly for the offline runner", page, err)
			}
		})
	}
}
