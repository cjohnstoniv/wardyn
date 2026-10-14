// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/google/uuid"
)

// ActivityFromRunner is H2 Admission.Activity, bound by H3 to the authenticated
// runner. It must be called only under H2's current-session replacement barrier.
func (s *Server) ActivityFromRunner(ctx context.Context, runnerID, runID uuid.UUID) error {
	st, ok := s.cfg.Store.(interface {
		TouchRunnerActivity(context.Context, uuid.UUID, uuid.UUID) error
	})
	if !ok {
		return store.ErrRunnerExecutionRefused
	}
	return st.TouchRunnerActivity(ctx, runnerID, runID)
}
