// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// teardownHold is one TeardownUnconfirmed hold: the refused action's kind (kill or end) and when the
// hold began.
type teardownHold struct {
	Kind  types.RunnerActionKind `json:"kind"`
	Since time.Time              `json:"since"`
}

// holdTeardownLocked holds run id's kind of teardown as unconfirmed, keeping the time a hold already
// there began. m.mu is held.
func (m *Manager) holdTeardownLocked(id uuid.UUID, kind types.RunnerActionKind) error {
	r, ok := m.state.Runs[id]
	if !ok {
		return nil
	}
	next := &teardownHold{Kind: kind, Since: m.now().UTC()}
	if old := r.TeardownUnconfirmed; old != nil {
		if old.Kind == kind {
			return nil
		}
		next.Since = old.Since
	}
	r.TeardownUnconfirmed = next
	m.state.Runs[id] = r
	return m.save()
}

// retryUnconfirmedTeardowns makes each held kill or end again, as the refused action would have, with
// m.mu unlocked: one that finishes drops its hold, and any other answer keeps it and is logged.
func (m *Manager) retryUnconfirmedTeardowns(ctx context.Context) {
	m.actionMu.Lock()
	defer m.actionMu.Unlock()
	m.mu.Lock()
	held := map[uuid.UUID]localRun{}
	for id, r := range m.state.Runs {
		if r.TeardownUnconfirmed != nil && r.Ref != "" {
			held[id] = r
		}
	}
	m.mu.Unlock()
	for id, r := range held {
		var err error
		if ender, ok := m.sub.(runner.SandboxEnder); ok && r.TeardownUnconfirmed.Kind == types.RunnerActionEnd {
			err = runner.KeptDespiteAWSCacheWipe(ctx, r.Ref, ender.EndSandbox(ctx, r.Ref))
		} else {
			err = m.sub.KillSandbox(ctx, r.Ref)
		}
		if err != nil {
			slog.WarnContext(ctx, "runner: a sandbox teardown is still not known to have finished", "run_id", id, "error", err)
			continue
		}
		m.mu.Lock()
		if cur, ok := m.state.Runs[id]; ok && cur.TeardownUnconfirmed != nil {
			cur.TeardownUnconfirmed = nil
			m.state.Runs[id] = cur
			if err := m.save(); err != nil {
				slog.WarnContext(ctx, "runner: could not save a finished sandbox teardown", "run_id", id, "error", err)
			}
		}
		m.mu.Unlock()
	}
}
