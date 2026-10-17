// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// RunnerArchCounts counts live runners per reported architecture: a revoked runner and one that reported
// none are left out.
func TestPG_RunnerArchCounts(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	if got, err := pg.RunnerArchCounts(ctx); err != nil || len(got) != 0 {
		t.Fatalf("no runners = %v, %v; want empty", got, err)
	}
	for _, arch := range []string{"arm64", "arm64", "amd64", ""} {
		r := newRunner("alice")
		r.Arch = arch
		if _, err := pg.CreateRunner(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	revoked := newRunner("alice")
	revoked.Arch = "arm64"
	if _, err := pg.CreateRunner(ctx, revoked); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.RevokeRunner(ctx, revoked.ID, time.Now().UTC(), "alice", ""); err != nil {
		t.Fatal(err)
	}
	got, err := pg.RunnerArchCounts(ctx)
	if err != nil || len(got) != 2 || got["arm64"] != 2 || got["amd64"] != 1 {
		t.Fatalf("counts = %v, %v; want arm64 2 and amd64 1", got, err)
	}
}
