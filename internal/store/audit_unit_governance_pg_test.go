// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The unit feed reads a listed governance row with no run when it names one of the units: by its target,
// by data.unit_id, or inside data.units. An unlisted action, a unit outside the set and a row with a run
// of its own never match that way. An actor filter reads run rows only. The test logs the plan of the arm's
// query so its cost on a large log can be read.
func TestAuditUnitGovernanceArmQueryPG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	u1, u2, other := uuid.New(), uuid.New(), uuid.New()
	run := newRun(types.RunRunning)
	run.UnitID = &u1
	runID := persistRun(t, ctx, pool, run).ID

	seed := func(runRef *uuid.UUID, action, target string, data any) uuid.UUID {
		t.Helper()
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: runRef, ActorType: types.ActorHuman,
			Actor: "carol@corp.example", Action: action, Target: target, Outcome: "success", Data: raw}
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatalf("seed %s: %v", action, err)
		}
		return ev.ID
	}
	byTarget := seed(nil, "governance.unit.write", u1.String(), map[string]any{})
	byUnitID := seed(nil, "governance.membership.write", uuid.NewString(), map[string]any{"unit_id": u2.String()})
	byUnits := seed(nil, "governance.membership.delete", "", map[string]any{"units": []string{other.String(), u1.String()}})
	byRun := seed(&runID, "run.create", runID.String(), map[string]any{})
	absent := map[string]uuid.UUID{
		"an unlisted action":    seed(nil, "auth.sign_in", u1.String(), map[string]any{"unit_id": u1.String()}),
		"a change row":          seed(nil, "governance.change.approve", u1.String(), map[string]any{"unit_id": u1.String()}),
		"another unit":          seed(nil, "governance.unit.write", other.String(), map[string]any{"unit_id": other.String()}),
		"units is not an array": seed(nil, "governance.membership.delete", "", map[string]any{"units": u1.String()}),
		"no unit named":         seed(nil, "governance.unit.write", "ua-a", map[string]any{}),
		"units holds objects":   seed(nil, "governance.membership.delete", "", map[string]any{"units": []any{map[string]any{"id": u1.String()}}}),
	}
	// A laptop may forward any action naming any unit; the view would show its row as a security admin's.
	dev, err := pg.CreateDevice(ctx, types.Device{ID: uuid.New(), Name: "laptop"}, "wdd_"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	forged := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC().Truncate(time.Microsecond), ActorType: types.ActorHuman,
		Actor: "carol@corp.example", Action: "governance.unit.write", Target: u1.String(), Outcome: "success",
		Data: json.RawMessage(`{"unit_id":"` + u1.String() + `"}`)}
	forged.RowHash = auditRowHash(t, pool, "", forged)
	if _, err := pg.IngestDeviceAudit(ctx, dev.ID, "10.9.9.9", []types.FederatedAuditEvent{{AuditEvent: forged, Seq: 1}}); err != nil {
		t.Fatalf("a laptop forwarding a governance row: %v", err)
	}
	absent["a laptop's forwarded row"] = forged.ID
	units := []uuid.UUID{u1, u2}

	feed, err := pg.QueryAuditEventsForUnits(ctx, units, store.AuditFilter{}, store.Page{Limit: 1000})
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	got := map[uuid.UUID]bool{}
	for _, ev := range feed {
		got[ev.ID] = true
	}
	for name, id := range map[string]uuid.UUID{"target": byTarget, "data.unit_id": byUnitID, "data.units": byUnits, "a run's row": byRun} {
		if !got[id] {
			t.Errorf("the feed lacks the row matched by %s", name)
		}
	}
	for name, id := range absent {
		if got[id] {
			t.Errorf("the feed serves the row with %s", name)
		}
	}
	filtered, err := pg.QueryAuditEventsForUnits(ctx, units, store.AuditFilter{Action: "governance.membership.write"}, store.Page{Limit: 1000})
	if err != nil || len(filtered) != 1 || filtered[0].ID != byUnitID {
		t.Errorf("the feed filtered by action = %d rows (%v), want the one unit_id row", len(filtered), err)
	}

	// An actor filter must not reach the governance arm: the view masks a governance row's actor, so a
	// non-empty answer to ?actor=<name> would confirm who wrote about the reader's units.
	byActor, err := pg.QueryAuditEventsForUnits(ctx, units, store.AuditFilter{Actor: "carol@corp.example"}, store.Page{Limit: 1000})
	if err != nil || len(byActor) != 1 || byActor[0].ID != byRun {
		t.Errorf("the feed filtered by actor = %d rows (%v), want the run's row alone", len(byActor), err)
	}

	q, args := store.UnitAuditQueryForTest(units, store.AuditFilter{}, true)
	rows, err := pool.Query(ctx, "EXPLAIN "+q, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		t.Log("governance arm plan: " + line)
	}
}
