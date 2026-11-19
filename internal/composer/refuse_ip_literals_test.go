// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// refuse_ip_literals only ever narrows: a meet ORs it, a clamp keeps the ceiling's, and Leq reads
// true as the narrower side.

func TestRefuseIPLiterals_MeetORs(t *testing.T) {
	on := Authority{Ceiling: types.RunPolicySpec{AllowAllEgress: true, RefuseIPLiterals: true}}
	if got := mustApply(t, on, overlayOf(types.CeilingOverlay{RefuseIPLiterals: ptr(false)})); !got.Ceiling.RefuseIPLiterals {
		t.Error("an overlay's false turned the base's refuse_ip_literals off")
	}
	off := Authority{Ceiling: types.RunPolicySpec{AllowAllEgress: true}}
	if got := mustApply(t, off, overlayOf(types.CeilingOverlay{RefuseIPLiterals: ptr(true)})); !got.Ceiling.RefuseIPLiterals {
		t.Error("an overlay's true did not turn refuse_ip_literals on")
	}
	if got := mustApply(t, off, overlayOf(types.CeilingOverlay{})); got.Ceiling.RefuseIPLiterals {
		t.Error("an overlay that names nothing turned refuse_ip_literals on")
	}
}

func TestRefuseIPLiterals_ClampKeepsTheCeilings(t *testing.T) {
	ceiling := types.RunPolicySpec{AllowAllEgress: true, RefuseIPLiterals: true}
	if got, _ := Clamp(types.RunPolicySpec{AllowAllEgress: true}, ceiling, types.GovernanceLimits{}); !got.RefuseIPLiterals {
		t.Error("a proposal without refuse_ip_literals escaped the ceiling's")
	}
	if got, _ := Clamp(types.RunPolicySpec{AllowAllEgress: true, RefuseIPLiterals: true}, types.RunPolicySpec{AllowAllEgress: true}, types.GovernanceLimits{}); !got.RefuseIPLiterals {
		t.Error("a proposal's own refuse_ip_literals was dropped")
	}
}

func TestRefuseIPLiterals_LeqReadsTrueAsNarrower(t *testing.T) {
	on := types.RunPolicySpec{AllowAllEgress: true, RefuseIPLiterals: true}
	off := types.RunPolicySpec{AllowAllEgress: true}
	if !Leq(on, off, types.GovernanceLimits{}, types.GovernanceLimits{}) {
		t.Error("refuse_ip_literals on is not within off")
	}
	if Leq(off, on, types.GovernanceLimits{}, types.GovernanceLimits{}) {
		t.Error("refuse_ip_literals off counted as within on")
	}
}
