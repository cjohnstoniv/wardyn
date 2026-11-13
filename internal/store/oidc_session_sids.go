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
// until expiresAt (oidc.SessionSIDs, migration 0229). The same person's row keeps the later end, so
// two sign-ins under one sid that record out of order leave the newer session's end.
func (s PG) RecordOIDCSessionSID(ctx context.Context, issuer, sid, sub string, expiresAt time.Time) error {
	const q = `INSERT INTO oidc_session_sids (issuer, sid, sub, expires_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (issuer, sid) DO UPDATE SET sub = EXCLUDED.sub, expires_at = CASE
			WHEN oidc_session_sids.sub = EXCLUDED.sub THEN GREATEST(oidc_session_sids.expires_at, EXCLUDED.expires_at)
			ELSE EXCLUDED.expires_at END`
	if _, err := s.Pool.Exec(ctx, q, issuer, sid, sub, expiresAt); err != nil {
		return fmt.Errorf("store: record oidc session sid: %w", err)
	}
	return nil
}

// LookupOIDCSessionSID returns issuer's sid row's person and the row's version (its xmin, which every
// write moves); ok is false with no row, and with one whose session has ended but the prune has not
// yet removed, so that row cuts no sessions.
func (s PG) LookupOIDCSessionSID(ctx context.Context, issuer, sid string) (string, string, bool, error) {
	var sub, version string
	err := s.Pool.QueryRow(ctx, `SELECT sub, xmin::text FROM oidc_session_sids WHERE issuer = $1 AND sid = $2 AND expires_at > now()`, issuer, sid).Scan(&sub, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("store: look up oidc session sid: %w", err)
	}
	return sub, version, true, nil
}

// ForgetOIDCSessionSID deletes issuer's sid row if it is still at version, the one a lookup read.
func (s PG) ForgetOIDCSessionSID(ctx context.Context, issuer, sid, version string) error {
	if _, err := s.Pool.Exec(ctx, `DELETE FROM oidc_session_sids WHERE issuer = $1 AND sid = $2 AND xmin::text = $3`, issuer, sid, version); err != nil {
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
