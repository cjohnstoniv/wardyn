// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import "github.com/cjohnstoniv/wardyn/internal/types"

// TightenRunLimits is captured with each bound that binds a live run replaced
// by the profile's current one wherever that is tighter, and only there. The
// defaults are left alone: they shape a new run, not a live one.
//
// Shared by the live-run reclamp and by the overlay meet, so a running child is
// reclamped to exactly the bounds a new run under the same composition gets.
func TightenRunLimits(captured, profile types.RunLimits) types.RunLimits {
	out := captured
	out.MaxEndAheadSec = TighterSec(captured.MaxEndAheadSec, profile.MaxEndAheadSec)
	out.MaxWaitSec = TighterSec(captured.MaxWaitSec, profile.MaxWaitSec)
	out.PauseIdleAfterSec = TighterSec(captured.PauseIdleAfterSec, profile.PauseIdleAfterSec)
	out.AllowNoEnd = captured.AllowNoEnd && profile.AllowNoEnd
	out.UserChangesLimits = captured.UserChangesLimits && profile.UserChangesLimits
	return out
}

// TighterSec is the tighter of two bounds where 0 is no bound.
func TighterSec(a, b int) int {
	if a == 0 || (b > 0 && b < a) {
		return b
	}
	return a
}
