// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// unitAuditReads is the unit-scoped audit read the test drives, asserted at run time.
type unitAuditReads interface {
	QueryAuditEventsForUnits(ctx context.Context, units []uuid.UUID, f store.AuditFilter, p store.Page) ([]types.AuditEvent, error)
}

// The unit read serves the rows of runs frozen to one of the units, newest first, with the filter applied
// on top; a run with no unit, a run of another unit and a row with no run never match.
func TestQueryAuditEventsForUnitsPG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	reads, ok := any(pg).(unitAuditReads)
	if !ok {
		t.Fatal("store.PG has no unit-scoped audit read")
	}

	u1, u2, u3 := uuid.New(), uuid.New(), uuid.New()
	runIn := func(unit *uuid.UUID) uuid.UUID {
		r := newRun(types.RunRunning)
		r.UnitID = unit
		return persistRun(t, ctx, pool, r).ID
	}
	r1, r2, r3, rNone := runIn(&u1), runIn(&u2), runIn(&u3), runIn(nil)
	want := map[uuid.UUID]bool{}
	seed := func(run *uuid.UUID, action, outcome string) {
		t.Helper()
		ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: run, ActorType: types.ActorHuman,
			Actor: "alice@corp.example", Action: action, Outcome: outcome}
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if run != nil && (*run == r1 || *run == r2) {
			want[ev.ID] = true
		}
	}
	for i := range 4 {
		for _, run := range []uuid.UUID{r1, r2, r3, rNone} {
			seed(&run, "unit.probe", "success")
			if i%2 == 0 {
				seed(&run, "unit.other", "failure")
			}
		}
		seed(nil, "unit.probe", "success")
	}

	units := []uuid.UUID{u1, u2, uuid.Nil}
	all, err := reads.QueryAuditEventsForUnits(ctx, units, store.AuditFilter{}, store.Page{Limit: 1000})
	if err != nil {
		t.Fatalf("unit read: %v", err)
	}
	if len(all) != len(want) {
		t.Errorf("unit read returned %d rows, want the %d of the units' runs", len(all), len(want))
	}
	for _, ev := range all {
		if !want[ev.ID] {
			t.Errorf("unit read returned %s of run %v, outside the units", ev.Action, ev.RunID)
		}
	}
	recent, err := pg.QueryRecentAuditEventsPage(ctx, store.Page{})
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	var order []uuid.UUID
	for _, ev := range recent {
		if want[ev.ID] {
			order = append(order, ev.ID)
		}
	}
	if !slices.Equal(auditIDs(all), order) {
		t.Errorf("unit read is not newest first")
	}

	f := store.AuditFilter{Action: "unit.other", Outcome: "failure"}
	filtered, err := reads.QueryAuditEventsForUnits(ctx, units, f, store.Page{Limit: 1000})
	if err != nil {
		t.Fatalf("filtered unit read: %v", err)
	}
	if len(filtered) != 4 {
		t.Errorf("filtered unit read returned %d rows, want the 4 unit.other rows of r1 and r2", len(filtered))
	}
	for _, ev := range filtered {
		if !want[ev.ID] || ev.Action != "unit.other" {
			t.Errorf("filtered unit read returned %s of run %v", ev.Action, ev.RunID)
		}
	}
	page, err := reads.QueryAuditEventsForUnits(ctx, units, store.AuditFilter{}, store.Page{Limit: 3, Offset: 2})
	if err != nil || !slices.Equal(auditIDs(page), order[2:5]) {
		t.Errorf("page 3 rows from offset 2 = %v (%v), want %v", auditIDs(page), err, order[2:5])
	}

	for name, set := range map[string][]uuid.UUID{"no units": {}, "the nil unit only": {uuid.Nil}} {
		if evs, err := reads.QueryAuditEventsForUnits(ctx, set, store.AuditFilter{}, store.Page{Limit: 1000}); err != nil || len(evs) != 0 {
			t.Errorf("%s: %d rows (%v), want none", name, len(evs), err)
		}
	}
}
