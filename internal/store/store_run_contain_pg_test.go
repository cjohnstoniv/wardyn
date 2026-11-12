// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"bytes"
	"context"
	"fmt"
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

// TestPG_ContainmentStreakDue_DBClock: a recorded failure counts when it is at least tau old on the database's
// clock and not older than the run's due or than since. A stamp the process clock put ahead of the database's is
// not old enough, one from before since or before the due never counts, and a run with no stamp, or no row, is false.
func TestPG_ContainmentStreakDue_DBClock(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	var dbNow time.Time
	if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		t.Fatalf("read the database clock: %v", err)
	}
	dbNow = dbNow.UTC().Truncate(time.Microsecond)
	r := persistRun(t, ctx, pool, newRun(types.RunKilled))
	due := dbNow.Add(-time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET ended_at = $2 WHERE id = $1`, r.ID, due.Add(-store.ContainmentLag)); err != nil {
		t.Fatalf("place the run: %v", err)
	}
	at := func(ts time.Time) *time.Time { return &ts }
	since, early := dbNow.Add(-time.Minute), dbNow.Add(-2*time.Hour)
	for name, tc := range map[string]struct {
		stamp *time.Time
		since time.Time
		want  bool
	}{
		"no stamp":                      {nil, since, false},
		"old enough":                    {at(dbNow.Add(-5 * time.Second)), since, true},
		"ahead of the database's clock": {at(dbNow.Add(time.Hour)), since, false},
		"before since":                  {at(since.Add(-time.Second)), since, false},
		"at since":                      {at(since), since, true},
		"before the due":                {at(due.Add(-time.Second)), early, false},
		"at the due":                    {at(due), early, true},
	} {
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET containment_error_at = $2 WHERE id = $1`, r.ID, tc.stamp); err != nil {
			t.Fatalf("%s: set the stamp: %v", name, err)
		}
		if got, err := pg.ContainmentStreakDue(ctx, r.ID, time.Second, tc.since); err != nil || got != tc.want {
			t.Errorf("%s: ContainmentStreakDue = %v, %v; want %v", name, got, err, tc.want)
		}
	}
	if got, err := pg.ContainmentStreakDue(ctx, uuid.New(), time.Second, since); err != nil || got {
		t.Errorf("no such run: ContainmentStreakDue = %v, %v; want false", got, err)
	}
	// With a one-minute spacing the stamp is set from the database's own clock, so the spacing is exercised at
	// its real scale: 5 s old is too young, 61 s old is old enough. A spacing dropped, or read in the wrong unit,
	// gets one of the two wrong.
	for _, tc := range []struct {
		age  int
		want bool
	}{{5, false}, {61, true}} {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`UPDATE agent_runs SET containment_error_at = now() - interval '%d seconds' WHERE id = $1`, tc.age), r.ID); err != nil {
			t.Fatalf("%d s old: set the stamp: %v", tc.age, err)
		}
		if got, err := pg.ContainmentStreakDue(ctx, r.ID, time.Minute, early); err != nil || got != tc.want {
			t.Errorf("a stamp %d s old with a one-minute spacing: ContainmentStreakDue = %v, %v; want %v", tc.age, got, err, tc.want)
		}
	}
}

// TestPG_ClearRunContainmentErrorForRef: a failure is cleared by a stop of the sandbox the run holds, and is
// kept by a stop of any other sandbox, an empty ref, or a run with nothing recorded.
func TestPG_ClearRunContainmentErrorForRef(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	run := newRun(types.RunKilled)
	run.SandboxRef = "sbx-current"
	r := persistRun(t, ctx, pool, run)
	failed := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET containment_error = 'boom', containment_error_at = now() WHERE id = $1`, r.ID); err != nil {
			t.Fatalf("record a failure: %v", err)
		}
	}
	for name, tc := range map[string]struct {
		ref  string
		want bool
	}{"another sandbox's ref": {"sbx-older", false}, "an empty ref": {"", false}, "the held ref": {"sbx-current", true}} {
		failed()
		got, err := pg.ClearRunContainmentErrorForRef(ctx, r.ID, tc.ref)
		if err != nil || got != tc.want {
			t.Errorf("%s: cleared = %v, %v; want %v", name, got, err, tc.want)
		}
		if still := containErrAt(t, ctx, pg, r.ID) != nil; still == tc.want {
			t.Errorf("%s: failure still recorded = %v, want %v", name, still, !tc.want)
		}
	}
	if got, err := pg.ClearRunContainmentErrorForRef(ctx, r.ID, "sbx-current"); err != nil || got {
		t.Errorf("nothing recorded: cleared = %v, %v; want false", got, err)
	}
}
