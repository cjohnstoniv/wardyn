// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// TestPG_PruneAWSSSOSpentTokens_DeletesOnlyBeforeCutoff: a spent mark pruned
// early lets a refresh token already known dead grade "renewable" again, so
// the prune must take only rows marked strictly before the cutoff. A throwaway
// database, because the prune is table-wide.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.
func TestPG_PruneAWSSSOSpentTokens_DeletesOnlyBeforeCutoff(t *testing.T) {
	pool := throwawayDatabase(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pg := store.NewPG(pool)

	cutoff := time.Now().UTC().Truncate(time.Second)
	marks := map[string]time.Time{
		"fp-old":       cutoff.Add(-time.Hour),
		"fp-at-cutoff": cutoff,
		"fp-fresh":     cutoff.Add(time.Hour),
	}
	for fp, at := range marks {
		if err := pg.MarkAWSSSOTokenSpent(ctx, fp, "owner@corp.example", at); err != nil {
			t.Fatalf("mark %s: %v", fp, err)
		}
	}

	n, err := pg.PruneAWSSSOSpentTokens(ctx, cutoff)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d rows, want 1", n)
	}
	for fp, wantSpent := range map[string]bool{"fp-old": false, "fp-at-cutoff": true, "fp-fresh": true} {
		spent, err := pg.AWSSSOTokenSpent(ctx, fp)
		if err != nil {
			t.Fatalf("read %s: %v", fp, err)
		}
		if spent != wantSpent {
			t.Errorf("%s (marked %s, cutoff %s): spent = %v after the prune, want %v", fp, marks[fp], cutoff, spent, wantSpent)
		}
	}
}
