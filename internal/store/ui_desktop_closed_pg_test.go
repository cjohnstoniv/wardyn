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

// A Close upserts, reads as closed only while it has not expired, and the prune takes only the
// expired ones.
func TestPG_UIDesktopClosed_CloseReadPrune(t *testing.T) {
	pool := throwawayDatabase(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pg := store.NewPG(pool)
	now := time.Now().UTC().Truncate(time.Second)

	for key, exp := range map[string]time.Time{"k-expired": now.Add(-time.Minute), "k-at-now": now, "k-open": now.Add(time.Hour)} {
		if err := pg.CloseUIDesktop(ctx, key, exp); err != nil {
			t.Fatalf("close %s: %v", key, err)
		}
	}
	if err := pg.CloseUIDesktop(ctx, "k-expired", now.Add(-2*time.Minute)); err != nil {
		t.Errorf("closing again: %v, want an upsert", err)
	}
	for key, want := range map[string]bool{"k-expired": false, "k-at-now": false, "k-open": true, "k-never": false} {
		if closed, err := pg.UIDesktopClosed(ctx, key, now); err != nil || closed != want {
			t.Errorf("%s closed = %v (%v), want %v", key, closed, err, want)
		}
	}
	n, err := pg.PruneUIDesktopClosed(ctx, now)
	if err != nil || n != 2 {
		t.Fatalf("prune = %d, %v, want 2 (k-expired, k-at-now)", n, err)
	}
	if closed, err := pg.UIDesktopClosed(ctx, "k-open", now); err != nil || !closed {
		t.Errorf("k-open after the prune = %v (%v), want closed", closed, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ui_desktop_closed`).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("rows after the prune = %d (%v), want 1", rows, err)
	}
}
