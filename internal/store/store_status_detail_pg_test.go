// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration test for agent_runs.status_detail (migration 0063) — the one
// column a STARTING run's substrate reason is written to, and the one scoped
// UPDATE in the tree that must NOT bump updated_at. Guarded by WARDYN_TEST_PG.
package store_test

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestSetRunStatusDetail_RoundTripsAndDoesNotTouchUpdatedAt pins both halves of
// the write.
//
// The round trip is the ordinary half. The updated_at half is the one that would
// have been a real defect: agent_runs.updated_at is the clock the idle reaper
// measures idleness by AND the clock the killed-run tail-upload grace is
// measured from (TouchRun's doc comment states both). A status heartbeat firing
// every time the kubelet changes its mind would silently extend both windows —
// a diagnostic line quietly buying a run more life is exactly the coupling
// TouchRun's own terminal-run guard exists to prevent.
func TestSetRunStatusDetail_RoundTripsAndDoesNotTouchUpdatedAt(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)

	run := persistRun(t, ctx, pool, newRun(types.RunStarting))
	if run.StatusDetail != "" {
		t.Fatalf("a fresh run's status_detail = %q, want empty", run.StatusDetail)
	}
	before, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	const detail = "agent: ImagePullBackOff: rpc error: code = Unknown desc = pull access denied"
	if err := s.SetRunStatusDetail(ctx, run.ID, detail); err != nil {
		t.Fatalf("SetRunStatusDetail: %v", err)
	}
	after, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun after set: %v", err)
	}
	if after.StatusDetail != detail {
		t.Errorf("status_detail = %q, want %q", after.StatusDetail, detail)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("updated_at moved %v -> %v on a status write; the idle reaper and the tail-upload grace both measure from it",
			before.UpdatedAt, after.UpdatedAt)
	}

	// Overwrite: the column carries the LAST reason, not an append.
	const second = "agent: ContainerCreating"
	if err := s.SetRunStatusDetail(ctx, run.ID, second); err != nil {
		t.Fatalf("SetRunStatusDetail (second): %v", err)
	}
	again, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun after second set: %v", err)
	}
	if again.StatusDetail != second {
		t.Errorf("status_detail = %q, want %q", again.StatusDetail, second)
	}
	if !again.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("updated_at moved on the second status write: %v -> %v", before.UpdatedAt, again.UpdatedAt)
	}
}
