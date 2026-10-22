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

// UnitAdminStore keeps the unit-admin grants (migration 0211): a person or an IdP group that governs
// one unit and everything under it. A side interface, as BoundaryStore is: a store that does not
// implement it holds none, so only a security caller governs.
type UnitAdminStore interface {
	// ListUnitAdmins is every grant, in unit then subject order.
	ListUnitAdmins(ctx context.Context) ([]types.GovernanceUnitAdmin, error)
	// ListUnitAdminOf is every grant naming one of users (user rows) or one of groups (group rows).
	ListUnitAdminOf(ctx context.Context, users, groups []string) ([]types.GovernanceUnitAdmin, error)
	// UnitScope reports whether one of users or groups holds a grant on unit or on a unit above it;
	// strict leaves unit's own grants out.
	UnitScope(ctx context.Context, unit uuid.UUID, users, groups []string, strict bool) (bool, error)
	GetUnitAdmin(ctx context.Context, unit, id uuid.UUID) (types.GovernanceUnitAdmin, error)
}

const unitAdminCols = `id, unit_id, subject_type, subject, granted_by, approved_by, created_at`

func (s PG) ListUnitAdmins(ctx context.Context) ([]types.GovernanceUnitAdmin, error) {
	return ListUnitAdminsQ(ctx, s.Pool)
}

func (s PG) ListUnitAdminOf(ctx context.Context, users, groups []string) ([]types.GovernanceUnitAdmin, error) {
	return collect(ctx, s.Pool, "list", "unit admins of a caller", `SELECT `+unitAdminCols+` FROM governance_unit_admins
		WHERE (subject_type = 'user' AND subject = ANY($1::text[])) OR (subject_type = 'group' AND subject = ANY($2::text[]))
		ORDER BY unit_id, subject_type, subject`, []any{nonNil(users), nonNil(groups)}, scanUnitAdmin)
}

func (s PG) UnitScope(ctx context.Context, unit uuid.UUID, users, groups []string, strict bool) (bool, error) {
	return UnitScopeQ(ctx, s.Pool, unit, users, groups, strict)
}

func (s PG) GetUnitAdmin(ctx context.Context, unit, id uuid.UUID) (types.GovernanceUnitAdmin, error) {
	return GetUnitAdminQ(ctx, s.Pool, unit, id)
}

// ListUnitAdminsQ is ListUnitAdmins on q.
func ListUnitAdminsQ(ctx context.Context, q Querier) ([]types.GovernanceUnitAdmin, error) {
	return collect(ctx, q, "list", "unit admins", `SELECT `+unitAdminCols+` FROM governance_unit_admins
		ORDER BY unit_id, subject_type, subject`, nil, scanUnitAdmin)
}

// UnitScopeQ is UnitScope on q: one recursive statement walking parent_id up from unit (bounded at
// MaxUnitDepthCap rows), joined to the grants on any unit of that path. An unknown unit is out of scope.
func UnitScopeQ(ctx context.Context, q Querier, unit uuid.UUID, users, groups []string, strict bool) (bool, error) {
	var in bool
	err := q.QueryRow(ctx, `
		WITH RECURSIVE chain AS (
			SELECT u.id, u.parent_id, 0 AS n FROM governance_units u WHERE u.id = $1
			UNION ALL
			SELECT u.id, u.parent_id, c.n + 1
			FROM governance_units u JOIN chain c ON u.id = c.parent_id
			WHERE c.n < 63
		)
		SELECT EXISTS (
			SELECT 1 FROM chain c JOIN governance_unit_admins a ON a.unit_id = c.id
			WHERE (NOT $4 OR c.n > 0)
			  AND ((a.subject_type = 'user' AND a.subject = ANY($2::text[]))
			    OR (a.subject_type = 'group' AND a.subject = ANY($3::text[])))
		)`, unit, nonNil(users), nonNil(groups), strict).Scan(&in)
	if err != nil {
		return false, fmt.Errorf("store: read unit scope: %w", err)
	}
	return in, nil
}

// GetUnitAdminQ is one grant on unit. ErrNotFound when none.
func GetUnitAdminQ(ctx context.Context, q Querier, unit, id uuid.UUID) (types.GovernanceUnitAdmin, error) {
	a, err := scanUnitAdmin(q.QueryRow(ctx, `SELECT `+unitAdminCols+` FROM governance_unit_admins
		WHERE id = $1 AND unit_id = $2`, id, unit))
	if errors.Is(err, pgx.ErrNoRows) {
		return types.GovernanceUnitAdmin{}, ErrNotFound
	}
	if err != nil {
		return types.GovernanceUnitAdmin{}, fmt.Errorf("store: get unit admin: %w", err)
	}
	return a, nil
}

// GrantUnitAdminQ stores a grant on q (a nil id gets a fresh one). ErrConflict when the subject already
// holds one on the unit, ErrNotFound when the unit is unknown.
func GrantUnitAdminQ(ctx context.Context, q Querier, a types.GovernanceUnitAdmin) (types.GovernanceUnitAdmin, error) {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	out, err := scanUnitAdmin(q.QueryRow(ctx, `
		INSERT INTO governance_unit_admins (id, unit_id, subject_type, subject, granted_by, approved_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+unitAdminCols, a.ID, a.UnitID, string(a.SubjectType), a.Subject, a.GrantedBy, a.ApprovedBy))
	switch {
	case isUniqueViolation(err):
		return types.GovernanceUnitAdmin{}, ErrConflict
	case isForeignKeyViolation(err):
		return types.GovernanceUnitAdmin{}, ErrNotFound
	case err != nil:
		return types.GovernanceUnitAdmin{}, fmt.Errorf("store: grant unit admin: %w", err)
	}
	return out, nil
}

// RevokeUnitAdminQ removes one grant on unit and returns the row it removed. ErrNotFound when none.
func RevokeUnitAdminQ(ctx context.Context, q Querier, unit, id uuid.UUID) (types.GovernanceUnitAdmin, error) {
	a, err := scanUnitAdmin(q.QueryRow(ctx, `DELETE FROM governance_unit_admins WHERE id = $1 AND unit_id = $2
		RETURNING `+unitAdminCols, id, unit))
	if errors.Is(err, pgx.ErrNoRows) {
		return types.GovernanceUnitAdmin{}, ErrNotFound
	}
	if err != nil {
		return types.GovernanceUnitAdmin{}, fmt.Errorf("store: revoke unit admin: %w", err)
	}
	return a, nil
}

func scanUnitAdmin(row pgx.Row) (types.GovernanceUnitAdmin, error) {
	var a types.GovernanceUnitAdmin
	var st string
	err := row.Scan(&a.ID, &a.UnitID, &st, &a.Subject, &a.GrantedBy, &a.ApprovedBy, &a.CreatedAt)
	a.SubjectType = types.CapabilitySubjectType(st)
	return a, err
}

// nonNil is a text[] argument with nil as the empty array, so ANY matches nothing.
func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}
