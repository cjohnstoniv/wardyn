// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-gated coverage for ListDecidedAlwaysEgressPage, the heal's narrow
// read of decided `always` egress verdicts. Guarded by WARDYN_TEST_PG.
package store_test

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_ListDecidedAlwaysEgressPage seeds, through the store, run-scoped
// denials with large reasons, an always-scoped non-egress decision, a pending
// egress request, and always-scoped egress verdicts in both directions (two
// sharing a requested_at). A walk two rows at a time returns exactly the
// always-scoped egress verdicts of each state, newest request first, each once,
// with no reason or other unread column filled in.
func TestPG_ListDecidedAlwaysEgressPage(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	base := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)

	seed := func(kind types.ApprovalKind, host string, requestedAt time.Time, state types.ApprovalState, scope types.ApprovalScope, reason string) uuid.UUID {
		t.Helper()
		ap, err := pg.CreateApproval(ctx, types.ApprovalRequest{
			ID: uuid.New(), RunID: run.ID, Kind: kind,
			RequestedScope: []byte(`{"host":"` + host + `"}`),
			State:          types.ApprovalPending, RequestedAt: requestedAt,
		})
		if err != nil {
			t.Fatalf("create approval: %v", err)
		}
		if state != types.ApprovalPending {
			if _, err := pg.DecideApproval(ctx, ap.ID, types.ApprovalDecision{
				State: state, DecidedBy: "op@example.com", Reason: reason, Scope: scope,
			}); err != nil {
				t.Fatalf("decide approval: %v", err)
			}
		}
		return ap.ID
	}

	large := strings.Repeat("r", 256<<10)
	for i := range 8 {
		seed(types.ApprovalEgressDomain, "run"+string(rune('a'+i))+".example.com", base.Add(time.Duration(i)*time.Second), types.ApprovalDenied, types.ScopeRun, large)
	}
	seed(types.ApprovalToolCall, "tool.example.com", base, types.ApprovalApproved, types.ScopeAlways, large)
	seed(types.ApprovalEgressDomain, "pending.example.com", base, types.ApprovalPending, "", "")

	want := map[types.ApprovalState][]uuid.UUID{}
	for i, host := range []string{"a1", "a2", "a3", "a4", "a5"} {
		at := base.Add(time.Duration(i) * time.Minute)
		if i == 4 {
			at = base.Add(3 * time.Minute) // shares a4's requested_at
		}
		want[types.ApprovalApproved] = append(want[types.ApprovalApproved], seed(types.ApprovalEgressDomain, host+".example.com", at, types.ApprovalApproved, types.ScopeAlways, "always"))
	}
	for i, host := range []string{"d1", "d2", "d3"} {
		want[types.ApprovalDenied] = append(want[types.ApprovalDenied], seed(types.ApprovalEgressDomain, host+".example.com", base.Add(time.Duration(i)*time.Minute), types.ApprovalDenied, types.ScopeAlways, "always"))
	}

	for _, state := range []types.ApprovalState{types.ApprovalApproved, types.ApprovalDenied} {
		var got []types.ApprovalRequest
		var afterAt time.Time
		afterID := uuid.Nil
		for pages := 0; ; pages++ {
			if pages > 10 {
				t.Fatalf("%s: the keyset walk did not end", state)
			}
			page, err := pg.ListDecidedAlwaysEgressPage(ctx, state, afterAt, afterID, 2)
			if err != nil {
				t.Fatalf("%s page: %v", state, err)
			}
			if len(page) > 2 {
				t.Fatalf("%s page of %d rows, limit 2", state, len(page))
			}
			if len(page) == 0 {
				break
			}
			got = append(got, page...)
			afterAt, afterID = page[len(page)-1].RequestedAt, page[len(page)-1].ID
		}
		var ids []uuid.UUID
		for i, ap := range got {
			ids = append(ids, ap.ID)
			if ap.Kind != types.ApprovalEgressDomain || ap.DecisionScope != types.ScopeAlways || ap.State != state ||
				ap.RunID != run.ID || ap.DecidedAt == nil || len(ap.RequestedScope) == 0 {
				t.Fatalf("%s row %d: %+v is not a decided always-scoped egress verdict with its projection filled", state, i, ap)
			}
			if ap.Reason != "" || ap.DecidedBy != "" || ap.GrantID != nil || ap.MintedJTI != "" || ap.ExpiresAt != nil {
				t.Fatalf("%s row %d: the projection read a column the heal does not use: %+v", state, i, ap)
			}
			if i > 0 {
				prev := got[i-1]
				if c := ap.RequestedAt.Compare(prev.RequestedAt); c > 0 || (c == 0 && bytes.Compare(ap.ID[:], prev.ID[:]) >= 0) {
					t.Fatalf("%s rows %d and %d are not newest request first", state, i-1, i)
				}
			}
		}
		slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
		w := slices.Clone(want[state])
		slices.SortFunc(w, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
		if !slices.Equal(ids, w) {
			t.Fatalf("%s: walked %d rows %v, want exactly the %d always-scoped egress verdicts %v", state, len(ids), ids, len(w), w)
		}
	}
}

// TestPG_ListDecidedAlwaysEgressPageSplitsATie puts a page boundary inside a
// requested_at tie: three verdicts share one request time and are walked two at
// a time, so the second page starts between tied rows. The keyset must carry the
// id across that boundary, or the third row is skipped.
func TestPG_ListDecidedAlwaysEgressPageSplitsATie(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	tied := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)

	var want []uuid.UUID
	for _, host := range []string{"t1", "t2", "t3"} {
		ap, err := pg.CreateApproval(ctx, types.ApprovalRequest{
			ID: uuid.New(), RunID: run.ID, Kind: types.ApprovalEgressDomain,
			RequestedScope: []byte(`{"host":"` + host + `.example.com"}`),
			State:          types.ApprovalPending, RequestedAt: tied,
		})
		if err != nil {
			t.Fatalf("create approval: %v", err)
		}
		if _, err := pg.DecideApproval(ctx, ap.ID, types.ApprovalDecision{
			State: types.ApprovalDenied, DecidedBy: "op@example.com", Scope: types.ScopeAlways,
		}); err != nil {
			t.Fatalf("decide approval: %v", err)
		}
		want = append(want, ap.ID)
	}

	var got []uuid.UUID
	var afterAt time.Time
	afterID := uuid.Nil
	var sizes []int
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("the keyset walk did not end")
		}
		page, err := pg.ListDecidedAlwaysEgressPage(ctx, types.ApprovalDenied, afterAt, afterID, 2)
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		sizes = append(sizes, len(page))
		if len(page) == 0 {
			break
		}
		for _, ap := range page {
			got = append(got, ap.ID)
		}
		afterAt, afterID = page[len(page)-1].RequestedAt, page[len(page)-1].ID
	}
	if !slices.Equal(sizes, []int{2, 1, 0}) {
		t.Fatalf("page sizes = %v, want [2 1 0]: the second page must start inside the tie", sizes)
	}
	slices.SortFunc(got, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	slices.SortFunc(want, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	if !slices.Equal(got, want) {
		t.Fatalf("walked %v, want each of the three tied verdicts once: %v", got, want)
	}
}
