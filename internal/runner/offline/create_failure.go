// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// CreateFailed retains the durable intent and contains even ambiguous partial
// provisioning immediately. StopProxy cannot prove the agent absent, so this
// callback never authorizes deleting output or retrying the same run creation.
func (m *Manager) CreateFailed(ctx context.Context, spec runner.SandboxSpec) error {
	m.mu.Lock()
	r, ok := m.state.Runs[spec.RunID]
	if !ok {
		m.mu.Unlock()
		return errors.New("failed create has no durable intent")
	}
	r.FailedCreation = true
	m.state.Runs[spec.RunID] = r
	err := m.save()
	m.mu.Unlock()
	safety, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return errors.Join(err, m.CheckLapses(safety))
}
