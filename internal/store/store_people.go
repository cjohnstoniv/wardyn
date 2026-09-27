// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// PersonStore holds the identities an admin created or confirmed before their
// first sign-in (migration 0090, #1157). Optional, like DeviceStore: the API
// answers 501 on a store without it.
type PersonStore interface {
	// CreatePerson inserts p, or returns the row already stored under
	// p.Principal with created=false. A different principal holding the same
	// email (case-insensitively) is ErrConflict.
	CreatePerson(ctx context.Context, p types.Person) (out types.Person, created bool, err error)
	// GetPerson is the row stored under exactly principal, or ErrNotFound.
	GetPerson(ctx context.Context, principal string) (types.Person, error)
	// ListPeople returns every row, oldest first.
	ListPeople(ctx context.Context) ([]types.Person, error)
	// MarkPersonSignedIn stamps first_signed_in_at on principal's row the first
	// time they sign in. No row is not an error: most people were never
	// pre-created.
	MarkPersonSignedIn(ctx context.Context, principal string, now time.Time) error
}

var _ PersonStore = PG{}

const personCols = `principal, email, created_by, created_at, first_signed_in_at`

func scanPerson(row pgx.Row) (types.Person, error) {
	var p types.Person
	err := row.Scan(&p.Principal, &p.Email, &p.CreatedBy, &p.CreatedAt, &p.FirstSignedInAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.Person{}, ErrNotFound
	}
	if err != nil {
		return types.Person{}, fmt.Errorf("store: scan person: %w", err)
	}
	return p, nil
}

func (s PG) CreatePerson(ctx context.Context, p types.Person) (types.Person, bool, error) {
	out, err := scanPerson(s.Pool.QueryRow(ctx, `
		INSERT INTO people (principal, email, created_by) VALUES ($1, $2, $3)
		ON CONFLICT (principal) DO NOTHING
		RETURNING `+personCols, p.Principal, p.Email, p.CreatedBy))
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return types.Person{}, false, ErrConflict
	case errors.Is(err, ErrNotFound):
		existing, gerr := s.GetPerson(ctx, p.Principal)
		return existing, false, gerr
	case err != nil:
		return types.Person{}, false, err
	}
	return out, true, nil
}

func (s PG) GetPerson(ctx context.Context, principal string) (types.Person, error) {
	return scanPerson(s.Pool.QueryRow(ctx, `SELECT `+personCols+` FROM people WHERE principal = $1`, principal))
}

func (s PG) ListPeople(ctx context.Context) ([]types.Person, error) {
	return collect(ctx, s.Pool, "list", "people", `SELECT `+personCols+` FROM people ORDER BY created_at, principal`, nil, scanPerson)
}

func (s PG) MarkPersonSignedIn(ctx context.Context, principal string, now time.Time) error {
	if _, err := s.Pool.Exec(ctx,
		`UPDATE people SET first_signed_in_at = $2 WHERE principal = $1 AND first_signed_in_at IS NULL`, principal, now); err != nil {
		return fmt.Errorf("store: mark person signed in: %w", err)
	}
	return nil
}
