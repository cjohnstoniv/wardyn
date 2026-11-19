// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RunAllowedModels is the record of the governed allowed-models list a run was dispatched under
// (0255), held as the JSON object the run read serves. Optional like RunModelAllowlists: a test
// double that embeds store.Store would route these calls to a nil interface, so the api layer
// type-asserts.
type RunAllowedModels interface {
	// RunAllowedModels reads run id's record; nil when none is held.
	RunAllowedModels(ctx context.Context, id uuid.UUID) ([]byte, error)
	// SetRunAllowedModels replaces run id's record; nil clears it.
	SetRunAllowedModels(ctx context.Context, id uuid.UUID, record []byte) error
}

var _ RunAllowedModels = PG{}

// RunAllowedModels implements RunAllowedModels.
func (s PG) RunAllowedModels(ctx context.Context, id uuid.UUID) ([]byte, error) {
	var raw []byte
	err := s.Pool.QueryRow(ctx, `SELECT allowed_models FROM agent_runs WHERE id = $1`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: read run allowed models: %w", err)
	}
	return raw, nil
}

// SetRunAllowedModels implements RunAllowedModels.
func (s PG) SetRunAllowedModels(ctx context.Context, id uuid.UUID, record []byte) error {
	if len(record) == 0 {
		record = nil
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET allowed_models = $2 WHERE id = $1`, id, record); err != nil {
		return fmt.Errorf("store: record run allowed models: %w", err)
	}
	return nil
}
