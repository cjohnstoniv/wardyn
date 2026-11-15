// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration test for SetEndedRunFailureHint, the failure-hint write for a run that is over. Guarded by
// WARDYN_TEST_PG.
package store_test

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A late hint lands on a run that is over and leaves updated_at, which the run's tail-upload grace is
// measured from, where it was; SetRunFailureHint beside it still moves the column.
func TestSetEndedRunFailureHint_LeavesUpdatedAt(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)

	run := persistRun(t, ctx, pool, newRun(types.RunFailed))
	before, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	const hint = "Amazon Bedrock throttled this run's model calls."
	if err := s.SetEndedRunFailureHint(ctx, run.ID, hint); err != nil {
		t.Fatalf("SetEndedRunFailureHint: %v", err)
	}
	after, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun after set: %v", err)
	}
	if after.FailureHint != hint || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("failure_hint %q, updated_at %v -> %v; want the hint written and updated_at unmoved",
			after.FailureHint, before.UpdatedAt, after.UpdatedAt)
	}
	if err := s.SetRunFailureHint(ctx, run.ID, hint); err != nil {
		t.Fatalf("SetRunFailureHint: %v", err)
	}
	if bumped, err := s.GetRun(ctx, run.ID); err != nil || !bumped.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("SetRunFailureHint left updated_at at %v (err %v); the pin above would not tell the two writes apart",
			bumped.UpdatedAt, err)
	}
}
