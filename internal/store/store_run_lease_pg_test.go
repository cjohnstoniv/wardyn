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

// TestPG_RunLease pins migration 0073 through the lease surface: which runs the
// sweep lists, the end's claim (once, only at or past the end, only RUNNING),
// lost_at/lost_reason reading back on the run, and a kept run never being
// claimed by the stale-watcher sweep.
func TestPG_RunLease(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }

	due := newRun(types.RunRunning)
	due.EndsAt, due.SandboxRef = at(-time.Minute), "ref-due"
	notYet := newRun(types.RunRunning)
	notYet.EndsAt = at(time.Hour)
	noEnd := newRun(types.RunRunning)
	finished := newRun(types.RunCompleted)
	finished.EndsAt = at(-time.Minute)
	for _, r := range []types.AgentRun{due, notYet, noEnd, finished} {
		persistRun(t, ctx, pool, r)
	}

	listed, err := pg.ListLeasedRuns(ctx)
	if err != nil {
		t.Fatalf("ListLeasedRuns: %v", err)
	}
	has := func(id uuid.UUID) bool {
		return slices.ContainsFunc(listed, func(r types.AgentRun) bool { return r.ID == id })
	}
	if !has(due.ID) || !has(notYet.ID) || has(noEnd.ID) || has(finished.ID) {
		t.Errorf("listed due=%v notYet=%v noEnd=%v finished=%v; want only the two RUNNING runs with an end",
			has(due.ID), has(notYet.ID), has(noEnd.ID), has(finished.ID))
	}

	for _, tc := range []struct {
		name string
		id   uuid.UUID
		want bool
	}{
		{"before its end", notYet.ID, false},
		{"a terminal run", finished.ID, false},
		{"at its end", due.ID, true},
		{"already ended", due.ID, false},
	} {
		got, err := pg.MarkRunEnded(ctx, tc.id, now)
		if err != nil || got != tc.want {
			t.Errorf("MarkRunEnded(%s) = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	ended, err := pg.GetRun(ctx, due.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if ended.LostAt == nil || !ended.LostAt.Equal(now) || ended.LostReason != types.LostEnded || ended.State != types.RunRunning {
		t.Errorf("ended run = lost %v %q state %s; want lost at %v, ended, still RUNNING",
			ended.LostAt, ended.LostReason, ended.State, now)
	}

	// A stale lease on the kept run must not hand it to a watcher.
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET watcher_heartbeat = now() - interval '1 hour' WHERE id=$1`, due.ID); err != nil {
		t.Fatalf("age the lease: %v", err)
	}
	claimed, err := pg.ClaimStaleRunWatchers(ctx, "lease-test", time.Minute)
	if err != nil {
		t.Fatalf("ClaimStaleRunWatchers: %v", err)
	}
	if slices.ContainsFunc(claimed, func(r types.AgentRun) bool { return r.ID == due.ID }) {
		t.Error("the stale-watcher sweep claimed a kept run; its watcher would finalize and tear it down")
	}
}

// TestPG_MarkRunEndingSoon pins the warning bookkeeping: once per threshold per
// end, a closer threshold after a wider one but not the reverse, and a moved
// end re-arming everything.
func TestPG_MarkRunEndingSoon(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	end := time.Now().UTC().Add(50 * time.Minute).Truncate(time.Microsecond)
	r := newRun(types.RunRunning)
	r.EndsAt = &end
	persistRun(t, ctx, pool, r)

	later := end.Add(time.Hour)
	for _, tc := range []struct {
		name   string
		endsAt time.Time
		sec    int
		want   bool
	}{
		{"1 h, first", end, 3600, true},
		{"1 h, again", end, 3600, false},
		{"24 h after 1 h", end, 86400, false},
		{"10 min", end, 600, true},
		{"a stale end", later, 3600, false},
	} {
		got, err := pg.MarkRunEndingSoon(ctx, r.ID, tc.endsAt, tc.sec)
		if err != nil || got != tc.want {
			t.Errorf("%s: MarkRunEndingSoon = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET ends_at=$1 WHERE id=$2`, later, r.ID); err != nil {
		t.Fatalf("move the end: %v", err)
	}
	if got, err := pg.MarkRunEndingSoon(ctx, r.ID, later, 3600); err != nil || !got {
		t.Errorf("after the end moved: MarkRunEndingSoon = %v, %v; want true", got, err)
	}
}

// TestPG_SetRunEndAndWait pins the change a PATCH lands: only against the end
// and wait the caller read, never on a kept or terminal run, and No end as NULL.
func TestPG_SetRunEndAndWait(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	end := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	later := end.Add(24 * time.Hour)
	live := newRun(types.RunRunning)
	live.EndsAt, live.WaitBudgetSec = &end, 600
	kept := newRun(types.RunRunning)
	kept.EndsAt = &end
	finished := newRun(types.RunCompleted)
	finished.EndsAt = &end
	for _, r := range []types.AgentRun{live, kept, finished} {
		persistRun(t, ctx, pool, r)
	}
	if _, err := pg.MarkRunEnded(ctx, kept.ID, end); err != nil {
		t.Fatalf("MarkRunEnded: %v", err)
	}

	for _, tc := range []struct {
		name     string
		id       uuid.UUID
		fromEnd  *time.Time
		fromWait int
		want     bool
	}{
		{"a stale end", live.ID, &later, 600, false},
		{"a stale wait", live.ID, &end, 60, false},
		{"a kept run", kept.ID, &end, 0, false},
		{"a terminal run", finished.ID, &end, 0, false},
		{"the values read", live.ID, &end, 600, true},
		{"the same values again", live.ID, &end, 600, false},
	} {
		got, err := pg.SetRunEndAndWait(ctx, tc.id, tc.fromEnd, tc.fromWait, &later, 1200, nil)
		if err != nil || got != tc.want {
			t.Errorf("%s: SetRunEndAndWait = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	moved, err := pg.GetRun(ctx, live.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if moved.EndsAt == nil || !moved.EndsAt.Equal(later) || moved.WaitBudgetSec != 1200 {
		t.Errorf("run = ends %v wait %d; want %v and 1200", moved.EndsAt, moved.WaitBudgetSec, later)
	}
	if got, err := pg.SetRunEndAndWait(ctx, live.ID, &later, 1200, nil, 1200, nil); err != nil || !got {
		t.Fatalf("to No end: %v, %v; want true", got, err)
	}
	if moved, _ := pg.GetRun(ctx, live.ID); moved.EndsAt != nil {
		t.Errorf("ends_at = %v, want NULL (no end)", moved.EndsAt)
	}
	if got, err := pg.SetRunEndAndWait(ctx, live.ID, nil, 1200, &end, 1200, nil); err != nil || !got {
		t.Errorf("from No end: %v, %v; want true — a NULL end compares as the value read", got, err)
	}
}

// endedRun persists a RUNNING run whose end passed an hour ago and which the
// lease ended a minute ago; it returns the run and when it ended.
func endedRun(t *testing.T, ctx context.Context, pg store.PG, now time.Time) (types.AgentRun, time.Time) {
	t.Helper()
	end := now.Add(-time.Hour)
	r := newRun(types.RunRunning)
	r.EndsAt = &end
	persistRun(t, ctx, pg.Pool, r)
	endedAt := now.Add(-time.Minute)
	if ok, err := pg.MarkRunEnded(ctx, r.ID, endedAt); err != nil || !ok {
		t.Fatalf("MarkRunEnded = %v, %v", ok, err)
	}
	return r, endedAt
}

const testEndedGrace = 7 * 24 * time.Hour

// keptAt is the ended condition for the mark lostAt, decided at at.
func keptAt(lostAt, at time.Time) *store.EndedKept {
	return &store.EndedKept{LostAt: lostAt, KeptAfter: at.Add(-testEndedGrace), Now: at}
}

// TestPG_EndedRunExtensionHonorsTheKeptMark pins #1061's extension write: a
// run its own end stopped moves its end only under the ended condition, only
// for the exact mark the caller read, and only while its files grace is live
// as of the write; the mark itself stays, so the grace is not renewed. The
// ended condition never lets a run that is not ended move.
func TestPG_EndedRunExtensionHonorsTheKeptMark(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	r, endedAt := endedRun(t, ctx, pg, now)
	later := now.Add(24 * time.Hour)

	for _, tc := range []struct {
		name  string
		ended *store.EndedKept
		want  bool
	}{
		{"without the ended condition", nil, false},
		{"another mark", keptAt(endedAt.Add(time.Second), now), false},
		{"the grace has run out", keptAt(endedAt, endedAt.Add(testEndedGrace)), false},
		{"the mark read, inside the grace", keptAt(endedAt, now), true},
	} {
		got, err := pg.SetRunEndAndWait(ctx, r.ID, r.EndsAt, r.WaitBudgetSec, &later, r.WaitBudgetSec, tc.ended)
		if err != nil || got != tc.want {
			t.Errorf("%s: SetRunEndAndWait = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	got, err := pg.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.EndsAt == nil || !got.EndsAt.Equal(later) || got.LostAt == nil || !got.LostAt.Equal(endedAt) ||
		got.LostReason != types.LostEnded || got.State != types.RunRunning {
		t.Errorf("run = ends %v lost %v %q state %s; want the new end and the SAME ended mark %v, still RUNNING",
			got.EndsAt, got.LostAt, got.LostReason, got.State, endedAt)
	}

	live := newRun(types.RunRunning)
	live.EndsAt = &later
	persistRun(t, ctx, pool, live)
	if ok, err := pg.SetRunEndAndWait(ctx, live.ID, &later, live.WaitBudgetSec, &now, live.WaitBudgetSec, keptAt(endedAt, now)); err != nil || ok {
		t.Errorf("a live run under the ended condition = %v, %v; want false", ok, err)
	}
}
