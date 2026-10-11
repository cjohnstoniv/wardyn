// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Live tests for the acknowledged-delivery checkpoint (#1513): a first enable starts at the
// audit table's current head and never moves afterwards, and the checkpoint's status reports
// lag, the oldest undelivered event, resets and the halt state. Guarded by WARDYN_TEST_PG
// through runsPGPoolIsolated, so each test migrates its own throwaway database (no other
// writer's rows can move the head under it).
package store_test

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const auditDeliveryDest = "webhook"

// deliveryProbe is one audit_events row a test appended through the store's own insert.
type deliveryProbe struct {
	seq        int64
	rowHash    string
	recordedAt time.Time
}

// insertDeliveryProbes appends n events through store.InsertAuditEvent — the only path that
// allocates seq and the chain hashes — and reads each stored row back. It uses the store's
// insert rather than raw SQL because the cursor's whole contract is "a row that really is in
// the trail", which only that path produces.
func insertDeliveryProbes(t *testing.T, pool *pgxpool.Pool, n int) []deliveryProbe {
	t.Helper()
	ctx := context.Background()
	out := make([]deliveryProbe, 0, n)
	for i := range n {
		ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem,
			Actor: "audit-delivery-probe", Action: "audit.delivery.probe", Outcome: "success",
			Data: json.RawMessage(`{"probe":` + strconv.Itoa(i) + `}`)}
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatalf("InsertAuditEvent %d: %v", i, err)
		}
		var p deliveryProbe
		if err := pool.QueryRow(ctx, `SELECT seq, row_hash, recorded_at FROM audit_events WHERE id = $1`, ev.ID).
			Scan(&p.seq, &p.rowHash, &p.recordedAt); err != nil {
			t.Fatalf("read appended row %d: %v", i, err)
		}
		out = append(out, p)
	}
	return out
}

// statusOf is the single status row for one destination, failing the test when it is absent.
func statusOf(t *testing.T, st store.PG, dest string) store.AuditDeliveryStatus {
	t.Helper()
	rows, err := st.AuditDeliveryStatuses(context.Background())
	if err != nil {
		t.Fatalf("AuditDeliveryStatuses: %v", err)
	}
	for _, r := range rows {
		if r.Destination == dest {
			return r
		}
	}
	t.Fatalf("no status row for destination %q in %+v", dest, rows)
	return store.AuditDeliveryStatus{}
}

// First enable starts at the head, and every later Ensure is a no-op: an enable (or a restart)
// must never rewind the checkpoint to a head that has since moved, which would resend history
// the collector already holds.
func TestPG_AuditDelivery_EnsureStartsAtHeadAndNeverMoves(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	// A table with no committed rows: the checkpoint starts at the zero position.
	if err := st.EnsureAuditDeliveryCursor(ctx, auditDeliveryDest); err != nil {
		t.Fatalf("EnsureAuditDeliveryCursor on an empty table: %v", err)
	}
	if seq, hash, err := st.GetAuditDeliveryCursor(ctx, auditDeliveryDest); err != nil || seq != 0 || hash != "" {
		t.Fatalf("cursor on an empty table = (%d, %q) err=%v, want (0, \"\")", seq, hash, err)
	}

	// Three events and a deleted row (an operator clearing the checkpoint, or a table reset
	// that lost it): Ensure picks the newest row's seq and hash.
	probes := insertDeliveryProbes(t, pool, 3)
	if _, err := pool.Exec(ctx, `DELETE FROM audit_delivery_cursors WHERE destination = $1`, auditDeliveryDest); err != nil {
		t.Fatalf("delete the checkpoint: %v", err)
	}
	if err := st.EnsureAuditDeliveryCursor(ctx, auditDeliveryDest); err != nil {
		t.Fatalf("EnsureAuditDeliveryCursor with three events: %v", err)
	}
	seq, hash, err := st.GetAuditDeliveryCursor(ctx, auditDeliveryDest)
	if err != nil {
		t.Fatalf("GetAuditDeliveryCursor: %v", err)
	}
	if want := probes[2]; seq != want.seq || hash != want.rowHash {
		t.Fatalf("first enable = (%d, %q), want the third row's (%d, %q)", seq, hash, want.seq, want.rowHash)
	}

	// A fourth event and another Ensure over the row that already exists: the
	// checkpoint does not follow the new head.
	insertDeliveryProbes(t, pool, 1)
	if err := st.EnsureAuditDeliveryCursor(ctx, auditDeliveryDest); err != nil {
		t.Fatalf("EnsureAuditDeliveryCursor with four events: %v", err)
	}
	seq, hash, err = st.GetAuditDeliveryCursor(ctx, auditDeliveryDest)
	if err != nil {
		t.Fatalf("GetAuditDeliveryCursor: %v", err)
	}
	if want := probes[2]; seq != want.seq || hash != want.rowHash {
		t.Fatalf("second enable = (%d, %q), want the still-held (%d, %q)", seq, hash, want.seq, want.rowHash)
	}
}

// Set advances the checkpoint only with a real acceptance: acked_at is stamped, the status
// reports the lag to the head and the oldest undelivered event's age, a reset re-send from the
// zero position counts one, and a collector error halts delivery with its text recorded.
func TestPG_AuditDelivery_SetCountsResetsAndStatusReportsLag(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	if err := st.EnsureAuditDeliveryCursor(ctx, auditDeliveryDest); err != nil {
		t.Fatalf("EnsureAuditDeliveryCursor: %v", err)
	}
	// Four probes: migrate's rolled-back boot canary has already taken seq 1, so these
	// occupy seqs 2..5 and acking the second leaves two.
	probes := insertDeliveryProbes(t, pool, 4)

	// Accept the second row: acked_at is stamped and the status lags the two after it.
	if err := st.SetAuditDeliveryCursor(ctx, auditDeliveryDest, probes[1].seq, probes[1].rowHash); err != nil {
		t.Fatalf("SetAuditDeliveryCursor: %v", err)
	}
	if scalar[bool](t, pool, `SELECT acked_at IS NOT NULL FROM audit_delivery_cursors WHERE destination = $1`, auditDeliveryDest) != true {
		t.Error("acked_at is NULL after an accepted seq")
	}
	status := statusOf(t, st, auditDeliveryDest)
	if status.HeadSeq != probes[3].seq {
		t.Errorf("head = %d, want the newest probe's %d", status.HeadSeq, probes[3].seq)
	}
	if want := probes[3].seq - probes[1].seq; status.HeadSeq-status.AckedSeq != want {
		t.Errorf("lag = head %d - acked %d = %d, want %d", status.HeadSeq, status.AckedSeq, status.HeadSeq-status.AckedSeq, want)
	}
	if status.OldestUndelivered == nil || !status.OldestUndelivered.Equal(probes[2].recordedAt) {
		t.Errorf("oldest undelivered = %v, want the third row's recorded_at %v", status.OldestUndelivered, probes[2].recordedAt)
	}

	// The zero position is a reset: it counts one and leaves acked_at where it was.
	if err := st.SetAuditDeliveryCursor(ctx, auditDeliveryDest, 0, ""); err != nil {
		t.Fatalf("SetAuditDeliveryCursor(0): %v", err)
	}
	if status := statusOf(t, st, auditDeliveryDest); status.Resets != 1 {
		t.Errorf("resets after a re-send from zero = %d, want 1", status.Resets)
	}

	// A collector error halts the loop and records its text.
	if err := st.SetAuditDeliveryError(ctx, auditDeliveryDest, "collector answered HTTP 401", true); err != nil {
		t.Fatalf("SetAuditDeliveryError: %v", err)
	}
	status = statusOf(t, st, auditDeliveryDest)
	if !status.Halted || status.LastError != "collector answered HTTP 401" {
		t.Errorf("status after an error = halted %v, last_error %q, want halted with the collector's answer", status.Halted, status.LastError)
	}
}
