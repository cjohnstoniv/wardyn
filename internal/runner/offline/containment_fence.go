// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (m *Manager) beginContainment(a runnerwire.PendingAction, r localRun) error {
	switch a.Kind {
	case types.RunnerActionKill, types.RunnerActionEnd, types.RunnerActionStopProxy:
	default:
		return errors.New("unknown pending containment action")
	}
	if r.Containment == nil || containmentRank(a.Kind) > containmentRank(types.RunnerActionKind(r.Containment.Operation)) {
		r.Containment = &types.RunnerMutationFailure{ID: a.ID, Generation: r.Generation, Operation: string(a.Kind)}
	}
	if r.Containment.Operation == string(a.Kind) {
		marker := *r.Containment
		marker.Contained = false
		r.Containment = &marker
	}
	if r.Lease.TokenRenewedAt.After(r.LeaseAfter) {
		r.LeaseAfter = r.Lease.TokenRenewedAt
	}
	m.state.Runs[a.RunID] = r
	m.state.Actions[a.ID] = appliedAction{Action: a}
	delete(m.deadlines, a.RunID)
	delete(m.pending, a.RunID)
	return m.save()
}

func containmentRank(kind types.RunnerActionKind) int {
	switch kind {
	case types.RunnerActionKill:
		return 3
	case types.RunnerActionEnd:
		return 2
	case types.RunnerActionStopProxy:
		return 1
	default:
		return 0
	}
}

// recoverContainment does not issue authority. H3 must authorize this exact
// action first, then separately issue a new lease above the persisted replay floor.
func (m *Manager) recoverContainment(a runnerwire.PendingAction, r localRun) (runnerwire.ActionResult, error) {
	proof := a.Recovery
	if proof == nil || r.Containment == nil || *proof != *r.Containment || !proof.Contained || !m.mutationsDrained(a.RunID) || r.Quarantine != nil || r.ContainmentOnly || r.FailedCreation {
		return runnerwire.ActionResult{}, errors.New("containment recovery proof does not match settled disposition")
	}
	if proof.Operation != string(types.RunnerActionEnd) && proof.Operation != string(types.RunnerActionStopProxy) {
		return runnerwire.ActionResult{}, errors.New("terminal containment cannot be recovered")
	}
	if proof.Generation != r.Generation {
		return runnerwire.ActionResult{}, errors.New("containment recovery generation changed")
	}
	r.Containment = nil
	m.state.Runs[a.RunID] = r
	delete(m.deadlines, a.RunID)
	delete(m.pending, a.RunID)
	copyProof := *proof
	a.Recovery = &copyProof
	result := runnerwire.ActionResult{ID: a.ID, Outcome: "success", ObservedAt: m.now().UTC()}
	m.state.Actions[a.ID] = appliedAction{Action: a, Result: result}
	return result, m.save()
}
