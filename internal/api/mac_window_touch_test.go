// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "testing"

// TestMacWindowOpen_TouchesRun pins the window's own idle-clock touches, which do not depend on any egress
// decision: one when the window opens (macWindowOpen) and one for each re-check that lets it stay
// (macWindowRecheck). An open window keeps the run from idling out whatever its egress rows say.
func TestMacWindowOpen_TouchesRun(t *testing.T) {
	r := newMacWindowRig(t)
	r.mustOpen()
	if got := r.st.touched.Load(); got != 1 {
		t.Fatalf("touches after the open = %d, want 1", got)
	}
	r.tick()
	macWindowEventually(t, "the staying re-check's touch", func() bool { return r.st.touched.Load() >= 2 })
}
