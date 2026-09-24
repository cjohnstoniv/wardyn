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
