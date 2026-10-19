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

// A refused mark upserts (a later refusal replaces the detail and the age), clears, and the prune
// takes only marks older than the cutoff.
func TestPG_AWSSSORefusedTokens_MarkClearPrune(t *testing.T) {
	pool := throwawayDatabase(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pg := store.NewPG(pool)
	cutoff := time.Now().UTC().Truncate(time.Second)

	for fp, at := range map[string]time.Time{"fp-old": cutoff.Add(-time.Hour), "fp-at-cutoff": cutoff, "fp-fresh": cutoff.Add(time.Hour)} {
		if err := pg.MarkAWSSSOTokenRefused(ctx, fp, "owner", "invalid_request", at); err != nil {
			t.Fatalf("mark %s: %v", fp, err)
		}
	}
	if err := pg.MarkAWSSSOTokenRefused(ctx, "fp-fresh", "owner", "access_denied: x", cutoff.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if detail, ok, err := pg.AWSSSOTokenRefused(ctx, "fp-fresh"); err != nil || !ok || detail != "access_denied: x" {
		t.Errorf("re-marked = (%q, %v, %v), want the newer detail", detail, ok, err)
	}
	if err := pg.ClearAWSSSOTokenRefused(ctx, "fp-at-cutoff"); err != nil {
		t.Fatal(err)
	}
	if err := pg.ClearAWSSSOTokenRefused(ctx, "never-marked"); err != nil {
		t.Errorf("clearing an unmarked token: %v, want none", err)
	}
	n, err := pg.PruneAWSSSORefusedTokens(ctx, cutoff)
	if err != nil || n != 1 {
		t.Fatalf("prune = %d, %v, want 1 (fp-old)", n, err)
	}
	for fp, want := range map[string]bool{"fp-old": false, "fp-at-cutoff": false, "fp-fresh": true} {
		if _, ok, err := pg.AWSSSOTokenRefused(ctx, fp); err != nil || ok != want {
			t.Errorf("%s marked = %v (%v), want %v", fp, ok, err, want)
		}
	}
}
