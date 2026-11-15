// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// A sign-in's sid row upserts, lookup finds it only under its own issuer and leaves it, forget
// deletes it, and the prune takes only rows whose session expired before now: a row for a session
// that lasts longer than a day survives the prune until that session's own expiry.
func TestPG_OIDCSessionSIDs_RecordLookupForgetPrune(t *testing.T) {
	pool := throwawayDatabase(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pg := store.NewPG(pool)
	now := time.Now().UTC().Truncate(time.Second)
	const iss = "https://idp.example"

	for sid, until := range map[string]time.Time{"sid-ended": now.Add(-time.Hour), "sid-30h": now.Add(30 * time.Hour)} {
		if err := pg.RecordOIDCSessionSID(ctx, iss, sid, "first", until); err != nil {
			t.Fatalf("record %s: %v", sid, err)
		}
	}
	if err := pg.RecordOIDCSessionSID(ctx, iss, "sid-take", "pat", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := pg.RecordOIDCSessionSID(ctx, iss, "sid-take", "pairwise-1", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if sub, _, ok, err := pg.LookupOIDCSessionSID(ctx, "https://other.example", "sid-take"); err != nil || ok {
		t.Errorf("lookup under another issuer = (%q, %v, %v), want no row", sub, ok, err)
	}
	var version string
	for range 2 {
		sub, v, ok, err := pg.LookupOIDCSessionSID(ctx, iss, "sid-take")
		if err != nil || !ok || sub != "pairwise-1" {
			t.Errorf("lookup = (%q, %v, %v), want the re-recorded person, row kept", sub, ok, err)
		}
		version = v
	}
	if err := pg.ForgetOIDCSessionSID(ctx, iss, "sid-take", version); err != nil {
		t.Fatal(err)
	}
	if sub, _, ok, err := pg.LookupOIDCSessionSID(ctx, iss, "sid-take"); err != nil || ok {
		t.Errorf("lookup after forget = (%q, %v, %v), want the row gone", sub, ok, err)
	}
	n, err := pg.PruneOIDCSessionSIDs(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("prune = %d, %v, want 1 (sid-ended)", n, err)
	}
	for sid, want := range map[string]bool{"sid-ended": false, "sid-30h": true} {
		if _, _, ok, err := pg.LookupOIDCSessionSID(ctx, iss, sid); err != nil || ok != want {
			t.Errorf("%s present = %v (%v), want %v", sid, ok, err, want)
		}
	}
	if n, err := pg.PruneOIDCSessionSIDs(ctx, now.Add(31*time.Hour)); err != nil || n != 1 {
		t.Errorf("prune past the session's expiry = %d, %v, want 1 (sid-30h)", n, err)
	}
}

// A row whose session has ended, not yet pruned, looks up as no row, so the front-channel logout that
// names its sid cuts none of that person's sessions.
func TestPG_OIDCSessionSIDs_ExpiredRowLooksUpAsNone(t *testing.T) {
	pool := throwawayDatabase(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pg := store.NewPG(pool)
	const iss = "https://idp.example"
	if err := pg.RecordOIDCSessionSID(ctx, iss, "sid-ended", "first", time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if sub, _, ok, err := pg.LookupOIDCSessionSID(ctx, iss, "sid-ended"); err != nil || ok {
		t.Errorf("lookup of an ended session's row = (%q, %v, %v), want no row", sub, ok, err)
	}
}

// Two sign-ins of one person under one sid that record out of order leave the later end, another
// person on the sid takes the row with its own end, and a forget deletes only the row version its
// lookup read: a sign-in that recorded the sid again since keeps its row.
func TestPG_OIDCSessionSIDs_KeepTheLaterEndAndForgetOnlyTheRowItRead(t *testing.T) {
	pool := throwawayDatabase(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pg := store.NewPG(pool)
	now := time.Now().UTC().Truncate(time.Second)
	const iss = "https://idp.example"

	for _, until := range []time.Time{now.Add(2 * time.Hour), now.Add(time.Hour)} { // the newer sign-in records first
		if err := pg.RecordOIDCSessionSID(ctx, iss, "sid-tabs", "pat", until); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := pg.PruneOIDCSessionSIDs(ctx, now.Add(90*time.Minute)); err != nil || n != 0 {
		t.Fatalf("prune between the two ends = %d, %v; want 0: the row lives until the later end", n, err)
	}
	if err := pg.RecordOIDCSessionSID(ctx, iss, "sid-tabs", "sam", now.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n, err := pg.PruneOIDCSessionSIDs(ctx, now.Add(45*time.Minute)); err != nil || n != 1 {
		t.Fatalf("prune past the other person's end = %d, %v; want 1: their row keeps their own end", n, err)
	}

	if err := pg.RecordOIDCSessionSID(ctx, iss, "sid-forget", "pat", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, read, ok, err := pg.LookupOIDCSessionSID(ctx, iss, "sid-forget")
	if err != nil || !ok {
		t.Fatalf("lookup = %v, %v; want the row", ok, err)
	}
	if err := pg.RecordOIDCSessionSID(ctx, iss, "sid-forget", "pat", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := pg.ForgetOIDCSessionSID(ctx, iss, "sid-forget", read); err != nil {
		t.Fatal(err)
	}
	sub, again, ok, err := pg.LookupOIDCSessionSID(ctx, iss, "sid-forget")
	if err != nil || !ok || sub != "pat" || again == read {
		t.Fatalf("after a forget of the earlier version = (%q, %q, %v, %v); want the re-recorded row kept at a new version", sub, again, ok, err)
	}
	if err := pg.ForgetOIDCSessionSID(ctx, iss, "sid-forget", again); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := pg.LookupOIDCSessionSID(ctx, iss, "sid-forget"); err != nil || ok {
		t.Errorf("after a forget of the version read = %v, %v; want the row gone", ok, err)
	}
}
