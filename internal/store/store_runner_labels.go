// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunnerLabelStore keeps runners' label sets (the runner_labels table).
type RunnerLabelStore interface {
	// SetRunnerLabels replaces runnerID's set of source and returns the set it replaced (nil for none). An
	// empty set deletes the row.
	SetRunnerLabels(ctx context.Context, runnerID uuid.UUID, source types.RunnerLabelSource, labels map[string]string, by string, at time.Time) (before map[string]string, err error)
	// RunnerLabels is ids' sets, keyed by runner then source; a runner with none is absent.
	RunnerLabels(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]map[types.RunnerLabelSource]RunnerLabelSet, error)
}

// RunnerLabelSet is one source's labels on a runner, with who wrote them and when.
type RunnerLabelSet struct {
	Labels    map[string]string
	CreatedBy string
	CreatedAt time.Time
}

var _ RunnerLabelStore = PG{}

// SetRunnerLabels replaces runnerID's set of source in one transaction; ErrNotFound for an unknown runner.
func (s PG) SetRunnerLabels(ctx context.Context, runnerID uuid.UUID, source types.RunnerLabelSource, labels map[string]string, by string, at time.Time) (map[string]string, error) {
	var before map[string]string
	err := s.inTx(ctx, func(q Querier) error {
		var raw []byte
		err := q.QueryRow(ctx, `SELECT labels FROM runner_labels WHERE runner_id = $1 AND source = $2 FOR UPDATE`, runnerID, string(source)).Scan(&raw)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			if err := json.Unmarshal(raw, &before); err != nil {
				return err
			}
		}
		if len(labels) == 0 {
			_, err = q.Exec(ctx, `DELETE FROM runner_labels WHERE runner_id = $1 AND source = $2`, runnerID, string(source))
			return err
		}
		enc, err := json.Marshal(labels)
		if err != nil {
			return err
		}
		_, err = q.Exec(ctx, `INSERT INTO runner_labels (runner_id, source, labels, created_by, created_at) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (runner_id, source) DO UPDATE SET labels = EXCLUDED.labels, created_by = EXCLUDED.created_by, created_at = EXCLUDED.created_at
				WHERE runner_labels.labels IS DISTINCT FROM EXCLUDED.labels`,
			runnerID, string(source), enc, by, at)
		return err
	})
	if isForeignKeyViolation(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: set runner labels: %w", err)
	}
	return before, nil
}

// RunnerLabels is ids' sets, keyed by runner then source; a runner with none is absent.
func (s PG) RunnerLabels(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]map[types.RunnerLabelSource]RunnerLabelSet, error) {
	out := map[uuid.UUID]map[types.RunnerLabelSource]RunnerLabelSet{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT runner_id, source, labels, created_by, created_at FROM runner_labels WHERE runner_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: read runner labels: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id     uuid.UUID
			source string
			raw    []byte
			set    RunnerLabelSet
		)
		if err := rows.Scan(&id, &source, &raw, &set.CreatedBy, &set.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: read runner labels: %w", err)
		}
		if err := json.Unmarshal(raw, &set.Labels); err != nil {
			return nil, fmt.Errorf("store: read runner labels: %w", err)
		}
		if out[id] == nil {
			out[id] = map[types.RunnerLabelSource]RunnerLabelSet{}
		}
		out[id][types.RunnerLabelSource(source)] = set
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read runner labels: %w", err)
	}
	return out, nil
}
