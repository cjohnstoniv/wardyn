// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A runner's health is one row, overwritten: a later summary replaces the earlier one only once the gap
// passed, a relay status alone keeps the summary, and a summary past the size cap is refused.
func TestPG_RunnerHealthOneRowPerRunnerAndCapped(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	r := newRunner("alice")
	if _, err := pg.CreateRunner(ctx, r); err != nil {
		t.Fatal(err)
	}
	if s, rel, err := pg.RunnerHealth(ctx, r.ID); s != nil || rel != nil || err != nil {
		t.Fatalf("a runner that reported nothing: %v %v %v", s, rel, err)
	}
	t0 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	sum := func(reason string) *types.RunnerOfflineSummary {
		return &types.RunnerOfflineSummary{Stage: types.RunnerStageDNS, Reason: reason, FirstFailureAt: t0, LastFailureAt: t0}
	}
	const gap = 10 * time.Second
	if ok, err := pg.RecordRunnerHealth(ctx, r.ID, sum("first"), nil, t0, gap); !ok || err != nil {
		t.Fatalf("first summary stored = %v, %v", ok, err)
	}
	if ok, err := pg.RecordRunnerHealth(ctx, r.ID, sum("too soon"), nil, t0.Add(gap-time.Second), gap); ok || err != nil {
		t.Fatalf("a summary inside the gap stored = %v, %v", ok, err)
	}
	relay := &types.RunnerRelayStatus{State: types.RelayUnreachable, Since: t0, Reason: "nothing answers"}
	if ok, err := pg.RecordRunnerHealth(ctx, r.ID, nil, relay, t0.Add(time.Second), gap); ok || err != nil {
		t.Fatalf("a relay status alone = %v, %v", ok, err)
	}
	if ok, err := pg.RecordRunnerHealth(ctx, r.ID, sum("second"), nil, t0.Add(gap), gap); !ok || err != nil {
		t.Fatalf("a summary after the gap stored = %v, %v", ok, err)
	}
	s, rel, err := pg.RunnerHealth(ctx, r.ID)
	if err != nil || s == nil || s.Reason != "second" || rel == nil || rel.State != types.RelayUnreachable {
		t.Fatalf("health = %+v %+v %v; want the second summary and the relay status kept", s, rel, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runner_health WHERE runner_id = $1`, r.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("%d rows for one runner, %v", rows, err)
	}
	big := sum(strings.Repeat("<", types.MaxRunnerReason))
	if _, err := pg.RecordRunnerHealth(ctx, r.ID, big, nil, t0.Add(time.Hour), gap); !errors.Is(err, store.ErrRunnerHealthTooLarge) {
		t.Fatalf("a summary past the cap: %v, want ErrRunnerHealthTooLarge", err)
	}
	if s, _, _ := pg.RunnerHealth(ctx, r.ID); s == nil || s.Reason != "second" {
		t.Fatalf("a refused summary replaced the stored one: %+v", s)
	}
}
