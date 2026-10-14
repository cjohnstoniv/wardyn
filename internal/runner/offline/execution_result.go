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
	if err := m.ensureDurable(); err != nil {
		return runnerwire.ExecutionResult{}, err
	}
	_, r, ok := m.findRef(ref)
	if !ok || generation == uuid.Nil || r.Generation != generation {
		return runnerwire.ExecutionResult{}, errors.New("execution result has no matching durable intent")
	}
	result := durableExecutionResult(r)
	if result == nil {
		return runnerwire.ExecutionResult{}, errors.New("execution result has no matching durable intent")
	}
	return *result, nil
}

func durableExecutionResult(r localRun) *runnerwire.ExecutionResult {
	if r.Generation == uuid.Nil || r.Executions[r.Generation] == "" {
		return nil
	}
	return &runnerwire.ExecutionResult{Generation: r.Generation, Operation: r.Executions[r.Generation], ExecID: r.ExecID, Attempted: true, MutationFailure: r.MutationFailure, Containment: r.Containment}
}
