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

	"github.com/cjohnstoniv/wardyn/internal/audit"
)

// The subject a WARDYN_AUDIT_SEAL=full row carries in its actor column is the
// id of the person's oldest principal_identities row; these are the reads that
// map a person to it and back (audit.SubjectDirectory).

var _ audit.SubjectDirectory = PG{}

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

// AuditSubjectEras is the identity row id's binding history (principal_identity_unbinds), oldest
// first; nil for an id that is not a uuid. An erased principal reads "". SubjectGone is bounded by
// the unbind, so a later holder's erasure, which destroys under the subject after it, is not that
// era's.
func (s PG) AuditSubjectEras(ctx context.Context, id string) ([]audit.SubjectEra, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		return nil, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT COALESCE(h.principal, ''), h.unbound_at,
		COALESCE((SELECT max(k.destroyed_at) FROM principal_keys k WHERE k.owner = $2 AND k.purpose = 'audit-seal'
			AND k.destroyed_at <= h.unbound_at), 'epoch')
		FROM principal_identity_unbinds h WHERE h.identity_id = $1 ORDER BY h.unbound_at`, u, audit.SubjectActorPrefix+u.String())
	if err != nil {
		return nil, fmt.Errorf("store: read an audit subject's earlier bindings: %w", err)
	}
	eras, err := pgx.CollectRows(rows, pgx.RowToStructByPos[audit.SubjectEra])
	if err != nil {
		return nil, fmt.Errorf("store: read an audit subject's earlier bindings: %w", err)
	}
	return eras, nil
}
