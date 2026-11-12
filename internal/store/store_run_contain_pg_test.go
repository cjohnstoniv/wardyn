// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// containErrAt reads a run's containment_error_at.
func containErrAt(t *testing.T, ctx context.Context, pg store.PG, id uuid.UUID) *time.Time {
	t.Helper()
	got, err := pg.GetRun(ctx, id)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	return got.ContainmentErrorAt
}

// TestPG_SetRunContainmentErrorSince: no stamp, or one older than since, becomes now; one not older than
// since is kept; a run that is not terminal has no row. Proven red by the COALESCE form, which keeps a
// kept-phase stamp from before the end.
func TestPG_SetRunContainmentErrorSince(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	since := now.Add(-time.Minute)
	stamp := func(id uuid.UUID, at *time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET containment_error_at = $2 WHERE id = $1`, id, at); err != nil {
			t.Fatalf("set stamp: %v", err)
		}
	}
	older, newer := now.Add(-time.Hour), now.Add(-30*time.Second)

	for name, tc := range map[string]struct {
		state       types.RunState
		stamp       *time.Time
		wantStarted bool
		wantAt      *time.Time
	}{
		"null":        {types.RunKilled, nil, true, &now},
		"older":       {types.RunKilled, &older, true, &now},
		"newer":       {types.RunKilled, &newer, false, &newer},
		"at since":    {types.RunKilled, &since, false, &since},
		"running row": {types.RunRunning, &older, false, &older},
	} {
		r := persistRun(t, ctx, pool, newRun(tc.state))
		stamp(r.ID, tc.stamp)
		started, err := pg.SetRunContainmentErrorSince(ctx, r.ID, "boom", now, since)
		if err != nil || started != tc.wantStarted {
			t.Errorf("%s: SetRunContainmentErrorSince = %v, %v; want %v", name, started, err, tc.wantStarted)
		}
		got := containErrAt(t, ctx, pg, r.ID)
		if got == nil || !got.Equal(*tc.wantAt) {
			t.Errorf("%s: containment_error_at = %v, want %v", name, got, tc.wantAt)
		}
		run, err := pg.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if wantMsg := tc.state.IsTerminal(); (run.ContainmentError == "boom") != wantMsg {
			t.Errorf("%s: containment_error = %q, written = %v, want %v", name, run.ContainmentError, run.ContainmentError == "boom", wantMsg)
		}
	}
}

// TestPG_MarkRunFencedOnlyTerminal: fenced_at is set once on a terminal run and never on a live one.
func TestPG_MarkRunFencedOnlyTerminal(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	fencedAt := func(id uuid.UUID) *time.Time {
		t.Helper()
		var at *time.Time
		if err := pool.QueryRow(ctx, `SELECT fenced_at FROM agent_runs WHERE id = $1`, id).Scan(&at); err != nil {
			t.Fatalf("read fenced_at: %v", err)
		}
		return at
	}
	live := persistRun(t, ctx, pool, newRun(types.RunRunning))
	if err := pg.MarkRunFenced(ctx, live.ID); err != nil || fencedAt(live.ID) != nil {
		t.Errorf("a RUNNING run: err %v, fenced_at %v; want nil, nil", err, fencedAt(live.ID))
	}
	for _, st := range []types.RunState{types.RunCompleted, types.RunFailed, types.RunKilled, types.RunStopped, types.RunArchived} {
		r := persistRun(t, ctx, pool, newRun(st))
		if err := pg.MarkRunFenced(ctx, r.ID); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
		first := fencedAt(r.ID)
		if first == nil {
			t.Fatalf("%s: fenced_at not set", st)
		}
		time.Sleep(10 * time.Millisecond)
		if err := pg.MarkRunFenced(ctx, r.ID); err != nil {
			t.Fatalf("%s again: %v", st, err)
		}
		if again := fencedAt(r.ID); again == nil || !again.Equal(*first) {
			t.Errorf("%s: fenced_at moved from %v to %v", st, first, again)
		}
	}
}

// TestPG_ListTerminalSandboxPage_Keyset: pages follow (end, id) newest first with ties broken by id, a
// fenced row is not listed when unfencedOnly, and a running run, or one with no sandbox, never is.
func TestPG_ListTerminalSandboxPage_Keyset(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	t0 := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	add := func(state types.RunState, ref string, end time.Time, fenced bool) uuid.UUID {
		t.Helper()
		run := newRun(state)
		run.SandboxRef = ref
		r := persistRun(t, ctx, pool, run)
		var fencedAt *time.Time
		if fenced {
			fencedAt = &end
		}
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET ended_at = $2, updated_at = $2, fenced_at = $3 WHERE id = $1`, r.ID, end, fencedAt); err != nil {
			t.Fatalf("place run: %v", err)
		}
		return r.ID
	}
	tie := []uuid.UUID{add(types.RunKilled, "ref-a", t0, false), add(types.RunFailed, "ref-b", t0, false), add(types.RunStopped, "ref-c", t0, false)}
	older := add(types.RunCompleted, "ref-d", t0.Add(-time.Minute), false)
	fenced := add(types.RunKilled, "ref-e", t0.Add(-2*time.Minute), true)
	add(types.RunRunning, "ref-live", t0, false)
	add(types.RunKilled, "", t0, false)

	slices.SortFunc(tie, func(a, b uuid.UUID) int { return bytes.Compare(b[:], a[:]) }) // id descending
	walk := func(unfencedOnly bool) []uuid.UUID {
		t.Helper()
		var got []uuid.UUID
		var cur *store.TerminalSandboxCursor
		for pages := 0; ; pages++ {
			rows, next, err := pg.ListTerminalSandboxPage(ctx, cur, 2, unfencedOnly)
			if err != nil {
				t.Fatalf("ListTerminalSandboxPage: %v", err)
			}
			for _, r := range rows {
				got = append(got, r.ID)
			}
			if next == nil {
				return got
			}
			if pages > 10 {
				t.Fatal("the cursor never ended")
			}
			cur = next
		}
	}
	want := append(slices.Clone(tie), older)
	if got := walk(true); !slices.Equal(got, want) {
		t.Errorf("unfenced pages = %v, want %v", got, want)
	}
	if got := walk(false); !slices.Equal(got, append(want, fenced)) {
		t.Errorf("all pages = %v, want %v then the fenced run", got, want)
	}
}

// TestPG_ContainmentDueMatchesGo: store.ContainmentDueSQL is COALESCE(EndedAt, UpdatedAt) plus
// store.ContainmentLag, the value api.containmentDue computes for the same row.
func TestPG_ContainmentDueMatchesGo(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	t0 := time.Now().UTC().Truncate(time.Microsecond)
	u := t0.Add(-time.Hour)
	for name, tc := range map[string]struct {
		endedAt *time.Time
		updated time.Time
	}{
		"ended at t":    {&t0, u},
		"ended_at null": {nil, u},
	} {
		r := persistRun(t, ctx, pool, newRun(types.RunFailed))
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET ended_at = $2, updated_at = $3 WHERE id = $1`, r.ID, tc.endedAt, tc.updated); err != nil {
			t.Fatalf("%s: place run: %v", name, err)
		}
		var got time.Time
		if err := pool.QueryRow(ctx, `SELECT `+store.ContainmentDueSQL+` FROM agent_runs WHERE id = $1`, r.ID).Scan(&got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := tc.updated
		if tc.endedAt != nil {
			want = *tc.endedAt
		}
		if want = want.Add(store.ContainmentLag); !got.Equal(want) {
			t.Errorf("%s: SQL due = %v, Go due = %v", name, got, want)
		}
	}
}

// TestPG_TerminalDueFlags: ListTerminalSandboxPageDue and RunDueFlags answer Due (the containment due has passed)
// and CapPassed (the end plus keep has passed) against the database's now(), the same for the page and the single
// read; a run with no ended_at is timed from updated_at; the plain listing fills Due the same way and leaves
// CapPassed false; an unknown run is ErrNotFound. Catches: either flag decided from a caller's clock, and the two reads disagreeing.
func TestPG_TerminalDueFlags(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	keep := time.Hour
	now := time.Now().UTC().Truncate(time.Microsecond)
	place := func(endedAt *time.Time, updated time.Time) uuid.UUID {
		t.Helper()
		r := newRun(types.RunFailed)
		r.SandboxRef = "sbx-" + r.ID.String()
		r = persistRun(t, ctx, pool, r)
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET sandbox_ref = $2, ended_at = $3, updated_at = $4 WHERE id = $1`, r.ID, r.SandboxRef, endedAt, updated); err != nil {
			t.Fatalf("place run: %v", err)
		}
		return r.ID
	}
	ended := func(ago time.Duration) *time.Time { at := now.Add(-ago); return &at }
	cases := map[string]struct {
		id        uuid.UUID
		due, past bool
	}{
		"ended a minute ago":                  {place(ended(time.Minute), now), false, false},
		"ended past the containment due":      {place(ended(store.ContainmentLag+time.Minute), now), true, false},
		"ended past the keep":                 {place(ended(keep+time.Minute), now), true, true},
		"no ended_at, updated past the keep":  {place(nil, now.Add(-keep-time.Minute)), true, true},
		"no ended_at, updated a minute ago":   {place(nil, now.Add(-time.Minute)), false, false},
		"no ended_at, updated past the due":   {place(nil, now.Add(-store.ContainmentLag-time.Minute)), true, false},
		"ended long ago, updated just before": {place(ended(keep+time.Minute), now.Add(-time.Second)), true, true},
	}
	rows := map[uuid.UUID]store.TerminalSandboxRow{}
	plain := map[uuid.UUID]store.TerminalSandboxRow{}
	for _, walk := range []struct {
		into map[uuid.UUID]store.TerminalSandboxRow
		page func(*store.TerminalSandboxCursor) ([]store.TerminalSandboxRow, *store.TerminalSandboxCursor, error)
	}{
		{rows, func(c *store.TerminalSandboxCursor) ([]store.TerminalSandboxRow, *store.TerminalSandboxCursor, error) {
			return pg.ListTerminalSandboxPageDue(ctx, c, 500, false, keep)
		}},
		{plain, func(c *store.TerminalSandboxCursor) ([]store.TerminalSandboxRow, *store.TerminalSandboxCursor, error) {
			return pg.ListTerminalSandboxPage(ctx, c, 500, false)
		}},
	} {
		var cur *store.TerminalSandboxCursor
		for {
			page, next, err := walk.page(cur)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range page {
				walk.into[r.ID] = r
			}
			if next == nil {
				break
			}
			cur = next
		}
	}
	for name, c := range cases {
		if got := rows[c.id]; got.ID != c.id || got.Due != c.due || got.CapPassed != c.past {
			t.Errorf("%s: listing says due %v, cap passed %v; want %v and %v", name, got.Due, got.CapPassed, c.due, c.past)
		}
		one, err := pg.RunDueFlags(ctx, c.id, keep)
		if err != nil || one.Due != c.due || one.CapPassed != c.past {
			t.Errorf("%s: RunDueFlags = %+v, %v; want due %v, cap passed %v", name, one, err, c.due, c.past)
		}
		if got := plain[c.id]; got.ID != c.id || got.Due != c.due || got.CapPassed {
			t.Errorf("%s: the plain listing answers due %v, cap passed %v; want due %v and cap passed left false", name, got.Due, got.CapPassed, c.due)
		}
	}
	if _, err := pg.RunDueFlags(ctx, uuid.New(), keep); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an unknown run: %v; want store.ErrNotFound", err)
	}
}

// TestPG_PlainListingFillsDue: the plain ListTerminalSandboxPage answers Due from the same SQL as the Due listing,
// for a row 30 s short of its containment due and one 30 s past it, both seeded with the database's now(), so a
// zero Due means not due. Catches: the plain listing leaving Due at its default.
func TestPG_PlainListingFillsDue(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	place := func(offset time.Duration) uuid.UUID {
		t.Helper()
		r := newRun(types.RunFailed)
		r.SandboxRef = "sbx-" + r.ID.String()
		r = persistRun(t, ctx, pool, r)
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET sandbox_ref = $2, ended_at = now() - make_interval(secs => $3) WHERE id = $1`,
			r.ID, r.SandboxRef, (store.ContainmentLag + offset).Seconds()); err != nil {
			t.Fatalf("place run: %v", err)
		}
		return r.ID
	}
	short, past := place(-30*time.Second), place(30*time.Second)
	got := map[uuid.UUID]bool{}
	var cur *store.TerminalSandboxCursor
	for {
		page, next, err := pg.ListTerminalSandboxPage(ctx, cur, 500, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page {
			got[r.ID] = r.Due
		}
		if next == nil {
			break
		}
		cur = next
	}
	if due, ok := got[short]; !ok || due {
		t.Errorf("a row 30 s short of its due: listed %v, Due %v; want listed and not due", ok, due)
	}
	if due, ok := got[past]; !ok || !due {
		t.Errorf("a row 30 s past its due: listed %v, Due %v; want listed and due", ok, due)
	}
}
