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

// TestPG_RunLost pins migration 0076 through the lost-run surface: a new row
// starts with a fresh token stamp, the sweep lists only RUNNING, unkept runs
// whose stamp is older than the token's life, a renew's stamp takes a run out
// of that list and wins against a mark that raced it, and a kept run can be
// neither stamped nor marked again.
func TestPG_RunLost(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	const life = time.Hour

	lapsed := newRun(types.RunRunning)
	fresh := newRun(types.RunRunning)
	finished := newRun(types.RunCompleted)
	for _, r := range []types.AgentRun{lapsed, fresh, finished} {
		persistRun(t, ctx, pool, r)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET token_renewed_at = now() - interval '2 hours'
		WHERE id = ANY($1)`, []uuid.UUID{lapsed.ID, finished.ID}); err != nil {
		t.Fatalf("age the token stamps: %v", err)
	}

	listed := func() []uuid.UUID {
		t.Helper()
		runs, err := pg.ListLapsedTokenRuns(ctx, life)
		if err != nil {
			t.Fatalf("ListLapsedTokenRuns: %v", err)
		}
		var ids []uuid.UUID
		for _, r := range runs {
			ids = append(ids, r.ID)
		}
		return ids
	}
	if got := listed(); !slices.Contains(got, lapsed.ID) || slices.Contains(got, fresh.ID) || slices.Contains(got, finished.ID) {
		t.Fatalf("listed %v; want the lapsed RUNNING run only (not the fresh one %s, not the terminal one %s)",
			got, fresh.ID, finished.ID)
	}

	// The token condition: a run with a fresh stamp cannot be marked lost to an
	// outage, while the same run can be marked lost to a reboot.
	if got, err := pg.MarkRunLost(ctx, fresh.ID, types.LostOutage, now, life); err != nil || got {
		t.Errorf("MarkRunLost(outage) of a fresh run = %v, %v; want false", got, err)
	}
	if got, err := pg.MarkRunLost(ctx, finished.ID, types.LostOutage, now, life); err != nil || got {
		t.Errorf("MarkRunLost of a terminal run = %v, %v; want false", got, err)
	}

	// A renew landing before the mark wins: the stamp clears the lapse.
	if ok, err := pg.StampRunTokenRenewed(ctx, lapsed.ID); err != nil || !ok {
		t.Fatalf("StampRunTokenRenewed = %v, %v; want true", ok, err)
	}
	if got := listed(); slices.Contains(got, lapsed.ID) {
		t.Error("a just-renewed run is still listed as lapsed")
	}
	if got, err := pg.MarkRunLost(ctx, lapsed.ID, types.LostOutage, now, life); err != nil || got {
		t.Errorf("MarkRunLost(outage) after a renew = %v, %v; want false", got, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET token_renewed_at = now() - interval '2 hours' WHERE id=$1`, lapsed.ID); err != nil {
		t.Fatalf("age the stamp again: %v", err)
	}
	if got, err := pg.MarkRunLost(ctx, lapsed.ID, types.LostOutage, now, life); err != nil || !got {
		t.Fatalf("MarkRunLost(outage) of a lapsed run = %v, %v; want true", got, err)
	}
	if got, err := pg.MarkRunLost(ctx, lapsed.ID, types.LostReboot, now, 0); err != nil || got {
		t.Errorf("a second MarkRunLost = %v, %v; want false", got, err)
	}
	run, err := pg.GetRun(ctx, lapsed.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.LostAt == nil || !run.LostAt.Equal(now) || run.LostReason != types.LostOutage || run.State != types.RunRunning {
		t.Errorf("lost run = lost %v %q state %s; want lost at %v, outage, still RUNNING", run.LostAt, run.LostReason, run.State, now)
	}
	if got := listed(); slices.Contains(got, lapsed.ID) {
		t.Error("a kept run is still listed as lapsed")
	}
	if ok, err := pg.StampRunTokenRenewed(ctx, lapsed.ID); err != nil || ok {
		t.Errorf("StampRunTokenRenewed of a kept run = %v, %v; want false — a lost run never renews", ok, err)
	}

	if got, err := pg.MarkRunLost(ctx, fresh.ID, types.LostReboot, now, 0); err != nil || !got {
		t.Errorf("MarkRunLost(reboot) of a fresh run = %v, %v; want true — a reboot has no token condition", got, err)
	}
}
