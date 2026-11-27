// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestClampOutage(t *testing.T) {
	ceiling := types.RunPolicySpec{Outage: &types.OutageControls{AllowanceMinutes: 60, Strict: true}}
	for _, tc := range []struct {
		name     string
		proposed *types.OutageControls
		want     types.OutageControls
		warns    int
	}{
		{"longer proposal narrowed", &types.OutageControls{AllowanceMinutes: 120}, types.OutageControls{AllowanceMinutes: 60, Strict: true}, 1},
		{"unset proposal takes the ceiling's", nil, types.OutageControls{AllowanceMinutes: 60, Strict: true}, 0},
		{"proposal within kept", &types.OutageControls{AllowanceMinutes: 30, Strict: true}, types.OutageControls{AllowanceMinutes: 30, Strict: true}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := types.RunPolicySpec{Outage: tc.proposed}
			warns := clampOutage(&out, ceiling, nil)
			if out.Outage == nil || *out.Outage != tc.want || len(warns) != tc.warns {
				t.Fatalf("clampOutage(%+v) = %+v %q, want %+v and %d warning(s)", tc.proposed, out.Outage, warns, tc.want, tc.warns)
			}
		})
	}
	within := types.RunPolicySpec{Outage: &types.OutageControls{AllowanceMinutes: 30}}
	ceiling60 := types.RunPolicySpec{Outage: &types.OutageControls{AllowanceMinutes: 60}}
	if warns := clampOutage(&within, ceiling60, nil); len(warns) != 0 || within.Outage.AllowanceMinutes != 30 {
		t.Fatalf("a proposal of 30 under a ceiling of 60 was changed: %+v %q", within.Outage, warns)
	}
	got, warns := Clamp(types.RunPolicySpec{Outage: &types.OutageControls{AllowanceMinutes: 240}}, ceiling, types.GovernanceLimits{})
	if got.Outage == nil || *got.Outage != (types.OutageControls{AllowanceMinutes: 60, Strict: true}) || len(warns) == 0 {
		t.Fatalf("Clamp kept a longer allowance than the ceiling's: %+v %q", got.Outage, warns)
	}
}

func TestLeqOutage(t *testing.T) {
	spec := func(o *types.OutageControls) types.RunPolicySpec { return types.RunPolicySpec{Outage: o} }
	var none types.GovernanceLimits
	for _, tc := range []struct {
		name string
		a, b *types.OutageControls
		want bool
	}{
		{"both unset", nil, nil, true},
		{"anything under an unset b", &types.OutageControls{AllowanceMinutes: 1440}, nil, true},
		{"unset a reads as the longest", nil, &types.OutageControls{AllowanceMinutes: 60}, false},
		{"shorter allowance", &types.OutageControls{AllowanceMinutes: 30}, &types.OutageControls{AllowanceMinutes: 60}, true},
		{"longer allowance", &types.OutageControls{AllowanceMinutes: 120}, &types.OutageControls{AllowanceMinutes: 60}, false},
		{"strict dropped", &types.OutageControls{AllowanceMinutes: 30}, &types.OutageControls{Strict: true}, false},
		{"strict kept", &types.OutageControls{Strict: true}, &types.OutageControls{Strict: true}, true},
		{"end dropped", &types.OutageControls{Unattended: types.OutageUnattendedKeep}, &types.OutageControls{Unattended: types.OutageUnattendedEnd}, false},
		{"end kept", &types.OutageControls{Unattended: types.OutageUnattendedEnd}, &types.OutageControls{Unattended: types.OutageUnattendedEnd}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Leq(spec(tc.a), spec(tc.b), none, none); got != tc.want {
				t.Fatalf("Leq(outage %+v, outage %+v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestApplyOverlay_OutageNarrowsAndWidenIsReported(t *testing.T) {
	base := Authority{Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2, Outage: &types.OutageControls{AllowanceMinutes: 60}}}
	narrow := Overlay{Ceiling: types.CeilingOverlay{Outage: &types.OutageControls{AllowanceMinutes: 30}}}
	got, warns, err := ApplyOverlay(base, narrow)
	if err != nil || got.Ceiling.Outage == nil || got.Ceiling.Outage.AllowanceMinutes != 30 || len(warns) != 0 {
		t.Fatalf("overlay 30 over base 60 = %+v %q %v, want 30 and no warning", got.Ceiling.Outage, warns, err)
	}
	wide := Overlay{Ceiling: types.CeilingOverlay{Outage: &types.OutageControls{AllowanceMinutes: 120}}}
	got, warns, err = ApplyOverlay(base, wide)
	if err != nil || got.Ceiling.Outage == nil || got.Ceiling.Outage.AllowanceMinutes != 60 || len(warns) != 1 {
		t.Fatalf("overlay 120 over base 60 = %+v %q %v, want 60 and one warning", got.Ceiling.Outage, warns, err)
	}
	if err := ValidateOverlay(base, wide); err == nil {
		t.Fatal("an overlay lengthening the base's allowance was accepted at write")
	}
	bad := Overlay{Ceiling: types.CeilingOverlay{Outage: &types.OutageControls{AllowanceMinutes: 10}}}
	if err := ValidateOverlay(base, bad); err == nil {
		t.Fatal("an overlay allowance of 10 minutes was accepted at write")
	}
}
