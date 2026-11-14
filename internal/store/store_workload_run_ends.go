// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// WorkloadRunEndKey is an owed end's place in the order the owed-end sweep reads them: when it was owed,
// then its run.
type WorkloadRunEndKey struct {
	At    time.Time
	RunID uuid.UUID
}

// workloadEndWaitingStates are the states of a queued replacement whose owed end still waits on it: it has
// not run and has not ended. The sweep acts on an end in any other state (it takes over once the
// replacement runs, and restores or takes over once it ended), so none of those states may be listed here.
var workloadEndWaitingStates = []types.RunState{types.RunQueued, types.RunPending, types.RunStarting}

// ListDueWorkloadRunEnds is ListWorkloadRunEnds for the owed-end sweep: up to limit owed ends recorded more
// than olderThan ago that the sweep can act on now, in order, after the key after (nil reads from the
// oldest). An end that waits on a replacement still in a waiting state is left out before the limit, so ends
// that wait, however many, never hold back one that is due. next is the key of the last end returned when
// limit were returned, for the next pass to read after, so ends that keep failing at the front never hold
// back the ones behind them; nil once fewer were returned, so the next pass reads from the oldest again.
func (s PG) ListDueWorkloadRunEnds(ctx context.Context, olderThan time.Duration, after *WorkloadRunEndKey, limit int) ([]WorkloadRunEnd, *WorkloadRunEndKey, error) {
	q := `
		SELECT e.run_id, e.owner, e.name, e.generation, e.reason, e.after_run_id, COALESCE(a.state, ''), COALESCE(a.status_detail, ''),
			COALESCE(` + workloadRunRanSQL("a", "$3") + `, false), e.created_at
		FROM workload_run_ends e LEFT JOIN agent_runs a ON a.id = e.after_run_id
		WHERE e.created_at < now() - make_interval(secs => $1::float8) AND COALESCE(a.state, '') <> ALL($4)`
	args := []any{olderThan.Seconds(), limit, slotHoldingStateNames(), stateNames(workloadEndWaitingStates)}
	if after != nil {
		q += ` AND (e.created_at, e.run_id) > ($5::timestamptz, $6::uuid)`
		args = append(args, after.At, after.RunID)
	}
	type keyed struct {
		end WorkloadRunEnd
		key WorkloadRunEndKey
	}
	rows, err := collect(ctx, s.Pool, "list", "due workload run ends", q+` ORDER BY e.created_at, e.run_id LIMIT $2`, args, func(row pgx.Row) (keyed, error) {
		var k keyed
		e := &k.end
		err := row.Scan(&e.RunID, &e.Owner, &e.Name, &e.Generation, &e.Reason, &e.AfterRunID, &e.AfterState, &e.AfterDetail, &e.AfterRan, &k.key.At)
		k.key.RunID = e.RunID
		return k, err
	})
	if err != nil {
		return nil, nil, err
	}
	ends := make([]WorkloadRunEnd, 0, len(rows))
	for _, k := range rows {
		ends = append(ends, k.end)
	}
	if len(rows) == 0 || len(rows) < limit {
		return ends, nil, nil
	}
	return ends, &rows[len(rows)-1].key, nil
}
