// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// A sweep skips an ended grant another transaction holds instead of waiting on it, and deletes at most one
// batch of the rest.
func TestPG_SweepExpiredPortalGrantsSkipsHeldRowsAndStopsAtTheBatch(t *testing.T) {
	st, pool := isolatedStore(t)
	p := newIssuerPortal(t, st, "")
	ctx := context.Background()

	// (a) The lowest-id ended grant is held by an open transaction: the sweep deletes the other two and returns.
	var ids []uuid.UUID
	for range 3 {
		g, _, _ := mintFor(t, st, p, "alice")
		ids = append(ids, g.ID)
	}
	expireGrants(t, st, ids...)
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM portal_grants WHERE id = $1 FOR UPDATE`, ids[0]); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	sweepCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	n, err := st.SweepExpiredPortalGrants(sweepCtx, time.Now())
	cancel()
	_ = tx.Rollback(ctx)
	if err != nil || n != 2 {
		t.Fatalf("sweep with a held row = %d, %v; want 2, nil", n, err)
	}
	if left := rowCount(t, pool, `SELECT count(*) FROM portal_grants WHERE id = $1`, ids[0]); left != 1 {
		t.Fatalf("the held grant was deleted")
	}

	// (b) A sweep deletes at most one batch.
	defer store.SetPortalGrantSweepBatchForTest(2)()
	var more []uuid.UUID
	for range 5 {
		g, _, _ := mintFor(t, st, p, "bob")
		more = append(more, g.ID)
	}
	expireGrants(t, st, more...)
	for i := 1; i <= 2; i++ {
		if n, err := st.SweepExpiredPortalGrants(ctx, time.Now()); err != nil || n != 2 {
			t.Fatalf("sweep %d = %d, %v; want 2, nil", i, n, err)
		}
	}
}
