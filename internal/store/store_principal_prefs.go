// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Per-person preferences (migration 0072_user_view_type).
package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// PrincipalPrefStore persists per-person preferences. Like RunLayoutStore it
// is not part of Store, so the api test doubles need not grow it; the api
// type-asserts and treats its absence as "nothing remembered".
type PrincipalPrefStore interface {
	// GetPrincipalPref returns principal's value for key, or ErrNotFound.
	GetPrincipalPref(ctx context.Context, principal, key string) (json.RawMessage, error)
	// PutPrincipalPref upserts principal's value for key.
	PutPrincipalPref(ctx context.Context, principal, key string, value json.RawMessage) error
}

var _ PrincipalPrefStore = PG{}

func (s PG) GetPrincipalPref(ctx context.Context, principal, key string) (json.RawMessage, error) {
	var v json.RawMessage
	err := s.Pool.QueryRow(ctx, `SELECT value FROM principal_prefs WHERE principal = $1 AND key = $2`, principal, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

func (s PG) PutPrincipalPref(ctx context.Context, principal, key string, value json.RawMessage) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO principal_prefs (principal, key, value, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (principal, key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at`,
		principal, key, value)
	return err
}
