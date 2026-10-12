// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// projectRunnerFacts names the runner of each local run, so the run page and the Runs board can say where
// a run is and whether that computer is connected. The facts are read at request time and never stored:
// a runner's row is never renamed and a claimed row is never deleted, so its current name is the name at
// create, even once the runner is revoked. A runner whose row cannot be read leaves all three facts off
// that run, never fails the response, and is not read again for another run in the same request.
func (s *Server) projectRunnerFacts(r *http.Request, runs []types.AgentRun) {
	rs, ok := s.cfg.Store.(interface {
		GetRunner(ctx context.Context, id uuid.UUID) (types.Runner, error)
	})
	if !ok {
		return
	}
	rows := map[uuid.UUID]*types.Runner{}
	for i := range runs {
		run := &runs[i]
		if run.Placement != types.PlacementLocal || run.RunnerID == nil {
			continue
		}
		id := *run.RunnerID
		row, seen := rows[id]
		if !seen {
			if got, err := rs.GetRunner(r.Context(), id); err == nil {
				row = &got
			}
			rows[id] = row
		}
		if row == nil {
			continue
		}
		run.RunnerName, run.RunnerLastSeenAt = row.Name, row.LastSeenAt
		online := row.State == types.RunnerClaimed && s.runnerOnline(id)
		// Under WARDYN_HA a replica that does not hold the runner's session answers offline for a runner
		// connected to another, so there false is a guess and stays unset (unknown); true is always a fact.
		if s.cfg.HA && !online {
			continue
		}
		run.RunnerOnline = &online
	}
}
