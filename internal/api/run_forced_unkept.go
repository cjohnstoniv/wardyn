// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// forceUnkeptHeld is leaseRun's answer to a kept-run visit whose lock is held. A run whose target is known
// not to keep its sandbox has nothing to wait for but the lock: the visit ends it through stopKeptRun, whose
// held path (forceKeptStop) takes the end without the lock keptEndHoldOffLimit after the run was lost, not
// after its keep grace. A target that keeps, or one not yet known, is left to the grace and the next pass,
// as before.
func (s *Server) forceUnkeptHeld(ctx context.Context, leaser store.RunLeaser, run types.AgentRun) {
	keeps, known := s.endKeepsRunTarget(ctx, run)
	if !known || keeps {
		return
	}
	if run.LostReason == types.LostEnded {
		s.stopKeptRun(ctx, leaser, run, types.RunStopped, "run.ended", map[string]any{
			"kept": false, "end_error": runner.ErrEndUnsupported.Error(), "ended_at": run.LostAt,
		})
		return
	}
	s.stopKeptRun(ctx, leaser, run, types.RunFailed, "run.lost", map[string]any{
		"kept": false, "lost_error": runner.ErrEndUnsupported.Error(), "reason": string(run.LostReason),
	})
}
