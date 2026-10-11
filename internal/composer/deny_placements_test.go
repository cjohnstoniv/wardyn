// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"reflect"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A composed profile narrows deny_placements by union (overlay kind `list`):
// a denial is only ever added, so an overlay can neither lift the base's nor
// replace its list.
func TestDenyPlacements_OverlayNarrowsByUnion(t *testing.T) {
	remote, local := types.PlacementRemote, types.PlacementLocal
	tests := []struct {
		name string
		base []types.Placement
		ov   *[]types.Placement
		want []types.Placement
	}{
		{"an overlay adds to an empty base", nil, &[]types.Placement{remote}, []types.Placement{remote}},
		{"an empty overlay list does not lift the base's denial", []types.Placement{local}, &[]types.Placement{}, []types.Placement{local}},
		{"an overlay that names nothing keeps the base's denial", []types.Placement{local}, nil, []types.Placement{local}},
		{"the same denial twice is one", []types.Placement{local}, &[]types.Placement{local}, []types.Placement{local}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mustApply(t, Authority{Limits: types.GovernanceLimits{DenyPlacements: tc.base}},
				limitsOverlay(types.LimitsOverlay{DenyPlacements: tc.ov}))
			if !reflect.DeepEqual(got.Limits.DenyPlacements, tc.want) {
				t.Errorf("deny_placements = %v, want %v", got.Limits.DenyPlacements, tc.want)
			}
		})
	}

	t.Run("a union that denies every placement is unsatisfiable, not applied", func(t *testing.T) {
		base := Authority{Limits: types.GovernanceLimits{DenyPlacements: []types.Placement{local}}}
		_, _, err := ApplyOverlay(base, limitsOverlay(types.LimitsOverlay{DenyPlacements: &[]types.Placement{remote}}))
		wantOverlayErr(t, err, ReasonOverlayUnsatisfiable, "limits.deny_placements")
	})
	t.Run("an overlay naming a placement that does not exist is invalid", func(t *testing.T) {
		_, _, err := ApplyOverlay(Authority{}, limitsOverlay(types.LimitsOverlay{DenyPlacements: &[]types.Placement{"cloud"}}))
		wantOverlayErr(t, err, ReasonOverlayInvalid, "deny_placements")
	})
}
