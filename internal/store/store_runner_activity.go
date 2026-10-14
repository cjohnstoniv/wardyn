// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TouchRunnerActivity is only for H2's current-connection, nonreplayed signal.
// The committing transaction rechecks runner ownership and lapse; timestamps
// come from the database. Neither audit ingestion nor replay calls this method.
func (s PG) TouchRunnerActivity(ctx context.Context, runnerID, runID uuid.UUID) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed tx is already closed
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='2s'`); err != nil {
		return err
	}
	run, err := runnerExecutionRun(ctx, tx, runnerID, runID)
	if err != nil {
		return err
	}
	if run.State != types.RunRunning || run.LostAt != nil {
		return ErrRunnerExecutionRefused
	}
	tag, err := tx.Exec(ctx, `UPDATE agent_runs SET updated_at=clock_timestamp(),active_at=clock_timestamp() WHERE id=$1 AND state='RUNNING' AND lost_at IS NULL AND token_renewed_at > clock_timestamp() - $2::interval AND (ends_at IS NULL OR ends_at > clock_timestamp())`, runID, identity.RunTokenLapseAfter.String())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrRunnerExecutionRefused
	}
	return tx.Commit(ctx)
}
