// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"

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
// proof is lostProof's answer for the same action.
func (m *Manager) containmentIdentity(a runnerwire.PendingAction, local string, proof error) (localRun, error) {
	if !missingIdentityAction(a) {
		return localRun{}, refusal(errors.New("missing local identity only permits exact queued containment"))
	}
	if proof != nil {
		return localRun{}, proof
	}
	r := localRun{RunID: a.RunID, Ref: local, ContainmentOnly: true, Lease: runnerwire.Lease{RunID: a.RunID}}
	m.state.Runs[a.RunID] = r
	m.state.Actions[a.ID] = appliedAction{Action: a}
	return r, m.save()
}

// errNoProof answers for an action whose run the runner held when lostProof
// looked; it reaches containment only if the run was dropped since.
var errNoProof = errors.New("lost-identity proof was not taken; retry the action")

// lostProof proves, outside the manager lock and within proofTimeout, the ref
// a queued containment action names when the runner holds no record of its
// run, so a hung engine never holds up other manager calls.
func (m *Manager) lostProof(ctx context.Context, a runnerwire.PendingAction) error {
	m.mu.Lock()
	_, held := m.state.Runs[a.RunID]
	m.mu.Unlock()
	local, belongs := strings.CutPrefix(a.Ref, placement.RunnerRefPrefix(m.runnerID.String()))
	if held || !belongs || local == "" || local != m.expectedRef(a.RunID) || !missingIdentityAction(a) {
		return errNoProof
	}
	prove, cancel := context.WithTimeout(ctx, m.proofTimeout)
	defer cancel()
	err := m.proveLost(prove, local)
	if err != nil && prove.Err() == nil {
		return refusal(err)
	}
	return err
}

// proveLost refuses a lost-identity ref the substrate cannot prove a sandbox
// this runner created, so a queued action never reaches an unrelated
// container that happens to carry a deterministic Wardyn name.
func (m *Manager) proveLost(ctx context.Context, local string) error {
	prover, ok := m.sub.(runner.RunnerSandboxProver)
	if !ok {
		return errors.New("substrate cannot prove a sandbox is this runner's")
	}
	if err := prover.ProveRunnerSandbox(ctx, local, m.runnerID.String()); err != nil {
		slog.WarnContext(ctx, "runner: left alone a container it cannot prove it created", slog.String("ref", local), slog.String("err", err.Error()))
		return fmt.Errorf("pending action names a container this runner cannot prove it created: %w", err)
	}
	return nil
}

func (m *Manager) pendingRun(a runnerwire.PendingAction, proof error) (localRun, error) {
	if a.ID == uuid.Nil || a.RunID == uuid.Nil {
		return localRun{}, errors.New("pending action has no identity")
	}
	r, ok := m.state.Runs[a.RunID]
	local, belongs := strings.CutPrefix(a.Ref, placement.RunnerRefPrefix(m.runnerID.String()))
	if !ok && belongs && local != "" && local == m.expectedRef(a.RunID) {
		var err error
		r, err = m.containmentIdentity(a, local, proof)
		if err != nil {
			return localRun{}, err
		}
		ok = true
	}
	if !ok || !belongs || local == "" || r.Ref != local {
		return localRun{}, refusal(errors.New("pending action does not match a local run"))
	}
	return r, nil
}
