// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The acknowledged audit-delivery checkpoint (#1513): the durable per-destination cursor the
// leader-only webhook delivery loop (internal/audit/sinks) reads the already-masked audit
// trail from, and the status its metrics report. Methods live on PG, not Store: like the
// federation cursor (store_devices.go) they serve one background loop, so a test double that
// implements Store need never grow them.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AuditDeliveryStatus is one destination's checkpoint plus the head it lags behind, the whole
// input the delivery metrics emit. AckedAt and OldestUndelivered are nil when nothing has been
// acknowledged yet and when the checkpoint is caught up.
type AuditDeliveryStatus struct {
	Destination       string
	HeadSeq           int64
	AckedSeq          int64
	Resets            int64
	AckedAt           *time.Time
	OldestUndelivered *time.Time
	LastError         string
	Halted            bool
}

// EnsureAuditDeliveryCursor creates dest's checkpoint at the audit table's CURRENT head, so the
// first enable sends only new events, never the whole history. ON CONFLICT DO NOTHING keeps the
// row an existing checkpoint already holds, so a restart (or a second replica's call) never
// rewinds delivery.
func (s PG) EnsureAuditDeliveryCursor(ctx context.Context, dest string) error {
	const q = `INSERT INTO audit_delivery_cursors (destination, acked_seq, acked_row_hash)
		SELECT $1, COALESCE(h.seq,0), COALESCE(h.row_hash,'') FROM (SELECT 1) one
		LEFT JOIN LATERAL (SELECT seq, row_hash FROM audit_events ORDER BY seq DESC LIMIT 1) h ON true
		ON CONFLICT (destination) DO NOTHING`
	if _, err := s.Pool.Exec(ctx, q, dest); err != nil {
		return fmt.Errorf("store: ensure audit delivery cursor: %w", err)
	}
	return nil
}

// GetAuditDeliveryCursor returns dest's acknowledged position and the row_hash that seq
// carried, (0, "") while no checkpoint row exists — the same zero-value-on-no-row contract as
// GetFederationCursor, so a cursor loaded before Ensure reads as "deliver from the beginning".
func (s PG) GetAuditDeliveryCursor(ctx context.Context, dest string) (int64, string, error) {
	var seq int64
	var rowHash string
	err := s.Pool.QueryRow(ctx, `SELECT acked_seq, acked_row_hash FROM audit_delivery_cursors WHERE destination = $1`, dest).
		Scan(&seq, &rowHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("store: get audit delivery cursor: %w", err)
	}
	return seq, rowHash, nil
}

// SetAuditDeliveryCursor advances dest's checkpoint after the collector accepted a batch. The
// zero position is written only after a cursor reset (the acknowledged row is gone or its hash
// changed), which is why seq 0 counts one reset and leaves acked_at alone: an acceptance that
// delivered nothing is not an acceptance at all.
func (s PG) SetAuditDeliveryCursor(ctx context.Context, dest string, seq int64, rowHash string) error {
	// $2 is cast at its first use: arriving untyped, Postgres deduces int4 from the reset
	// CASE's literals against int8 from the column and refuses the statement (SQLSTATE 42P08).
	const q = `UPDATE audit_delivery_cursors
		SET acked_seq = $2::bigint, acked_row_hash = $3,
		    acked_at = CASE WHEN $2 > 0 THEN now() ELSE acked_at END,
		    resets = resets + CASE WHEN $2 = 0 THEN 1 ELSE 0 END,
		    updated_at = now()
		WHERE destination = $1`
	if _, err := s.Pool.Exec(ctx, q, dest, seq, rowHash); err != nil {
		return fmt.Errorf("store: set audit delivery cursor: %w", err)
	}
	return nil
}

// SetAuditDeliveryError records the collector's last answer and whether it halted delivery
// until wardynd restarts (a terminal 4xx). An empty msg with halted false clears both.
func (s PG) SetAuditDeliveryError(ctx context.Context, dest, msg string, halted bool) error {
	const q = `UPDATE audit_delivery_cursors SET last_error = $2, halted = $3, updated_at = now() WHERE destination = $1`
	if _, err := s.Pool.Exec(ctx, q, dest, msg, halted); err != nil {
		return fmt.Errorf("store: set audit delivery error: %w", err)
	}
	return nil
}

// AuditDeliveryStatuses returns one row per checkpoint destination, ordered by destination,
// with the audit table's head and the recorded_at of the oldest event past the checkpoint.
// One statement, so a lag reading cannot straddle two queries; the subqueries read the
// append-only table, so it answers on any replica, not just the leader.
func (s PG) AuditDeliveryStatuses(ctx context.Context) ([]AuditDeliveryStatus, error) {
	const q = `
		SELECT c.destination,
		       (SELECT COALESCE(max(e.seq), 0) FROM audit_events e),
		       c.acked_seq,
		       c.resets,
		       c.acked_at,
		       (SELECT e.recorded_at FROM audit_events e WHERE e.seq > c.acked_seq ORDER BY e.seq LIMIT 1),
		       c.last_error,
		       c.halted
		FROM audit_delivery_cursors c
		ORDER BY c.destination`
	return collect(ctx, s.Pool, "list", "audit delivery statuses", q, nil, func(row pgx.Row) (AuditDeliveryStatus, error) {
		var st AuditDeliveryStatus
		if err := row.Scan(&st.Destination, &st.HeadSeq, &st.AckedSeq, &st.Resets, &st.AckedAt,
			&st.OldestUndelivered, &st.LastError, &st.Halted); err != nil {
			return AuditDeliveryStatus{}, fmt.Errorf("store: scan audit delivery status: %w", err)
		}
		return st, nil
	})
}
