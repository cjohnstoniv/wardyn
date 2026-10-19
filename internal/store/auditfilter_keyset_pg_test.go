// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func seedAudit(t *testing.T, ctx context.Context, pg store.PG, n int, run *uuid.UUID, action string) {
	t.Helper()
	for i := range n {
		ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: run, ActorType: types.ActorHuman,
			Actor: "alice@corp.example", Action: action, Outcome: "success"}
		if i%2 == 1 {
			ev.Action = action + ".odd"
		}
		if err := store.InsertAuditEvent(ctx, pg.Pool, &ev); err != nil {
			t.Fatalf("seed audit: %v", err)
		}
	}
}

func auditIDs(evs []types.AuditEvent) []uuid.UUID {
	out := make([]uuid.UUID, len(evs))
	for i, ev := range evs {
		out[i] = ev.ID
	}
	return out
}

// walkKeyset reads the whole filtered feed through the keyset reader, size rows at a time. A cursor that
// does not advance would repeat a page forever, so the walk is bounded.
func walkKeyset(t *testing.T, ctx context.Context, pg store.PG, run *uuid.UUID, f store.AuditFilter, size int, between func(page int)) []uuid.UUID {
	t.Helper()
	var out []uuid.UUID
	var after int64
	for page := 0; page < 500; page++ {
		evs, last, err := pg.QueryAuditEventsFilteredAfter(ctx, run, f, after, size)
		if err != nil {
			t.Fatalf("keyset page %d: %v", page, err)
		}
		out = append(out, auditIDs(evs)...)
		if len(evs) < size {
			return out
		}
		after = last
		if between != nil {
			between(page)
		}
	}
	t.Fatalf("the keyset walk did not finish in 500 pages: the cursor does not advance")
	return nil
}

// The export's keyset walk returns what the OFFSET pages returned, in the same order, for the global
// newest-first feed, a filtered feed and a run's oldest-first trail; and a row written between two pages
// cannot move the next one, which the OFFSET walk did (it sent the previous page's last row again).
func TestPG_AuditKeysetWalkMatchesOffsetPagesAndSurvivesNewRows(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	run := newRun(types.RunRunning)
	persistRun(t, ctx, pool, run)
	seedAudit(t, ctx, pg, 30, nil, "export.a")
	seedAudit(t, ctx, pg, 11, &run.ID, "export.run")
	seedAudit(t, ctx, pg, 12, nil, "export.b")

	for _, c := range []struct {
		name string
		run  *uuid.UUID
		f    store.AuditFilter
	}{
		{"global feed", nil, store.AuditFilter{}},
		{"filtered feed", nil, store.AuditFilter{ActionPrefix: "export.a"}},
		{"outcome and actor", nil, store.AuditFilter{Outcome: "success", Actor: "alice@corp.example"}},
		{"a run's trail", &run.ID, store.AuditFilter{}},
	} {
		offsetWalk := func(size int) []uuid.UUID {
			var out []uuid.UUID
			for off := 0; ; off += size {
				evs, err := pg.QueryAuditEventsFilteredPage(ctx, c.run, c.f, store.Page{Limit: size, Offset: off})
				if err != nil {
					t.Fatalf("%s: offset page: %v", c.name, err)
				}
				out = append(out, auditIDs(evs)...)
				if len(evs) < size {
					return out
				}
			}
		}
		for _, size := range []int{1, 5, 7, 100} {
			want, got := offsetWalk(size), walkKeyset(t, ctx, pg, c.run, c.f, size, nil)
			if len(want) == 0 || !slices.Equal(got, want) {
				t.Errorf("%s, pages of %d: keyset walk %v, want the OFFSET walk %v", c.name, size, got, want)
			}
		}
	}

	// Rows committed mid-export: newest-first they land ahead of the cursor, and the walk neither repeats
	// a row nor skips one that existed when it began.
	want := walkKeyset(t, ctx, pg, nil, store.AuditFilter{}, 6, nil)
	got := walkKeyset(t, ctx, pg, nil, store.AuditFilter{}, 6, func(page int) { seedAudit(t, ctx, pg, 4, nil, "export.late") })
	if !slices.Equal(got, want) {
		t.Errorf("rows written mid-walk changed the export: got %d rows, want the %d that existed at the start", len(got), len(want))
	}
}
