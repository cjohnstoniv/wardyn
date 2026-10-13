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
func (m *Manager) containmentIdentity(ctx context.Context, a runnerwire.PendingAction, local string) (localRun, error) {
	if !missingIdentityAction(a) {
		return localRun{}, errors.New("missing local identity only permits exact queued containment")
	}
	if err := m.proveLost(ctx, local); err != nil {
		return localRun{}, err
	}
	r := localRun{RunID: a.RunID, Ref: local, ContainmentOnly: true, Lease: runnerwire.Lease{RunID: a.RunID}}
	m.state.Runs[a.RunID] = r
	m.state.Actions[a.ID] = appliedAction{Action: a}
	return r, m.save()
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

func (m *Manager) pendingRun(ctx context.Context, a runnerwire.PendingAction) (localRun, error) {
	if a.ID == uuid.Nil {
		return localRun{}, errors.New("pending action has no identity")
	}
	r, ok := m.state.Runs[a.RunID]
	local, belongs := strings.CutPrefix(a.Ref, placement.RunnerRefPrefix(m.runnerID.String()))
	if !ok && belongs && local != "" && local == m.expectedRef(a.RunID) {
		var err error
		r, err = m.containmentIdentity(ctx, a, local)
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
