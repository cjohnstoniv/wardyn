// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func quarantineAction(m *Manager, id uuid.UUID) runnerwire.PendingAction {
	return runnerwire.PendingAction{ID: uuid.New(), RunID: id, Ref: placement.RunnerRefPrefix(m.runnerID.String()) + id.String(), Kind: types.RunnerActionQuarantine, Quarantine: &types.RunnerQuarantine{ID: uuid.New(), Generation: m.state.Runs[id].Generation}}
}

func TestQuarantinePreservesEvidenceAndOtherInventory(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	if err := m.MutationFinished(t.Context(), runnerwire.MethodCreateSandbox, "", id, uuid.Nil, nil); err != nil {
		t.Fatal(err)
	}
	spool, err := runnerspool.Open(filepath.Join(m.dir, "spools", id.String()), id)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	if err := spool.Append(t.Context(), egress.DecisionLog{Request: egress.Request{RunID: id, Host: "example.test", Time: time.Now()}, Decision: egress.Allow}); err != nil {
		t.Fatal(err)
	}
	a := quarantineAction(m, id)
	restore := failStateSave(t, m)
	if _, err := m.apply(t.Context(), a); err == nil || sub.stops != 0 {
		t.Fatal("quarantine side effect before durable disposition")
	}
	restore()
	result, err := m.apply(t.Context(), a)
	if err != nil || result.Outcome != "success" {
		t.Fatalf("quarantine: %+v %v", result, err)
	}
	if err := m.Lease(t.Context(), runnerwire.Lease{RunID: id, TokenRenewedAt: org}); err == nil {
		t.Fatal("quarantine accepted renewal")
	}
	if err := m.BeforeRefCall(t.Context(), runnerwire.MethodReplaceProxy, id.String(), uuid.Nil); err == nil {
		t.Fatal("quarantine allowed proxy restart")
	}
	if err := m.Replay(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	pos, err := spool.Get(t.Context())
	depth, _, _, statsErr := spool.Stats(t.Context())
	if err != nil || statsErr != nil || pos.Seq != 0 || depth != 1 {
		t.Fatal("quarantine acknowledged or deleted historical evidence")
	}
	other := createLocal(t, m, org)
	rows, err := m.State(t.Context())
	if err != nil || len(rows) != 1 || rows[0].RunID != other {
		t.Fatalf("unrelated inventory stranded: %+v %v", rows, err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(m.dir, m.runnerID, sub, uuid.UUID.String)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replay, err := reopened.apply(t.Context(), a)
	if err != nil || replay != result {
		t.Fatalf("quarantine lost-ACK receipt: %+v %v", replay, err)
	}
	if reopened.state.Runs[id].Quarantine == nil {
		t.Fatal("quarantine lost on restart")
	}
}

func TestQuarantineDefersUntilLateMutationFinishes(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	if err := m.MutationFinished(t.Context(), runnerwire.MethodCreateSandbox, "", id, uuid.Nil, nil); err != nil {
		t.Fatal(err)
	}
	gen := executionIntent(t, m, id, org, runnerwire.MethodExec)
	a := quarantineAction(m, id)
	deferred, err := m.apply(t.Context(), a)
	if err != nil || deferred.Outcome != "deferred" {
		t.Fatalf("in-flight quarantine falsely settled: %+v %v", deferred, err)
	}
	// The old call may finish after the quarantine ACK; its success cannot
	// restart egress or discharge containment under the still-fresh lease.
	callErr := m.AfterRefCall(t.Context(), runnerwire.MethodExec, id.String(), "late-handle", gen)
	if callErr == nil {
		t.Fatal("late mutation reported ordinary success")
	}
	if err := m.MutationFinished(t.Context(), runnerwire.MethodExec, id.String(), uuid.Nil, gen, callErr); err == nil {
		t.Fatal("late completion ignored quarantine")
	}
	if sub.stops == 0 || !m.mutationsDrained(id) || m.state.Runs[id].ProxyStoppedAt == nil {
		t.Fatal("late mutation not contained before drain")
	}
	replayed, err := m.apply(t.Context(), a)
	if err != nil || replayed != deferred {
		t.Fatal("deferred receipt changed after convergence")
	}
	a.ID = uuid.New()
	settled, err := m.apply(t.Context(), a)
	if err != nil || settled.Outcome != "success" {
		t.Fatalf("fresh action did not settle: %+v %v", settled, err)
	}
	wrong := a
	wrong.ID = uuid.New()
	proof := *a.Quarantine
	proof.Generation = uuid.New()
	wrong.Quarantine = &proof
	if _, err := m.apply(t.Context(), wrong); err == nil {
		t.Fatal("wrong-generation quarantine accepted")
	}
}
