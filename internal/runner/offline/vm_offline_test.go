// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// No new Linux VM sandbox starts while the org is unreachable: a VM create is admitted like a
// container's, so the link being down, or the initial lease having lapsed, refuses it before any
// intent is recorded (OFFLINE-VM rule a).
func TestVMCreateIsRefusedWhileTheOrgIsUnreachable(t *testing.T) {
	ctx := context.Background()
	vmSpec := func(id uuid.UUID) runner.SandboxSpec {
		return runner.SandboxSpec{RunID: id, MachineKind: types.MachineLinuxVM}
	}
	t.Run("link down", func(t *testing.T) {
		m, _, _, org := managerFixture(t)
		id := uuid.New()
		if err := m.Lease(ctx, runnerwire.Lease{RunID: id, TokenRenewedAt: org, EndsAt: org.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		m.Offline(ctx)
		if err := m.BeforeCreate(ctx, vmSpec(id)); !errors.Is(err, runner.ErrRunnerOffline) {
			t.Fatalf("a VM create with the link down = %v, want %v", err, runner.ErrRunnerOffline)
		}
		if _, ok := m.state.Runs[id]; ok {
			t.Fatal("a refused VM create left an intent")
		}
	})
	t.Run("initial lease lapsed", func(t *testing.T) {
		m, _, elapsed, org := managerFixture(t)
		id := uuid.New()
		if err := m.Lease(ctx, runnerwire.Lease{RunID: id, TokenRenewedAt: org, EndsAt: org.Add(3 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
		*elapsed = identity.RunTokenLapseAfter
		if err := m.BeforeCreate(ctx, vmSpec(id)); err == nil {
			t.Fatal("a VM create on a lapsed initial lease was admitted")
		}
		if _, ok := m.state.Runs[id]; ok {
			t.Fatal("a refused VM create left an intent")
		}
	})
}
