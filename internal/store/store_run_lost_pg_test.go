// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_RunLost pins migration 0083 through the lost-run surface: a new row
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
	if ok, err := pg.StampRunTokenRenewed(ctx, lapsed.ID, "jti-new", "jti-presented"); err != nil || !ok {
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
	if ok, err := pg.StampRunTokenRenewed(ctx, lapsed.ID, "", ""); err != nil || ok {
		t.Errorf("StampRunTokenRenewed of a kept run = %v, %v; want false — a lost run never renews", ok, err)
	}

	if got, err := pg.MarkRunLost(ctx, fresh.ID, types.LostReboot, now, 0); err != nil || !got {
		t.Errorf("MarkRunLost(reboot) of a fresh run = %v, %v; want true — a reboot has no token condition", got, err)
	}
}

// TestPG_RunTokenLineage: a revive's cut returns the ids the last renewal recorded and from then on
// a renewal presenting one of them is refused (and records nothing) while the fresh token renews;
// a restore hands the old lineage back only while the fresh token still heads it alone. A run never
// renewed has no lineage to check.
func TestPG_RunTokenLineage(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	run := newRun(types.RunRunning)
	persistRun(t, ctx, pool, run)

	if ok, err := pg.StampRunTokenRenewed(ctx, run.ID, "jti-1", "jti-dispatch"); err != nil || !ok {
		t.Fatalf("first renewal = %v, %v; want it recorded", ok, err)
	}
	if ok, err := pg.StampRunTokenRenewed(ctx, run.ID, "jti-2", "jti-1"); err != nil || !ok {
		t.Fatalf("renewal presenting the last minted token = %v, %v; want it recorded", ok, err)
	}
	jti, prev, err := pg.RetireRunTokenLineage(ctx, run.ID, "jti-fresh")
	if err != nil || jti != "jti-2" || prev != "jti-1" {
		t.Fatalf("RetireRunTokenLineage = %q, %q, %v; want jti-2, jti-1", jti, prev, err)
	}
	for _, presented := range []string{"jti-2", "jti-1"} {
		if ok, err := pg.StampRunTokenRenewed(ctx, run.ID, "jti-escaped", presented); ok || !errors.Is(err, store.ErrRunTokenLineage) {
			t.Errorf("old-lineage renewal presenting %s = %v, %v; want ErrRunTokenLineage", presented, ok, err)
		}
	}
	if err := pg.RestoreRunTokenLineage(ctx, run.ID, "jti-other", jti, prev); err != nil {
		t.Fatal(err)
	}
	if ok, err := pg.StampRunTokenRenewed(ctx, run.ID, "jti-escaped", "jti-2"); !errors.Is(err, store.ErrRunTokenLineage) {
		t.Errorf("a restore naming another fresh token handed the lineage back: %v, %v", ok, err)
	}
	if ok, err := pg.StampRunTokenRenewed(ctx, run.ID, "jti-fresh-1", "jti-fresh"); err != nil || !ok {
		t.Fatalf("the fresh token's renewal = %v, %v; want it recorded", ok, err)
	}
	if err := pg.RestoreRunTokenLineage(ctx, run.ID, "jti-fresh", jti, prev); err != nil {
		t.Fatal(err)
	}
	if ok, err := pg.StampRunTokenRenewed(ctx, run.ID, "jti-escaped", "jti-2"); !errors.Is(err, store.ErrRunTokenLineage) {
		t.Errorf("a restore after the fresh token renewed handed the lineage back: %v, %v", ok, err)
	}

	kept := newRun(types.RunRunning)
	persistRun(t, ctx, pool, kept)
	if _, _, err := pg.RetireRunTokenLineage(ctx, kept.ID, "kept-fresh"); err != nil {
		t.Fatal(err)
	}
	if err := pg.RestoreRunTokenLineage(ctx, kept.ID, "kept-fresh", "kept-2", "kept-1"); err != nil {
		t.Fatal(err)
	}
	if ok, err := pg.StampRunTokenRenewed(ctx, kept.ID, "kept-3", "kept-2"); err != nil || !ok {
		t.Errorf("the kept proxy's renewal after a restore = %v, %v; want it recorded", ok, err)
	}
}
