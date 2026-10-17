// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func executionIntent(t *testing.T, m *Manager, id uuid.UUID, _ time.Time, method string) uuid.UUID {
	t.Helper()
	generation := uuid.New()
	lease := m.state.Runs[id].Lease
	lease.ExecutionGeneration = generation
	if err := m.Lease(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if err := m.BeforeRefCall(context.Background(), method, id.String(), generation); err != nil {
		t.Fatal(err)
	}
	return generation
}

func TestExecutionIntentSurvivesAmbiguousRetryAndRestart(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	ctx := context.Background()
	generation := executionIntent(t, m, id, org, runnerwire.MethodExec)
	if err := m.BeforeRefCall(ctx, runnerwire.MethodExec, id.String(), generation); err == nil {
		t.Fatal("ambiguous Exec attempted twice")
	}
	dir, rn := m.dir, m.runnerID
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(dir, rn, sub, uuid.UUID.String, noOutput, noErase)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.Clock(runnerwire.Hello{ServerTime: org}, restarted.now()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.BeforeReady(ctx, &runnerwire.Peer{}, true); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Lease(ctx, restarted.state.Runs[id].Lease); err != nil {
		t.Fatal(err)
	}
	if err := restarted.BeforeRefCall(ctx, runnerwire.MethodExec, id.String(), generation); err == nil {
		t.Fatal("restart forgot execution intent")
	}
	// A real correlated result can be persisted, but restart ambiguity remains
	// faulted until the org explicitly reconciles and echoes its failure marker.
	if err := restarted.AfterRefCall(ctx, runnerwire.MethodExec, id.String(), "exec-1", generation); err == nil {
		t.Fatal("ambiguous restart bypassed recovery barrier")
	}
	if err := restarted.CheckLapses(ctx); err != nil {
		t.Fatal(err)
	}
	proof := *restarted.state.Runs[id].MutationFailure
	action := runnerwire.PendingAction{ID: uuid.New(), Kind: types.RunnerActionRecoverMutation, RunID: id, Ref: "runner:" + restarted.runnerID.String() + "/" + id.String(), Recovery: &proof}
	if _, err := restarted.apply(ctx, action); err != nil {
		t.Fatal(err)
	}
	if err := restarted.AfterRefCall(ctx, runnerwire.MethodExec, id.String(), "exec-2", generation); err == nil {
		t.Fatal("execution handle rebound")
	}
	if err := restarted.AgentExited(ctx, id, id.String(), 0); err != nil {
		t.Fatal(err)
	}
	exit := restarted.state.Runs[id].Exit
	if exit == nil || exit.Generation != generation || exit.ExecID != "exec-1" {
		t.Fatalf("unbound exit %+v", exit)
	}
}

func TestExecutionCallRequiresExactGenerationAndContainmentDoesNot(t *testing.T) {
	m, _, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	ctx := context.Background()
	lease := m.state.Runs[id].Lease
	lease.ExecutionGeneration = uuid.New()
	if err := m.Lease(ctx, lease); err != nil {
		t.Fatal(err)
	}
	for _, gen := range []uuid.UUID{uuid.Nil, uuid.New()} {
		for _, method := range []string{runnerwire.MethodExec, runnerwire.MethodStart} {
			if err := m.BeforeRefCall(ctx, method, id.String(), gen); err == nil {
				t.Fatal("missing/stale execution generation admitted")
			}
		}
	}
	if err := m.BeforeRefCall(ctx, runnerwire.MethodStart, id.String(), lease.ExecutionGeneration); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{runnerwire.MethodKill, runnerwire.MethodStop, runnerwire.MethodStopProxy, runnerwire.MethodEnd} {
		if err := m.BeforeRefCall(ctx, method, id.String(), uuid.Nil); err != nil {
			t.Fatal(err)
		}
	}
}

// A selftest is admitted only under the lease's own generation and only before
// that generation's Exec was attempted, and it writes nothing either way.
func TestCheckSelftestAdmitsOnlyTheLeasesUnattemptedGeneration(t *testing.T) {
	m, _, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	ctx := context.Background()
	generation := uuid.New()
	lease := m.state.Runs[id].Lease
	lease.ExecutionGeneration = generation
	if err := m.Lease(ctx, lease); err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(m.state)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CheckSelftest(ctx, id.String(), generation); err != nil {
		t.Fatalf("selftest before any attempt = %v, want admitted", err)
	}
	if after, _ := json.Marshal(m.state); !bytes.Equal(before, after) {
		t.Fatalf("an admitted selftest wrote state:\n%s\n%s", before, after)
	}
	if err := m.CheckSelftest(ctx, id.String(), uuid.New()); err == nil {
		t.Fatal("selftest admitted under a generation that is not the lease's")
	}
	if err := m.CheckSelftest(ctx, id.String(), uuid.Nil); err == nil {
		t.Fatal("selftest admitted under the nil generation")
	}
	if err := m.BeforeRefCall(ctx, runnerwire.MethodExec, id.String(), generation); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckSelftest(ctx, id.String(), generation); err == nil {
		t.Fatal("selftest admitted after the generation's Exec was attempted")
	}
}
