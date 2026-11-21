// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AuditEventsStored is the stored event time of each id that the shared audit table holds; an absent id is left
// out of the map. It reads the committed table, so an event that only waits in a replica's local spool, or that a
// recorder said it accepted, never counts. A caller that owes events settles only when every id it expects is
// present and its stored time equals the time it recorded.
func (s PG) AuditEventsStored(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]time.Time, error) {
	type row struct {
		id uuid.UUID
		at time.Time
	}
	rows, err := collect(ctx, s.Pool, "list", "stored audit events", `SELECT id, "time" FROM audit_events WHERE id = ANY($1)`, []any{ids},
		func(r pgx.Row) (row, error) {
			var v row
			return v, r.Scan(&v.id, &v.at)
		})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]time.Time, len(rows))
	for _, v := range rows {
		out[v.id] = v.at
	}
	return out, nil
}
