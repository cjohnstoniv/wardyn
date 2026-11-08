// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/setup"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// backendPlacement is what Review's fold read of the placement a run lands on, for the "backend" setup
// row: the barriers that placement enforces (the runner the request pins, the deployment's executor, or a
// pool's eligible candidates), and whether it could be read at all. The compose panel has no placement and
// passes nil, which keeps the row probing the control plane's own runner.
type backendPlacement struct {
	classes []types.ConfinementClass
	read    bool
}

// advertisedConfinementRead is advertisedConfinement that also says whether the placement could be read:
// an offline runner is read=false. A deployment with no executor at all is read, with no class: nothing will
// answer at launch, so the row is missing, not left to a runner that does not exist.
func (s *Server) advertisedConfinementRead(ctx context.Context, req createRunRequest) ([]types.ConfinementClass, bool) {
	pc, err := s.requestCapabilities(ctx, req)
	if errors.Is(err, errNoRemoteExecutor) {
		return nil, true
	}
	if err != nil {
		return nil, false
	}
	return pc.ConfinementClasses, true
}

// placedBackendItem completes the "backend" row from the placement's own barriers, by the same membership
// test the launch gate makes. A placement the fold could not read is unverified, since the runner answers at
// launch.
func placedBackendItem(it SetupItem, final types.ConfinementClass, placed backendPlacement) SetupItem {
	switch {
	case !placed.read:
		it.Status = "unverified"
		it.Detail = "Checked at the runner when the run starts."
	case slices.Contains(placed.classes, final):
		it.Status = "satisfied"
	default:
		return backendMissingItem(it, final)
	}
	return it
}

// backendMissingItem is the "backend" row for a class the host cannot enforce: "needs setup" when the
// runtime is installable and "not fixable on this host" otherwise. Its Fix is always "none".
func backendMissingItem(it SetupItem, final types.ConfinementClass) SetupItem {
	it.Status = "missing"
	it.Fix = &SetupFix{Action: "none"}
	switch final {
	case types.CC3:
		// Single-source the KVM copy: VaultKVMDetail names the real fix
		// (bind-mount /dev/kvm) for a containerized wardynd instead of
		// asserting a bare "hardware limit no install can fix".
		it.Detail = setup.VaultKVMDetail()
	case types.CC2:
		it.Detail = "no Wall (gVisor) runtime registered on this host yet — fixable, run `wardyn setup wall`. See Getting Started."
	default:
		it.Detail = "no sandbox runtime registered on this host at all — check that wardynd's runner is configured and its container daemon is reachable. See Getting Started."
	}
	return it
}
