// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RunCorrelationID reads a run's correlation_id for the audit sinks, which stamp it on the run's rows on
// their way out. found is false for a run the table does not hold (not yet inserted, or deleted); the
// id is "" for a run whose caller gave none.
func RunCorrelationID(ctx context.Context, pool *pgxpool.Pool, runID uuid.UUID) (id string, found bool, err error) {
	err = pool.QueryRow(ctx, `SELECT correlation_id FROM agent_runs WHERE id = $1`, runID).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("store: read run correlation id: %w", err)
	}
	return id, true, nil
}
