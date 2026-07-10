// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/identity/embedded"
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

// TestReconcileFinalize_TeardownErrorAudited covers finding N2: when the boot
// reconciler finalizes a stranded run and the sandbox teardown fails, it must
// audit a run.reconcile/failure event carrying teardown_error — otherwise the
// live/routable container is abandoned forever (the run is now terminal, so the
// next boot skips it) with no record. PG-gated.
func TestReconcileFinalize_TeardownErrorAudited(t *testing.T) {
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed reconcile test")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)

	audit := &recRecorder{}
	idp, err := embedded.New(nil, "wardyn.local", embedded.NewMemRevocationStore(), audit)
	if err != nil {
		t.Fatalf("embedded.New: %v", err)
	}
	fr := &errTeardownRunner{fakeRunner: &fakeRunner{}, stopErr: errors.New("docker: no such container")}
	srv := New(Config{
		Store:           store.NewPG(pool),
		Identity:        idp,
		Approvals:       newFakeApprovals(),
		Broker:          &fakeBroker{},
		Audit:           audit,
		Runner:          fr,
		AdminToken:      adminToken,
		TrustDomain:     "wardyn.local",
		DefaultPolicy:   types.RunPolicySpec{MinConfinementClass: types.CC2},
		ControlPlaneURL: "http://wardynd:8080",
	})

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
	if _, err := store.CreateRun(ctx, pool, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, runID) })

	srv.reconcileFinalize(ctx, runID, types.RunFailed, run.SandboxRef, "reconciled after restart")

	// The run must still be finalized terminal despite the teardown failure.
	if got, _ := store.GetRun(ctx, pool, runID); got.State != types.RunFailed {
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
