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

// TestPG_ReclampRunLimits pins migration 0086 through the re-clamp surface:
// which runs the sweep lists, the write landing only against the limits, end
// and wait read (never on a kept or terminal run), end_tightened_at reading
// back, and a person moving the end clearing it.
func TestPG_ReclampRunLimits(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	end := now.Add(20 * 24 * time.Hour)
	profile := uuid.New()
	limits := types.RunLimits{MaxEndAheadSec: 30 * 86400, UserChangesLimits: true}

	profiled := func(state types.RunState) types.AgentRun {
		r := newRun(state)
		r.EndsAt, r.WaitBudgetSec, r.RunLimits, r.GovernanceProfileID = &end, 3600, limits, &profile
		return r
	}
	live, pending, kept, finished, rebootLost := profiled(types.RunRunning), profiled(types.RunPending),
		profiled(types.RunRunning), profiled(types.RunCompleted), profiled(types.RunRunning)
	unprofiled := newRun(types.RunRunning)
	for _, r := range []types.AgentRun{live, pending, kept, finished, rebootLost, unprofiled} {
		persistRun(t, ctx, pool, r)
	}
	if _, err := pg.MarkRunEnded(ctx, kept.ID, end); err != nil {
		t.Fatalf("MarkRunEnded: %v", err)
	}
	// A run lost to a reboot or an outage is not kept BY ITS OWN END — it is
	// still profiled-live, the same way it can still have its end extended
	// (F1, long-holds design rev 4 §2.3). Only LostEnded (MarkRunEnded, above)
	// takes a run out of the sweep.
	if _, err := pg.MarkRunLost(ctx, rebootLost.ID, types.LostReboot, now, 0); err != nil {
		t.Fatalf("MarkRunLost: %v", err)
	}

	listed, err := pg.ListProfiledLiveRuns(ctx)
	if err != nil {
		t.Fatalf("ListProfiledLiveRuns: %v", err)
	}
	var ids []uuid.UUID
	for _, r := range listed {
		ids = append(ids, r.ID)
	}
	for _, r := range []types.AgentRun{live, pending, rebootLost} {
		if !slices.Contains(ids, r.ID) {
			t.Errorf("ListProfiledLiveRuns misses %s", r.ID)
		}
	}
	for _, r := range []types.AgentRun{kept, finished, unprofiled} {
		if slices.Contains(ids, r.ID) {
			t.Errorf("ListProfiledLiveRuns lists %s, which is kept, terminal or unprofiled", r.ID)
		}
	}

	tight := types.RunLimits{MaxEndAheadSec: 2 * 86400}
	cut := now.Add(2 * 24 * time.Hour)
	stale := live
	stale.RunLimits = types.RunLimits{MaxEndAheadSec: 7 * 86400}
	for _, tc := range []struct {
		name string
		from types.AgentRun
		want bool
	}{
		{"stale limits", stale, false},
		{"a kept run", kept, false},
		{"a terminal run", finished, false},
		{"a reboot-lost run", rebootLost, true},
		{"the run as read", live, true},
		{"the same run again", live, false},
	} {
		got, err := pg.ReclampRunLimits(ctx, tc.from, tight, &cut, 600, &now)
		if err != nil || got != tc.want {
			t.Errorf("%s: ReclampRunLimits = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	got, err := pg.GetRun(ctx, live.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.RunLimits != tight || got.EndsAt == nil || !got.EndsAt.Equal(cut) || got.WaitBudgetSec != 600 ||
		got.EndTightenedAt == nil || !got.EndTightenedAt.Equal(now) {
		t.Fatalf("run = limits %+v ends %v wait %d tightened %v; want %+v, %v, 600, %v",
			got.RunLimits, got.EndsAt, got.WaitBudgetSec, got.EndTightenedAt, tight, cut, now)
	}

	// A PATCH decided against the limits before the re-clamp cannot land.
	if ok, err := pg.SetRunEndAndWait(ctx, live.ID, limits, &cut, 600, &end, 600); err != nil || ok {
		t.Errorf("PATCH against the old limits = %v, %v; want false", ok, err)
	}
	sooner := now.Add(24 * time.Hour)
	if ok, err := pg.SetRunEndAndWait(ctx, live.ID, tight, &cut, 600, &sooner, 600); err != nil || !ok {
		t.Fatalf("PATCH against the current limits = %v, %v; want true", ok, err)
	}
	if got, _ := pg.GetRun(ctx, live.ID); got.EndTightenedAt != nil {
		t.Errorf("end_tightened_at = %v after a person moved the end, want cleared", got.EndTightenedAt)
	}
}
