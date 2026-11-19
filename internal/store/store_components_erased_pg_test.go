// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A person's saved component erased after a create read it is refused inside the snapshot's own
// transaction, on the direct path and on the queued one: no run_components row with its content is
// committed, so none can outlive the erasure when the process dies before any later check. Catches a
// snapshot that commits the copy and leaves the erasure check to the caller.
func TestPG_RunComponents_RefuseASavedComponentErasedBeforeTheyCommit(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	alice := "alice-" + uuid.NewString()
	saved := newComponent(t, st, alice, "saved")
	if _, err := st.DeleteComponentsByOwner(ctx, alice); err != nil {
		t.Fatal(err)
	}
	comps := []types.RunComponent{{ComponentID: &saved.ID, Owner: alice, Name: saved.Name, Version: saved.Version,
		Definition: saved.Definition, SelfDefined: true}}

	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	if err := st.PutRunComponents(ctx, run.ID, comps); !errors.Is(err, store.ErrRunComponentGone) {
		t.Errorf("a snapshot of the erased component = %v, want ErrRunComponentGone", err)
	}
	if got, err := st.ListRunComponents(ctx, run.ID); err != nil || len(got) != 0 {
		t.Errorf("the refused snapshot left %+v, %v; want no row", got, err)
	}

	p := queuePool(t, st)
	now := time.Now().UTC()
	queued := poolRun(p, types.RunQueued, alice)
	if _, err := st.CreateQueuedRun(ctx, queued, store.RunQueueEntry{QueuedAt: now, DeadlineAt: now.Add(time.Hour), Components: comps}, 0); !errors.Is(err, store.ErrRunComponentGone) {
		t.Errorf("a queued create copying the erased component = %v, want ErrRunComponentGone", err)
	}
	if _, err := st.GetRun(ctx, queued.ID); !errors.Is(err, store.ErrNotFound) || queueRows(t, pool, queued.ID) != 0 {
		t.Errorf("after the refused queued create: run = %v, %d queue rows; want neither", err, queueRows(t, pool, queued.ID))
	}
}
