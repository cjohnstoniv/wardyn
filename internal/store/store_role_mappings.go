// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Console-managed role mappings (migration 0051): the store half of
// internal/auth/oidc's RoleMappingSource. Kept out of store.go on purpose,
// mirroring store_capabilities.go's split.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const roleMappingCols = `id, value, role, created_at, created_by`

// UpsertRoleMapping writes one row, keyed on the natural UNIQUE (value):
// re-adding an already-mapped value FLIPS its role in place rather than
// leaving a second row behind — the same "re-granting the same key updates
// it" contract UpsertCapabilityGrant follows, for the same reason (two rows
// for one value cannot happen; the UNIQUE index already forbids it, this just
// makes the natural retry path a clean update instead of a 409).
//
// The returned row carries the row's real id, which on a conflict is the
// EXISTING one, not m.ID — the caller needs the id the DELETE route will be
// given, and an admin re-submitting the same value must not be handed an id
// that names no row.
//
// A-9: the conflict path updates role only, deliberately NOT created_by —
// creation provenance (who ADDED this mapping) stays with the original
// creator across a later role flip by a different admin, the same way
// created_at is untouched on conflict (no SET at all, so Postgres leaves it).
func (s PG) UpsertRoleMapping(ctx context.Context, m types.RoleMapping) (types.RoleMapping, error) {
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	const q = `
		INSERT INTO role_mappings (id, value, role, created_by)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (value) DO UPDATE
			SET role = EXCLUDED.role
		RETURNING ` + roleMappingCols
	return scanRoleMapping(s.Pool.QueryRow(ctx, q, m.ID, m.Value, m.Role, m.CreatedBy))
}

// DeleteRoleMapping removes one row by id. Returns ErrNotFound when no row
// matched — admin-only surface, so there is no principal to scope the delete
// to and no existence oracle to worry about, exactly DeleteCapabilityGrant.
func (s PG) DeleteRoleMapping(ctx context.Context, id uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM role_mappings WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete role mapping: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListRoleMappings returns every row, oldest first — both the console's
// People screen and internal/auth/oidc's login-time merge (mergeRoleMaps) read
// the whole table in one call; there is no per-caller scoping to filter on,
// this is deployment-wide config.
func (s PG) ListRoleMappings(ctx context.Context) ([]types.RoleMapping, error) {
	const q = `SELECT ` + roleMappingCols + ` FROM role_mappings ORDER BY created_at`
	return collect(ctx, s.Pool, "list", "role mappings", q, nil, scanRoleMapping)
}

func scanRoleMapping(row pgx.Row) (types.RoleMapping, error) {
	var m types.RoleMapping
	err := row.Scan(&m.ID, &m.Value, &m.Role, &m.CreatedAt, &m.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RoleMapping{}, ErrNotFound
	}
	if err != nil {
		return types.RoleMapping{}, fmt.Errorf("store: scan role mapping: %w", err)
	}
	return m, nil
}
