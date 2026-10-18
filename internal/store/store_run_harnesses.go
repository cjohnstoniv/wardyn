// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// PutRunHarnesses — see RunHarnessStore.
func (s PG) PutRunHarnesses(ctx context.Context, runID uuid.UUID, hs []types.RunHarness) error {
	if len(hs) == 0 {
		return errors.New("store: put run harnesses: no harnesses; a run without any writes no rows")
	}
	err := s.inTx(ctx, func(q Querier) error { return insertRunHarnesses(ctx, q, runID, hs) })
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		return ErrNotFound
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return ErrConflict
	case err != nil:
		return fmt.Errorf("store: put run harnesses: %w", err)
	}
	return nil
}

// insertRunHarnesses writes hs as runID's harness rows on q.
func insertRunHarnesses(ctx context.Context, q Querier, runID uuid.UUID, hs []types.RunHarness) error {
	const ins = `
		INSERT INTO run_harnesses (run_id, ordinal, harness, model_provider_id, starts)
		VALUES ($1, $2, $3, $4, $5)`
	for i, h := range hs {
		if _, err := q.Exec(ctx, ins, runID, i, h.Harness, h.ModelProviderID, h.Starts); err != nil {
			return err
		}
	}
	return nil
}

// ListRunHarnesses — see RunHarnessStore.
func (s PG) ListRunHarnesses(ctx context.Context, runID uuid.UUID) ([]types.RunHarness, error) {
	const q = `SELECT harness, model_provider_id, starts
		FROM run_harnesses WHERE run_id = $1 ORDER BY ordinal`
	return collect(ctx, s.Pool, "list", "run harnesses", q, []any{runID}, scanRunHarness)
}

func scanRunHarness(row pgx.Row) (types.RunHarness, error) {
	var h types.RunHarness
	if err := row.Scan(&h.Harness, &h.ModelProviderID, &h.Starts); err != nil {
		return types.RunHarness{}, fmt.Errorf("store: scan run harness: %w", err)
	}
	return h, nil
}
