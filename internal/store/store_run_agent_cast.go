// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

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
	// ListRunsAgentCastOwed returns the terminal runs that still owe their cast, last changed more
	// than age ago, oldest obligation first.
	ListRunsAgentCastOwed(ctx context.Context, age time.Duration, limit int) ([]uuid.UUID, error)
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
func (s PG) ListRunsAgentCastOwed(ctx context.Context, age time.Duration, limit int) ([]uuid.UUID, error) {
	nonTerminal := make([]string, len(types.NonTerminalRunStates))
	for i, st := range types.NonTerminalRunStates {
		nonTerminal[i] = string(st)
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id FROM agent_runs
		WHERE agent_cast_owed_at IS NOT NULL AND state <> ALL($1) AND updated_at < now() - make_interval(secs => $2)
		ORDER BY agent_cast_owed_at LIMIT $3`, nonTerminal, age.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list runs owing an agent cast: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan a run owing an agent cast: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
