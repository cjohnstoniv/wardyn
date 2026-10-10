// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/google/uuid"
)

// ExecutionResult reads only the exact durable intent requested by the org.
// An attempted intent without a handle is ambiguous and must never run again.
func (m *Manager) ExecutionResult(ctx context.Context, ref string, generation uuid.UUID) (runnerwire.ExecutionResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return runnerwire.ExecutionResult{}, err
	}
	_, r, ok := m.findRef(ref)
	if !ok || generation == uuid.Nil || r.Generation != generation || r.Executions[generation] == "" {
		return runnerwire.ExecutionResult{}, errors.New("execution result has no matching durable intent")
	}
	return runnerwire.ExecutionResult{Generation: generation, Operation: r.Executions[generation], ExecID: r.ExecID, Attempted: true}, nil
}
