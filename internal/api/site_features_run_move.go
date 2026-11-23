// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// features.run_move: moving a paused run to another machine. The key can be turned on only once the move is
// reachable (moveGateOpen); until then a write that turns it on or sets it is refused, and a stored true reads off.

// moveMaxCarriedGiBMax is the most max_carried_gib may be set to, and moveMaxCarriedGiBDefault what 0 or absent reads.
const (
	moveMaxCarriedGiBMax     = 200
	moveMaxCarriedGiBDefault = 20
)

func init() {
	registerSiteFeature(featureRunMove, siteFeatureHook{
		available: func() bool { return moveGateOpen() },
		validate:  validateRunMoveFeature,
	})
}

// validateRunMoveFeature refuses a max_carried_gib outside 0 or 1 to moveMaxCarriedGiBMax.
func validateRunMoveFeature(_ context.Context, _ *Server, next, _ *types.SiteFeatureSwitches) error {
	if next == nil || next.RunMove == nil {
		return nil
	}
	if n := next.RunMove.MaxCarriedGiB; n < 0 || n > moveMaxCarriedGiBMax {
		return fmt.Errorf("features.run_move.max_carried_gib is %d: set a number of GiB from 1 to %d, or leave it out for %d",
			n, moveMaxCarriedGiBMax, moveMaxCarriedGiBDefault)
	}
	return nil
}

// moveAvailable reports whether moving a run is on: this release can turn it on and site config does.
func moveAvailable(sc types.SiteConfig) bool {
	return siteFeatureOn(sc, featureRunMove)
}

// moveToPersonalRunners reports whether a run may move onto a person's own runner. Off when absent.
func moveToPersonalRunners(sc types.SiteConfig) bool {
	m := siteFeatureSettings(sc).RunMove
	return m != nil && m.ToPersonalRunners != nil && *m.ToPersonalRunners
}

// moveMaxCarriedBytes is what one move may carry: max_carried_gib, or the default when it is 0 or absent.
func moveMaxCarriedBytes(sc types.SiteConfig) int64 {
	gib := moveMaxCarriedGiBDefault
	if m := siteFeatureSettings(sc).RunMove; m != nil && m.MaxCarriedGiB > 0 {
		gib = m.MaxCarriedGiB
	}
	return int64(gib) << 30
}
