// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Launch presets (migration 0087_launch_presets). Round-trips rows; the
// request body is validated at the API boundary (internal/api/presets.go).
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const launchPresetCols = `name, version, description, user_types, request, created_at, updated_at, created_by, updated_by`

// PresetWrite is what PutLaunchPreset did: created the row, replaced it (and
// moved its version), or found it already identical and wrote nothing.
type PresetWrite string

const (
	PresetCreated   PresetWrite = "created"
	PresetUpdated   PresetWrite = "updated"
	PresetUnchanged PresetWrite = "unchanged"
)

// ListLaunchPresets returns every preset by name.
func (s PG) ListLaunchPresets(ctx context.Context) ([]types.LaunchPreset, error) {
	const q = `SELECT ` + launchPresetCols + ` FROM launch_presets ORDER BY name`
	return collect(ctx, s.Pool, "list", "launch presets", q, nil, scanLaunchPreset)
}

// GetLaunchPreset returns one preset by name, or ErrNotFound.
func (s PG) GetLaunchPreset(ctx context.Context, name string) (types.LaunchPreset, error) {
	const q = `SELECT ` + launchPresetCols + ` FROM launch_presets WHERE name = $1`
	return scanLaunchPreset(s.Pool.QueryRow(ctx, q, name))
}

// PutLaunchPreset creates p at version 1, or replaces the named row and moves
// its version by one. A write identical to the stored row changes nothing, not
// even the version, so re-applying an unchanged document is a no-op. The
// comparison and the bump are one statement, so two concurrent writers cannot
// both land on the same version.
func (s PG) PutLaunchPreset(ctx context.Context, p types.LaunchPreset) (types.LaunchPreset, PresetWrite, error) {
	const q = `
		INSERT INTO launch_presets (name, description, user_types, request, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $5)
		ON CONFLICT (name) DO UPDATE SET
			version = launch_presets.version + 1,
			description = EXCLUDED.description,
			user_types = EXCLUDED.user_types,
			request = EXCLUDED.request,
			updated_by = EXCLUDED.updated_by,
			updated_at = now()
		WHERE (launch_presets.description, launch_presets.user_types, launch_presets.request)
			IS DISTINCT FROM (EXCLUDED.description, EXCLUDED.user_types, EXCLUDED.request)
		RETURNING ` + launchPresetCols + `, (xmax = 0)`
	userTypes := p.UserTypes
	if userTypes == nil {
		userTypes = []string{}
	}
	var out types.LaunchPreset
	var created bool
	err := s.Pool.QueryRow(ctx, q, p.Name, p.Description, userTypes, []byte(p.Request), p.CreatedBy).Scan(
		&out.Name, &out.Version, &out.Description, &out.UserTypes, &out.Request,
		&out.CreatedAt, &out.UpdatedAt, &out.CreatedBy, &out.UpdatedBy, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		out, err = s.GetLaunchPreset(ctx, p.Name)
		return out, PresetUnchanged, err
	}
	if err != nil {
		return types.LaunchPreset{}, "", fmt.Errorf("store: put launch preset: %w", err)
	}
	if created {
		return out, PresetCreated, nil
	}
	return out, PresetUpdated, nil
}

// DeleteLaunchPreset removes a preset by name, returning the removed row, or
// ErrNotFound. Runs launched from it keep their preset/preset_version stamp.
func (s PG) DeleteLaunchPreset(ctx context.Context, name string) (types.LaunchPreset, error) {
	return scanLaunchPreset(s.Pool.QueryRow(ctx, `DELETE FROM launch_presets WHERE name = $1 RETURNING `+launchPresetCols, name))
}

func scanLaunchPreset(row pgx.Row) (types.LaunchPreset, error) {
	var p types.LaunchPreset
	err := row.Scan(&p.Name, &p.Version, &p.Description, &p.UserTypes, &p.Request,
		&p.CreatedAt, &p.UpdatedAt, &p.CreatedBy, &p.UpdatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.LaunchPreset{}, ErrNotFound
	}
	if err != nil {
		return types.LaunchPreset{}, fmt.Errorf("store: scan launch preset: %w", err)
	}
	return p, nil
}
