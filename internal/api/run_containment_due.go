// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// containmentLag is L: how long after a run's end the containment loop owns it. It is longer than the
// longest end hold, 4 min 30 s.
const containmentLag = store.ContainmentLag

// containmentDue is when the containment loop takes an ended run: its end plus containmentLag, the end
// being EndedAt, or UpdatedAt when EndedAt is unset. It is false for a run that is not terminal: a
// running run is not the containment loop's, and an ended run's due is never earlier than its end plus L.
// store.ContainmentDueSQL is its SQL twin.
func containmentDue(run types.AgentRun) (time.Time, bool) {
	if !isTerminalRunState(run.State) {
		return time.Time{}, false
	}
	end := run.UpdatedAt
	if run.EndedAt != nil {
		end = *run.EndedAt
	}
	return end.Add(containmentLag), true
}
