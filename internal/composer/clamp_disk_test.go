// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestCapDiskMiB is the min() both the preview and dispatch share.
//
// The ZERO rows are the ones that matter: 0 on the request side means "unbounded
// scratch" and a ceiling must not invent a size there (fill from a MAXIMUM and
// every request-less run gets a non-zero DiskMiB, which the docker driver fails
// the create closed on over overlay2-on-ext4 — every laptop). 0 on the ceiling
// side means "no bound", the zero-value rule every GovernanceLimits field follows.
func TestCapDiskMiB(t *testing.T) {
	for _, tc := range []struct {
		name             string
		disk, ceil, want int
	}{
		{"over the ceiling is clamped", 8192, 4096, 4096},
		{"under the ceiling is untouched", 1024, 4096, 1024},
		{"at the ceiling is untouched", 4096, 4096, 4096},
		{"no ceiling leaves the request alone", 8192, 0, 8192},
		{"a zero request is NEVER filled from the ceiling", 0, 4096, 0},
		{"zero on both sides stays unbounded", 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CapDiskMiB(tc.disk, tc.ceil); got != tc.want {
				t.Errorf("CapDiskMiB(%d, %d) = %d, want %d", tc.disk, tc.ceil, got, tc.want)
			}
		})
	}
}

// TestClamp_GovernanceEphemeralDiskLimitIsPreviewParity pins the reason the limit
// is an argument of Clamp at all.
//
// Dispatch is the ONE site that binds GovernanceLimits.MaxEphemeralDiskMiB for
// every lane (api.applyEphemeralDisk). Clamp is what a MEMBER sees first — POST
// /runs/preflight and the New Run Review rail run through it — so without the
// limit here a member previewed a size their run then silently did not get. The
// warning is the one capField already emits: there is no new string.
func TestClamp_GovernanceEphemeralDiskLimitIsPreviewParity(t *testing.T) {
	ceiling := operatorCeiling(t)
	ceiling.Resources = nil // no SPEC cap: the LIMIT is the only thing bounding disk

	got, warns := Clamp(types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		Resources:           &types.ResourceLimits{DiskMiB: 40960},
	}, ceiling, 4096)
	if got.Resources == nil || got.Resources.DiskMiB != 4096 {
		t.Fatalf("DiskMiB = %v, want 4096 (clamped to the profile's MaxEphemeralDiskMiB)", got.Resources)
	}
	if !hasWarn(warns, WarnResourcesCapped) {
		t.Errorf("warns = %v, want %q", warns, WarnResourcesCapped)
	}

	// A ZERO request under the same limit stays zero — the limit bounds a
	// request, and only the org's default_disk_mib (at dispatch) ever fills one.
	got, _ = Clamp(types.RunPolicySpec{MinConfinementClass: types.CC2}, ceiling, 4096)
	if got.Resources != nil && got.Resources.DiskMiB != 0 {
		t.Errorf("DiskMiB = %d, want 0 — a maximum must never fill a request-less run", got.Resources.DiskMiB)
	}

	// No limit: byte-for-byte the pre-0.7.2 answer for the same proposal.
	got, _ = Clamp(types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		Resources:           &types.ResourceLimits{DiskMiB: 40960},
	}, ceiling, 0)
	if got.Resources == nil || got.Resources.DiskMiB != 40960 {
		t.Errorf("DiskMiB = %v, want 40960 left alone when the profile sets no limit", got.Resources)
	}
}
