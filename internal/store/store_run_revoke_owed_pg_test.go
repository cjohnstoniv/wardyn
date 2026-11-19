// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

func TestPG_RevokeOwedMarkAndClear(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	run := manifestRun(t, pool, "alice")

	if err := pg.MarkRevokeOwed(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET revoke_owed_at = revoke_owed_at - interval '1 hour' WHERE id = $1`, run); err != nil {
		t.Fatal(err)
	}
	var stamped time.Time
	if err := pool.QueryRow(ctx, `SELECT revoke_owed_at FROM agent_runs WHERE id = $1`, run).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if err := pg.MarkRevokeOwed(ctx, run); err != nil {
		t.Fatal(err)
	}
	var again time.Time
	if err := pool.QueryRow(ctx, `SELECT revoke_owed_at FROM agent_runs WHERE id = $1`, run).Scan(&again); err != nil {
		t.Fatal(err)
	}
	if !again.Equal(stamped) {
		t.Fatalf("a second mark moved the stamp from %v to %v; the first stamp must stay", stamped, again)
	}

	if err := pg.ClearRevokeOwed(ctx, run); err != nil {
		t.Fatal(err)
	}
	if set, err := pg.RevokeOwedSet(ctx, run); err != nil || set {
		t.Fatalf("after a clear, set=%v err=%v", set, err)
	}
	if err := pg.ClearRevokeOwed(ctx, run); err != nil {
		t.Fatalf("clearing a run with no mark: %v", err)
	}
	if err := pg.MarkRevokeOwed(ctx, uuid.New()); err != nil {
		t.Fatalf("marking an unknown run: %v", err)
	}
}

func TestPG_ListRevokeOwedAfter(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)

	owedAt := func(run uuid.UUID, age string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET revoke_owed_at = now() - $2::text::interval WHERE id = $1`, run, age); err != nil {
			t.Fatal(err)
		}
	}
	oldest, tieA, tieB, young := manifestRun(t, pool, "a"), manifestRun(t, pool, "a"), manifestRun(t, pool, "a"), manifestRun(t, pool, "a")
	manifestRun(t, pool, "a") // never marked
	owedAt(oldest, "3 hours")
	owedAt(young, "5 seconds")
	// Two runs with the same stamp: the id breaks the tie.
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET revoke_owed_at = now() - interval '2 hours' WHERE id IN ($1, $2)`, tieA, tieB); err != nil {
		t.Fatal(err)
	}
	tieFirst, tieSecond := tieA, tieB
	if bytes.Compare(tieB[:], tieA[:]) < 0 {
		tieFirst, tieSecond = tieB, tieA
	}

	all, cur, err := pg.ListRevokeOwedAfter(ctx, time.Minute, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []uuid.UUID{oldest, tieFirst, tieSecond}
	if len(all) != 3 || all[0] != want[0] || all[1] != want[1] || all[2] != want[2] {
		t.Fatalf("older than a minute = %v, want %v (the young and the unmarked runs left out, ties by id)", all, want)
	}
	if cur != nil {
		t.Fatalf("a short page answered cursor %+v, want nil", cur)
	}

	var walked []uuid.UUID
	var after *store.RevokeOwedCursor
	for i := 0; i < 5; i++ {
		page, next, err := pg.ListRevokeOwedAfter(ctx, time.Minute, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		walked = append(walked, page...)
		if next == nil {
			break
		}
		after = next
	}
	if len(walked) != 3 || walked[0] != want[0] || walked[1] != want[1] || walked[2] != want[2] {
		t.Fatalf("a cursor walk of pages of 2 = %v, want %v", walked, want)
	}

	if page, next, err := pg.ListRevokeOwedAfter(ctx, time.Minute, &store.RevokeOwedCursor{At: time.Now().Add(time.Hour), ID: uuid.Nil}, 2); err != nil || len(page) != 0 || next != nil {
		t.Fatalf("past the last row: %v %+v %v", page, next, err)
	}
	if page, _, err := pg.ListRevokeOwedAfter(ctx, 0, nil, 10); err != nil || len(page) != 4 {
		t.Fatalf("older than zero lists %d (err %v), want the 4 marked runs", len(page), err)
	}
}

func TestPG_RevokeOwedSet(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	run := manifestRun(t, pool, "alice")

	if set, err := pg.RevokeOwedSet(ctx, run); err != nil || set {
		t.Fatalf("a fresh run: set=%v err=%v", set, err)
	}
	if err := pg.MarkRevokeOwed(ctx, run); err != nil {
		t.Fatal(err)
	}
	if set, err := pg.RevokeOwedSet(ctx, run); err != nil || !set {
		t.Fatalf("a marked run: set=%v err=%v", set, err)
	}
	if set, err := pg.RevokeOwedSet(ctx, uuid.New()); err != nil || set {
		t.Fatalf("an unknown run: set=%v err=%v", set, err)
	}
}
