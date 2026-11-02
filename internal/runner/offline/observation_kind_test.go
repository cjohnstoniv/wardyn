// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// kindSub is a localSub that declares which machine kinds it can enumerate (runner.KindCoverer).
type kindSub struct {
	*localSub
	covers map[types.MachineKind]bool
}

func (s *kindSub) CoversKind(k types.MachineKind) bool { return s.covers[k.Normalize()] }

// createKind is createLocalAs for a run of the given machine kind.
func createKind(t *testing.T, m *Manager, org time.Time, kind types.MachineKind) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if err := m.Lease(ctx, runnerwire.Lease{RunID: id, TokenRenewedAt: org, EndsAt: org.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	spec := runner.SandboxSpec{RunID: id, MachineKind: kind}
	if err := m.BeforeCreate(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := m.Created(ctx, spec, id.String()); err != nil {
		t.Fatal(err)
	}
	return id
}

// presentOf reads the inventory and answers each run's SandboxPresent.
func presentOf(t *testing.T, m *Manager) map[uuid.UUID]bool {
	t.Helper()
	rows, err := allState(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[uuid.UUID]bool, len(rows))
	for _, row := range rows {
		out[row.RunID] = row.SandboxPresent
	}
	return out
}

func (s *localSub) statusCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statuses
}

func (s *localSub) setStatus(st runner.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = st
}

func TestObserveRunReportsUncoveredKindAsPresent(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	m.sub = &kindSub{localSub: sub, covers: map[types.MachineKind]bool{types.MachineMacOSVM: false}}
	sub.setStatus(runner.Status{State: types.RunStopped})
	id := createKind(t, m, org, types.MachineMacOSVM)
	before := sub.statusCalls()
	if present := presentOf(t, m); !present[id] {
		t.Fatal("a macOS VM run the substrate cannot enumerate read absent")
	}
	if after := sub.statusCalls(); after != before {
		t.Fatalf("an uncovered kind was probed: %d Status calls, want %d", after, before)
	}
}

func TestObserveRunReportsCoveredGoneKindAbsent(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	m.sub = &kindSub{localSub: sub, covers: map[types.MachineKind]bool{types.MachineMacOSVM: true}}
	sub.setStatus(runner.Status{State: types.RunStopped})
	id := createKind(t, m, org, types.MachineMacOSVM)
	if presentOf(t, m)[id] {
		t.Fatal("a covered macOS VM run whose sandbox is gone read present")
	}
	sub.setStatus(runner.Status{State: types.RunRunning})
	if !presentOf(t, m)[id] {
		t.Fatal("a covered running macOS VM run read absent")
	}
}

func TestObserveRunTreatsUndeclaredSubstrateAsContainerOrLinuxVM(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	sub.setStatus(runner.Status{State: types.RunStopped})
	unset := createKind(t, m, org, "")
	container := createKind(t, m, org, types.MachineContainer)
	linux := createKind(t, m, org, types.MachineLinuxVM)
	mac := createKind(t, m, org, types.MachineMacOSVM)
	present := presentOf(t, m)
	for name, id := range map[string]uuid.UUID{"unset": unset, "container": container, "linux_vm": linux} {
		if present[id] {
			t.Errorf("%s run with a gone sandbox read present on a substrate that declares no kinds", name)
		}
	}
	if !present[mac] {
		t.Error("macos_vm run read absent on a substrate that declares no kinds")
	}
}

func TestObserveRunReportsARecordedExitAsPresent(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	sub.setStatus(runner.Status{State: types.RunStopped})
	id := createKind(t, m, org, types.MachineContainer)
	setExit := func(acked bool) {
		t.Helper()
		m.mu.Lock()
		defer m.mu.Unlock()
		r := m.state.Runs[id]
		r.Exit = &runnerwire.AgentExit{Generation: r.Generation, Ref: placement.RunnerRefPrefix(m.runnerID.String()) + r.Ref, Code: 0, ObservedAt: m.now().UTC()}
		r.ExitAcked = acked
		m.state.Runs[id] = r
		if err := m.save(); err != nil {
			t.Fatal(err)
		}
	}
	setExit(false)
	if !presentOf(t, m)[id] {
		t.Fatal("a run holding an unacknowledged exit read absent")
	}
	setExit(true)
	if presentOf(t, m)[id] {
		t.Fatal("a run whose exit was acknowledged and whose sandbox is gone read present")
	}
}
