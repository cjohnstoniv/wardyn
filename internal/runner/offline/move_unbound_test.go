// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"testing"

	"github.com/google/uuid"
)

// TestMoveUnbound_OmittedFromState: a run whose sandbox the organisation says was moved away is left out
// of the run state; every other run stays in it, and a notice for an unknown run or another sandbox of the
// run changes nothing. The mark is durable.
func TestMoveUnbound_OmittedFromState(t *testing.T) {
	m, _, _, org := managerFixture(t)
	ctx := t.Context()
	moved := createKind(t, m, org, "")
	kept := createKind(t, m, org, "")
	if present := presentOf(t, m); len(present) != 2 {
		t.Fatalf("before any notice the state holds %d runs; want 2", len(present))
	}
	if drained, err := m.SpoolDrained(ctx, moved.String()); err != nil || !drained {
		t.Fatalf("SpoolDrained of a run with no decisions = %v, %v; want true", drained, err)
	}

	// Notices that name nothing this runner holds change nothing.
	if err := m.MoveUnbound(ctx, uuid.New(), "nobody"); err != nil {
		t.Fatalf("a notice for an unknown run = %v; want nil", err)
	}
	if err := m.MoveUnbound(ctx, kept, "another-sandbox"); err != nil {
		t.Fatalf("a notice for another sandbox of a run = %v; want nil", err)
	}
	if present := presentOf(t, m); len(present) != 2 {
		t.Fatalf("after notices naming nothing held the state holds %d runs; want 2", len(present))
	}

	if err := m.MoveUnbound(ctx, moved, moved.String()); err != nil {
		t.Fatal(err)
	}
	present := presentOf(t, m)
	if _, ok := present[moved]; ok {
		t.Error("a run moved away is still in the run state")
	}
	if _, ok := present[kept]; !ok {
		t.Error("a run that was not moved left the run state")
	}
	if err := m.MoveUnbound(ctx, moved, moved.String()); err != nil {
		t.Fatalf("a repeated notice = %v; want nil", err)
	}

	// The mark is written with the run's record, so a restart keeps it.
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := New(m.dir, m.runnerID, m.sub, uuid.UUID.String, noOutput, noErase)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Close() })
	if got := again.state.Runs[moved].MovedAway; got != moved.String() {
		t.Errorf("after a restart the mark reads %q; want %q", got, moved.String())
	}
}
