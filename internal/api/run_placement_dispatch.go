// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// unsupportedLocalDispatch closes non-HTTP dispatch lanes too, before their
// CAS, renewal, grant authoring or mask-manifest writes. H3/D117 must replace
// this refusal with the complete classified plan, never merely a runner id.
func (s *Server) unsupportedLocalDispatch(ctx context.Context, run types.AgentRun) bool {
	if run.Placement != types.PlacementLocal {
		return false
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch", run.ID.String(), "failure", mustJSON(map[string]any{"reason": placement.ReasonPlacementUnavailable})))
	s.failAndRevoke(context.WithoutCancel(ctx), run.ID, types.RunPending, "Your own runner is not available: this server cannot place a run on a runner yet.")
	return true
}
