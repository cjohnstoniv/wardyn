// Copyright 2026 The Wardyn Authors
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

// AuditSubjectReleased implements audit.SubjectDirectory: of the identity rows whose first recorded
// unbind at or after at released principal, the one released first. An erased principal's name is
// cleared from the binding history, so they match none.
func (s PG) AuditSubjectReleased(ctx context.Context, principal string, at time.Time) (string, bool, error) {
	var id uuid.UUID
	err := s.Pool.QueryRow(ctx, `SELECT h.identity_id FROM principal_identity_unbinds h
		WHERE h.principal = $1 AND h.unbound_at >= $2 AND NOT EXISTS (SELECT 1 FROM principal_identity_unbinds e
			WHERE e.identity_id = h.identity_id AND e.unbound_at >= $2 AND e.unbound_at < h.unbound_at)
		ORDER BY h.unbound_at LIMIT 1`, principal, at).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read the identity row an unbind released a person from: %w", err)
	}
	return id.String(), true, nil
}
