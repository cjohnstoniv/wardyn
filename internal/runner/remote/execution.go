// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

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
