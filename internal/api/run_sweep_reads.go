// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The reads below are what the boot reconcile and the periodic sweeps need of agent_runs. A store with
// store.RunSweepReader (PG) answers each with a narrow query; any other store gets ListRuns filtered in Go,
// which is what every one of these callers did before, so the answer is the same either way.

// undispatchedRuns is the slot-holding runs that never got a sandbox ref.
func (s *Server) undispatchedRuns(ctx context.Context) ([]types.AgentRun, error) {
	if r, ok := s.cfg.Store.(store.RunSweepReader); ok {
		return r.ListUndispatchedRuns(ctx)
	}
	return s.listRunsWhere(ctx, func(run types.AgentRun) bool {
		return slices.Contains(types.SlotHoldingRunStates, run.State) && run.SandboxRef == ""
	})
}

// terminalRunsWithSandbox is the terminal runs that still carry a sandbox ref.
func (s *Server) terminalRunsWithSandbox(ctx context.Context) ([]types.AgentRun, error) {
	if r, ok := s.cfg.Store.(store.RunSweepReader); ok {
		return r.ListTerminalRunsWithSandbox(ctx)
	}
	return s.listRunsWhere(ctx, func(run types.AgentRun) bool {
		return isTerminalRunState(run.State) && run.SandboxRef != ""
	})
}

// liveRuns is every non-terminal run.
func (s *Server) liveRuns(ctx context.Context) ([]types.AgentRun, error) {
	if r, ok := s.cfg.Store.(store.RunSweepReader); ok {
		return r.ListLiveRuns(ctx)
	}
	return s.listRunsWhere(ctx, func(run types.AgentRun) bool { return !isTerminalRunState(run.State) })
}

// runsByID is the runs among ids by id. A store without the narrow read returns every run, which a caller
// that only looks ids up in the result cannot tell apart.
func (s *Server) runsByID(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]types.AgentRun, error) {
	var runs []types.AgentRun
	var err error
	if r, ok := s.cfg.Store.(store.RunSweepReader); ok {
		runs, err = r.GetRunsByID(ctx, ids)
	} else {
		runs, err = s.cfg.Store.ListRuns(ctx)
	}
	if err != nil {
		return nil, err
	}
	byID := make(map[uuid.UUID]types.AgentRun, len(runs))
	for _, run := range runs {
		byID[run.ID] = run
	}
	return byID, nil
}

func (s *Server) listRunsWhere(ctx context.Context, keep func(types.AgentRun) bool) ([]types.AgentRun, error) {
	runs, err := s.cfg.Store.ListRuns(ctx)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(runs, func(run types.AgentRun) bool { return !keep(run) }), nil
}
