// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// A sweep that pages by cursor reads every run once however many are created between its pages. 30 runs share
// one created_at and sit at positions 186-215 of the order, so the first 200-row page ends inside the tie and the
// id has to break it. The same two pages by OFFSET, after the inserts, repeat 50 runs the first page read.
func TestPG_ListRunsKeysetCoversEveryRunOnceAcrossInserts(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	base := time.Now().UTC().Truncate(time.Second).Add(-24 * time.Hour)

	insert := func(from, to int, at string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO agent_runs (id, created_at, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
			SELECT gen_random_uuid(), `+at+`, 'keyset', 'claude-code', 'o/r', 'CC2', 'COMPLETED', 'spiffe://x/' || gen_random_uuid(), 'docker'
			FROM generate_series($1::int, $2::int) g`, from, to, base); err != nil {
			t.Fatalf("seed runs %d-%d: %v", from, to, err)
		}
	}
	insert(1, 186, `$3::timestamptz - g * interval '1 minute'`)   // positions 0-185
	insert(1, 30, `$3::timestamptz - 187 * interval '1 minute'`)  // positions 186-215: one created_at
	insert(188, 421, `$3::timestamptz - g * interval '1 minute'`) // positions 216-449

	rows, err := pool.Query(ctx, `SELECT id FROM agent_runs ORDER BY created_at DESC, id DESC`)
	if err != nil {
		t.Fatalf("read the order: %v", err)
	}
	original, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil || len(original) != 450 {
		t.Fatalf("original order: %d runs, %v; want 450", len(original), err)
	}

	cursorOf := func(runs []store.RunCursor) store.RunCursor { return runs[len(runs)-1] }
	var seen []uuid.UUID
	var cursor store.RunCursor
	page := func(label string) {
		t.Helper()
		runs, err := pg.ListRunsKeyset(ctx, cursor, 200)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		cursors := make([]store.RunCursor, len(runs))
		for i, r := range runs {
			seen = append(seen, r.ID)
			cursors[i] = store.RunCursor{CreatedAt: r.CreatedAt, ID: r.ID}
		}
		if len(cursors) > 0 {
			cursor = cursorOf(cursors)
		}
	}
	page("first page")
	firstPage := slices.Clone(seen)
	if firstPage[199] != original[199] {
		t.Fatalf("the first page does not end at position 199")
	}

	// 250 runs newer than any read so far arrive between the two pages.
	insert(1, 250, `$3::timestamptz + g * interval '1 second'`)

	// The control: an OFFSET page after the inserts repeats runs the first page read.
	offsetPage, err := pg.ListRunsPage(ctx, store.Page{Limit: 200, Offset: 200})
	if err != nil {
		t.Fatalf("offset page: %v", err)
	}
	repeated := 0
	for _, r := range offsetPage {
		if slices.Contains(firstPage, r.ID) {
			repeated++
		}
	}
	if repeated != 50 {
		t.Errorf("the OFFSET page after the inserts repeats %d runs of the first page, want 50 (the control the cursor is judged against)", repeated)
	}

	page("second page")
	page("third page")
	if !slices.Equal(seen, original) {
		t.Errorf("the cursor pages hold %d runs, not the original %d once each, in order", len(seen), len(original))
	}

	plan := planOfCall(t, ctx, pool, func(p store.PG) {
		if _, err := p.ListRunsKeyset(ctx, store.RunCursor{CreatedAt: base.Add(-time.Hour), ID: original[100]}, 200); err != nil {
			t.Fatalf("cursor page: %v", err)
		}
	})
	t.Logf("plan for a cursor page:\n%s", plan)
	if !strings.Contains(plan, "agent_runs_created_at_idx") {
		t.Errorf("a cursor page does not use agent_runs_created_at_idx:\n%s", plan)
	}
}
