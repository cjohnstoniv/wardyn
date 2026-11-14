// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// UnitModelUsage sums the usage of the runs frozen to the units and created in [since, until), grouped by
// unit, vendor, host and model; UnitOpaqueTunnels counts their llm.scan.bypass rows the same way.
func TestModelUsageForUnitsPG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	var reader store.UnitModelUsageReader = pg
	u1, u2, u3 := uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC().Truncate(time.Second)

	runAt := func(unit *uuid.UUID, created time.Time) uuid.UUID {
		t.Helper()
		r := newRun(types.RunRunning)
		r.UnitID = unit
		id := persistRun(t, ctx, pool, r).ID
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET created_at = $2 WHERE id = $1`, id, created); err != nil {
			t.Fatalf("set created_at: %v", err)
		}
		return id
	}
	call := func(run uuid.UUID, host, model string, in, out int64, state, reason string) {
		t.Helper()
		u := egress.ModelUsage{CallID: uuid.New(), Vendor: "anthropic", Host: host, Model: model, Input: in, Output: out, State: state, Reason: reason}
		if _, err := pg.InsertModelUsage(ctx, run, u, now, ""); err != nil {
			t.Fatalf("insert usage: %v", err)
		}
	}
	bypass := func(run uuid.UUID) {
		t.Helper()
		ev := types.AuditEvent{ID: uuid.New(), Time: now, RunID: &run, ActorType: types.ActorAgent, Actor: "proxy", Action: "llm.scan.bypass", Outcome: "success"}
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatalf("seed bypass: %v", err)
		}
	}
	r1, r1b := runAt(&u1, now.Add(-time.Hour)), runAt(&u1, now.Add(-2*time.Hour))
	r2 := runAt(&u2, now.Add(-time.Hour))
	r3 := runAt(&u3, now.Add(-time.Hour))
	old := runAt(&u1, now.Add(-100*24*time.Hour))
	none := runAt(nil, now.Add(-time.Hour))

	call(r1, "api.anthropic.com", "claude-x", 10, 5, "counted", "")
	call(r1b, "api.anthropic.com", "claude-x", 1, 2, "counted", "")
	call(r1, "api.anthropic.com", "claude-y", 4, 0, "partial", "stream_cut")
	call(r1, "api.openai.com", "gpt-x", 0, 0, "uncounted", "stream_without_usage")
	call(r1b, "api.openai.com", "gpt-x", 0, 0, "uncounted", "stream_without_usage")
	call(r2, "api.anthropic.com", "claude-x", 100, 100, "counted", "")
	call(r3, "api.anthropic.com", "claude-x", 5000, 5000, "counted", "")
	call(old, "api.anthropic.com", "claude-x", 7000, 7000, "counted", "")
	call(none, "api.anthropic.com", "claude-x", 9000, 9000, "counted", "")
	bypass(r1)
	bypass(r1b)
	bypass(r2)
	bypass(r3)
	bypass(old)

	units := []uuid.UUID{u1, u2}
	since, until := now.Add(-30*24*time.Hour), now
	got, err := reader.UnitModelUsage(ctx, units, since, until)
	if err != nil {
		t.Fatalf("UnitModelUsage: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("usage for %d units, want u1 and u2 alone: %v", len(got), got)
	}
	byModel := func(s store.ModelUsageSummary, host, model string) (store.ModelUsageGroup, bool) {
		for _, g := range s.Groups {
			if g.Host == host && g.Model == model {
				return g, true
			}
		}
		return store.ModelUsageGroup{}, false
	}
	if g, ok := byModel(got[u1], "api.anthropic.com", "claude-x"); !ok || g.Input != 11 || g.Output != 7 || g.Calls != 2 {
		t.Errorf("u1 claude-x = %+v, want the two recent runs' 11 in, 7 out, 2 calls and not the old run's", g)
	}
	if g, ok := byModel(got[u1], "api.anthropic.com", "claude-y"); !ok || g.PartialCalls != 1 {
		t.Errorf("u1 claude-y = %+v, want one partial call", g)
	}
	if g, ok := byModel(got[u1], "api.openai.com", "gpt-x"); !ok || g.UncountedCalls != 2 || g.UncountedReasons["stream_without_usage"] != 2 {
		t.Errorf("u1 gpt-x = %+v, want two uncounted calls with their reason", g)
	}
	if g, ok := byModel(got[u2], "api.anthropic.com", "claude-x"); !ok || g.Input != 100 || len(got[u2].Groups) != 1 {
		t.Errorf("u2 = %+v, want its one run's usage", got[u2])
	}
	if got[u1].UpdatedAt == nil {
		t.Error("u1 has no UpdatedAt")
	}
	if _, ok := got[u3]; ok {
		t.Error("u3, which was not asked for, is summed")
	}

	tun, err := reader.UnitOpaqueTunnels(ctx, units, since, until)
	if err != nil {
		t.Fatalf("UnitOpaqueTunnels: %v", err)
	}
	if tun[u1] != 2 || tun[u2] != 1 || len(tun) != 2 {
		t.Errorf("tunnels = %v, want u1 2 (the old run's is outside the window) and u2 1", tun)
	}

	wide, err := reader.UnitModelUsage(ctx, []uuid.UUID{u1}, now.Add(-200*24*time.Hour), now)
	if err != nil {
		t.Fatalf("wide window: %v", err)
	}
	if g, _ := byModel(wide[u1], "api.anthropic.com", "claude-x"); g.Input != 7011 || g.Calls != 3 {
		t.Errorf("a 200-day window sums u1 claude-x to %+v, want 7011 in over 3 calls", g)
	}
	if empty, err := reader.UnitModelUsage(ctx, nil, since, until); err != nil || len(empty) != 0 {
		t.Errorf("no units = %v (%v), want nothing", empty, err)
	}
	if edge, err := reader.UnitModelUsage(ctx, units, now.Add(-time.Hour), now); err != nil || len(edge[u2].Groups) != 1 {
		t.Errorf("a window starting at a run's creation time = %v (%v), want that run counted (since is inclusive)", edge, err)
	}
	if edge, err := reader.UnitModelUsage(ctx, units, since, now.Add(-time.Hour)); err != nil || len(edge[u2].Groups) != 0 {
		t.Errorf("a window ending at a run's creation time = %v (%v), want that run left out (until is exclusive)", edge, err)
	}
}

// A unit whose calls add up past an int64 reads them as not counted, instead of failing the whole read.
func TestModelUsageForUnitsPastInt64PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	unit := uuid.New()
	now := time.Now().UTC().Truncate(time.Second)
	r := newRun(types.RunRunning)
	r.UnitID = &unit
	id := persistRun(t, ctx, pool, r).ID
	for _, in := range []int64{math.MaxInt64, 1} {
		u := egress.ModelUsage{CallID: uuid.New(), Vendor: "anthropic", Host: "api.anthropic.com", Model: "claude-x", Input: in, Output: 1, State: "counted"}
		if _, err := pg.InsertModelUsage(ctx, id, u, now, ""); err != nil {
			t.Fatalf("insert usage: %v", err)
		}
	}
	got, err := pg.UnitModelUsage(ctx, []uuid.UUID{unit}, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || len(got[unit].Groups) != 1 {
		t.Fatalf("UnitModelUsage = %v, %v", got, err)
	}
	if g := got[unit].Groups[0]; g.Input != 0 || g.Output != 0 || g.Calls != 2 || g.UncountedCalls != 2 || g.UncountedReasons[store.UncountedReasonOutOfRange] != 2 {
		t.Errorf("unit claude-x = %+v, want 2 calls not counted", g)
	}
}
