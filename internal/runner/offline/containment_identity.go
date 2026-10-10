// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func missingIdentityAction(a runnerwire.PendingAction) bool {
	return a.RunID != uuid.Nil && a.Recovery == nil && a.Quarantine == nil &&
		(a.Kind == types.RunnerActionKill || a.Kind == types.RunnerActionStopProxy)
}

// containmentIdentity requires a previously checked deterministic local ref.
// The org action may recover containment authority, never execution identity.
func (m *Manager) containmentIdentity(a runnerwire.PendingAction, local string) (localRun, error) {
	if !missingIdentityAction(a) {
		return localRun{}, errors.New("missing local identity only permits exact queued containment")
	}
	r := localRun{RunID: a.RunID, Ref: local, ContainmentOnly: true, Lease: runnerwire.Lease{RunID: a.RunID}}
	m.state.Runs[a.RunID] = r
	m.state.Actions[a.ID] = appliedAction{Action: a}
	return r, m.save()
}
