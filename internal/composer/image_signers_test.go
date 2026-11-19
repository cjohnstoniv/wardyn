// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"reflect"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// required_image_signers is a requirement: each named signer must also vouch for a member's image, so more
// names is tighter. A is at most as permissive as B when A names every signer B names.
func TestRequiredImageSigners_Leq(t *testing.T) {
	lim := func(n ...string) types.GovernanceLimits { return types.GovernanceLimits{RequiredImageSigners: n} }
	for _, tc := range []struct {
		name string
		a, b types.GovernanceLimits
		want bool
	}{
		{"a superset is tighter", lim("corp-ci", "vendor-key"), lim("corp-ci"), true},
		{"the same set", lim("corp-ci"), lim("corp-ci"), true},
		{"the same set in another order", lim("vendor-key", "corp-ci"), lim("corp-ci", "vendor-key"), true},
		{"a subset is looser", lim("corp-ci"), lim("corp-ci", "vendor-key"), false},
		{"a disjoint set drops a requirement", lim("vendor-key"), lim("corp-ci"), false},
		{"an empty b admits any a", lim("corp-ci"), lim(), true},
		{"both empty", lim(), lim(), true},
		{"an empty a against a requirement", lim(), lim("corp-ci"), false},
	} {
		if got := leqLimits(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: leqLimits(%v, %v) = %v, want %v", tc.name, tc.a.RequiredImageSigners, tc.b.RequiredImageSigners, got, tc.want)
		}
	}
}

// A composed profile unions required_image_signers: an overlay adds requirements and can never drop the
// base's. The result is deduplicated and keeps the base's order, then the overlay's new names in its order.
func TestRequiredImageSigners_Meet(t *testing.T) {
	for _, tc := range []struct {
		name string
		base []string
		ov   *[]string
		want []string
	}{
		{"an overlay adds to an open base", nil, &[]string{"corp-ci"}, []string{"corp-ci"}},
		{"the union of two lists", []string{"corp-ci"}, &[]string{"vendor-key"}, []string{"corp-ci", "vendor-key"}},
		{"a shared name is kept once", []string{"corp-ci", "vendor-key"}, &[]string{"vendor-key", "wardyn-release"}, []string{"corp-ci", "vendor-key", "wardyn-release"}},
		{"base order wins over overlay order", []string{"corp-ci"}, &[]string{"vendor-key", "corp-ci"}, []string{"corp-ci", "vendor-key"}},
		{"an empty overlay list keeps the base's", []string{"corp-ci"}, &[]string{}, []string{"corp-ci"}},
		{"an overlay that names nothing keeps the base's", []string{"corp-ci"}, nil, []string{"corp-ci"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mustApply(t, Authority{Limits: types.GovernanceLimits{RequiredImageSigners: tc.base}},
				limitsOverlay(types.LimitsOverlay{RequiredImageSigners: tc.ov}))
			if !reflect.DeepEqual(got.Limits.RequiredImageSigners, tc.want) {
				t.Errorf("required_image_signers = %v, want %v", got.Limits.RequiredImageSigners, tc.want)
			}
		})
	}
	t.Run("the meet result is never looser than the base", func(t *testing.T) {
		base := types.GovernanceLimits{RequiredImageSigners: []string{"corp-ci"}}
		got := mustApply(t, Authority{Limits: base}, limitsOverlay(types.LimitsOverlay{RequiredImageSigners: &[]string{"vendor-key"}}))
		if !leqLimits(got.Limits, base) {
			t.Errorf("composed limits %v are looser than the base %v", got.Limits.RequiredImageSigners, base.RequiredImageSigners)
		}
	})
	t.Run("a malformed name is refused at write", func(t *testing.T) {
		_, _, err := ApplyOverlay(Authority{}, limitsOverlay(types.LimitsOverlay{RequiredImageSigners: &[]string{"Not A Name"}}))
		wantOverlayErr(t, err, ReasonOverlayInvalid, "required_image_signers")
	})
}
