// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AuditEventKey names one audit event by its fixed event time and id.
type AuditEventKey struct {
	ID uuid.UUID
	At time.Time
}

// AuditEventsStored reports which of the wanted events the shared audit table holds, matched on the event time
// and the id together as the replay lookup is (InsertAuditEvent), so each partition can use its time index. A key
// is in the result, as true, only when the committed table has a row with that time and that id; an absent one
// is left out. Times are compared and returned at microsecond precision in UTC. It reads the committed table, so
// an event that only waits in a replica's local spool, or that a recorder said it accepted, never counts. A
// caller that owes events settles only when every key it expects is present.
func (s PG) AuditEventsStored(ctx context.Context, want []AuditEventKey) (map[AuditEventKey]bool, error) {
	ids := make([]uuid.UUID, len(want))
	ats := make([]time.Time, len(want))
	for i, k := range want {
		ids[i], ats[i] = k.ID, k.At.UTC().Truncate(time.Microsecond)
	}
	held, err := collect(ctx, s.Pool, "list", "stored audit events", `
		SELECT k.id, k.at FROM unnest($1::uuid[], $2::timestamptz[]) AS k(id, at)
		 WHERE EXISTS (SELECT 1 FROM audit_events e WHERE e."time" = k.at AND e.id = k.id)`, []any{ids, ats},
		func(r pgx.Row) (AuditEventKey, error) {
			var k AuditEventKey
			err := r.Scan(&k.ID, &k.At)
			k.At = k.At.UTC().Truncate(time.Microsecond)
			return k, err
		})
	if err != nil {
		return nil, err
	}
	out := make(map[AuditEventKey]bool, len(held))
	for _, k := range held {
		out[k] = true
	}
	return out, nil
}
