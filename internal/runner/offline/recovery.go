// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"errors"
	"reflect"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/google/uuid"
)

func sameContainmentTarget(a, b localRun) bool {
	if a.Ref != b.Ref || a.Generation != b.Generation {
		return false
	}
	if a.MutationFailure == nil || b.MutationFailure == nil {
		return a.MutationFailure == nil && b.MutationFailure == nil
	}
	return *a.MutationFailure == *b.MutationFailure
}

func samePendingAction(a, b runnerwire.PendingAction) bool {
	if a.ID != b.ID || a.Kind != b.Kind || a.RunID != b.RunID || a.Ref != b.Ref {
		return false
	}
	if (a.Retirement == nil) != (b.Retirement == nil) || (a.Retirement != nil && *a.Retirement != *b.Retirement) {
		return false
	}
	if (a.Quarantine == nil) != (b.Quarantine == nil) || (a.Quarantine != nil && *a.Quarantine != *b.Quarantine) {
		return false
	}
	if !reflect.DeepEqual(a.NodeRecord, b.NodeRecord) {
		return false
	}
	if a.Recovery == nil || b.Recovery == nil {
		return a.Recovery == nil && b.Recovery == nil
	}
	return *a.Recovery == *b.Recovery
}

// recoverMutation runs under mu, after ordinary pending run/ref authority has
// been checked. It clears only an org-reconciled exact durable failure marker;
// the action success receipt and clearance are persisted in one state write.
func (m *Manager) recoverMutation(a runnerwire.PendingAction, r localRun) (runnerwire.ActionResult, error) {
	if err := m.ensureDurable(); err != nil {
		return runnerwire.ActionResult{}, err
	}
	if a.Recovery == nil || r.MutationFailure == nil || *a.Recovery != *r.MutationFailure || !r.MutationFailure.Contained || r.ProxyStoppedAt == nil || r.FailedCreation || r.ContainmentOnly || r.Quarantine != nil || r.Containment != nil {
		return runnerwire.ActionResult{}, errors.New("mutation recovery proof does not match contained failure")
	}
	if r.Generation != a.Recovery.Generation {
		return runnerwire.ActionResult{}, errors.New("mutation recovery generation changed")
	}
	if r.Generation == uuid.Nil {
		if len(r.Executions) != 0 {
			return runnerwire.ActionResult{}, errors.New("mutation recovery cannot erase an issued execution")
		}
	} else if r.Executions[r.Generation] == "" || r.ExecID == "" {
		return runnerwire.ActionResult{}, errors.New("ambiguous execution requires independent result reconciliation")
	}
	r.MutationFailure = nil
	m.state.Runs[a.RunID] = r
	result := runnerwire.ActionResult{ID: a.ID, Outcome: "success", ObservedAt: m.now().UTC()}
	// Copy pointer-bearing input so the retained receipt has immutable identity.
	recovery := *a.Recovery
	a.Recovery = &recovery
	m.state.Actions[a.ID] = appliedAction{Action: a, Result: result}
	if err := m.save(); err != nil {
		return runnerwire.ActionResult{}, err
	}
	return result, nil
}
