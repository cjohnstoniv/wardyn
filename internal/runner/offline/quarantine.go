// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
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
	if r.Quarantine == nil {
		at := m.now().UTC()
		r.QuarantinedAt = &at
	}
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

// eraseAssertedEvidence deletes the spool, output and exit of an org-erased
// (asserted) run once its quarantine is contained and either the runner is
// revoked or types.RunnerAssertedEvidenceTTL has passed since the quarantine.
// The compact identity, quarantine and attempted generations stay behind as
// tombstones, so the run can never be recreated or executed. Quarantine admits
// no new observer or producer, so erasure waits only for those admitted before
// it to drain; a later pass erases.
func (m *Manager) eraseAssertedEvidence() error {
	m.mu.Lock()
	if err := m.ensureDurable(); err != nil {
		m.mu.Unlock()
		return err
	}
	now := m.now()
	var due []uuid.UUID
	for id, r := range m.state.Runs {
		if r.Quarantine != nil && r.Quarantine.Asserted && !r.EvidenceErased && r.ProxyStoppedAt != nil && r.QuarantinedAt != nil && m.mutationsDrained(id) &&
			(m.state.Revoked || now.Sub(*r.QuarantinedAt) >= types.RunnerAssertedEvidenceTTL) {
			due = append(due, id)
		}
	}
	m.mu.Unlock()
	var errs []error
	for _, id := range due {
		if err := errors.Join(os.RemoveAll(filepath.Join(m.dir, "spools", id.String())), m.eraseOutput(id)); err != nil {
			errs = append(errs, fmt.Errorf("erase run %s evidence: %w", id, err))
			continue
		}
		m.mu.Lock()
		before := m.state.Runs[id]
		r := before
		r.Exit, r.EvidenceErased = nil, true
		m.state.Runs[id] = r
		if err := m.save(); err != nil {
			m.state.Runs[id] = before
			errs = append(errs, err)
		}
		m.mu.Unlock()
	}
	return errors.Join(errs...)
}
