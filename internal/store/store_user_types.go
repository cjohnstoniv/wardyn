// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// User types (migration 0071_user_types). Round-trips rows; every write is
// validated at the API boundary (internal/api/user_types.go).
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const userTypeCols = `id, name, description, priority, built_in, created_at, updated_at, created_by`

// userTypeRowRefs counts the subject rows that name user type $1: capability
// grants, governance assignments and drive grants. ONE expression, read by
// both UserTypeReferences and DeleteUserType, so the count a 409 reports and
// the predicate the delete honours cannot disagree.
const userTypeRowRefs = `(
	(SELECT count(*) FROM capability_grants      WHERE subject_type = 'user_type' AND subject = $1) +
	(SELECT count(*) FROM governance_assignments WHERE subject_type = 'user_type' AND subject = $1) +
	(SELECT count(*) FROM user_drive_grants      WHERE subject_type = 'user_type' AND subject = $1))`

// userTypeRowRefsExist is userTypeRowRefs' boolean twin, read by CreateUserType
// alone: a subject row can outlive the type it names (userTypeSubjectExists'
// own check-then-insert race against a concurrent DeleteUserType), and that
// orphan would silently rebind to a later type created with the same id. This
// refuses to recreate an id any subject row still names, so the id stays dead
// until an operator clears the orphan rows themselves.
const userTypeRowRefsExist = `(
	EXISTS (SELECT 1 FROM capability_grants      WHERE subject_type = 'user_type' AND subject = $1) OR
	EXISTS (SELECT 1 FROM governance_assignments WHERE subject_type = 'user_type' AND subject = $1) OR
	EXISTS (SELECT 1 FROM user_drive_grants      WHERE subject_type = 'user_type' AND subject = $1))`

// ListUserTypes returns every type: the built-in first, then by priority
// (highest first) and name.
func (s PG) ListUserTypes(ctx context.Context) ([]types.UserType, error) {
	const q = `SELECT ` + userTypeCols + ` FROM user_types ORDER BY built_in DESC, priority DESC, name`
	return collect(ctx, s.Pool, "list", "user types", q, nil, scanUserType)
}

// GetUserType returns one type by id, or ErrNotFound.
func (s PG) GetUserType(ctx context.Context, id string) (types.UserType, error) {
	const q = `SELECT ` + userTypeCols + ` FROM user_types WHERE id = $1`
	return scanUserType(s.Pool.QueryRow(ctx, q, id))
}

// CreateUserType inserts a custom type. ErrConflict when the id or the name is
// taken, or the id is still named by an orphaned subject row (see
// userTypeRowRefsExist). built_in is never written: the only built-in row is
// the seeded one.
func (s PG) CreateUserType(ctx context.Context, t types.UserType) (types.UserType, error) {
	const q = `
		INSERT INTO user_types (id, name, description, priority, created_by)
		SELECT $1, $2, $3, $4, $5
		WHERE NOT ` + userTypeRowRefsExist + `
		RETURNING ` + userTypeCols
	var out types.UserType
	err := s.Pool.QueryRow(ctx, q, t.ID, t.Name, t.Description, t.Priority, t.CreatedBy).Scan(
		&out.ID, &out.Name, &out.Description, &out.Priority, &out.BuiltIn, &out.CreatedAt, &out.UpdatedAt, &out.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.UserType{}, ErrConflict
	}
	return out, uniqueConflict(err)
}

// UpdateUserType rewrites a type's name, description and priority; the id,
// built_in and creation provenance never change. ErrNotFound when no row has
// the id, ErrConflict when the name is taken.
func (s PG) UpdateUserType(ctx context.Context, t types.UserType) (types.UserType, error) {
	const q = `
		UPDATE user_types
		SET name = $2, description = $3, priority = $4, updated_at = now()
		WHERE id = $1
		RETURNING ` + userTypeCols
	out, err := scanUserType(s.Pool.QueryRow(ctx, q, t.ID, t.Name, t.Description, t.Priority))
	return out, uniqueConflict(err)
}

// UserTypeReferences counts the capability-grant, governance-assignment and
// drive-grant rows that name the type as their subject.
func (s PG) UserTypeReferences(ctx context.Context, id string) (int, error) {
	var n int
	if err := s.Pool.QueryRow(ctx, `SELECT `+userTypeRowRefs, id).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count user type references: %w", err)
	}
	return n, nil
}

// DeleteUserType removes a custom type that nothing names. ErrNotFound when no
// row has the id; ErrConflict when the row is built in, still named by a
// subject row, or held by a foreign key (a later role_mappings.user_type is ON
// DELETE RESTRICT). The reference check rides the DELETE itself, so a row
// written between the caller's own check and this statement still refuses.
func (s PG) DeleteUserType(ctx context.Context, id string) error {
	tag, err := s.Pool.Exec(ctx,
		`DELETE FROM user_types WHERE id = $1 AND NOT built_in AND `+userTypeRowRefs+` = 0`, id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return ErrConflict
		}
		return fmt.Errorf("store: delete user type: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var exists bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_types WHERE id = $1)`, id).Scan(&exists); err != nil {
		return fmt.Errorf("store: delete user type: %w", err)
	}
	if exists {
		return ErrConflict
	}
	return ErrNotFound
}

func uniqueConflict(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrConflict
	}
	return err
}

func scanUserType(row pgx.Row) (types.UserType, error) {
	var t types.UserType
	err := row.Scan(&t.ID, &t.Name, &t.Description, &t.Priority, &t.BuiltIn, &t.CreatedAt, &t.UpdatedAt, &t.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.UserType{}, ErrNotFound
	}
	if err != nil {
		return types.UserType{}, fmt.Errorf("store: scan user type: %w", err)
	}
	return t, nil
}
