// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestAttachRefResolvesALiveRunAndRefusesAnUnknownOne(t *testing.T) {
	m, _, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	if got := m.AttachRef(id); got != id.String() {
		t.Fatalf("AttachRef(live run) = %q, want %q", got, id.String())
	}
	if got := m.AttachRef(uuid.New()); got != "" {
		t.Fatalf("AttachRef(unknown run) = %q", got)
	}
}

func TestAttachRefRefusesAContainedOrEndedRun(t *testing.T) {
	for name, mark := range map[string]func(*localRun){
		"contained": func(r *localRun) { r.Containment = &types.RunnerMutationFailure{} },
		"ended":     func(r *localRun) { r.Retired = true },
	} {
		t.Run(name, func(t *testing.T) {
			m, _, _, org := managerFixture(t)
			id := createLocal(t, m, org)
			m.mu.Lock()
			r := m.state.Runs[id]
			mark(&r)
			m.state.Runs[id] = r
			m.mu.Unlock()
			if got := m.AttachRef(id); got != "" {
				t.Fatalf("AttachRef(%s run) = %q, want refused", name, got)
			}
		})
	}
}

// The exec half of the shared refusal: a contained run gets no agent execution.
func TestExecRefusedForAContainedRun(t *testing.T) {
	m, _, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	generation := uuid.New()
	lease := m.state.Runs[id].Lease
	lease.ExecutionGeneration = generation
	if err := m.Lease(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	r := m.state.Runs[id]
	r.Containment = &types.RunnerMutationFailure{}
	m.state.Runs[id] = r
	m.mu.Unlock()
	if err := m.BeforeRefCall(context.Background(), runnerwire.MethodExec, id.String(), generation); err == nil {
		t.Fatal("a contained run was admitted to exec")
	}
}
