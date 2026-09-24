// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// A forwarded row must never read as one of the organisation's own, and a
// minted enrolment token must be cancellable before it is redeemed. Guarded by
// WARDYN_TEST_PG like the rest of this package's Postgres tests.
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A laptop claiming to be the organisation's admin is stored under its own
// device prefix: a filter on the admin's principal finds nothing, the row
// reads back with the forwarding device's id, ?origin= puts it on the device
// side and a genuine organisation row on the other, and the device's claim
// still re-checks from the stored row.
func TestPG_Devices_ForwardedRowNeverReadsAsAnOrgPrincipal(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	d := federationDevice(t, st)
	action := "governance.profile.update." + uuid.NewString()
	const admin = "admin@org.example"

	ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond),
		ActorType: types.ActorHuman, Actor: admin, Action: action, Target: "profile:default", Outcome: "success",
		Data: json.RawMessage(`{"k":1}`)}
	ev.RowHash = auditRowHash(t, pool, "", ev)
	if _, err := st.IngestDeviceAudit(ctx, d.ID, testPeer, []types.FederatedAuditEvent{{AuditEvent: ev, Seq: 1}}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	// The organisation's own row for the same action, by the real admin, with
	// a device_origin key of its own in data: one mark alone is not a device's.
	org := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman, Actor: admin,
		Action: action, Outcome: "success", Data: json.RawMessage(`{"device_origin":{"device_id":"` + d.ID.String() + `"}}`)}
	if err := store.InsertAuditEvent(ctx, pool, &org); err != nil {
		t.Fatal(err)
	}

	page := store.Page{Limit: 10}
	query := func(f store.AuditFilter) []types.AuditEvent {
		t.Helper()
		f.Action = action
		got, err := st.QueryAuditEventsFilteredPage(ctx, nil, f, page)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := query(store.AuditFilter{Actor: admin}); len(got) != 1 || got[0].ID != org.ID {
		t.Fatalf("?actor=%s = %+v, want only the organisation's own row", admin, got)
	}
	dev := query(store.AuditFilter{Origin: store.AuditOriginDevice})
	if len(dev) != 1 || dev[0].ID != ev.ID {
		t.Fatalf("?origin=device = %+v, want only the forwarded row", dev)
	}
	if dev[0].Actor != store.FederatedActor(d.ID, admin) || dev[0].DeviceID == nil || *dev[0].DeviceID != d.ID {
		t.Fatalf("forwarded row reads as actor %q device_id %v, want %q and %s", dev[0].Actor, dev[0].DeviceID, store.FederatedActor(d.ID, admin), d.ID)
	}
	orgRows := query(store.AuditFilter{Origin: store.AuditOriginOrganisation})
	if len(orgRows) != 1 || orgRows[0].ID != org.ID || orgRows[0].DeviceID != nil {
		t.Fatalf("?origin=organisation = %+v, want only the organisation's row, with no device_id", orgRows)
	}
	// The Go predicate (the fetch-all fallback) agrees with the SQL one.
	for _, e := range append(dev, orgRows...) {
		for _, o := range []string{store.AuditOriginDevice, store.AuditOriginOrganisation} {
			f := store.AuditFilter{Origin: o}
			want := slices.ContainsFunc(query(f), func(g types.AuditEvent) bool { return g.ID == e.ID })
			if f.Matches(e) != want {
				t.Errorf("row %s origin=%s: Matches = %v, SQL = %v", e.ID, o, !want, want)
			}
		}
	}

	var claimOK bool
	if err := pool.QueryRow(ctx, `SELECT `+store.FederatedClaimHashSQL+` = e.data->'device_origin'->>'row_hash'
		FROM audit_events e WHERE e.id = $1`, ev.ID).Scan(&claimOK); err != nil || !claimOK {
		t.Fatalf("the device's claim does not re-check from the stored row: ok=%v err=%v", claimOK, err)
	}
	if status, err := st.VerifyAuditChain(ctx); err != nil || !status.OK {
		t.Fatalf("organisation chain verify: %+v %v", status, err)
	}
}

// Only a still-redeemable token is listed and revocable; a revoked one can no
// longer be redeemed, and a second revoke is ErrNotFound (no second audit row).
func TestPG_Devices_EnrolmentToken_ListAndRevoke(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	now := time.Now().UTC()
	mint := func(name string, ttl time.Duration) (types.DeviceEnrolmentToken, string) {
		t.Helper()
		raw := uuid.NewString()
		tok, err := st.MintEnrolmentToken(ctx, raw, types.DeviceEnrolmentToken{ID: uuid.New(), DeviceName: name,
			MintedBy: "admin@org.example", ExpiresAt: now.Add(ttl)})
		if err != nil {
			t.Fatal(err)
		}
		return tok, raw
	}
	pending, pendingRaw := mint("pending", time.Hour)
	redeemed, redeemedRaw := mint("redeemed", time.Hour)
	expired, _ := mint("expired", -time.Minute)
	if _, ok, err := st.ConsumeEnrolmentToken(ctx, redeemedRaw, now); !ok || err != nil {
		t.Fatalf("consume: %v %v", ok, err)
	}
	listed := func() []uuid.UUID {
		t.Helper()
		got, err := st.ListEnrolmentTokens(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		var ids []uuid.UUID
		for _, g := range got {
			if slices.Contains([]uuid.UUID{pending.ID, redeemed.ID, expired.ID}, g.ID) {
				ids = append(ids, g.ID)
			}
		}
		return ids
	}
	if got := listed(); !slices.Equal(got, []uuid.UUID{pending.ID}) {
		t.Fatalf("listed = %v, want only the pending token %s", got, pending.ID)
	}
	for _, id := range []uuid.UUID{redeemed.ID, expired.ID, uuid.New()} {
		if _, err := st.RevokeEnrolmentToken(ctx, id, now); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("revoke %s: err = %v, want ErrNotFound", id, err)
		}
	}
	got, err := st.RevokeEnrolmentToken(ctx, pending.ID, now)
	if err != nil || got.ID != pending.ID || got.ConsumedAt == nil {
		t.Fatalf("revoke pending: %+v %v", got, err)
	}
	if _, err := st.RevokeEnrolmentToken(ctx, pending.ID, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second revoke: err = %v, want ErrNotFound", err)
	}
	if _, ok, err := st.ConsumeEnrolmentToken(ctx, pendingRaw, now); ok || err != nil {
		t.Fatalf("redeeming a revoked token: ok=%v err=%v, want false/nil", ok, err)
	}
	if got := listed(); len(got) != 0 {
		t.Fatalf("listed after revoke = %v, want none", got)
	}
}
