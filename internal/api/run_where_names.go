// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runPoolName is the name of the pool a run was admitted into, "" when it names none or the name cannot
// be read. A deleted pool keeps its row (a tombstone), so a run that outlives its pool still names it.
func (s *Server) runPoolName(ctx context.Context, run types.AgentRun) string {
	ps, ok := s.cfg.Store.(store.RunnerPoolStore)
	if !ok || run.RunnerPoolID == nil {
		return ""
	}
	pool, err := ps.GetRunnerPool(ctx, *run.RunnerPoolID)
	if err != nil {
		return ""
	}
	return pool.Name
}

// runnerNameOf is the display name of the runner a run is bound to, a self-hosted runner's or a
// remote-hosted runner's, whichever its placement says; "" when it carries none or the name cannot be read.
func (s *Server) runnerNameOf(ctx context.Context, run types.AgentRun) string {
	switch {
	case run.Placement == types.PlacementLocal && run.RunnerID != nil:
		if rs, ok := s.cfg.Store.(store.RunnerStore); ok {
			if row, err := rs.GetRunner(ctx, *run.RunnerID); err == nil {
				return row.Name
			}
		}
	case run.Placement == types.PlacementRemote && run.RunnerPoolExecutor != "":
		if rs, ok := s.cfg.Store.(store.RemoteRunnerStore); ok {
			if row, err := rs.GetRemoteRunner(ctx, run.RunnerPoolExecutor); err == nil {
				return row.Runner.Name
			}
		}
	}
	return ""
}
