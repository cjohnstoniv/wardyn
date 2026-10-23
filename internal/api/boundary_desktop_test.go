// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The Wardyn boundary's desktop knob (#2967): it only blocks more, at resolve
// (boundary_desktop_pg_test.go) and again at the final seal.

var allDesktopAllowed = types.DesktopControls{PasteIn: types.TransferAllowed, CopyOut: types.TransferAllowed,
	Upload: types.TransferAllowed, Download: types.TransferAllowed}

// TestBoundarySealBlocksTheDesktopDirectionsItBlocks: a boundary that blocks copy out blocks it at the
// seal for a run whose policy allows it, records the boundary as deciding desktop and leaves the
// directions it allows open; a run the boundary already holds is left as it is spelled, and a
// boundary that cannot be read blocks every direction.
func TestBoundarySealBlocksTheDesktopDirectionsItBlocks(t *testing.T) {
	wall := allDesktopAllowed
	wall.CopyOut = types.TransferBlocked
	hard := types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{Desktop: &wall}}

	open := allDesktopAllowed
	policy := types.RunPolicySpec{Desktop: &open}
	prov := sealUnder(sealBoundaryStore{b: hard}, &policy)
	if got := policy.Desktop.Effective(); got != wall {
		t.Errorf("sealed desktop = %+v, want %+v (copy out blocked, the rest kept)", got, wall)
	}
	if prov["desktop"].Layer != policyref.LayerBoundary {
		t.Errorf("provenance desktop = %+v, want the boundary", prov["desktop"])
	}

	held := types.RunPolicySpec{}
	if prov := sealUnder(sealBoundaryStore{b: hard}, &held); held.Desktop != nil || len(prov) != 0 {
		t.Errorf("a run already inside the boundary sealed to %+v (%v), want it untouched", held.Desktop, prov)
	}

	unread := types.RunPolicySpec{Desktop: &allDesktopAllowed}
	sealUnder(sealBoundaryStore{err: errors.New("down")}, &unread)
	if got := unread.Desktop.Effective(); got.PasteIn != types.TransferBlocked || got.CopyOut != types.TransferBlocked ||
		got.Upload != types.TransferBlocked || got.Download != types.TransferBlocked {
		t.Errorf("unreadable boundary sealed desktop = %+v, want every direction blocked", got)
	}
}

// TestBoundaryRefusesAnUnknownDesktopRule: a boundary desktop rule is allowed or blocked, as a policy's is.
func TestBoundaryRefusesAnUnknownDesktopRule(t *testing.T) {
	bad := types.BoundaryWrite{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{Desktop: &types.DesktopControls{CopyOut: "maybe"}}}
	if err := validateBoundary(&bad); err == nil || !strings.Contains(err.Error(), "desktop.copy_out") {
		t.Fatalf("validateBoundary = %v, want desktop.copy_out refused", err)
	}
	good := types.BoundaryWrite{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{Desktop: &allDesktopAllowed}}
	if err := validateBoundary(&good); err != nil {
		t.Fatalf("validateBoundary(allowed) = %v", err)
	}
}
