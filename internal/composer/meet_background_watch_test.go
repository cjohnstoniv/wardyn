// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestMeetDenyBackgroundDesktopWatch: the watch door only ever closes. An overlay that sets it wins, and one
// that leaves it false or silent keeps the base's denial, so an overlay can narrow the watch and never widen it.
func TestMeetDenyBackgroundDesktopWatch(t *testing.T) {
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
			base := Authority{Limits: types.GovernanceLimits{DenyBackgroundDesktopWatch: tc.base}}
			got := mustApply(t, base, limitsOverlay(types.LimitsOverlay{DenyBackgroundDesktopWatch: tc.ov}))
			if got.Limits.DenyBackgroundDesktopWatch != tc.want {
				t.Errorf("DenyBackgroundDesktopWatch = %v, want %v", got.Limits.DenyBackgroundDesktopWatch, tc.want)
			}
		})
	}
}
