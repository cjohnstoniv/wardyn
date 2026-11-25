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
// container's, so the link being down (runner_offline), or the initial lease having lapsed (a plain error), refuses it before any
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
		// The plain error of every create on a lapsed lease (not a runner_offline refusal).
		if err := m.BeforeCreate(ctx, vmSpec(id)); err == nil || err.Error() != "runner create requires a live initial lease" {
			t.Fatalf("a VM create on a lapsed initial lease = %v, want the live-initial-lease error", err)
		}
		if _, ok := m.state.Runs[id]; ok {
			t.Fatal("a refused VM create left an intent")
		}
	})
}

// No new macOS VM sandbox starts while the org is unreachable either: the same create gate that refuses a
// Linux VM refuses it, before any intent is recorded (#2950).
func TestMacOSVMCreateIsRefusedWhileTheOrgIsUnreachable(t *testing.T) {
	ctx := context.Background()
	m, _, _, org := managerFixture(t)
	id := uuid.New()
	if err := m.Lease(ctx, runnerwire.Lease{RunID: id, TokenRenewedAt: org, EndsAt: org.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	m.Offline(ctx)
	spec := runner.SandboxSpec{RunID: id, MachineKind: types.MachineMacOSVM}
	if err := m.BeforeCreate(ctx, spec); !errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("a macOS VM create with the link down = %v, want %v", err, runner.ErrRunnerOffline)
	}
	if _, ok := m.state.Runs[id]; ok {
		t.Fatal("a refused macOS VM create left an intent")
	}
}
