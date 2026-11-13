// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// SweepCursor is where an oldest-first sweep resumes: the last row of its previous page, in (stamp, id)
// order. The sweep carries it from pass to pass, so rows that fail on every pass cannot hold its first page.
type SweepCursor struct {
	At time.Time
	ID uuid.UUID
}

// scanSweepPage reads a page of (id, stamp) rows. The cursor is the last row's, and nil when the page was
// short (or limit is not positive): the caller then starts again from the oldest. It closes rows.
func scanSweepPage(rows pgx.Rows, limit int, what string) ([]uuid.UUID, *SweepCursor, error) {
	defer rows.Close()
	var ids []uuid.UUID
	var last SweepCursor
	for rows.Next() {
		var id uuid.UUID
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, nil, fmt.Errorf("store: scan %s: %w", what, err)
		}
		ids = append(ids, id)
		last = SweepCursor{At: at, ID: id}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: list %s: %w", what, err)
	}
	if limit <= 0 || len(ids) < limit {
		return ids, nil, nil
	}
	return ids, &last, nil
}

// RunAgentCastStore keeps a macOS VM task run's agent output recording obligation
// (agent_runs.agent_cast_owed_at): set when the run's agent output cast opens at dispatch, cleared
// when a cast of the run is stored or the run is marked runner_declined for want of one.
type RunAgentCastStore interface {
	// SetRunAgentCastOwed records that the run owes its agent output cast.
	SetRunAgentCastOwed(ctx context.Context, id uuid.UUID) error
	// DeclineRunAgentCast marks a run that still owes its cast not_recorded runner_declined, and
	// reports whether the run owes it (a run already marked, by a decline whose row or clear failed,
	// does); the obligation stays until ClearRunAgentCastOwed.
	DeclineRunAgentCast(ctx context.Context, id uuid.UUID) (bool, error)
	// ClearRunAgentCastOwed clears the obligation; recorded (a cast was stored) also clears
	// runner_declined.
	ClearRunAgentCastOwed(ctx context.Context, id uuid.UUID, recorded bool) error
	// ListRunsAgentCastOwed returns up to limit terminal runs that still owe their cast, last changed more
	// than age ago, oldest obligation first (the id breaks a tie), after the cursor when one is given. The
	// returned cursor is the last row's, and nil when the page was short: the caller then starts again from
	// the oldest.
	ListRunsAgentCastOwed(ctx context.Context, age time.Duration, after *SweepCursor, limit int) ([]uuid.UUID, *SweepCursor, error)
}

// SetRunAgentCastOwed — see RunAgentCastStore.
func (s PG) SetRunAgentCastOwed(ctx context.Context, id uuid.UUID) error {
	return s.execRun(ctx, "set run agent cast owed", `UPDATE agent_runs SET agent_cast_owed_at = now() WHERE id = $1`, id)
}

// DeclineRunAgentCast — see RunAgentCastStore.
func (s PG) DeclineRunAgentCast(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET not_recorded = $2 WHERE id = $1 AND agent_cast_owed_at IS NOT NULL`,
		id, string(types.NotRecordedRunnerDeclined))
	if err != nil {
		return false, fmt.Errorf("store: decline run agent cast: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ClearRunAgentCastOwed — see RunAgentCastStore.
func (s PG) ClearRunAgentCastOwed(ctx context.Context, id uuid.UUID, recorded bool) error {
	_, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET agent_cast_owed_at = NULL,
		not_recorded = CASE WHEN $2 AND not_recorded = $3 THEN '' ELSE not_recorded END WHERE id = $1`,
		id, recorded, string(types.NotRecordedRunnerDeclined))
	if err != nil {
		return fmt.Errorf("store: clear run agent cast owed: %w", err)
	}
	return nil
}

// ListRunsAgentCastOwed — see RunAgentCastStore.
func (s PG) ListRunsAgentCastOwed(ctx context.Context, age time.Duration, after *SweepCursor, limit int) ([]uuid.UUID, *SweepCursor, error) {
	nonTerminal := make([]string, len(types.NonTerminalRunStates))
	for i, st := range types.NonTerminalRunStates {
		nonTerminal[i] = string(st)
	}
	const head = `SELECT id, agent_cast_owed_at FROM agent_runs
		WHERE agent_cast_owed_at IS NOT NULL AND state <> ALL($1) AND updated_at < now() - make_interval(secs => $2)`
	const tail = ` ORDER BY agent_cast_owed_at, id LIMIT `
	var rows pgx.Rows
	var err error
	if after != nil {
		rows, err = s.Pool.Query(ctx, head+` AND (agent_cast_owed_at, id) > ($3, $4)`+tail+`$5`,
			nonTerminal, age.Seconds(), after.At, after.ID, limit)
	} else {
		rows, err = s.Pool.Query(ctx, head+tail+`$3`, nonTerminal, age.Seconds(), limit)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("store: list runs owing an agent cast: %w", err)
	}
	return scanSweepPage(rows, limit, "runs owing an agent cast")
}
