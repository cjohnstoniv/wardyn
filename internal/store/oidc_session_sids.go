// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// RecordOIDCSessionSID upserts the person a sign-in under issuer's session sid established
// (oidc.SessionSIDs, migration 0229).
func (s PG) RecordOIDCSessionSID(ctx context.Context, issuer, sid, sub string, at time.Time) error {
	const q = `INSERT INTO oidc_session_sids (issuer, sid, sub, signed_in_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (issuer, sid) DO UPDATE SET sub = EXCLUDED.sub, signed_in_at = EXCLUDED.signed_in_at`
	if _, err := s.Pool.Exec(ctx, q, issuer, sid, sub, at); err != nil {
		return fmt.Errorf("store: record oidc session sid: %w", err)
	}
	return nil
}

// TakeOIDCSessionSID deletes issuer's sid row and returns its person; ok is false with no row.
func (s PG) TakeOIDCSessionSID(ctx context.Context, issuer, sid string) (string, bool, error) {
	var sub string
	err := s.Pool.QueryRow(ctx, `DELETE FROM oidc_session_sids WHERE issuer = $1 AND sid = $2 RETURNING sub`, issuer, sid).Scan(&sub)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: take oidc session sid: %w", err)
	}
	return sub, true, nil
}

// PruneOIDCSessionSIDs deletes rows of sign-ins before cutoff, reporting the count removed
// (oidc_session_sids_signed_in_at_idx backs the WHERE clause).
func (s PG) PruneOIDCSessionSIDs(ctx context.Context, cutoff time.Time) (int, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM oidc_session_sids WHERE signed_in_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: prune oidc session sids: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
