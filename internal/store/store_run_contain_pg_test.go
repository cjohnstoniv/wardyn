// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"bytes"
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/testfloor"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_SetRunContainmentErrorSince: a missing stamp, or one older than the streak start, becomes the
// database's now(); one not older is kept; a run that is not terminal has no row. The streak start is the
// latest of the run's due time (end plus the lag), now minus startedAgo and now minus unobservedAgo, all on
// the database's clock. Each case seeds ended_at and the stamp as seconds before now() in SQL, so nothing
// sleeps and no Go time reaches the comparison. Proven red by the COALESCE form, which keeps a kept-phase
// stamp from before the end; by dropping the due time (the "due wins" cases), startedAgo ("started wins")
// or unobservedAgo ("unobserved wins") from the GREATEST; and by stamping a time the caller passes in.
func TestPG_SetRunContainmentErrorSince(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	secs := func(n float64) *float64 { return &n }
	ago := func(d time.Duration) *time.Duration { return &d }
	lag := store.ContainmentLag.Seconds()
	seeded := func(p *float64) string {
		if p == nil {
			return "none"
		}
		return strconv.FormatFloat(*p, 'f', -1, 64) + " s"
	}

	for name, tc := range map[string]struct {
		state       types.RunState
		endedAgo    float64  // seconds before now() the run ended
		stampAgo    *float64 // seconds before now() of the stored stamp; nil: none
		startedAgo  time.Duration
		unobserved  *time.Duration
		wantStarted bool
	}{
		"null stamp":                    {types.RunKilled, 3600, nil, time.Minute, nil, true},
		"older than the start":          {types.RunKilled, 3600, secs(3000), time.Minute, nil, true},
		"newer than the start":          {types.RunKilled, 3600, secs(30), time.Minute, nil, false},
		"due wins, stamp before it":     {types.RunKilled, lag + 60, secs(120), time.Hour, nil, true},
		"due wins, stamp after it":      {types.RunKilled, lag + 60, secs(30), time.Hour, nil, false},
		"started wins, stamp before":    {types.RunKilled, 3600, secs(600), 5 * time.Minute, nil, true},
		"started wins, stamp after":     {types.RunKilled, 3600, secs(120), 5 * time.Minute, nil, false},
		"unobserved wins, stamp before": {types.RunKilled, 3600, secs(60), time.Hour, ago(30 * time.Second), true},
		"unobserved wins, stamp after":  {types.RunKilled, 3600, secs(10), time.Hour, ago(30 * time.Second), false},
		"running row":                   {types.RunRunning, 3600, secs(3000), time.Minute, nil, false},
	} {
		run := newRun(tc.state)
		run.SandboxRef = "ref-" + run.ID.String()
		r := persistRun(t, ctx, pool, run)
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET ended_at = now() - $2::float8 * interval '1 second',
			containment_error_at = now() - $3::float8 * interval '1 second' WHERE id = $1`, r.ID, tc.endedAgo, tc.stampAgo); err != nil {
			t.Fatalf("%s: seed the row: %v", name, err)
		}
		started, err := pg.SetRunContainmentErrorSince(ctx, r.ID, "boom", tc.startedAgo, tc.unobserved)
		if err != nil || started != tc.wantStarted {
			t.Errorf("%s: SetRunContainmentErrorSince = %v, %v; want %v", name, started, err, tc.wantStarted)
		}
		// The stamp's age in SQL: a stamp this call set is within 5 s of now(); one kept is still the seeded
		// age, give or take the seconds the test took.
		var age *float64
		if err := pool.QueryRow(ctx, `SELECT extract(epoch FROM now() - containment_error_at)::float8 FROM agent_runs WHERE id = $1`, r.ID).Scan(&age); err != nil {
			t.Fatalf("%s: read the stamp's age: %v", name, err)
		}
		switch {
		case age == nil:
			t.Errorf("%s: containment_error_at is NULL", name)
		case tc.wantStarted && (*age < 0 || *age >= 5):
			t.Errorf("%s: stamp is %.2f s old, want within 5 s of the database's now()", name, *age)
		case !tc.wantStarted && (tc.stampAgo == nil || *age < *tc.stampAgo || *age > *tc.stampAgo+5):
			t.Errorf("%s: stamp is %.2f s old, want the seeded age (%s) or up to 5 s more", name, *age, seeded(tc.stampAgo))
		}
		got, err := pg.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if wantMsg := tc.state.IsTerminal(); (got.ContainmentError == "boom") != wantMsg {
			t.Errorf("%s: containment_error = %q, written = %v, want %v", name, got.ContainmentError, got.ContainmentError == "boom", wantMsg)
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
	withRef := func(state types.RunState) types.AgentRun {
		run := newRun(state)
		run.SandboxRef = "ref-" + run.ID.String()
		return run
	}
	live := persistRun(t, ctx, pool, withRef(types.RunRunning))
	if err := pg.MarkRunFenced(ctx, live.ID); err != nil || fencedAt(live.ID) != nil {
		t.Errorf("a RUNNING run: err %v, fenced_at %v; want nil, nil", err, fencedAt(live.ID))
	}
	for _, st := range []types.RunState{types.RunCompleted, types.RunFailed, types.RunKilled, types.RunStopped, types.RunArchived} {
		r := persistRun(t, ctx, pool, withRef(st))
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

// TestPG_ContainWritesSkipARunWithNoSandbox: an ended run whose sandbox_ref is empty is neither stamped
// with a containment error nor marked fenced. Proven red by dropping the sandbox_ref condition from
// either statement.
func TestPG_ContainWritesSkipARunWithNoSandbox(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	for _, state := range []types.RunState{types.RunKilled, types.RunFailed} {
		run := newRun(state)
		run.SandboxRef = "ref-" + run.ID.String()
		r := persistRun(t, ctx, pool, run)
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET sandbox_ref = '' WHERE id = $1`, r.ID); err != nil {
			t.Fatalf("clear the ref: %v", err)
		}
		started, err := pg.SetRunContainmentErrorSince(ctx, r.ID, "boom", time.Minute, nil)
		if err != nil || started {
			t.Errorf("%s: SetRunContainmentErrorSince = %v, %v; want false, nil", state, started, err)
		}
		if err := pg.MarkRunFenced(ctx, r.ID); err != nil {
			t.Errorf("%s: MarkRunFenced: %v", state, err)
		}
		var errAt, fencedAt *time.Time
		var msg *string
		if err := pool.QueryRow(ctx, `SELECT containment_error_at, fenced_at, containment_error FROM agent_runs WHERE id = $1`, r.ID).Scan(&errAt, &fencedAt, &msg); err != nil {
			t.Fatalf("read the row: %v", err)
		}
		if errAt != nil || fencedAt != nil || msg != nil {
			t.Errorf("%s: containment_error_at %v, fenced_at %v, containment_error %v; want none of them written", state, errAt, fencedAt, msg)
		}
	}
}

// TestPG_ListTerminalSandboxPage_DueIsTheDatabases: each listed row says whether it is due by the
// database's now(), with ended_at seeded in SQL 30 s short of the lag and a few seconds past it, so no Go
// time is in any statement. Proven red by a due that is always true or always false, or a listing that
// leaves it unset. That the server's own clock never decides it is pinned in the api package, where the
// two clocks can differ.
func TestPG_ListTerminalSandboxPage_DueIsTheDatabases(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	lag := store.ContainmentLag.Seconds()
	add := func(ref string, endedAgo float64) uuid.UUID {
		t.Helper()
		run := newRun(types.RunKilled)
		run.SandboxRef = ref
		r := persistRun(t, ctx, pool, run)
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET ended_at = now() - $2::float8 * interval '1 second' WHERE id = $1`, r.ID, endedAgo); err != nil {
			t.Fatalf("place run: %v", err)
		}
		return r.ID
	}
	notYet, due := add("ref-not-yet", lag-30), add("ref-due", lag+2)
	rows, _, err := pg.ListTerminalSandboxPage(ctx, nil, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]bool{}
	for _, r := range rows {
		got[r.ID] = r.Due
	}
	if len(got) != 2 {
		t.Fatalf("listed %d rows, want the 2 seeded", len(got))
	}
	if got[notYet] {
		t.Error("a run 30 s short of due listed Due = true, want false")
	}
	if !got[due] {
		t.Error("a run 2 s past due listed Due = false, want true")
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
// clock and not older than the streak start: the latest of the run's due, now() minus startedAgo and now()
// minus unobservedAgo. The run's end and every stamp are seeded as seconds before now() in SQL, so no Go time
// reaches a comparison. A stamp ahead of the database's clock is not old enough, one from before the streak
// start never counts, and a run with no stamp, or no row, is false. Proven red by dropping any one of the
// three from the GREATEST, by a spacing dropped or read in the wrong unit.
func TestPG_ContainmentStreakDue_DBClock(t *testing.T) {
	testfloor.Mark(t, "pg")
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	r := persistRun(t, ctx, pool, newRun(types.RunKilled))
	// The run's due is an hour before now().
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET ended_at = now() - $2::float8 * interval '1 second' WHERE id = $1`,
		r.ID, (time.Hour + store.ContainmentLag).Seconds()); err != nil {
		t.Fatalf("place the run: %v", err)
	}
	secs := func(f float64) *float64 { return &f }
	minute, long := time.Minute, 2*time.Hour
	for name, tc := range map[string]struct {
		ago        *float64 // the stamp, seconds before now(); nil: no stamp
		started    time.Duration
		unobserved *time.Duration
		want       bool
	}{
		"no stamp":                         {nil, long, nil, false},
		"old enough":                       {secs(5), long, nil, true},
		"ahead of the database's clock":    {secs(-3600), long, nil, false},
		"before the due":                   {secs(3610), long, nil, false},
		"after the due":                    {secs(3590), long, nil, true},
		"before this process started":      {secs(70), minute, nil, false},
		"after this process started":       {secs(50), minute, nil, true},
		"before the last unobserved round": {secs(70), long, &minute, false},
		"after the last unobserved round":  {secs(50), long, &minute, true},
	} {
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET containment_error_at = now() - $2::float8 * interval '1 second' WHERE id = $1`, r.ID, tc.ago); err != nil {
			t.Fatalf("%s: set the stamp: %v", name, err)
		}
		if got, err := pg.ContainmentStreakDue(ctx, r.ID, time.Second, tc.started, tc.unobserved); err != nil || got != tc.want {
			t.Errorf("%s: ContainmentStreakDue = %v, %v; want %v", name, got, err, tc.want)
		}
	}
	if got, err := pg.ContainmentStreakDue(ctx, uuid.New(), time.Second, long, nil); err != nil || got {
		t.Errorf("no such run: ContainmentStreakDue = %v, %v; want false", got, err)
	}
	// With a one-minute spacing the spacing is exercised at its real scale: 5 s old is too young, 61 s old is
	// old enough. A spacing dropped, or read in the wrong unit, gets one of the two wrong.
	for _, tc := range []struct {
		age  float64
		want bool
	}{{5, false}, {61, true}} {
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET containment_error_at = now() - $2::float8 * interval '1 second' WHERE id = $1`, r.ID, tc.age); err != nil {
			t.Fatalf("%v s old: set the stamp: %v", tc.age, err)
		}
		if got, err := pg.ContainmentStreakDue(ctx, r.ID, time.Minute, long, nil); err != nil || got != tc.want {
			t.Errorf("a stamp %v s old with a one-minute spacing: ContainmentStreakDue = %v, %v; want %v", tc.age, got, err, tc.want)
		}
	}
}

// TestPG_ContainmentPastDue_DBClock: an ended run is past its due plus wait only by the database's now(), with
// its end seeded as seconds before now() in SQL. A run 30 s past its due is past it with no wait and with
// 20 s, not with 40 s; a run 30 s short of it is not past it; a run that is not terminal, or no row, never
// is. Proven red by a due read from a Go time, by the wait dropped or read in the wrong unit, and by a
// missing state test.
func TestPG_ContainmentPastDue_DBClock(t *testing.T) {
	testfloor.Mark(t, "pg")
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	lag := store.ContainmentLag.Seconds()
	add := func(state types.RunState, endedAgo float64) uuid.UUID {
		t.Helper()
		r := persistRun(t, ctx, pool, newRun(state))
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET ended_at = now() - $2::float8 * interval '1 second' WHERE id = $1`, r.ID, endedAgo); err != nil {
			t.Fatalf("place the run: %v", err)
		}
		return r.ID
	}
	past, short, running := add(types.RunKilled, lag+30), add(types.RunKilled, lag-30), add(types.RunRunning, lag+3600)
	for name, tc := range map[string]struct {
		id   uuid.UUID
		wait time.Duration
		want bool
	}{
		"30 s past due, no wait":     {past, 0, true},
		"30 s past due, a 20 s wait": {past, 20 * time.Second, true},
		"30 s past due, a 40 s wait": {past, 40 * time.Second, false},
		"30 s short of due":          {short, 0, false},
		"running, its end long past": {running, 0, false},
		"no such run":                {uuid.New(), 0, false},
	} {
		if got, err := pg.ContainmentPastDue(ctx, tc.id, tc.wait); err != nil || got != tc.want {
			t.Errorf("%s: ContainmentPastDue = %v, %v; want %v", name, got, err, tc.want)
		}
	}
}

// TestPG_ClearRunContainmentErrorForRef: a failure is cleared by a stop of the sandbox the run holds, and is
// kept by a stop of any other sandbox, an empty ref, or a run with nothing recorded.
func TestPG_ClearRunContainmentErrorForRef(t *testing.T) {
	testfloor.Mark(t, "pg")
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
		after, err := pg.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatalf("%s: read the run back: %v", name, err)
		}
		if still := after.ContainmentErrorAt != nil; still == tc.want {
			t.Errorf("%s: failure still recorded = %v, want %v", name, still, !tc.want)
		}
	}
	if got, err := pg.ClearRunContainmentErrorForRef(ctx, r.ID, "sbx-current"); err != nil || got {
		t.Errorf("nothing recorded: cleared = %v, %v; want false", got, err)
	}
}
