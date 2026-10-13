// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_KilledStatusDetail_WrittenWithTheState: the KILLED state and the
// status_detail land in one statement, and a kill whose reason no longer holds on
// the row writes neither. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
func TestPG_KilledStatusDetail_WrittenWithTheState(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	now := time.Now().UTC().Truncate(time.Millisecond)
	ago, ahead := now.Add(-time.Minute), now.Add(time.Hour)

	mk := func(state types.RunState, exp types.RunExperience, end, capAt *time.Time) types.AgentRun {
		r := newFilterRun("detail-"+uuid.NewString(), state)
		r.Experience, r.EndsAt, r.LifetimeCapAt = exp, end, capAt
		if _, err := pg.CreateRun(ctx, r); err != nil {
			t.Fatalf("create: %v", err)
		}
		id := r.ID
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, id) })
		return r
	}
	kill := func(r types.AgentRun, reason types.RunEndReason) bool {
		ok, err := pg.KillRunAtLifetimeIf(ctx, r.ID, reason, "lifetime: "+string(reason), now)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	read := func(r types.AgentRun) types.AgentRun {
		got, err := pg.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	won := mk(types.RunRunning, types.ExperienceBackground, &ago, &ahead)
	if !kill(won, types.RunEndEndReached) {
		t.Fatal("end_reached on a Background run past its end was refused")
	}
	if got := read(won); got.State != types.RunKilled || got.StatusDetail != "lifetime: end_reached" || got.EndedAt == nil {
		t.Errorf("state=%s detail=%q ended_at=%v; want KILLED, the detail and an end time", got.State, got.StatusDetail, got.EndedAt)
	}

	// Each refusal below must leave the row exactly RUNNING with no detail.
	kept := mk(types.RunRunning, types.ExperienceBackground, &ago, &ahead)
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at=$2, lost_reason='ended' WHERE id=$1`, kept.ID, ago); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		run    types.AgentRun
		reason types.RunEndReason
	}{
		"the end moved to the future":      {mk(types.RunRunning, types.ExperienceBackground, &ahead, &ahead), types.RunEndEndReached},
		"an end at the cap is the maximum": {mk(types.RunRunning, types.ExperienceBackground, &ago, &ago), types.RunEndEndReached},
		"Interactive is kept at its end":   {mk(types.RunRunning, types.ExperienceInteractive, &ago, &ahead), types.RunEndEndReached},
		"the cap is still ahead":           {mk(types.RunRunning, types.ExperienceInteractive, nil, &ahead), types.RunEndMaxLifetimeReached},
		"a Background end came first":      {mk(types.RunRunning, types.ExperienceBackground, &ago, &now), types.RunEndMaxLifetimeReached},
		"a run its own end kept":           {kept, types.RunEndEndReached},
		"a completed run":                  {mk(types.RunCompleted, types.ExperienceBackground, &ago, &ahead), types.RunEndEndReached},
	} {
		if kill(tc.run, tc.reason) {
			t.Errorf("%s: the kill was applied", name)
		}
		if got := read(tc.run); got.State == types.RunKilled || got.StatusDetail != "" {
			t.Errorf("%s: state=%s detail=%q; want the row untouched", name, got.State, got.StatusDetail)
		}
	}

	// A cap-only run, and an Interactive run at the cap, are killed max_lifetime_reached.
	for name, r := range map[string]types.AgentRun{
		"cap-only background":    mk(types.RunRunning, types.ExperienceBackground, nil, &ago),
		"interactive at the cap": mk(types.RunRunning, types.ExperienceInteractive, &ago, &ago),
	} {
		if !kill(r, types.RunEndMaxLifetimeReached) {
			t.Errorf("%s: max_lifetime_reached was refused", name)
		}
	}
}
