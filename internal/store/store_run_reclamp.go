// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Tightened limits reach live runs (long-holds design rev 4, §2.2, RL-8;
// migration 0071): the read and the conditional write the re-clamp sweep
// needs. Kept out of store.go for the same size reason as store_run_lease.go.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunLimitsReclamper is the re-clamp surface. Optional like RunLeaser and for
// the same reason; the api layer runs no re-clamp when a store lacks it.
type RunLimitsReclamper interface {
	// ListProfiledLiveRuns returns every non-terminal, not-kept run that
	// captured a governance profile at create.
	ListProfiledLiveRuns(ctx context.Context) ([]types.AgentRun, error)
	// ReclampRunLimits writes run id's tightened limits, end and wait, but only
	// while the run still has the limits, end and wait in from (the run as the
	// caller read it), is not terminal and is not kept. A non-nil endTightenedAt
	// is stamped as end_tightened_at. false means the run changed since it was
	// read; the next sweep reads it again.
	ReclampRunLimits(ctx context.Context, from types.AgentRun, toLimits types.RunLimits, toEnd *time.Time, toWait int, endTightenedAt *time.Time) (bool, error)
}

var _ RunLimitsReclamper = PG{}

// ListProfiledLiveRuns — see RunLimitsReclamper.
func (s PG) ListProfiledLiveRuns(ctx context.Context) ([]types.AgentRun, error) {
	q := `SELECT ` + runCols + ` FROM agent_runs
		WHERE governance_profile_id IS NOT NULL AND lost_at IS NULL AND state = ANY($1)`
	return collect(ctx, s.Pool, "list", "profiled live runs", q, []any{nonTerminalStateNames()}, scanRun)
}

// ReclampRunLimits — see RunLimitsReclamper. The compare on the limits read is
// what keeps a PATCH decided against the old gate from landing after it, and
// this write from landing over a PATCH's.
func (s PG) ReclampRunLimits(ctx context.Context, from types.AgentRun, toLimits types.RunLimits, toEnd *time.Time, toWait int, endTightenedAt *time.Time) (bool, error) {
	fromJSON, err := json.Marshal(from.RunLimits)
	if err != nil {
		return false, fmt.Errorf("store: marshal run limits: %w", err)
	}
	toJSON, err := json.Marshal(toLimits)
	if err != nil {
		return false, fmt.Errorf("store: marshal run limits: %w", err)
	}
	tag, err := s.Pool.Exec(ctx, `
		UPDATE agent_runs SET run_limits=$5, ends_at=$6, wait_budget_sec=$7,
			end_tightened_at = COALESCE($8::timestamptz, end_tightened_at)
		WHERE id=$1 AND run_limits=$2 AND ends_at IS NOT DISTINCT FROM $3 AND wait_budget_sec=$4
		  AND lost_at IS NULL AND state = ANY($9)`,
		from.ID, fromJSON, from.EndsAt, from.WaitBudgetSec, toJSON, toEnd, toWait, endTightenedAt,
		nonTerminalStateNames())
	if err != nil {
		return false, fmt.Errorf("store: reclamp run limits: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
