// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// An observer admitted before an asserted quarantine still holds its drain
// slot when kill, quarantine and revocation arrive. Erasure stays pending until
// it drains, and its late exit is never published on the quarantined run, so
// the erased run keeps no exit and no spool directory.
func TestAssertedErasureWaitsForAdmittedObserver(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	if err := m.MutationFinished(t.Context(), runnerwire.MethodCreateSandbox, "", id, uuid.Nil, nil); err != nil {
		t.Fatal(err)
	}
	generation := uuid.New()
	m.mu.Lock()
	r := m.state.Runs[id]
	r.Generation, r.ExecID = generation, "agent"
	r.Executions = map[uuid.UUID]string{generation: runnerwire.MethodExec}
	m.state.Runs[id] = r
	err := m.save()
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(m.dir, "spools", id.String())
	spool, err := runnerspool.Open(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	appendDecision(t, spool, id)
	if err := spool.Close(); err != nil {
		t.Fatal(err)
	}

	held := heldExitSub{sub, make(chan struct{}), make(chan struct{})}
	m.sub = held
	observed := make(chan error, 1)
	go func() { _, err := m.observeRun(t.Context(), id); observed <- err }()
	<-held.entered

	ref := placement.RunnerRefPrefix(m.runnerID.String()) + id.String()
	kill := runnerwire.PendingAction{ID: uuid.New(), RunID: id, Ref: ref, Kind: types.RunnerActionKill}
	quarantine := runnerwire.PendingAction{ID: uuid.New(), RunID: id, Ref: ref, Kind: types.RunnerActionQuarantine,
		Quarantine: &types.RunnerQuarantine{ID: uuid.New(), Generation: generation, Asserted: true}}
	for _, a := range []runnerwire.PendingAction{kill, quarantine} {
		if result, err := m.apply(t.Context(), a); err != nil || result.Outcome != "deferred" {
			t.Fatalf("%s under an admitted observer: %+v %v", a.Kind, result, err)
		}
	}
	if err := m.Revoked(t.Context()); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	erased := m.state.Runs[id].EvidenceErased
	m.mu.Unlock()
	if erased {
		t.Fatal("evidence erased while an admitted observer still held its drain slot")
	}

	close(held.release)
	if err := <-observed; err != nil {
		t.Fatal(err)
	}
	if m.state.Runs[id].Exit != nil {
		t.Fatal("observer published an exit on a quarantined run")
	}
	if err := m.eraseAssertedEvidence(); err != nil {
		t.Fatal(err)
	}
	if r := m.state.Runs[id]; !r.EvidenceErased || r.Exit != nil || r.Quarantine == nil {
		t.Fatalf("erasure after drain: erased=%v exit=%+v", r.EvidenceErased, r.Exit)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("erased spool directory present: %v", err)
	}
}
