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
)

// RunModelAllowlists is the record of the model allowlists a run's proxy was handed (0202),
// optional like RunLocalFolders: a test double that embeds store.Store would route these calls
// to a nil interface, so the api layer type-asserts.
type RunModelAllowlists interface {
	// RunModelAllowlists reads run id's record, {provider id: models}; nil when none is held.
	RunModelAllowlists(ctx context.Context, id uuid.UUID) (map[string][]string, error)
	// SetRunModelAllowlists replaces run id's record; an empty map clears it.
	SetRunModelAllowlists(ctx context.Context, id uuid.UUID, lists map[string][]string) error
}

var _ RunModelAllowlists = PG{}

// RunModelAllowlists implements RunModelAllowlists.
func (s PG) RunModelAllowlists(ctx context.Context, id uuid.UUID) (map[string][]string, error) {
	var raw []byte
	err := s.Pool.QueryRow(ctx, `SELECT model_allowlists FROM agent_runs WHERE id = $1`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: read run model allowlists: %w", err)
	}
	if raw == nil {
		return nil, nil
	}
	var lists map[string][]string
	if err := json.Unmarshal(raw, &lists); err != nil {
		return nil, fmt.Errorf("store: unmarshal run model allowlists: %w", err)
	}
	return lists, nil
}

// SetRunModelAllowlists implements RunModelAllowlists.
func (s PG) SetRunModelAllowlists(ctx context.Context, id uuid.UUID, lists map[string][]string) error {
	var raw []byte
	if len(lists) > 0 {
		var err error
		if raw, err = json.Marshal(lists); err != nil {
			return fmt.Errorf("store: marshal run model allowlists: %w", err)
		}
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET model_allowlists = $2 WHERE id = $1`, id, raw); err != nil {
		return fmt.Errorf("store: record run model allowlists: %w", err)
	}
	return nil
}
