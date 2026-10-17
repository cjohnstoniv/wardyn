// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The subject a WARDYN_AUDIT_SEAL=full row carries in its actor column is the
// id of the person's oldest principal_identities row; these are the reads that
// map a person to it and back (audit.SubjectDirectory).

// AuditSubjectFor is principal's subject id. ok is false when the person has no
// identity row.
func (s PG) AuditSubjectFor(ctx context.Context, principal string) (string, bool, error) {
	var id uuid.UUID
	err := s.Pool.QueryRow(ctx, `SELECT id FROM principal_identities WHERE principal = $1 ORDER BY created_at, id LIMIT 1`, principal).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read an audit subject: %w", err)
	}
	return id.String(), true, nil
}

// AuditPrincipalOf is the principal a subject id names. ok is false for an id
// that is not a uuid or that no bound identity row carries.
func (s PG) AuditPrincipalOf(ctx context.Context, id string) (string, bool, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		return "", false, nil
	}
	var principal string
	err = s.Pool.QueryRow(ctx, `SELECT principal FROM principal_identities WHERE id = $1 AND principal IS NOT NULL`, u).Scan(&principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read an audit subject's person: %w", err)
	}
	return principal, true, nil
}

// AuditKeyDestroyedAt is when the latest generation of owner's audit-seal key
// was destroyed. ok is false while none has been.
func (s PG) AuditKeyDestroyedAt(ctx context.Context, owner string) (time.Time, bool, error) {
	var at *time.Time
	err := s.Pool.QueryRow(ctx, `SELECT max(destroyed_at) FROM principal_keys WHERE owner = $1 AND purpose = 'audit-seal'`, owner).Scan(&at)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: read an audit key's destruction: %w", err)
	}
	if at == nil {
		return time.Time{}, false, nil
	}
	return *at, true, nil
}
