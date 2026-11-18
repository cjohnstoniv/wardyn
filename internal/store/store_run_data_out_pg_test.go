// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for agent_runs.data_out: the run's data_out rule, kept on its row where audit
// retention cannot drop it. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRunDataOut_RaisedNeverLowered: a run starts with no rule on its row, the first write reads back through
// GetRun ("" as allowed), a stricter rule raises it, a looser one leaves it, an unknown rule is refused, and a
// run the table does not hold is ErrNotFound.
func TestRunDataOut_RaisedNeverLowered(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	r := newRun(types.RunPending)
	persistRun(t, ctx, pool, r)

	read := func() *types.TransferRule {
		t.Helper()
		got, err := pg.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		return got.DataOut
	}
	if got := read(); got != nil {
		t.Fatalf("before any write: %q, want none", *got)
	}
	for _, step := range []struct {
		rule, want types.TransferRule
	}{
		{"", types.TransferAllowed},
		{types.TransferReview, types.TransferReview},
		{types.TransferAllowed, types.TransferReview},
		{types.TransferBlocked, types.TransferBlocked},
		{types.TransferReview, types.TransferBlocked},
	} {
		if err := pg.RaiseRunDataOut(ctx, r.ID, step.rule); err != nil {
			t.Fatalf("raise to %q: %v", step.rule, err)
		}
		if got := read(); got == nil || *got != step.want {
			t.Errorf("after raising to %q: %v, want %q", step.rule, got, step.want)
		}
	}
	if err := pg.RaiseRunDataOut(ctx, r.ID, "open"); err == nil {
		t.Error("an unknown rule was stored")
	}
	if err := pg.RaiseRunDataOut(ctx, uuid.New(), types.TransferBlocked); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an absent run = %v, want ErrNotFound", err)
	}
}
