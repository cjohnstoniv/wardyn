// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AuditEventKey is one audit event as the shared table keys it: its time and its id.
type AuditEventKey struct {
	ID uuid.UUID
	At time.Time
}

// AuditEventsStored reports which of want the shared audit table holds, matched on (time, id) so the read uses
// the table's time index. Each key's At is normalised to UTC microseconds, the table's precision, and the map
// holds the normalised key of each event found. It reads the committed table, so an event that only waits in a
// replica's local spool, or that a recorder said it accepted, never counts.
func (s PG) AuditEventsStored(ctx context.Context, want []AuditEventKey) (map[AuditEventKey]bool, error) {
	ids := make([]uuid.UUID, len(want))
	ats := make([]time.Time, len(want))
	for i, k := range want {
		ids[i], ats[i] = k.ID, k.At.UTC().Truncate(time.Microsecond)
	}
	keys, err := collect(ctx, s.Pool, "list", "stored audit events", `
		SELECT a.id, a."time"
		  FROM unnest($1::uuid[], $2::timestamptz[]) AS w(id, at)
		  JOIN audit_events a ON a."time" = w.at AND a.id = w.id`, []any{ids, ats},
		func(r pgx.Row) (AuditEventKey, error) {
			var k AuditEventKey
			err := r.Scan(&k.ID, &k.At)
			return k, err
		})
	if err != nil {
		return nil, err
	}
	out := make(map[AuditEventKey]bool, len(keys))
	for _, k := range keys {
		out[AuditEventKey{ID: k.ID, At: k.At.UTC().Truncate(time.Microsecond)}] = true
	}
	return out, nil
}
