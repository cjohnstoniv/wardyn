// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunTrustStore keeps agent_runs.runner_trust_class: the trust class a local run's credential plan was
// admitted at (migration 0181).
type RunTrustStore interface {
	SetRunRunnerTrustClass(ctx context.Context, runID uuid.UUID, class types.RunnerTrustClass) error
	// RunRunnerTrustClass is "" for a run with none recorded; ErrNotFound for no such run.
	RunRunnerTrustClass(ctx context.Context, runID uuid.UUID) (types.RunnerTrustClass, error)
}

// SetRunRunnerTrustClass records the class a run was admitted at. ErrNotFound: no such run.
func (s PG) SetRunRunnerTrustClass(ctx context.Context, runID uuid.UUID, class types.RunnerTrustClass) error {
	if !class.Valid() {
		return fmt.Errorf("store: run trust class %q is not a trust class", class)
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET runner_trust_class = $2 WHERE id = $1`, runID, string(class))
	if err != nil {
		return fmt.Errorf("store: set run trust class: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RunRunnerTrustClass reads the class a run was admitted at.
func (s PG) RunRunnerTrustClass(ctx context.Context, runID uuid.UUID) (types.RunnerTrustClass, error) {
	var class *string
	err := s.Pool.QueryRow(ctx, `SELECT runner_trust_class FROM agent_runs WHERE id = $1`, runID).Scan(&class)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: read run trust class: %w", err)
	}
	if class == nil {
		return "", nil
	}
	return types.RunnerTrustClass(*class), nil
}
