// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The sweep's read of owed ends leaves out every end whose queued replacement is still waiting (QUEUED,
// PENDING or STARTING) before its limit, and reads the rest in pages that resume after the last end of a
// full page, ends owed in one transaction (one created_at) included: each due end exactly once, then nil to
// start over. The plain list still reads every end.
func TestPG_Workload_DueRunEndsSkipWaitersAndResumeAfterAFullPage(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := t.Context()
	run := func(state types.RunState) uuid.UUID { return persistRun(t, ctx, pool, newRun(state)).ID }
	queued, pending, starting, running, failed := run(types.RunQueued), run(types.RunPending), run(types.RunStarting), run(types.RunRunning), run(types.RunFailed)
	waiters := []uuid.UUID{run(types.RunRunning), run(types.RunRunning), run(types.RunRunning)}
	due := []uuid.UUID{run(types.RunRunning), run(types.RunRunning), run(types.RunRunning), run(types.RunRunning)}
	ids := append(append([]uuid.UUID{}, waiters...), due...)
	afters := []*uuid.UUID{&queued, &pending, &starting, &running, &failed, nil, nil}
	at := time.Now().Add(-time.Hour).Truncate(time.Microsecond) // one created_at for all, as ends owed in one transaction have
	for i, id := range ids {
		if _, err := pool.Exec(ctx, `INSERT INTO workload_run_ends (run_id, owner, name, generation, reason, after_run_id, created_at)
			VALUES ($1, 'sub-a', 'svc', 1, 'workload_replaced', $2, $3)`, id, afters[i], at); err != nil {
			t.Fatal(err)
		}
	}

	read := map[uuid.UUID]int{}
	var after *store.WorkloadRunEndKey
	for page := 1; ; page++ {
		ends, next, err := st.ListDueWorkloadRunEnds(ctx, 0, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ends {
			read[e.RunID]++
		}
		if page <= 2 && (len(ends) != 2 || next == nil || next.RunID != ends[1].RunID) {
			t.Fatalf("page %d = %d ends, next %+v; want a full page and its last end as next", page, len(ends), next)
		}
		if page == 3 {
			if len(ends) != 0 || next != nil {
				t.Fatalf("page 3 = %d ends, next %+v; want none and nil to start over", len(ends), next)
			}
			break
		}
		after = next
	}
	for _, id := range waiters {
		if read[id] != 0 {
			t.Fatalf("an end whose replacement waits was read: %s", id)
		}
	}
	for _, id := range due {
		if read[id] != 1 {
			t.Fatalf("due end %s read %d times, want once", id, read[id])
		}
	}
	if all, err := st.ListWorkloadRunEnds(ctx, 0, 10); err != nil || len(all) != len(ids) {
		t.Fatalf("plain list = %d ends (%v), want all %d", len(all), err, len(ids))
	}
}
