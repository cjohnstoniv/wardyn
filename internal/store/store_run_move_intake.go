// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// moveIgnoredRowTx reports whether a runner's STATE row for runID is one a move left behind, which the intake
// skips: no observation, no containment and no refusal. That is so when the run is not bound to the sending
// runner, and a move of the run names that runner, as its committed source or as the destination of any move but
// a refused one (which never had a destination sandbox). The organisation holds the binding in run_moves, so a
// source that never heard of its run's move, or a destination before commit, never fails its page over the run.
// A row from a runner no move names is judged as before, and refused when the run is not bound to it.
//
// The read is a plain SELECT, never a row lock, so the intake does not hold the run row before
// runnerExecutionRun takes the runner row and then the run row. A page racing a commit reads the run as still
// bound, waits on the commit's run row lock in runnerExecutionRun and fails once; the runner's next page is
// skipped here.
func moveIgnoredRowTx(ctx context.Context, tx pgx.Tx, runnerID, runID uuid.UUID, ignored *[]uuid.UUID) (bool, error) {
	var skip bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_runs a WHERE a.id = $2 AND a.runner_id IS DISTINCT FROM $1
		AND EXISTS (SELECT 1 FROM run_moves m WHERE m.run_id = a.id
			AND ((m.from_runner_id = $1 AND m.state = 'committed') OR (m.to_runner_id = $1 AND m.state <> 'refused'))))`,
		runnerID, runID).Scan(&skip); err != nil {
		return false, fmt.Errorf("store: moved run row: %w", err)
	}
	if skip {
		*ignored = append(*ignored, runID)
	}
	return skip, nil
}
