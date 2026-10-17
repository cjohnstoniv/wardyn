// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// Postgres-backed tests for SweepTicks: the shared per-sweep record every
// replica reads. Guarded by WARDYN_TEST_PG like every other *_pg_test.go here.

import (
	"context"
	"testing"
	"time"
)

func TestSweepTicks_RecordAndRead(t *testing.T) {
	ctx := context.Background()
	st := NewSweepTicks(pgPool(t))
	sweep := "test_" + t.Name()
	t.Cleanup(func() { _, _ = st.pool.Exec(ctx, `DELETE FROM sweep_ticks WHERE sweep = $1`, sweep) })

	t0 := time.Now().UTC().Truncate(time.Microsecond)
	if err := st.RecordTick(ctx, sweep, "replica-a", false, t0); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	got, err := st.Ticks(ctx)
	if err != nil {
		t.Fatalf("Ticks: %v", err)
	}
	tk := got[sweep]
	if !tk.AttemptedAt.Equal(t0) || !tk.SucceededAt.IsZero() || tk.Replica != "replica-a" {
		t.Fatalf("after an attempt: %+v, want attempted at %s and no success", tk, t0)
	}

	t1 := t0.Add(time.Second)
	if err := st.RecordTick(ctx, sweep, "replica-b", true, t1); err != nil {
		t.Fatalf("success: %v", err)
	}
	got, _ = st.Ticks(ctx)
	tk = got[sweep]
	if !tk.AttemptedAt.Equal(t1) || !tk.SucceededAt.Equal(t1) || tk.Replica != "replica-b" {
		t.Fatalf("after a success: %+v, want both at %s from replica-b", tk, t1)
	}
}

// An older write does not move a timestamp backwards, for either column, and
// does not take the replica from the newer attempt.
func TestSweepTicks_OlderWriteDoesNotMoveTimeBackwards(t *testing.T) {
	ctx := context.Background()
	st := NewSweepTicks(pgPool(t))
	sweep := "test_" + t.Name()
	t.Cleanup(func() { _, _ = st.pool.Exec(ctx, `DELETE FROM sweep_ticks WHERE sweep = $1`, sweep) })

	newer := time.Now().UTC().Truncate(time.Microsecond)
	older := newer.Add(-time.Hour)
	if err := st.RecordTick(ctx, sweep, "new", true, newer); err != nil {
		t.Fatal(err)
	}
	for _, success := range []bool{false, true} {
		if err := st.RecordTick(ctx, sweep, "old", success, older); err != nil {
			t.Fatalf("older write (success=%v): %v", success, err)
		}
	}
	got, _ := st.Ticks(ctx)
	tk := got[sweep]
	if !tk.AttemptedAt.Equal(newer) || !tk.SucceededAt.Equal(newer) || tk.Replica != "new" {
		t.Fatalf("an older write moved the record: %+v, want both at %s from \"new\"", tk, newer)
	}
}

// An attempt never clears or lowers a recorded success.
func TestSweepTicks_AttemptKeepsSuccess(t *testing.T) {
	ctx := context.Background()
	st := NewSweepTicks(pgPool(t))
	sweep := "test_" + t.Name()
	t.Cleanup(func() { _, _ = st.pool.Exec(ctx, `DELETE FROM sweep_ticks WHERE sweep = $1`, sweep) })

	t0 := time.Now().UTC().Truncate(time.Microsecond)
	if err := st.RecordTick(ctx, sweep, "r", true, t0); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordTick(ctx, sweep, "r", false, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Ticks(ctx)
	tk := got[sweep]
	if !tk.SucceededAt.Equal(t0) || !tk.AttemptedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("got %+v, want success %s kept and attempt %s", tk, t0, t0.Add(time.Minute))
	}
}
