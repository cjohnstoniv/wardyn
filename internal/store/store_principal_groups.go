// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The record of each person's last sign-in through each door (migration 0290_principal_groups).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PrincipalGroups is what a person's last sign-in through one door carried: the token's email as sent,
// the group snapshot and whether that snapshot is partial. Groups is nil only for a stored JSON null, which
// a capability resolution reads as no snapshot at all.
type PrincipalGroups struct {
	Email     string
	Groups    []string
	Truncated bool
}

// PutPrincipalGroups records a sign-in's email and group snapshot as principal's through portal ("" for
// an SSO sign-in to Wardyn itself, else the portal's id), replacing the one before through the same door
// and stamping the database's time. issuedAt is when the token the snapshot came from was issued: a
// record from a token issued later is kept, and the write changes nothing, so an older token still valid
// cannot put back a group a newer one dropped. A zero issuedAt stamps the database's time instead, so
// sign-ins through several servers are ordered by one clock. A nil groups is recorded as none. The sign-in gate and the
// portal token exchange call it before they issue a session or token and refuse on an error.
func (s PG) PutPrincipalGroups(ctx context.Context, principal, portal, email string, groups []string, truncated bool, issuedAt time.Time) error {
	if groups == nil {
		groups = []string{}
	}
	b, err := json.Marshal(groups)
	if err != nil {
		return fmt.Errorf("store: encode sign-in groups: %w", err)
	}
	var at any
	if !issuedAt.IsZero() {
		at = issuedAt
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO principal_groups (principal, portal, email, groups, truncated, issued_at)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6::timestamptz, now()))
		ON CONFLICT (principal, portal) DO UPDATE SET email = EXCLUDED.email, groups = EXCLUDED.groups,
			truncated = EXCLUDED.truncated, issued_at = EXCLUDED.issued_at, signed_in_at = now()
		WHERE principal_groups.issued_at IS NULL OR principal_groups.issued_at <= EXCLUDED.issued_at`,
		principal, portal, email, b, truncated, at); err != nil {
		return fmt.Errorf("store: record sign-in groups: %w", err)
	}
	return nil
}

// NarrowPrincipalGroups takes out of principal's record through portal every group a verified token
// that was refused there no longer carries, and marks the record partial when that token's snapshot was.
// It only narrows: it never adds a group, and a person with no record through portal keeps none, so a
// token the door did not admit writes no row. A record that is no list, or from a token issued after
// issuedAt, is left as it is.
func (s PG) NarrowPrincipalGroups(ctx context.Context, principal, portal string, groups []string, truncated bool, issuedAt time.Time) error {
	if groups == nil {
		groups = []string{}
	}
	b, err := json.Marshal(groups)
	if err != nil {
		return fmt.Errorf("store: encode sign-in groups: %w", err)
	}
	if _, err := s.Pool.Exec(ctx, `
		UPDATE principal_groups p SET groups = COALESCE((
			SELECT jsonb_agg(x) FROM jsonb_array_elements_text(p.groups) AS x
			WHERE x IN (SELECT jsonb_array_elements_text($3::jsonb))), '[]'::jsonb),
			truncated = p.truncated OR $4, issued_at = $5, signed_in_at = now()
		WHERE p.principal = $1 AND p.portal = $2 AND jsonb_typeof(p.groups) = 'array'
			AND (p.issued_at IS NULL OR p.issued_at <= $5)`, principal, portal, b, truncated, issuedAt); err != nil {
		return fmt.Errorf("store: narrow sign-in groups: %w", err)
	}
	return nil
}

// GetPrincipalGroups is principal's record through portal, or ErrNotFound when there is none: no
// sign-in through that door recorded since the table was filled, or their credentials erased since.
func (s PG) GetPrincipalGroups(ctx context.Context, principal, portal string) (PrincipalGroups, error) {
	var (
		rec PrincipalGroups
		raw []byte
	)
	err := s.Pool.QueryRow(ctx, `SELECT email, groups, truncated FROM principal_groups WHERE principal = $1 AND portal = $2`, principal, portal).
		Scan(&rec.Email, &raw, &rec.Truncated)
	if errors.Is(err, pgx.ErrNoRows) {
		return PrincipalGroups{}, ErrNotFound
	}
	if err != nil {
		return PrincipalGroups{}, fmt.Errorf("store: read sign-in groups: %w", err)
	}
	if err := json.Unmarshal(raw, &rec.Groups); err != nil {
		return PrincipalGroups{}, fmt.Errorf("store: decode sign-in groups: %w", err)
	}
	return rec, nil
}
