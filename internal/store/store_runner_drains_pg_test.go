// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sameDrain compares two drains, the time by instant: the database returns it in its own zone.
func sameDrain(a, b types.RunnerDrain) bool {
	return a.CreatedBy == b.CreatedBy && a.Reason == b.Reason && a.CreatedAt.Equal(b.CreatedAt)
}

// A drain is set once and kept as first written, listed by runner and deleted by an undrain. A revoke leaves
// the row, inert; a runner that is not claimed cannot be drained.
func TestRunnerDrains_RevokeLeavesTheRowInert_PG(t *testing.T) {
	ctx := t.Context()
	st := store.NewPG(runsPGPoolIsolated(t))
	id := claimedRunnerOf(t, st, "owner")
	first := types.RunnerDrain{CreatedBy: "alice", CreatedAt: time.Now().UTC().Truncate(time.Microsecond), Reason: "node pool upgrade"}
	stored, set, err := st.DrainRunner(ctx, id, first)
	if err != nil || !set || !sameDrain(stored, first) {
		t.Fatalf("DrainRunner = %+v set %v, %v; want the drain as written", stored, set, err)
	}
	stored, set, err = st.DrainRunner(ctx, id, types.RunnerDrain{CreatedBy: "bob", CreatedAt: first.CreatedAt.Add(time.Hour), Reason: "later"})
	if err != nil || set || !sameDrain(stored, first) {
		t.Fatalf("a second DrainRunner = %+v set %v, %v; want the first drain unchanged", stored, set, err)
	}
	if got, err := st.RunnerDrains(ctx, []uuid.UUID{id, uuid.New()}); err != nil || len(got) != 1 || !sameDrain(got[id], first) {
		t.Fatalf("RunnerDrains = %v, %v; want the one drain", got, err)
	}
	if _, err := st.RevokeRunner(ctx, id, time.Now(), "alice", "gone"); err != nil {
		t.Fatal(err)
	}
	if got, err := st.RunnerDrains(ctx, []uuid.UUID{id}); err != nil || !sameDrain(got[id], first) {
		t.Fatalf("RunnerDrains after a revoke = %v, %v; want the row kept", got, err)
	}
	was, cleared, err := st.UndrainRunner(ctx, id)
	if err != nil || !cleared || !sameDrain(was, first) {
		t.Fatalf("UndrainRunner = %+v cleared %v, %v; want the drain back", was, cleared, err)
	}
	if _, cleared, err := st.UndrainRunner(ctx, id); err != nil || cleared {
		t.Fatalf("a second UndrainRunner cleared %v, %v; want none", cleared, err)
	}
	unclaimed, err := st.CreateRunner(ctx, newRunner("owner"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.DrainRunner(ctx, unclaimed.ID, first); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DrainRunner on an unclaimed runner = %v, want ErrNotFound", err)
	}
}
