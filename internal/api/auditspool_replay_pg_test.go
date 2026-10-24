// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// A spooled line whose event already landed (its write committed after the caller gave up and spooled
// it) replays without a second row.
func TestPG_AuditSpool_ReplayOfALandedEventAddsNoRow(t *testing.T) {
	ctx := context.Background()
	pool := throwawayPGPool(t)
	sp, err := NewAuditSpool(filepath.Join(t.TempDir(), "audit-spool.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ev := newTestEvent("test.spool.replay")
	ev.Time = time.Now().UTC()
	landed := ev
	if err := store.InsertAuditEvent(ctx, pool, &landed); err != nil {
		t.Fatal(err)
	}
	if err := sp.Append(ev); err != nil {
		t.Fatal(err)
	}
	if n, err := sp.Drain(ctx, store.Recorder{Pool: pool}, 10); err != nil || n != 1 {
		t.Fatalf("drain = %d, %v; want the line replayed", n, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE id = $1`, ev.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("%d rows for an event that landed before its spooled line replayed, want 1", rows)
	}
}
