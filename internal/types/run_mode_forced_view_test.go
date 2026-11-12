// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

// Only a background run with a boot's report reads as forced: every other combination, including one
// whose mode and report disagree, reads as not forced.
func TestAgentRunDesktopForcedViewOnly(t *testing.T) {
	forced, notForced := &VMMachine{DesktopForcedViewOnly: true}, &VMMachine{}
	for _, c := range []struct {
		experience RunExperience
		machine    *VMMachine
		want       bool
	}{
		{ExperienceBackground, forced, true},
		{ExperienceBackground, notForced, false},
		{ExperienceBackground, nil, false},
		{ExperienceInteractive, forced, false},
		{"", forced, false},
	} {
		r := AgentRun{Experience: c.experience, VMMachine: c.machine}
		if got := r.DesktopForcedViewOnly(); got != c.want {
			t.Errorf("experience %q machine %+v: %v, want %v", c.experience, c.machine, got, c.want)
		}
	}
}
