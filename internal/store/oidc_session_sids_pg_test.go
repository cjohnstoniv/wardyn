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

// A sign-in's sid row upserts, a logout takes it exactly once and only under its own issuer, and the
// prune takes only rows signed in before the cutoff.
func TestPG_OIDCSessionSIDs_RecordTakePrune(t *testing.T) {
	pool := throwawayDatabase(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pg := store.NewPG(pool)
	cutoff := time.Now().UTC().Truncate(time.Second)
	const iss = "https://idp.example"

	for sid, at := range map[string]time.Time{"sid-old": cutoff.Add(-time.Hour), "sid-fresh": cutoff.Add(time.Hour)} {
		if err := pg.RecordOIDCSessionSID(ctx, iss, sid, "first", at); err != nil {
			t.Fatalf("record %s: %v", sid, err)
		}
	}
	if err := pg.RecordOIDCSessionSID(ctx, iss, "sid-take", "pat", cutoff); err != nil {
		t.Fatal(err)
	}
	if err := pg.RecordOIDCSessionSID(ctx, iss, "sid-take", "pairwise-1", cutoff); err != nil {
		t.Fatal(err)
	}
	if sub, ok, err := pg.TakeOIDCSessionSID(ctx, "https://other.example", "sid-take"); err != nil || ok {
		t.Errorf("take under another issuer = (%q, %v, %v), want no row", sub, ok, err)
	}
	if sub, ok, err := pg.TakeOIDCSessionSID(ctx, iss, "sid-take"); err != nil || !ok || sub != "pairwise-1" {
		t.Errorf("take = (%q, %v, %v), want the re-recorded person", sub, ok, err)
	}
	if sub, ok, err := pg.TakeOIDCSessionSID(ctx, iss, "sid-take"); err != nil || ok {
		t.Errorf("second take = (%q, %v, %v), want the row gone", sub, ok, err)
	}
	n, err := pg.PruneOIDCSessionSIDs(ctx, cutoff)
	if err != nil || n != 1 {
		t.Fatalf("prune = %d, %v, want 1 (sid-old)", n, err)
	}
	for sid, want := range map[string]bool{"sid-old": false, "sid-fresh": true} {
		if _, ok, err := pg.TakeOIDCSessionSID(ctx, iss, sid); err != nil || ok != want {
			t.Errorf("%s present = %v (%v), want %v", sid, ok, err, want)
		}
	}
}
