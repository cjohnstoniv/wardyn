// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// LatestOrganisationAuditEvent returns the most recent audit event with the
// given action that this organisation wrote itself as actorType/actor, or
// ErrNotFound when none exists. /healthz reads the ground-truth sensor's latest
// kernel.sensor.ping through it.
//
// Only the organisation's own rows, and only the named writer's: an enrolled
// laptop may forward any action with any data and any time (POST
// /devices/{id}/audit), so a forwarded row naming the same action must never
// stand in for the organisation's own. The filter sits inside the window, so
// forwarded rows can neither win it nor push the writer's own rows out of it.
//
// Most recent by `time`, picked from a seq-ordered window: the audit spool
// replays at-least-once while keeping each event's original ev.Time, so a beat
// spooled through a DB blip could carry the highest seq but an hours-old time
// and falsely mark the sensor degraded. The window orders by seq to keep using
// the purpose-built (action, seq DESC) index; ordering by time directly would
// sort the whole action's history on every probe. Twenty is a window, not a
// proof — a longer replay backlog can push the fresh row out, bounding the
// staleness to one heartbeat interval against /healthz's own TTL.
func (s PG) LatestOrganisationAuditEvent(ctx context.Context, action string, actorType types.ActorType, actor string) (types.AuditEvent, error) {
	const q = `
		SELECT ` + auditCols + ` FROM (
			SELECT ` + auditCols + `, seq
			FROM audit_events
			WHERE action=$1 AND actor_type=$2 AND actor=$3 AND NOT ` + federatedRowSQL + `
			ORDER BY seq DESC LIMIT 20
		) recent ORDER BY time DESC, seq DESC LIMIT 1`
	ev, err := scanAuditEvent(s.Pool.QueryRow(ctx, q, action, string(actorType), actor))
	if errors.Is(err, pgx.ErrNoRows) {
		return types.AuditEvent{}, ErrNotFound
	}
	if err != nil {
		return types.AuditEvent{}, fmt.Errorf("store: latest organisation audit event: %w", err)
	}
	return ev, nil
}
