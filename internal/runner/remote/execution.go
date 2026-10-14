// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// ExecutionResult reads the runner's durable result for one already-issued
// operation. H3 compares Operation with its persisted dispatch before binding
// the handle. An empty handle means ambiguous intent, never permission to retry.
// Call only from an asynchronous reconciler after READY, not a checked handler.
func (s *Substrate) ExecutionResult(ctx context.Context, ref string, generation uuid.UUID) (runnerwire.ExecutionResult, error) {
	var result runnerwire.ExecutionResult
	if err := s.own(ref); err != nil {
		return result, err
	}
	if generation == uuid.Nil {
		return result, runnerwire.Refuse("execution result requires the issued generation")
	}
	if err := s.call(ctx, runnerwire.MethodExecutionResult, runnerwire.ExecutionResultArgs{Ref: ref, Generation: generation}, &result); err != nil {
		return runnerwire.ExecutionResult{}, err
	}
	if result.Generation != generation || !result.Attempted || (result.Operation != runnerwire.MethodExec && result.Operation != runnerwire.MethodStart) {
		return runnerwire.ExecutionResult{}, fmt.Errorf("%w: execution result differs from requested intent", runnerwire.ErrBadFrame)
	}
	return result, nil
}

func (s *Substrate) executionGeneration(ctx context.Context, method, ref string) (uuid.UUID, error) {
	s.mu.Lock()
	required := s.requireExecutionGeneration
	s.mu.Unlock()
	if s.opts.ExecutionGeneration == nil {
		if required {
			return uuid.Nil, runnerwire.Refuse("runner execution dispatch is not configured")
		}
		return uuid.Nil, nil
	}
	if !s.t.Online() {
		return uuid.Nil, errOffline
	}
	generation, err := s.opts.ExecutionGeneration(ctx, method, ref)
	if err != nil {
		return uuid.Nil, err
	}
	if generation == uuid.Nil {
		return uuid.Nil, runnerwire.Refuse("runner execution dispatch lacks a persisted generation")
	}
	return generation, nil
}
