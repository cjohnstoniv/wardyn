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

// RunContainers reads a run's extras; none is nil, nil.
func (s PG) RunContainers(ctx context.Context, runID uuid.UUID) ([]types.RunExtraContainer, error) {
	var doc []byte
	err := s.Pool.QueryRow(ctx, `SELECT containers FROM run_containers WHERE run_id = $1`, runID).Scan(&doc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: read run containers: %w", err)
	}
	var out []types.RunExtraContainer
	if err := json.Unmarshal(doc, &out); err != nil {
		return nil, fmt.Errorf("store: decode run containers: %w", err)
	}
	return out, nil
}
