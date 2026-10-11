// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

type mutationKey struct {
	method, ref string
	generation  uuid.UUID
}

func (m *Manager) beginMutation(method, ref string, generation uuid.UUID) {
	if m.inflight == nil {
		m.inflight = map[mutationKey]int{}
	}
	m.inflight[mutationKey{method, ref, generation}]++
}

func mutationFailure(r localRun, method string) *types.RunnerMutationFailure {
	if r.MutationFailure != nil {
		failure := *r.MutationFailure
		failure.Contained = false
		return &failure
	}
	return &types.RunnerMutationFailure{ID: uuid.New(), Generation: r.Generation, Operation: method}
}

// MutationFinished releases the exact admitted mutation only after its safety
// follow-up. A failed substrate call may have taken effect and cannot clear the
// attempted intent or rely on a fresh lease instead of containment.
func (m *Manager) MutationFinished(ctx context.Context, method, ref string, runID, generation uuid.UUID, cause error) error {
	if method == runnerwire.MethodCreateSandbox {
		if runID == uuid.Nil || ref != "" || generation != uuid.Nil {
			return errors.New("create completion identity mismatch")
		}
		ref = m.expectedRef(runID)
	} else if runID != uuid.Nil {
		return errors.New("ref completion unexpectedly supplies run identity")
	}
	key := mutationKey{method, ref, generation}
	m.mu.Lock()
	if m.inflight[key] == 0 {
		m.mu.Unlock()
		return errors.New("mutation completion has no matching admission")
	}
	defer func() {
		m.mu.Lock()
		m.finishMutation(key)
		m.mu.Unlock()
	}()
	var err error
	id, r, known := m.findRef(ref)
	var late error
	if known && (r.Quarantine != nil || r.Containment != nil) {
		late = errors.New("mutation completed after run containment disposition")
		cause = errors.Join(cause, late)
	}
	if cause != nil && known {
		if method == runnerwire.MethodCreateSandbox {
			r.FailedCreation = true
		} else {
			r.MutationFailure = mutationFailure(r, method)
		}
		m.state.Runs[id] = r
		err = m.save()
	}
	m.mu.Unlock()
	if cause == nil {
		return nil
	}
	if known {
		return errors.Join(late, err, m.CheckLapses(ctx))
	}
	return m.stopper.StopProxy(ctx, ref)
}

// mutationsDrained runs under mu before any retirement absence/seal proof.
func (m *Manager) mutationsDrained(id uuid.UUID) bool {
	ref := m.expectedRef(id)
	for key := range m.inflight {
		if key.ref == ref {
			return false
		}
	}
	return true
}

func (m *Manager) finishMutation(key mutationKey) {
	m.inflight[key]--
	if m.inflight[key] == 0 {
		delete(m.inflight, key)
	}
}
