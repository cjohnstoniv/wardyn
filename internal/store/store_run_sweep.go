// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The narrow run reads the boot reconcile and the periodic sweeps make. Each used to take the whole
// of agent_runs through ListRuns and filter in Go, so its cost grew with run history forever.
package store

import (
	"context"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunSweepReader is deliberately not part of Store (like Pager and RunWatcherLeaser): ~30 test doubles embed
// store.Store and override ListRuns, so widening Store would route these reads to the embedded nil
// interface. The api layer type-asserts and, for a store without it, filters ListRuns exactly as before.
// Every method keeps ListRuns' order (created_at DESC) and the predicate the Go filter it replaces used.
type RunSweepReader interface {
	// ListUndispatchedRuns returns the slot-holding runs that never got a sandbox ref.
	ListUndispatchedRuns(ctx context.Context) ([]types.AgentRun, error)
	// ListTerminalRunsWithSandbox returns the terminal runs that still carry a sandbox ref.
	ListTerminalRunsWithSandbox(ctx context.Context) ([]types.AgentRun, error)
	// ListLiveRuns returns every non-terminal run.
	ListLiveRuns(ctx context.Context) ([]types.AgentRun, error)
	// GetRunsByID returns the runs among ids; an id with no row is simply absent.
	GetRunsByID(ctx context.Context, ids []uuid.UUID) ([]types.AgentRun, error)
}

var _ RunSweepReader = PG{}

func runStateStrings(states []types.RunState) []string {
	out := make([]string, len(states))
	for i, st := range states {
		out[i] = string(st)
	}
	return out
}

// ListUndispatchedRuns is the finalizeUndispatchedRuns candidate set; its age and watcher-lease gates stay
// in the api layer.
//
// ponytail: no new index; agent_runs_state_idx already narrows to the few live rows.
func (s PG) ListUndispatchedRuns(ctx context.Context) ([]types.AgentRun, error) {
	return collect(ctx, s.Pool, "list", "undispatched runs", `
		SELECT `+runCols+`
		FROM agent_runs WHERE state = ANY($1) AND sandbox_ref = '' ORDER BY created_at DESC`,
		[]any{runStateStrings(types.SlotHoldingRunStates)}, scanRun)
}

// ListTerminalRunsWithSandbox is the reconcileOrphanedSandbox candidate set. Terminal is the complement of
// the positive non-terminal list, the same predicate as RunState.IsTerminal (pinned by a types test).
//
// ponytail: one sequential scan that returns the few rows a failed teardown left; a partial index on a non-empty
// sandbox_ref is the upgrade if a deployment's history makes the scan itself slow.
func (s PG) ListTerminalRunsWithSandbox(ctx context.Context) ([]types.AgentRun, error) {
	return collect(ctx, s.Pool, "list", "terminal runs with a sandbox", `
		SELECT `+runCols+`
		FROM agent_runs WHERE sandbox_ref <> '' AND NOT (state = ANY($1)) ORDER BY created_at DESC`,
		[]any{runStateStrings(types.NonTerminalRunStates)}, scanRun)
}

// ListLiveRuns returns the non-terminal runs, for the boot pass that reloads their masking manifests.
func (s PG) ListLiveRuns(ctx context.Context) ([]types.AgentRun, error) {
	return collect(ctx, s.Pool, "list", "live runs", `
		SELECT `+runCols+`
		FROM agent_runs WHERE state = ANY($1) ORDER BY created_at DESC`,
		[]any{runStateStrings(types.NonTerminalRunStates)}, scanRun)
}

// GetRunsByID returns the runs among ids, newest first.
func (s PG) GetRunsByID(ctx context.Context, ids []uuid.UUID) ([]types.AgentRun, error) {
	return collect(ctx, s.Pool, "list", "runs by id", `
		SELECT `+runCols+`
		FROM agent_runs WHERE id = ANY($1) ORDER BY created_at DESC`, []any{ids}, scanRun)
}
