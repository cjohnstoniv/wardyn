// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// WriteGovernance runs fn on one read-committed transaction and commits it when fn returns nil: a
// governance write whose checks and statement must see one state. fn takes the locks it needs.
func (s PG) WriteGovernance(ctx context.Context, fn func(q Querier) error) error {
	return s.inTx(ctx, fn)
}

// MoveGovernanceMembershipQ points membership id at unitID, keeping its id, subject, priority, home
// and legacy flag. ErrNotFound when no membership has id or unitID names no unit (the foreign key);
// ErrConflict when the subject is already a member of unitID.
func MoveGovernanceMembershipQ(ctx context.Context, q Querier, id, unitID uuid.UUID) (types.GovernanceMembership, error) {
	out, err := scanGovernanceMembership(q.QueryRow(ctx,
		`UPDATE governance_memberships SET unit_id = $2 WHERE id = $1 RETURNING `+governanceMembershipCols, id, unitID))
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		return types.GovernanceMembership{}, ErrNotFound
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return types.GovernanceMembership{}, ErrConflict
	}
	return out, err
}

// PromoteGovernanceMembershipQ makes membership id a context (legacy false). ErrNotFound when none.
func PromoteGovernanceMembershipQ(ctx context.Context, q Querier, id uuid.UUID) (types.GovernanceMembership, error) {
	return scanGovernanceMembership(q.QueryRow(ctx,
		`UPDATE governance_memberships SET legacy = false WHERE id = $1 RETURNING `+governanceMembershipCols, id))
}
