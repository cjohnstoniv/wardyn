// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"reflect"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A composed profile narrows allowed_macos_bases by intersection, where empty allows any base: an overlay
// can narrow the base's list, never add a base it leaves out, and a disjoint pair is unsatisfiable.
func TestAllowedMacOSBases_OverlayNarrowsByIntersection(t *testing.T) {
	tests := []struct {
		name string
		base []string
		ov   *[]string
		want []string
	}{
		{"an overlay narrows an open base", nil, &[]string{"Standard"}, []string{"Standard"}},
		{"an overlay narrows the base's list", []string{"Standard", "xcode-16"}, &[]string{"xcode-16"}, []string{"xcode-16"}},
		{"an empty overlay list keeps the base's", []string{"Standard"}, &[]string{}, []string{"Standard"}},
		{"an overlay that names nothing keeps the base's", []string{"Standard"}, nil, []string{"Standard"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mustApply(t, Authority{Limits: types.GovernanceLimits{AllowedMacOSBases: tc.base}},
				limitsOverlay(types.LimitsOverlay{AllowedMacOSBases: tc.ov}))
			if !reflect.DeepEqual(got.Limits.AllowedMacOSBases, tc.want) {
				t.Errorf("allowed_macos_bases = %v, want %v", got.Limits.AllowedMacOSBases, tc.want)
			}
		})
	}
	t.Run("a disjoint overlay is unsatisfiable, not every base", func(t *testing.T) {
		base := Authority{Limits: types.GovernanceLimits{AllowedMacOSBases: []string{"Standard"}}}
		_, _, err := ApplyOverlay(base, limitsOverlay(types.LimitsOverlay{AllowedMacOSBases: &[]string{"xcode-16"}}))
		wantOverlayErr(t, err, ReasonOverlayUnsatisfiable, "allowed_macos_bases")
	})
	t.Run("an overlay naming no base name is invalid", func(t *testing.T) {
		_, _, err := ApplyOverlay(Authority{}, limitsOverlay(types.LimitsOverlay{AllowedMacOSBases: &[]string{"../x"}}))
		wantOverlayErr(t, err, ReasonOverlayInvalid, "allowed_macos_bases")
	})
}

func TestAllowedMacOSBases_Leq(t *testing.T) {
	lim := func(b ...string) types.GovernanceLimits { return types.GovernanceLimits{AllowedMacOSBases: b} }
	for _, tc := range []struct {
		a, b types.GovernanceLimits
		want bool
	}{
		{lim("Standard"), lim(), true},
		{lim(), lim(), true},
		{lim("Standard"), lim("Standard", "xcode-16"), true},
		{lim(), lim("Standard"), false},
		{lim("xcode-16"), lim("Standard"), false},
	} {
		if got := leqLimits(tc.a, tc.b); got != tc.want {
			t.Errorf("leqLimits(%v, %v) = %v, want %v", tc.a.AllowedMacOSBases, tc.b.AllowedMacOSBases, got, tc.want)
		}
	}
}
