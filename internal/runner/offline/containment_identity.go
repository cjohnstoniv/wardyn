// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"errors"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func missingIdentityAction(a runnerwire.PendingAction) bool {
	return a.RunID != uuid.Nil && a.Recovery == nil && a.Quarantine == nil && a.Retirement == nil &&
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

func (m *Manager) pendingRun(a runnerwire.PendingAction) (localRun, error) {
	if a.ID == uuid.Nil {
		return localRun{}, errors.New("pending action has no identity")
	}
	r, ok := m.state.Runs[a.RunID]
	local, belongs := strings.CutPrefix(a.Ref, placement.RunnerRefPrefix(m.runnerID.String()))
	if !ok && belongs && local != "" && local == m.expectedRef(a.RunID) {
		var err error
		r, err = m.containmentIdentity(a, local)
		if err != nil {
			return localRun{}, err
		}
		ok = true
	}
	if !ok || !belongs || local == "" || r.Ref != local {
		return localRun{}, errors.New("pending action does not match a local run")
	}
	return r, nil
}
