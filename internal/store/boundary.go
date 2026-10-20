// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// BoundaryStore reads the Wardyn boundary (migration 0200). A side interface, as RunPATReader
// is: a store that does not implement it has no boundary, so every run resolves as before.
type BoundaryStore interface {
	GetBoundary(ctx context.Context) (types.Boundary, error)
	PutBoundary(ctx context.Context, b types.BoundaryWrite, by string) (types.Boundary, error)
}

// GetBoundary returns the boundary, or an empty no_limits one when none was ever written.
func (s PG) GetBoundary(ctx context.Context) (types.Boundary, error) {
	return GetBoundaryQ(ctx, s.Pool)
}

// PutBoundary replaces the boundary.
func (s PG) PutBoundary(ctx context.Context, b types.BoundaryWrite, by string) (types.Boundary, error) {
	return PutBoundaryQ(ctx, s.Pool, b, by)
}

// GetBoundaryQ is GetBoundary on q.
func GetBoundaryQ(ctx context.Context, q Querier) (types.Boundary, error) {
	var b types.Boundary
	var spec, limits, contact []byte
	err := q.QueryRow(ctx, `SELECT mode, spec, limits, contact, updated_by, updated_at FROM governance_boundary WHERE singleton`).
		Scan(&b.Mode, &spec, &limits, &contact, &b.UpdatedBy, &b.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.Boundary{Mode: types.BoundaryNoLimits}, nil
	}
	if err != nil {
		return types.Boundary{}, fmt.Errorf("store: get boundary: %w", err)
	}
	if err := json.Unmarshal(spec, &b.Spec); err != nil {
		return types.Boundary{}, fmt.Errorf("store: unmarshal boundary spec: %w", err)
	}
	if err := json.Unmarshal(limits, &b.Limits); err != nil {
		return types.Boundary{}, fmt.Errorf("store: unmarshal boundary limits: %w", err)
	}
	if contact != nil {
		if err := json.Unmarshal(contact, &b.Contact); err != nil {
			return types.Boundary{}, fmt.Errorf("store: unmarshal boundary contact: %w", err)
		}
	}
	return b, nil
}

// PutBoundaryQ replaces the boundary on q and returns it as stored.
func PutBoundaryQ(ctx context.Context, q Querier, b types.BoundaryWrite, by string) (types.Boundary, error) {
	spec, err := json.Marshal(b.Spec)
	if err != nil {
		return types.Boundary{}, fmt.Errorf("store: marshal boundary spec: %w", err)
	}
	limits, err := json.Marshal(b.Limits)
	if err != nil {
		return types.Boundary{}, fmt.Errorf("store: marshal boundary limits: %w", err)
	}
	var contact []byte
	if b.Contact != nil {
		if contact, err = json.Marshal(b.Contact); err != nil {
			return types.Boundary{}, fmt.Errorf("store: marshal boundary contact: %w", err)
		}
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO governance_boundary (singleton, mode, spec, limits, contact, updated_by, updated_at)
		VALUES (true, $1, $2, $3, $4, $5, now())
		ON CONFLICT (singleton) DO UPDATE
			SET mode = EXCLUDED.mode, spec = EXCLUDED.spec, limits = EXCLUDED.limits, contact = EXCLUDED.contact,
			    updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at`,
		string(b.Mode), spec, limits, contact, by); err != nil {
		return types.Boundary{}, fmt.Errorf("store: put boundary: %w", err)
	}
	return GetBoundaryQ(ctx, q)
}
