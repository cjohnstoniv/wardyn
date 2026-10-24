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
	if sub, ok, err := pg.LookupOIDCSessionSID(ctx, "https://other.example", "sid-take"); err != nil || ok {
		t.Errorf("lookup under another issuer = (%q, %v, %v), want no row", sub, ok, err)
	}
	for range 2 {
		if sub, ok, err := pg.LookupOIDCSessionSID(ctx, iss, "sid-take"); err != nil || !ok || sub != "pairwise-1" {
			t.Errorf("lookup = (%q, %v, %v), want the re-recorded person, row kept", sub, ok, err)
		}
	}
	if err := pg.ForgetOIDCSessionSID(ctx, iss, "sid-take"); err != nil {
		t.Fatal(err)
	}
	if sub, ok, err := pg.LookupOIDCSessionSID(ctx, iss, "sid-take"); err != nil || ok {
		t.Errorf("lookup after forget = (%q, %v, %v), want the row gone", sub, ok, err)
	}
	n, err := pg.PruneOIDCSessionSIDs(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("prune = %d, %v, want 1 (sid-ended)", n, err)
	}
	for sid, want := range map[string]bool{"sid-ended": false, "sid-30h": true} {
		if _, ok, err := pg.LookupOIDCSessionSID(ctx, iss, sid); err != nil || ok != want {
			t.Errorf("%s present = %v (%v), want %v", sid, ok, err, want)
		}
	}
	if n, err := pg.PruneOIDCSessionSIDs(ctx, now.Add(31*time.Hour)); err != nil || n != 1 {
		t.Errorf("prune past the session's expiry = %d, %v, want 1 (sid-30h)", n, err)
	}
}
