// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-gated coverage for ListApprovalsKeysetByRun, the per-run keyset read behind the injection
// doors' approval scans. Guarded by WARDYN_TEST_PG.
package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A walk two rows at a time returns exactly one run's rows of the asked kinds, newest first, each
// once, across a requested_at tie; another run's rows and the other kinds never appear.
func TestPG_ListApprovalsKeysetByRun(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	other := persistRun(t, ctx, pool, newRun(types.RunRunning))
	base := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)

	n := 0
	seed := func(runID uuid.UUID, kind types.ApprovalKind, at time.Time) uuid.UUID {
		t.Helper()
		n++ // a distinct scope each time: an identical pending request is refused as a duplicate
		ap, err := pg.CreateApproval(ctx, types.ApprovalRequest{
			ID: uuid.New(), RunID: runID, Kind: kind, RequestedScope: []byte(fmt.Sprintf(`{"n":%d}`, n)),
			State: types.ApprovalPending, RequestedAt: at,
		})
		if err != nil {
			t.Fatalf("create approval: %v", err)
		}
		return ap.ID
	}
	var want []uuid.UUID
	for i := range 5 {
		at := base.Add(time.Duration(i/2) * time.Second) // rows 0-1, 2-3 share a requested_at
		kind := types.ApprovalToolCall
		if i%2 == 1 {
			kind = types.ApprovalCredentialReauth
		}
		want = append(want, seed(run.ID, kind, at))
		seed(run.ID, types.ApprovalEgressDomain, at)
		seed(other.ID, kind, at)
	}

	var got []uuid.UUID
	var afterAt time.Time
	var afterID uuid.UUID
	kinds := []types.ApprovalKind{types.ApprovalToolCall, types.ApprovalCredentialReauth}
	for range 10 {
		page, err := pg.ListApprovalsKeysetByRun(ctx, run.ID, "", kinds, afterAt, afterID, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range page {
			got = append(got, a.ID)
		}
		if len(page) < 2 {
			break
		}
		last := page[len(page)-1]
		afterAt, afterID = last.RequestedAt, last.ID
	}
	if len(got) != len(want) {
		t.Fatalf("walk read %d rows, want %d", len(got), len(want))
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range got {
		seen[id] = true
	}
	for _, id := range want {
		if !seen[id] {
			t.Errorf("row %s never read", id)
		}
	}
	if len(seen) != len(got) {
		t.Errorf("a row was read twice: %v", got)
	}
}
