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

// gpu round-trips through the insert, GetRun and the list; a run with none reads back empty; the column
// refuses an unknown kind; and the migration restricts the `gpu` feature value.
func TestPG_RunGPURoundTrip(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	marker := "gpu-" + uuid.NewString()
	withGPU := newRun(types.RunStarting)
	withGPU.CreatedBy = marker
	withGPU.GPU = types.GPUNvidia
	created := persistRun(t, ctx, pool, withGPU)
	if created.GPU != types.GPUNvidia {
		t.Fatalf("created run gpu = %q, want nvidia", created.GPU)
	}
	none := newRun(types.RunStarting)
	none.CreatedBy = marker
	noneRun := persistRun(t, ctx, pool, none)
	if noneRun.GPU != types.GPUNone {
		t.Fatalf("run with no gpu reads %q, want empty", noneRun.GPU)
	}

	got, err := pg.GetRun(ctx, created.ID)
	if err != nil || got.GPU != types.GPUNvidia {
		t.Fatalf("GetRun gpu = %q, %v; want nvidia", got.GPU, err)
	}
	listed, err := pg.ListRunsPageByCreator(ctx, marker, store.Page{Limit: 10}, nil)
	if err != nil || len(listed) != 2 {
		t.Fatalf("list = %d runs, %v; want 2", len(listed), err)
	}
	for _, r := range listed {
		want := types.GPUNone
		if r.ID == created.ID {
			want = types.GPUNvidia
		}
		if r.GPU != want {
			t.Errorf("listed run %s gpu = %q, want %q", r.ID, r.GPU, want)
		}
	}

	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET gpu='cuda' WHERE id=$1`, created.ID); err == nil {
		t.Fatal("agent_runs took an unknown gpu kind")
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM capability_restrictions WHERE capability='feature' AND value='gpu'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("capability_restrictions rows for feature/gpu = %d, %v; want 1", n, err)
	}
}
