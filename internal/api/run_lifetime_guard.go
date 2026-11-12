// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pastLifetimeCap reports whether run has reached its lifetime cap: the lease sweep is about to end it,
// so from this instant nothing may act for it as if it were live. A run with no cap never has.
func pastLifetimeCap(run types.AgentRun, now time.Time) bool {
	return run.LifetimeCapAt != nil && !now.Before(*run.LifetimeCapAt)
}

// lifetimePassedEnd is how a queued start ends a run whose captured cap has already passed, or nil.
func lifetimePassedEnd(anchor types.AgentRun, now time.Time) *queueEnd {
	if !pastLifetimeCap(anchor, now) {
		return nil
	}
	hint := types.RunEndMaxLifetimeReached.Sentence()
	if anchor.LifetimeCapSource == types.LimitSourceElevation {
		hint = "the elevation it was launched under has ended"
	}
	return &queueEnd{token: string(types.RunEndMaxLifetimeReached), hint: hint}
}
