// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// claimDispatch claims PENDING->STARTING conditionally; see the KILL-RACE GUARD
// note at its call site in dispatchRun.
func (s *Server) claimDispatch(ctx context.Context, run types.AgentRun) bool {
	claimed, cerr := s.casRunState(ctx, run.ID, types.RunPending, types.RunStarting)
	if cerr == nil && claimed {
		return true
	}
	data := map[string]any{"note": "run left PENDING by a concurrent kill/stop before dispatch; dispatch aborted"}
	if cerr != nil {
		data["error"] = cerr.Error()
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch",
		run.ID.String(), "failure", mustJSON(data)))
	return false
}
