// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestMeetDenyKeptMachines: the kept-machine door only ever closes. An overlay
// that sets it wins, and one that leaves it false or silent keeps the base's denial.
func TestMeetDenyKeptMachines(t *testing.T) {
	for _, tc := range []struct {
		name string
		base bool
		ov   *bool
		want bool
	}{
		{"overlay denies, base open", false, ptr(true), true},
		{"overlay open, base denies", true, ptr(false), true},
		{"overlay silent, base denies", true, nil, true},
		{"overlay silent, base open", false, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := Authority{Limits: types.GovernanceLimits{DenyKeptMachines: tc.base}}
			got := mustApply(t, base, limitsOverlay(types.LimitsOverlay{DenyKeptMachines: tc.ov}))
			if got.Limits.DenyKeptMachines != tc.want {
				t.Errorf("DenyKeptMachines = %v, want %v", got.Limits.DenyKeptMachines, tc.want)
			}
		})
	}
}
