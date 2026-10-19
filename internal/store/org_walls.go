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

// OrgWallsStore reads the organisation walls (migration 0192). A side interface, as RunPATReader
// is: a store that does not implement it has no walls, so every run resolves as before.
type OrgWallsStore interface {
	GetOrgWalls(ctx context.Context) (types.OrgWalls, error)
	PutOrgWalls(ctx context.Context, w types.OrgWallsWrite, by string) (types.OrgWalls, error)
}

// GetOrgWalls returns the walls, or the zero (off, empty) document when none were ever written.
func (s PG) GetOrgWalls(ctx context.Context) (types.OrgWalls, error) {
	return GetOrgWallsQ(ctx, s.Pool)
}

// PutOrgWalls replaces the walls.
func (s PG) PutOrgWalls(ctx context.Context, w types.OrgWallsWrite, by string) (types.OrgWalls, error) {
	return PutOrgWallsQ(ctx, s.Pool, w, by)
}

// GetOrgWallsQ is GetOrgWalls on q.
func GetOrgWallsQ(ctx context.Context, q Querier) (types.OrgWalls, error) {
	var w types.OrgWalls
	var spec, limits []byte
	err := q.QueryRow(ctx, `SELECT enabled, spec, limits, updated_by, updated_at FROM org_walls WHERE singleton`).
		Scan(&w.Enabled, &spec, &limits, &w.UpdatedBy, &w.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.OrgWalls{}, nil
	}
	if err != nil {
		return types.OrgWalls{}, fmt.Errorf("store: get org walls: %w", err)
	}
	if err := json.Unmarshal(spec, &w.Spec); err != nil {
		return types.OrgWalls{}, fmt.Errorf("store: unmarshal org walls spec: %w", err)
	}
	if err := json.Unmarshal(limits, &w.Limits); err != nil {
		return types.OrgWalls{}, fmt.Errorf("store: unmarshal org walls limits: %w", err)
	}
	return w, nil
}

// PutOrgWallsQ replaces the walls on q and returns them as stored.
func PutOrgWallsQ(ctx context.Context, q Querier, w types.OrgWallsWrite, by string) (types.OrgWalls, error) {
	spec, err := json.Marshal(w.Spec)
	if err != nil {
		return types.OrgWalls{}, fmt.Errorf("store: marshal org walls spec: %w", err)
	}
	limits, err := json.Marshal(w.Limits)
	if err != nil {
		return types.OrgWalls{}, fmt.Errorf("store: marshal org walls limits: %w", err)
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO org_walls (singleton, enabled, spec, limits, updated_by, updated_at)
		VALUES (true, $1, $2, $3, $4, now())
		ON CONFLICT (singleton) DO UPDATE
			SET enabled = EXCLUDED.enabled, spec = EXCLUDED.spec, limits = EXCLUDED.limits,
			    updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at`,
		w.Enabled, spec, limits, by); err != nil {
		return types.OrgWalls{}, fmt.Errorf("store: put org walls: %w", err)
	}
	return GetOrgWallsQ(ctx, q)
}
