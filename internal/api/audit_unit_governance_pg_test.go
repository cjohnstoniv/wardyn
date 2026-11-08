// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The governance arm of a unit admin's feed: the rows about their units that carry no run, listed in
// store.unitGovernanceActions, through the governance view. The export serves run rows only.

// seedGovernance stores a governance row with no run and reports its id.
func (e *unitAuditEnv) seedGovernance(action, actor, target string, data map[string]any) uuid.UUID {
	e.t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		e.t.Fatalf("marshal row data: %v", err)
	}
	ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman, Actor: actor,
		Action: action, Target: target, Outcome: "success", SourceIP: "203.0.113.7", Data: raw}
	if err := store.InsertAuditEvent(context.Background(), e.pool, &ev); err != nil {
		e.t.Fatalf("seed %s: %v", action, err)
	}
	return ev.ID
}

func ugByID(evs []types.AuditEvent, id uuid.UUID) (types.AuditEvent, bool) {
	for _, ev := range evs {
		if ev.ID == id {
			return ev, true
		}
	}
	return types.AuditEvent{}, false
}

func TestAuditUnitAdminGovernanceRowsSubtreeOnly(t *testing.T) {
	e := newUnitAuditEnv(t)
	byTarget := e.seedGovernance("governance.unit.write", "carol@corp.example", e.a.ID.String(), map[string]any{"unit_scope": "security", "parent_id": e.a.ID.String()})
	byTargetBelow := e.seedGovernance("governance.unit.delete", "carol@corp.example", e.a1.ID.String(), map[string]any{"unit_scope": e.a.ID.String()})
	byUnitID := e.seedGovernance("governance.membership.write", "carol@corp.example", uuid.NewString(), map[string]any{"unit_id": e.a1.ID.String(), "subject": "dana@corp.example", "subject_type": "user", "priority": 1})
	byUnits := e.seedGovernance("governance.membership.delete", "carol@corp.example", "", map[string]any{"units": []string{e.a2.ID.String(), e.b.ID.String()}})
	envelope := e.seedGovernance("governance.envelope.set", "carol@corp.example", e.a.ID.String(), map[string]any{"added": []string{"eng"}})
	deciders := e.seedGovernance("governance.deciders.set", "bob@corp.example", e.a.ID.String(), map[string]any{"unit_scope": "security", "rules": []any{map[string]any{"kind": "x"}}})
	got := e.read(e.adminA, "/api/v1/audit", "")
	for name, id := range map[string]uuid.UUID{"target": byTarget, "target below": byTargetBelow, "data.unit_id": byUnitID,
		"data.units": byUnits, "envelope": envelope, "deciders": deciders} {
		if _, ok := ugByID(got, id); !ok {
			t.Errorf("adminA's feed lacks the governance row matched by %s", name)
		}
	}
	below := e.read(e.adminA1, "/api/v1/audit", "")
	for name, want := range map[string]struct {
		id   uuid.UUID
		seen bool
	}{"target ua-a": {byTarget, false}, "target ua-a1": {byTargetBelow, true}, "unit_id ua-a1": {byUnitID, true},
		"units ua-a2 and ua-b": {byUnits, false}, "envelope ua-a": {envelope, false}, "deciders ua-a": {deciders, false}} {
		if _, ok := ugByID(below, want.id); ok != want.seen {
			t.Errorf("adminA1 is served the %s row = %v, want %v", name, ok, want.seen)
		}
	}
	if ev, ok := ugByID(got, deciders); ok {
		if _, has := ugData(t, ev)["rules"]; has || ev.Actor != "security" || ev.SourceIP != "" {
			t.Errorf("deciders row = actor %q source %q data %s, want the security label and no rules", ev.Actor, ev.SourceIP, ev.Data)
		}
	}
	if ev, ok := ugByID(got, byTargetBelow); ok && ev.Actor != "carol@corp.example" {
		t.Errorf("a row written by a unit admin of ua-a is served to adminA with actor %q, want that person", ev.Actor)
	}
	if ev, ok := ugByID(got, byUnits); ok {
		if units, _ := ugData(t, ev)["units"].([]any); len(units) != 1 || units[0] != e.a2.ID.String() {
			t.Errorf("units = %v, want ua-a2 alone", units)
		}
	}
}

func TestAuditUnitAdminGovernanceForeignUnitRowsAbsent(t *testing.T) {
	e := newUnitAuditEnv(t)
	foreign := map[string]uuid.UUID{
		"target":       e.seedGovernance("governance.unit.write", "carol@corp.example", e.b.ID.String(), nil),
		"unit_id":      e.seedGovernance("governance.membership.write", "carol@corp.example", uuid.NewString(), map[string]any{"unit_id": e.b.ID.String()}),
		"units":        e.seedGovernance("governance.membership.delete", "carol@corp.example", "", map[string]any{"units": []string{e.b.ID.String()}}),
		"default unit": e.seedGovernance("governance.unit.write", "carol@corp.example", store.DefaultUnitID.String(), nil),
		"unlisted":     e.seedGovernance("auth.sign_in", "carol@corp.example", e.a.ID.String(), map[string]any{"unit_id": e.a.ID.String()}),
		"a name":       e.seedGovernance("governance.unit.write", "carol@corp.example", "ua-a", map[string]any{"unit": "ua-a"}),
	}
	mine := e.seedGovernance("governance.unit.write", "carol@corp.example", e.a.ID.String(), nil)
	got := e.read(e.adminA, "/api/v1/audit", "")
	for name, id := range foreign {
		if _, ok := ugByID(got, id); ok {
			t.Errorf("adminA is served the %s row, which names no unit of theirs", name)
		}
	}
	if _, ok := ugByID(got, mine); !ok {
		t.Error("adminA lacks the row naming ua-a")
	}
	if _, ok := ugByID(e.read(e.adminB, "/api/v1/audit", ""), foreign["target"]); !ok {
		t.Error("adminB lacks the row naming ua-b")
	}
	if _, ok := ugByID(e.read(e.member, "/api/v1/audit", ""), mine); ok {
		t.Error("a plain member is served a governance row")
	}
}

func TestAuditUnitAdminGovernanceReparentOutStopsServing(t *testing.T) {
	e := newUnitAuditEnv(t)
	moved := e.child(e.a)
	id := e.seedGovernance("governance.unit.write", "carol@corp.example", moved.ID.String(), nil)
	if _, ok := ugByID(e.read(e.adminA, "/api/v1/audit", ""), id); !ok {
		t.Fatal("before the move adminA lacks the row naming the unit below ua-a")
	}
	if w := e.admin(http.MethodPut, "/api/v1/governance/units/"+moved.ID.String(), e.restate(moved, map[string]any{"parent_id": e.b.ID})); w.Code != http.StatusOK {
		t.Fatalf("move the unit below ua-b: %d %s", w.Code, w.Body.String())
	}
	if _, ok := ugByID(e.read(e.adminA, "/api/v1/audit", ""), id); ok {
		t.Error("after the move adminA is still served the row naming the moved unit")
	}
	if _, ok := ugByID(e.read(e.adminB, "/api/v1/audit", ""), id); !ok {
		t.Error("after the move adminB lacks the row naming the unit now below ua-b")
	}
}

func TestAuditUnitGovernanceChangeRowsAbsent(t *testing.T) {
	e := newUnitAuditEnv(t)
	ids := []uuid.UUID{
		e.seedGovernance("governance.change.propose", "carol@corp.example", uuid.NewString(), map[string]any{"units": []string{e.a.ID.String()}}),
		e.seedGovernance("governance.change.approve", "carol@corp.example", uuid.NewString(), map[string]any{"unit_id": e.a.ID.String()}),
		e.seedGovernance("governance.change.reject", "carol@corp.example", e.a.ID.String(), nil),
	}
	got := e.read(e.adminA, "/api/v1/audit", "")
	for _, id := range ids {
		if ev, ok := ugByID(got, id); ok {
			t.Errorf("adminA is served %s, which the governance arm leaves out", ev.Action)
		}
	}
}

// TestAuditUnitExportRowsUnchanged: the export serves run rows only, whatever governance rows the log
// holds, and its bytes do not change when they arrive.
func TestAuditUnitExportRowsUnchanged(t *testing.T) {
	e := newUnitAuditEnv(t)
	before := e.call(e.adminA, http.MethodGet, "/api/v1/audit/export", "")
	if before.Code != http.StatusOK || before.Body.Len() == 0 {
		t.Fatalf("export before: %d with %d bytes, want the subtree's run rows", before.Code, before.Body.Len())
	}
	id := e.seedGovernance("governance.unit.write", "carol@corp.example", e.a.ID.String(), nil)
	if _, ok := ugByID(e.read(e.adminA, "/api/v1/audit", ""), id); !ok {
		t.Fatal("the feed lacks the governance row, so this test proves nothing about the export")
	}
	after := e.call(e.adminA, http.MethodGet, "/api/v1/audit/export", "")
	if after.Code != before.Code || !bytes.Equal(after.Body.Bytes(), before.Body.Bytes()) {
		t.Errorf("the export changed when a governance row arrived:\n before %s\n after  %s", before.Body.String(), after.Body.String())
	}
	if _, none := uaRunCounts(uaRows(t, "/api/v1/audit/export", after)); none != 0 {
		t.Errorf("the export served %d rows with no run, want run rows only", none)
	}
}

// TestAuditUnitRunRowsUnchanged: with governance rows in the log, the feed's run rows are the export's.
func TestAuditUnitRunRowsUnchanged(t *testing.T) {
	e := newUnitAuditEnv(t)
	e.seedGovernance("governance.unit.write", "carol@corp.example", e.a.ID.String(), nil)
	feed, export := e.read(e.adminA, "/api/v1/audit", ""), e.read(e.adminA, "/api/v1/audit/export", "")
	runRows := map[uuid.UUID]string{}
	for _, ev := range feed {
		if ev.RunID != nil {
			b, _ := json.Marshal(ev)
			runRows[ev.ID] = string(b)
		}
	}
	if len(runRows) != len(export) {
		t.Errorf("the feed serves %d run rows, the export %d", len(runRows), len(export))
	}
	for _, ev := range export {
		b, _ := json.Marshal(ev)
		if runRows[ev.ID] != string(b) {
			t.Errorf("run row %s differs between the feed and the export:\n feed   %s\n export %s", ev.ID, runRows[ev.ID], b)
		}
	}
}
