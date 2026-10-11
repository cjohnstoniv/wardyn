// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_KilledStatusDetail_WrittenWithTheState: the KILLED state and the
// status_detail land together, and a compare-and-set that loses writes neither.
// Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
func TestPG_KilledStatusDetail_WrittenWithTheState(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	won := newFilterRun("detail-win-"+uuid.NewString(), types.RunRunning)
	lost := newFilterRun("detail-lost-"+uuid.NewString(), types.RunCompleted)
	for _, r := range []types.AgentRun{won, lost} {
		if _, err := pg.CreateRun(ctx, r); err != nil {
			t.Fatalf("create: %v", err)
		}
		id := r.ID
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, id) })
	}

	applied, err := pg.UpdateRunStateAndDetailIf(ctx, won.ID, types.RunRunning, types.RunKilled, "lifetime: end_reached")
	if err != nil || !applied {
		t.Fatalf("winning CAS = %v, %v", applied, err)
	}
	got, err := pg.GetRun(ctx, won.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.RunKilled || got.StatusDetail != "lifetime: end_reached" || got.EndedAt == nil {
		t.Errorf("state=%s detail=%q ended_at=%v; want KILLED, the detail and an end time", got.State, got.StatusDetail, got.EndedAt)
	}

	applied, err = pg.UpdateRunStateAndDetailIf(ctx, lost.ID, types.RunRunning, types.RunKilled, "lifetime: end_reached")
	if err != nil || applied {
		t.Fatalf("losing CAS = %v, %v; want not applied", applied, err)
	}
	got, err = pg.GetRun(ctx, lost.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.RunCompleted || got.StatusDetail != "" {
		t.Errorf("a lost CAS left state=%s detail=%q; want the completed run untouched", got.State, got.StatusDetail)
	}
}
