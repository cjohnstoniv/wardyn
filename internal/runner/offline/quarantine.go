// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/google/uuid"
)

// quarantineRun runs with actionMu and mu held. Suppression never advances an
// evidence cursor; it only isolates this run from unrelated reconnect progress.
func (m *Manager) quarantineRun(ctx context.Context, a runnerwire.PendingAction, r localRun) (runnerwire.ActionResult, error) {
	if a.Recovery != nil || a.Quarantine == nil || a.Quarantine.ID == uuid.Nil || a.Quarantine.Generation != r.Generation || (r.Generation == uuid.Nil && len(r.Executions) != 0) {
		return runnerwire.ActionResult{}, errors.New("quarantine proof does not match local identity")
	}
	if r.Quarantine != nil && *r.Quarantine != *a.Quarantine {
		return runnerwire.ActionResult{}, errors.New("quarantine identity changed")
	}
	proof := *a.Quarantine
	a.Quarantine = &proof
	r.Quarantine = &proof
	m.state.Runs[a.RunID] = r
	m.state.Actions[a.ID] = appliedAction{Action: a}
	if err := m.save(); err != nil {
		return runnerwire.ActionResult{}, err
	}
	// A terminal deferred receipt allows STATE/Replay for other runs while an
	// old admitted call finishes. H3 must issue a fresh action after convergence.
	if !m.mutationsDrained(a.RunID) {
		return m.actionReceipt(a, "deferred")
	}
	m.mu.Unlock()
	err := m.stopper.StopProxy(ctx, r.Ref)
	m.mu.Lock()
	if err != nil {
		return runnerwire.ActionResult{}, err
	}
	current := m.state.Runs[a.RunID]
	if current.Ref != r.Ref || current.Generation != r.Generation || !m.mutationsDrained(a.RunID) {
		return m.actionReceipt(a, "deferred")
	}
	at := m.now().UTC()
	current.ProxyStoppedAt = &at
	m.state.Runs[a.RunID] = current
	return m.actionReceipt(a, "success")
}

func (m *Manager) actionReceipt(a runnerwire.PendingAction, outcome string) (runnerwire.ActionResult, error) {
	result := runnerwire.ActionResult{ID: a.ID, Outcome: outcome, ObservedAt: m.now().UTC()}
	m.state.Actions[a.ID] = appliedAction{Action: a, Result: result}
	return result, m.save()
}
