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

// RecordOIDCSessionSID upserts the person a sign-in under issuer's session sid established, kept
// until expiresAt (oidc.SessionSIDs, migration 0229).
func (s PG) RecordOIDCSessionSID(ctx context.Context, issuer, sid, sub string, expiresAt time.Time) error {
	const q = `INSERT INTO oidc_session_sids (issuer, sid, sub, expires_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (issuer, sid) DO UPDATE SET sub = EXCLUDED.sub, expires_at = EXCLUDED.expires_at`
	if _, err := s.Pool.Exec(ctx, q, issuer, sid, sub, expiresAt); err != nil {
		return fmt.Errorf("store: record oidc session sid: %w", err)
	}
	return nil
}

// LookupOIDCSessionSID returns issuer's sid row's person; ok is false with no row.
func (s PG) LookupOIDCSessionSID(ctx context.Context, issuer, sid string) (string, bool, error) {
	var sub string
	err := s.Pool.QueryRow(ctx, `SELECT sub FROM oidc_session_sids WHERE issuer = $1 AND sid = $2`, issuer, sid).Scan(&sub)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: look up oidc session sid: %w", err)
	}
	return sub, true, nil
}

// ForgetOIDCSessionSID deletes issuer's sid row, if any.
func (s PG) ForgetOIDCSessionSID(ctx context.Context, issuer, sid string) error {
	if _, err := s.Pool.Exec(ctx, `DELETE FROM oidc_session_sids WHERE issuer = $1 AND sid = $2`, issuer, sid); err != nil {
		return fmt.Errorf("store: forget oidc session sid: %w", err)
	}
	return nil
}

// PruneOIDCSessionSIDs deletes rows whose session has ended by now, reporting the count removed
// (oidc_session_sids_expires_at_idx backs the WHERE clause).
func (s PG) PruneOIDCSessionSIDs(ctx context.Context, now time.Time) (int, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM oidc_session_sids WHERE expires_at < $1`, now)
	if err != nil {
		return 0, fmt.Errorf("store: prune oidc session sids: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
