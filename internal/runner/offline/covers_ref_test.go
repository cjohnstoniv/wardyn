// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// TestCoversRef pins A2's runner half (#3667 F1): a held run is covered only while the substrate covers
// its machine kind, and a ref this runner does not hold is covered, so the dispatcher's ownership proof
// decides it.
func TestCoversRef(t *testing.T) {
	refOf := func(m *Manager, id uuid.UUID) string {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.state.Runs[id].Ref
	}
	t.Run("unknown ref", func(t *testing.T) {
		m, _, _, _ := managerFixture(t)
		if !m.CoversRef("not-a-run-of-this-runner") {
			t.Fatal("an unknown ref is not covered; want covered")
		}
	})
	t.Run("container run", func(t *testing.T) {
		m, _, _, org := managerFixture(t)
		if id := createKind(t, m, org, types.MachineContainer); !m.CoversRef(refOf(m, id)) {
			t.Fatal("a container run is not covered")
		}
	})
	for _, tc := range []struct {
		name   string
		covers bool
	}{
		{name: "macos_vm off", covers: false},
		{name: "macos_vm on", covers: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, sub, _, org := managerFixture(t)
			m.sub = &kindSub{localSub: sub, covers: map[types.MachineKind]bool{types.MachineMacOSVM: tc.covers}}
			id := createKind(t, m, org, types.MachineMacOSVM)
			if got := m.CoversRef(refOf(m, id)); got != tc.covers {
				t.Fatalf("CoversRef = %v; want %v", got, tc.covers)
			}
		})
	}
	t.Run("undeclared substrate", func(t *testing.T) {
		m, _, _, org := managerFixture(t)
		linux := createKind(t, m, org, types.MachineLinuxVM)
		mac := createKind(t, m, org, types.MachineMacOSVM)
		if !m.CoversRef(refOf(m, linux)) {
			t.Fatal("a linux_vm run is not covered by a substrate that declares no kinds")
		}
		if m.CoversRef(refOf(m, mac)) {
			t.Fatal("a macos_vm run is covered by a substrate that declares no kinds")
		}
	})
}
