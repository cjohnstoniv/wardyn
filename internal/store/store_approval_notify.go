// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ApprovalNotifyReader is the read side of the approval notification outbox that the console
// needs: where each pending approval sits on its escalation schedule, and each channel's delivery
// history. A capability interface like ApprovalsForRunsPager: an absent implementation leaves the
// projection and the status endpoint empty, never an error, since both only describe notifications.
type ApprovalNotifyReader interface {
	// ApprovalEscalations reads, in one query, the escalation state of every approval in ids that has
	// outbox rows. Approvals with none are absent from the map.
	ApprovalEscalations(ctx context.Context, ids []uuid.UUID, now time.Time) (map[uuid.UUID]types.ApprovalEscalation, error)
	// ApprovalNotifyChannelStats returns one row per channel that has outbox rows.
	ApprovalNotifyChannelStats(ctx context.Context, now time.Time) ([]types.ApprovalNotifyChannelStat, error)
}

var _ ApprovalNotifyReader = PG{}

// escalationSQL: tier is the highest tier already due, and the next time is the earliest due time
// still ahead. A tier-0 row due at creation reads as tier 0, which the wire omits.
const escalationSQL = `
SELECT approval_id,
       COALESCE(max(tier) FILTER (WHERE due_at <= $2), 0)::smallint,
       min(due_at) FILTER (WHERE due_at > $2)
  FROM approval_notifications
 WHERE approval_id = ANY($1)
 GROUP BY approval_id`

// ApprovalEscalations — see ApprovalNotifyReader.
func (s PG) ApprovalEscalations(ctx context.Context, ids []uuid.UUID, now time.Time) (map[uuid.UUID]types.ApprovalEscalation, error) {
	out := map[uuid.UUID]types.ApprovalEscalation{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.Pool.Query(ctx, escalationSQL, ids, now)
	if err != nil {
		return nil, fmt.Errorf("store: read approval escalations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var e types.ApprovalEscalation
		if err := rows.Scan(&id, &e.Tier, &e.NextAt); err != nil {
			return nil, fmt.Errorf("store: scan approval escalation: %w", err)
		}
		out[id] = e
	}
	return out, rows.Err()
}

// channelStatsSQL: a successful send clears last_error, so the last error is read from the newest
// attempt that left one. A row goes dead only on a final failure, which is what the hour counts.
const channelStatsSQL = `
SELECT channel,
       max(sent_at),
       COALESCE((array_agg(last_error ORDER BY last_attempt_at DESC) FILTER (WHERE last_error <> ''))[1], ''),
       max(last_attempt_at) FILTER (WHERE last_error <> ''),
       (count(*) FILTER (WHERE state = 'dead' AND last_attempt_at > $1::timestamptz - interval '1 hour'))::int
  FROM approval_notifications
 GROUP BY channel`

// ApprovalNotifyChannelStats — see ApprovalNotifyReader.
func (s PG) ApprovalNotifyChannelStats(ctx context.Context, now time.Time) ([]types.ApprovalNotifyChannelStat, error) {
	return collect(ctx, s.Pool, "read", "approval notify channel stats", channelStatsSQL, []any{now},
		func(r pgx.Row) (st types.ApprovalNotifyChannelStat, err error) {
			err = r.Scan(&st.Channel, &st.LastSentAt, &st.LastError, &st.LastErrorAt, &st.FailedLastHour)
			return st, err
		})
}
