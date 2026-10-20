// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for the run_harnesses table (migration run_harnesses).
// Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRunHarnesses_PersistedOrAborted is the store half of T11: the rows round-trip
// in request order, a run's rows are written once, at most one row starts, and the
// table's own checks and foreign key refuse what the caller got wrong.
func TestRunHarnesses_PersistedOrAborted(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))

	if got, err := st.ListRunHarnesses(ctx, run.ID); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("a run launched without harnesses lists %#v, %v; want empty and not nil", got, err)
	}

	rows := []types.RunHarness{
		{Harness: "claude-code", ModelProviderID: "prov-a", Starts: true},
		{Harness: "codex-cli", ModelProviderID: "prov-b", Task: "write the tests"},
		{Harness: "opencode"},
	}
	if err := st.PutRunHarnesses(ctx, run.ID, rows); err != nil {
		t.Fatalf("put: %v", err)
	}
	if got, err := st.ListRunHarnesses(ctx, run.ID); err != nil || !reflect.DeepEqual(got, rows) {
		t.Fatalf("rows:\n got %+v (%v)\nwant %+v", got, err, rows)
	}

	// Rows are written once: a second put conflicts and leaves the first set whole.
	if err := st.PutRunHarnesses(ctx, run.ID, []types.RunHarness{{Harness: "other"}}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second put: err = %v, want ErrConflict", err)
	}
	if got, err := st.ListRunHarnesses(ctx, run.ID); err != nil || !reflect.DeepEqual(got, rows) {
		t.Fatalf("rows after a refused second put = %+v, %v; want the first set unchanged", got, err)
	}

	// A run that does not exist is ErrNotFound (the foreign key).
	if err := st.PutRunHarnesses(ctx, uuid.New(), rows[:1]); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("put for a run that does not exist: err = %v, want ErrNotFound", err)
	}

	// An empty set is refused, and is neither of the two store sentinels.
	for _, empty := range [][]types.RunHarness{nil, {}} {
		if err := st.PutRunHarnesses(ctx, run.ID, empty); err == nil || errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
			t.Fatalf("put of %#v: err = %v, want a refusal that is neither ErrNotFound nor ErrConflict", empty, err)
		}
	}

	// Refused sets write nothing, whole: the transaction rolls back every earlier row.
	sixteen := make([]types.RunHarness, 17)
	for i := range sixteen {
		sixteen[i] = types.RunHarness{Harness: fmt.Sprintf("h%d", i)}
	}
	cases := map[string]struct {
		set  []types.RunHarness
		want error
	}{
		"two harnesses that start":  {[]types.RunHarness{{Harness: "a", Starts: true}, {Harness: "b", Starts: true}}, store.ErrConflict},
		"a harness named twice":     {[]types.RunHarness{{Harness: "a"}, {Harness: "a"}}, store.ErrConflict},
		"a harness id with a space": {[]types.RunHarness{{Harness: "a"}, {Harness: "not valid"}}, nil},
		"an empty harness id":       {[]types.RunHarness{{Harness: "a"}, {Harness: ""}}, nil},
		"seventeen harnesses":       {sixteen, nil},
	}
	for name, c := range cases {
		other := persistRun(t, ctx, pool, newRun(types.RunRunning))
		err := st.PutRunHarnesses(ctx, other.ID, c.set)
		switch {
		case c.want != nil && !errors.Is(err, c.want):
			t.Fatalf("put %s: err = %v, want %v", name, err, c.want)
		case c.want == nil && (err == nil || errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict)):
			t.Fatalf("put %s: err = %v, want the CHECK to refuse it", name, err)
		}
		if got, err := st.ListRunHarnesses(ctx, other.ID); err != nil || len(got) != 0 {
			t.Fatalf("put %s left %+v, %v; want nothing written", name, got, err)
		}
	}

	// The same harness set on another run is no conflict: uniqueness is per run.
	third := persistRun(t, ctx, pool, newRun(types.RunRunning))
	if err := st.PutRunHarnesses(ctx, third.ID, rows); err != nil {
		t.Fatalf("the same set on another run: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM agent_runs WHERE id = $1`, run.ID); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_harnesses WHERE run_id = $1`, run.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("rows left after the run was deleted = %d, %v; want 0 (ON DELETE CASCADE)", left, err)
	}
}
