// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// insertRunContainers writes the extras of the run just inserted on q, the same transaction; none writes nothing.
func insertRunContainers(ctx context.Context, q queryRower, runID uuid.UUID, cs []types.RunExtraContainer) error {
	if len(cs) == 0 {
		return nil
	}
	doc, err := json.Marshal(cs)
	if err != nil {
		return fmt.Errorf("store: marshal run containers: %w", err)
	}
	var id uuid.UUID
	if err := q.QueryRow(ctx, `INSERT INTO run_containers (run_id, containers) VALUES ($1, $2) RETURNING run_id`,
		runID, doc).Scan(&id); err != nil {
		return fmt.Errorf("store: insert run containers: %w", err)
	}
	return nil
}

// RunContainers reads a run's extras; none is nil, nil. A role refused the read that also lacks INSERT on the table
// (a split-role app role the migrator's grant loop missed) can have written no row, so it reads none rather than
// failing every start (runContainersUnwritable); any other failure is returned, so a run's extras are never
// silently dropped.
func (s PG) RunContainers(ctx context.Context, runID uuid.UUID) ([]types.RunExtraContainer, error) {
	var doc []byte
	err := s.Pool.QueryRow(ctx, `SELECT containers FROM run_containers WHERE run_id = $1`, runID).Scan(&doc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		if none, perr := s.runContainersUnwritable(ctx, err); none || perr != nil {
			return nil, perr
		}
		return nil, fmt.Errorf("store: read run containers: %w", err)
	}
	var out []types.RunExtraContainer
	if err := json.Unmarshal(doc, &out); err != nil {
		return nil, fmt.Errorf("store: decode run containers: %w", err)
	}
	return out, nil
}

// runContainersUnwritable reports whether err, a statement's failure on run_containers, is the privilege refusal of
// a role that cannot INSERT into the table either, so it can have written no row and the statement is skipped. Only
// SQLSTATE 42501 (insufficient_privilege) asks; a role with INSERT, or any other failure, answers false. A failed
// privilege read is returned.
func (s PG) runContainersUnwritable(ctx context.Context, err error) (bool, error) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		return false, nil
	}
	var ok bool
	if err := s.Pool.QueryRow(ctx, `SELECT has_table_privilege(current_user, 'run_containers', 'INSERT')`).Scan(&ok); err != nil {
		return false, fmt.Errorf("store: read the run containers privilege: %w", err)
	}
	return !ok, nil
}
