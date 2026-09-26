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
		got, err := pg.SetRunEndAndWait(ctx, tc.id, tc.fromEnd, tc.fromWait, &later, 1200)
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
	if got, err := pg.SetRunEndAndWait(ctx, live.ID, &later, 1200, nil, 1200); err != nil || !got {
		t.Fatalf("to No end: %v, %v; want true", got, err)
	}
	if moved, _ := pg.GetRun(ctx, live.ID); moved.EndsAt != nil {
		t.Errorf("ends_at = %v, want NULL (no end)", moved.EndsAt)
	}
	if got, err := pg.SetRunEndAndWait(ctx, live.ID, nil, 1200, &end, 1200); err != nil || !got {
		t.Errorf("from No end: %v, %v; want true — a NULL end compares as the value read", got, err)
	}
}

// TestPG_RunContainmentError pins migration 0086 (#1060): the error is set only
// on a RUNNING kept run, refreshed with the first failure's time kept, read
// back on the run, and cleared once — only the clear that found it set says so,
// so the resolution is audited once. A revive's claim clears it too.
func TestPG_RunContainmentError(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	end := now.Add(-time.Minute)
	kept := newRun(types.RunRunning)
	kept.EndsAt = &end
	live := newRun(types.RunRunning)
	for _, r := range []types.AgentRun{kept, live} {
		persistRun(t, ctx, pool, r)
	}
	if _, err := pg.MarkRunEnded(ctx, kept.ID, now); err != nil {
		t.Fatalf("MarkRunEnded: %v", err)
	}
	read := func(id uuid.UUID) types.AgentRun {
		t.Helper()
		r, err := pg.GetRun(ctx, id)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		return r
	}

	for _, msg := range []string{"first", "second"} {
		if err := pg.SetRunContainmentError(ctx, kept.ID, msg, now.Add(time.Duration(len(msg))*time.Second)); err != nil {
			t.Fatalf("SetRunContainmentError(%s): %v", msg, err)
		}
		if err := pg.SetRunContainmentError(ctx, live.ID, msg, now); err != nil {
			t.Fatalf("SetRunContainmentError(live): %v", err)
		}
	}
	if r := read(kept.ID); r.ContainmentError != "second" || r.ContainmentErrorAt == nil || !r.ContainmentErrorAt.Equal(now.Add(5*time.Second)) {
		t.Errorf("kept run = %q at %v; want the latest error at the first failure's time %v",
			r.ContainmentError, r.ContainmentErrorAt, now.Add(5*time.Second))
	}
	if r := read(live.ID); r.ContainmentError != "" || r.ContainmentErrorAt != nil {
		t.Errorf("live run = %q at %v; want nothing — only a kept run's containment is unresolved", r.ContainmentError, r.ContainmentErrorAt)
	}

	for i, want := range []bool{true, false} {
		if got, err := pg.ClearRunContainmentError(ctx, kept.ID); err != nil || got != want {
			t.Errorf("ClearRunContainmentError #%d = %v, %v; want %v", i+1, got, err, want)
		}
	}
	if r := read(kept.ID); r.ContainmentError != "" || r.ContainmentErrorAt != nil {
		t.Errorf("after the clear = %q at %v; want both NULL", r.ContainmentError, r.ContainmentErrorAt)
	}

	lost := newRun(types.RunRunning)
	persistRun(t, ctx, pool, lost)
	if _, err := pg.MarkRunLost(ctx, lost.ID, types.LostOutage, now, 0); err != nil {
		t.Fatalf("MarkRunLost: %v", err)
	}
	if err := pg.SetRunContainmentError(ctx, lost.ID, "boom", now); err != nil {
		t.Fatalf("SetRunContainmentError(lost): %v", err)
	}
	if got, err := pg.MarkRunRevived(ctx, lost.ID, types.LostOutage); err != nil || !got {
		t.Fatalf("MarkRunRevived = %v, %v; want true", got, err)
	}
	if r := read(lost.ID); r.ContainmentError != "" || r.ContainmentErrorAt != nil {
		t.Errorf("after a revive = %q at %v; want both NULL — the new proxy replaces the one it was about", r.ContainmentError, r.ContainmentErrorAt)
	}
}
