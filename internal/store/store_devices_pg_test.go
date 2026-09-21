// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for hybrid enrolment and audit federation (migration
// 0066): device enrolment tokens, the device inventory, and IngestDeviceAudit
// -- the delicate part, whose hash-chain verification only a real database can
// prove (audit_row_hash is a Postgres function; there is nothing to unit test
// against in pure Go).
//
// Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
// Run: WARDYN_TEST_PG=postgres://... go test ./internal/store/ -run TestPG_Devices
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_Devices_EnrolmentToken_ConsumeOnce(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	now := time.Now().UTC()
	tok := uuid.NewString()

	minted, err := st.MintEnrolmentToken(ctx, tok, types.DeviceEnrolmentToken{
		ID: uuid.New(), DeviceName: "alices-laptop", MintedBy: "admin@example.com",
		ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if minted.DeviceName != "alices-laptop" || minted.MintedBy != "admin@example.com" {
		t.Fatalf("minted token round-trip lost fields: %+v", minted)
	}
	if minted.ConsumedAt != nil {
		t.Fatalf("freshly minted token already carries consumed_at: %+v", minted)
	}

	got, ok, err := st.ConsumeEnrolmentToken(ctx, tok, now)
	if err != nil || !ok {
		t.Fatalf("first consume: ok=%v err=%v", ok, err)
	}
	if got.DeviceName != "alices-laptop" {
		t.Fatalf("consumed token lost device_name: %+v", got)
	}

	// Consume-once: the conditional UPDATE already claimed the row.
	if _, ok, err := st.ConsumeEnrolmentToken(ctx, tok, now); ok || err != nil {
		t.Fatalf("second consume: ok=%v err=%v, want false/nil (single-use)", ok, err)
	}
}

func TestPG_Devices_EnrolmentToken_Expiry(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	now := time.Now().UTC()
	tok := uuid.NewString()

	if _, err := st.MintEnrolmentToken(ctx, tok, types.DeviceEnrolmentToken{
		ID: uuid.New(), ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("mint: %v", err)
	}

	if _, ok, err := st.ConsumeEnrolmentToken(ctx, tok, now.Add(2*time.Hour)); ok || err != nil {
		t.Fatalf("expired consume: ok=%v err=%v, want false/nil", ok, err)
	}
}

func TestPG_Devices_CreateGetRevoke(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	raw := uuid.NewString()

	d, err := st.CreateDevice(ctx, types.Device{ID: uuid.New(), Name: "bobs-laptop", EnrolledBy: "admin@example.com"}, raw)
	if err != nil {
		t.Fatalf("create device: %v", err)
	}
	if d.Name != "bobs-laptop" || d.RevokedAt != nil || d.LastSeq != 0 {
		t.Fatalf("created device round-trip: %+v", d)
	}

	got, err := st.GetDeviceByRaw(ctx, raw)
	if err != nil {
		t.Fatalf("get by raw: %v", err)
	}
	if got.ID != d.ID {
		t.Fatalf("got device %s, want %s", got.ID, d.ID)
	}

	// A wrong bearer is indistinguishable from an unknown one.
	if _, err := st.GetDeviceByRaw(ctx, uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get by wrong raw err = %v, want ErrNotFound", err)
	}

	if _, err := st.RevokeDevice(ctx, d.ID, time.Now().UTC()); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// A revoked credential is not an oracle: same ErrNotFound as unknown.
	if _, err := st.GetDeviceByRaw(ctx, raw); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get revoked device by raw err = %v, want ErrNotFound", err)
	}

	// Revoke is idempotent in EFFECT but a second call is ErrNotFound (no
	// second revoke audit row for an act that did not happen).
	if _, err := st.RevokeDevice(ctx, d.ID, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second revoke err = %v, want ErrNotFound", err)
	}
}

// auditRowHash computes what audit_row_hash would produce for one row,
// exactly as the device's own local trigger would have — the fixture builder
// every IngestDeviceAudit test below uses to construct a genuinely LINKED
// claim, rather than a hand-typed hash that happens to look right.
func auditRowHash(t *testing.T, pool *pgxpool.Pool, prev string, ev types.AuditEvent) string {
	t.Helper()
	var want string
	err := pool.QueryRow(context.Background(),
		`SELECT audit_row_hash($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb)`,
		prev, ev.ID, ev.Time, ev.RunID, string(ev.ActorType), ev.Actor,
		ev.Action, ev.Target, ev.Outcome, ev.SourceIP, []byte(ev.Data),
	).Scan(&want)
	if err != nil {
		t.Fatalf("compute audit_row_hash fixture: %v", err)
	}
	return want
}

// maxAuditSeq returns the current tail of audit_events, so a test can scope
// its own ListAuditEventsAfterSeq read to rows it is about to create instead
// of racing every other test in this package for the first 1000 rows in the
// shared database.
func maxAuditSeq(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var seq int64
	if err := pool.QueryRow(context.Background(), `SELECT COALESCE(max(seq),0) FROM audit_events`).Scan(&seq); err != nil {
		t.Fatalf("read max audit seq: %v", err)
	}
	return seq
}

// chainedRow builds one FederatedAuditEvent whose PrevHash/RowHash are
// genuinely linked to prev, at local seq, mirroring what a real device's own
// trigger would have chained.
func chainedRow(t *testing.T, pool *pgxpool.Pool, prev string, seq int64, action string) types.FederatedAuditEvent {
	t.Helper()
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      time.Now().UTC().Truncate(time.Microsecond),
		ActorType: types.ActorSystem,
		Actor:     "device-forwarder",
		Action:    action,
		Outcome:   "success",
		Data:      json.RawMessage(fmt.Sprintf(`{"seq":%d}`, seq)),
		PrevHash:  prev,
	}
	ev.RowHash = auditRowHash(t, pool, prev, ev)
	return types.FederatedAuditEvent{AuditEvent: ev, Seq: seq}
}

func TestPG_Devices_IngestDeviceAudit_LinkedBatchVerifies(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	d, err := st.CreateDevice(ctx, types.Device{ID: uuid.New(), Name: "carol-laptop"}, uuid.NewString())
	if err != nil {
		t.Fatalf("create device: %v", err)
	}

	baseline := maxAuditSeq(t, pool)
	r1 := chainedRow(t, pool, "", 1, "run.create")
	r2 := chainedRow(t, pool, r1.RowHash, 2, "egress.deny")
	r3 := chainedRow(t, pool, r2.RowHash, 3, "credential.mint")

	result, err := st.IngestDeviceAudit(ctx, d.ID, []types.FederatedAuditEvent{r1, r2, r3})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.Accepted != 3 || result.Reset {
		t.Fatalf("ingest result = %+v, want Accepted=3 Reset=false", result)
	}

	after, err := st.ListDevices(ctx)
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	var gotCursor types.Device
	for _, dv := range after {
		if dv.ID == d.ID {
			gotCursor = dv
		}
	}
	if gotCursor.LastSeq != 3 || gotCursor.LastRowHash != r3.RowHash {
		t.Errorf("device cursor after ingest = seq=%d hash=%q, want seq=3 hash=%q",
			gotCursor.LastSeq, gotCursor.LastRowHash, r3.RowHash)
	}

	// The rows landed as the ORGANISATION's own chained audit_events, each
	// carrying device_origin provenance, not the device's own claimed hashes.
	fed, err := st.ListAuditEventsAfterSeq(ctx, baseline, 1000)
	if err != nil {
		t.Fatalf("list audit events: %v", err)
	}
	found := 0
	for _, e := range fed {
		if e.ID == r1.ID || e.ID == r2.ID || e.ID == r3.ID {
			found++
			var data map[string]any
			if err := json.Unmarshal(e.Data, &data); err != nil {
				t.Fatalf("unmarshal inserted row data: %v", err)
			}
			origin, ok := data["device_origin"].(map[string]any)
			if !ok {
				t.Fatalf("inserted row %s missing device_origin: %v", e.ID, data)
			}
			if origin["device_id"] != d.ID.String() {
				t.Errorf("device_origin.device_id = %v, want %s", origin["device_id"], d.ID)
			}
		}
	}
	if found != 3 {
		t.Errorf("found %d of 3 ingested rows in audit_events", found)
	}

	// Idempotent retry: re-submitting the SAME already-ingested batch accepts
	// nothing new and does not error.
	retry, err := st.IngestDeviceAudit(ctx, d.ID, []types.FederatedAuditEvent{r1, r2, r3})
	if err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if retry.Accepted != 0 {
		t.Errorf("idempotent retry accepted %d rows, want 0", retry.Accepted)
	}
}

func TestPG_Devices_IngestDeviceAudit_EditedDataRefused(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	d, err := st.CreateDevice(ctx, types.Device{ID: uuid.New(), Name: "dave-laptop"}, uuid.NewString())
	if err != nil {
		t.Fatalf("create device: %v", err)
	}

	baseline := maxAuditSeq(t, pool)
	r1 := chainedRow(t, pool, "", 1, "run.create")
	r2 := chainedRow(t, pool, r1.RowHash, 2, "egress.deny")
	// Tamper with r2's data AFTER its RowHash was computed over the original —
	// exactly what an edited-in-flight or edited-at-rest row looks like.
	r2.Data = json.RawMessage(`{"seq":99}`)

	_, err = st.IngestDeviceAudit(ctx, d.ID, []types.FederatedAuditEvent{r1, r2})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("ingest edited batch err = %v, want ErrConflict", err)
	}

	// The whole batch refused: the device's cursor must not have moved.
	devices, err := st.ListDevices(ctx)
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	for _, dv := range devices {
		if dv.ID == d.ID && dv.LastSeq != 0 {
			t.Errorf("device cursor advanced to seq=%d despite the refused batch, want 0", dv.LastSeq)
		}
	}

	// ...and r1 must NOT have been partially inserted either — the batch is
	// one transaction, not a verified prefix plus a refusal.
	fed, err := st.ListAuditEventsAfterSeq(ctx, baseline, 1000)
	if err != nil {
		t.Fatalf("list audit events: %v", err)
	}
	for _, e := range fed {
		if e.ID == r1.ID {
			t.Errorf("row %s from the refused batch was inserted anyway — the batch is not atomic", e.ID)
		}
	}
}

func TestPG_Devices_IngestDeviceAudit_GenesisAfterReset(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	d, err := st.CreateDevice(ctx, types.Device{ID: uuid.New(), Name: "erin-laptop"}, uuid.NewString())
	if err != nil {
		t.Fatalf("create device: %v", err)
	}

	// First chain: establishes a recorded cursor.
	r1 := chainedRow(t, pool, "", 1, "run.create")
	r2 := chainedRow(t, pool, r1.RowHash, 2, "egress.deny")
	if _, err := st.IngestDeviceAudit(ctx, d.ID, []types.FederatedAuditEvent{r1, r2}); err != nil {
		t.Fatalf("ingest first chain: %v", err)
	}

	// The device's local table was purged and its forwarder starts a fresh
	// chain: a genesis row (empty PrevHash) at a NEW local seq, with no
	// relation to r1/r2's hashes.
	g1 := chainedRow(t, pool, "", 5, "run.create")

	result, err := st.IngestDeviceAudit(ctx, d.ID, []types.FederatedAuditEvent{g1})
	if err != nil {
		t.Fatalf("ingest genesis-after-reset: %v", err)
	}
	if !result.Reset {
		t.Errorf("ingest result = %+v, want Reset=true", result)
	}
	if result.Accepted != 1 {
		t.Errorf("ingest result = %+v, want Accepted=1", result)
	}

	devices, err := st.ListDevices(ctx)
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	for _, dv := range devices {
		if dv.ID == d.ID {
			if dv.LastSeq != 5 || dv.LastRowHash != g1.RowHash {
				t.Errorf("device cursor after reset = seq=%d hash=%q, want seq=5 hash=%q",
					dv.LastSeq, dv.LastRowHash, g1.RowHash)
			}
		}
	}
}
