// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/google/uuid"
)

func TestFailedSubstrateMutationLatchesBeforeDrain(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	if err := m.MutationFinished(t.Context(), runnerwire.MethodCreateSandbox, "", id, uuid.Nil, nil); err != nil {
		t.Fatal(err)
	}
	gen := executionIntent(t, m, id, org, runnerwire.MethodExec)
	if m.mutationsDrained(id) {
		t.Fatal("admitted execution was not tracked")
	}
	if err := m.MutationFinished(t.Context(), runnerwire.MethodExec, id.String(), uuid.Nil, uuid.New(), nil); err == nil || m.mutationsDrained(id) {
		t.Fatal("wrong-generation completion drained admitted mutation")
	}
	sub.stopErr = errors.New("containment unavailable")
	cause := errors.New("lost substrate reply")
	if err := m.MutationFinished(t.Context(), runnerwire.MethodExec, id.String(), uuid.Nil, gen, cause); !errors.Is(err, sub.stopErr) {
		t.Fatalf("cleanup failure lost: %v", err)
	}
	r := m.state.Runs[id]
	if !m.mutationsDrained(id) || r.MutationFailure == nil || r.MutationFailure.Contained || r.ExecID != "" || r.Executions[gen] != runnerwire.MethodExec {
		t.Fatalf("ambiguous intent lost or fabricated: %+v", r)
	}
	if err := m.BeforeRefCall(t.Context(), runnerwire.MethodReplaceProxy, id.String(), uuid.Nil); err == nil {
		t.Fatal("live lease bypassed failed substrate mutation")
	}
	marker := r.MutationFailure.ID
	sub.stopErr = nil
	if err := m.CheckLapses(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m.state.Runs[id].MutationFailure.ID != marker || !m.state.Runs[id].MutationFailure.Contained {
		t.Fatal("retry changed failure identity or missed containment")
	}
}

func TestCreateDrainIncludesLatePersistenceAndRollback(t *testing.T) {
	m, _, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	if m.mutationsDrained(id) {
		t.Fatal("Created incorrectly drained output/rollback lifetime")
	}
	if err := m.MutationFinished(t.Context(), runnerwire.MethodCreateSandbox, "", id, uuid.Nil, errors.New("late output bind failure")); err != nil {
		t.Fatal(err)
	}
	if !m.mutationsDrained(id) || !m.state.Runs[id].FailedCreation || m.state.Runs[id].ProxyStoppedAt == nil {
		t.Fatal("failed late create was not contained before drain")
	}
	if err := m.Created(t.Context(), runner.SandboxSpec{RunID: id}, id.String()); err == nil {
		t.Fatal("late callback revived failed creation")
	}
}

func TestFailedMutationPreservesExistingMarker(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	gen := executionIntent(t, m, id, org, runnerwire.MethodExec)
	restore := failStateSave(t, m)
	if err := m.AfterRefCall(t.Context(), runnerwire.MethodExec, id.String(), "handle", gen); err == nil {
		t.Fatal("fixture save succeeded")
	}
	marker := *m.state.Runs[id].MutationFailure
	restore()
	if err := m.MutationFinished(t.Context(), runnerwire.MethodExec, id.String(), uuid.Nil, gen, errors.New("persistence failed")); err != nil {
		t.Fatal(err)
	}
	got := m.state.Runs[id].MutationFailure
	if got.ID != marker.ID || got.Generation != marker.Generation || got.Operation != marker.Operation || !got.Contained || sub.stops < 2 {
		t.Fatalf("finish rotated marker or missed containment: %+v", got)
	}
}
