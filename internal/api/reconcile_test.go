// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errTeardownRunner reuses fakeRunner but makes StopSandbox fail, so we can
// exercise the boot-reconcile teardown-error path (finding N2).
type errTeardownRunner struct {
	*fakeRunner
	stopErr error
}

func (e *errTeardownRunner) StopSandbox(context.Context, string) error { return e.stopErr }

// TestAuditK8sNetpolIfUnenforced covers 6b commit 2: ReconcileOnBoot's
// auditK8sNetpolIfUnenforced writes "k8s.netpol_unenforced" for the two
// verdicts under which a sandbox runs unconfined (unenforced, acknowledged),
// carrying {verdict, driver} with a nil run id (the apitokens.go token.create/
// token.revoke precedent — a deployment-wide fact, not tied to a run).
// Negative controls: a PROVEN-enforced k8s driver, a non-k8s driver, and a k8s
// driver whose Capabilities() call itself errors must all stay silent.
func TestAuditK8sNetpolIfUnenforced(t *testing.T) {
	fire := func(t *testing.T, rn runner.Runner) *recRecorder {
		t.Helper()
		audit := &recRecorder{}
		srv := New(Config{Runner: rn, Audit: audit})
		srv.auditK8sNetpolIfUnenforced(context.Background())
		return audit
	}
	find := func(events []types.AuditEvent) *types.AuditEvent {
		for i := range events {
			if events[i].Action == "k8s.netpol_unenforced" {
				return &events[i]
			}
		}
		return nil
	}

	for _, tc := range []struct {
		name string
		rn   runner.Runner
	}{
		{"unenforced", k8sRunner{}},
		{"acknowledged", k8sRunner{networkPolicyAcknowledged: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			audit := fire(t, tc.rn)
			ev := find(audit.events)
			if ev == nil {
				t.Fatalf("no k8s.netpol_unenforced event recorded (have %d events)", len(audit.events))
			}
			if ev.RunID != nil {
				t.Errorf("RunID = %v, want nil (deployment-wide, not run-scoped)", ev.RunID)
			}
			if ev.ActorType != types.ActorSystem {
				t.Errorf("ActorType = %q, want %q", ev.ActorType, types.ActorSystem)
			}
			var data struct {
				Verdict string `json:"verdict"`
				Driver  string `json:"driver"`
			}
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatalf("decode Data: %v", err)
			}
			wantVerdict := tc.name
			if data.Verdict != wantVerdict {
				t.Errorf("Data.verdict = %q, want %q", data.Verdict, wantVerdict)
			}
			if data.Driver != "k8s" {
				t.Errorf("Data.driver = %q, want k8s", data.Driver)
			}
		})
	}

	t.Run("negative control: enforced stays silent", func(t *testing.T) {
		audit := fire(t, k8sRunner{networkPolicy: true})
		if ev := find(audit.events); ev != nil {
			t.Errorf("k8s.netpol_unenforced fired on a PROVEN-enforced verdict: %+v", ev)
		}
	})
	t.Run("negative control: non-k8s driver stays silent", func(t *testing.T) {
		audit := fire(t, &fakeRunner{})
		if ev := find(audit.events); ev != nil {
			t.Errorf("k8s.netpol_unenforced fired on a non-k8s (Docker) driver: %+v", ev)
		}
	})
	t.Run("negative control: Capabilities() error stays silent", func(t *testing.T) {
		audit := fire(t, k8sRunner{capsErr: errors.New("k8s api unreachable")})
		if ev := find(audit.events); ev != nil {
			t.Errorf("k8s.netpol_unenforced fired despite a Capabilities() error: %+v", ev)
		}
	})
}

// TestReconcileFinalize_TeardownErrorAudited covers finding N2: when the boot
// reconciler finalizes a stranded run and the sandbox teardown fails, it must
// audit a run.reconcile/failure event carrying teardown_error — otherwise the
// live/routable container is abandoned forever (the run is now terminal, so the
// next boot skips it) with no record. PG-gated.
func TestReconcileFinalize_TeardownErrorAudited(t *testing.T) {
	fr := &errTeardownRunner{fakeRunner: &fakeRunner{}, stopErr: errors.New("docker: no such container")}
	srv, pool := pgHarnessWithRunner(t, fr)
	audit := srv.cfg.Audit.(*recRecorder)
	ctx := context.Background()

	// A stranded RUNNING run with a sandbox ref (as a crash would leave it).
	runID := uuid.New()
	now := time.Now().UTC()
	run := types.AgentRun{
		ID:               runID,
		CreatedAt:        now,
		UpdatedAt:        now,
		CreatedBy:        "tester@example.com",
		Agent:            "claude-code",
		ConfinementClass: types.CC2,
		State:            types.RunRunning,
		SPIFFEID:         "spiffe://wardyn.local/agent-run/" + runID.String(),
		RunnerTarget:     "docker",
		SandboxRef:       "container-" + runID.String(),
	}
	if _, err := store.NewPG(pool).CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, runID) })

	srv.reconcileFinalize(ctx, runID, types.RunFailed, run.SandboxRef, "reconciled after restart")

	// The run must still be finalized terminal despite the teardown failure.
	if got, _ := store.NewPG(pool).GetRun(ctx, runID); got.State != types.RunFailed {
		t.Errorf("run state = %q, want FAILED (finalized regardless of teardown)", got.State)
	}

	// A run.reconcile/failure event carrying teardown_error MUST have been emitted.
	var failure *types.AuditEvent
	for i := range audit.events {
		ev := &audit.events[i]
		if ev.Action == "run.reconcile" && ev.Outcome == "failure" && ev.RunID != nil && *ev.RunID == runID {
			failure = ev
		}
	}
	if failure == nil {
		t.Fatal("no run.reconcile/failure event emitted for the abandoned sandbox")
	}
	if !strings.Contains(string(failure.Data), "teardown_error") {
		t.Errorf("run.reconcile/failure data should carry teardown_error, got %s", failure.Data)
	}
	if !strings.Contains(string(failure.Data), "no such container") {
		t.Errorf("run.reconcile/failure should include the teardown error text, got %s", failure.Data)
	}
}

// compile-time check that errTeardownRunner is a runner.Runner.
var _ runner.Runner = (*errTeardownRunner)(nil)
