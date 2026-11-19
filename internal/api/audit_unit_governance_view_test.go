// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The governance view is how a unit admin is served the rows about their units that carry no run. These
// tests are pure: the rows are built here and the view is called with the set of units in scope.

var (
	ugIn1   = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	ugIn2   = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	ugOut   = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	ugOther = uuid.MustParse("44444444-4444-4444-8444-444444444444")
)

func ugScope() map[uuid.UUID]bool { return map[uuid.UUID]bool{ugIn1: true, ugIn2: true} }

// ugRow is a governance row with no run: the person who wrote it and the data it carries.
func ugRow(t *testing.T, action, actor, target string, data map[string]any) types.AuditEvent {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal row data: %v", err)
	}
	return types.AuditEvent{ID: uuid.New(), ActorType: types.ActorHuman, Actor: actor, Action: action, Target: target,
		Outcome: "success", SourceIP: "203.0.113.7", Data: raw}
}

func ugData(t *testing.T, ev types.AuditEvent) map[string]any {
	t.Helper()
	out := map[string]any{}
	if len(ev.Data) > 0 {
		if err := json.Unmarshal(ev.Data, &out); err != nil {
			t.Fatalf("%s data is not an object: %s", ev.Action, ev.Data)
		}
	}
	return out
}

// TestAuditUnitGovernanceAllowListCensus pins the governance view's data allow list: every key it serves
// is a key docs/AUDIT-ACTIONS.md documents, and none is a key that holds what only the security tier may
// read. The run view's never-list applies, except that the governance view serves subject: a unit admin
// already reads their units' memberships in GET /governance.
func TestAuditUnitGovernanceAllowListCensus(t *testing.T) {
	doc, err := os.ReadFile("../../docs/AUDIT-ACTIONS.md")
	if err != nil {
		t.Fatalf("read the audit action docs: %v", err)
	}
	if len(unitGovernanceDataKeys) == 0 {
		t.Fatal("the governance view serves no data key")
	}
	for _, k := range unitGovernanceDataKeys {
		if !bytes.Contains(doc, []byte("`"+k+"`")) {
			t.Errorf("governance view data key %q is not documented in docs/AUDIT-ACTIONS.md", k)
		}
	}
	never := []string{"policy_source", "personal_walls", "wall_preset", "restriction_ids", "overrides_diff",
		"contact", "spec", "inline_policy", "secret_name", "error", "detail", "owner",
		"approver", "proposed_by", "reason", "rules", "limits", "name", "contact_fields", "overlay_fields"}
	for _, k := range never {
		if slices.Contains(unitGovernanceDataKeys, k) {
			t.Errorf("governance view data key %q holds what a unit admin must not read", k)
		}
	}
	if !slices.Contains(unitGovernanceDataKeys, "subject") {
		t.Error("the governance view must serve subject")
	}
	if slices.Contains(unitViewDataKeys, "subject") {
		t.Error("the run view must keep refusing subject")
	}
}

// TestAuditUnitGovernanceActorMasking: whose name a unit admin reads on a governance row.
func TestAuditUnitGovernanceActorMasking(t *testing.T) {
	in := ugScope()
	const person = "bob@corp.example"
	cases := []struct {
		name   string
		action string
		typ    types.ActorType
		data   map[string]any
		want   string
	}{
		{"security scope", "governance.unit.write", types.ActorHuman, map[string]any{"unit_scope": "security"}, "security"},
		{"no scope key", "governance.unit.write", types.ActorHuman, map[string]any{"unit_id": ugIn1.String()}, "security"},
		{"a unit above the reader", "governance.membership.write", types.ActorHuman, map[string]any{"unit_scope": ugOut.String()}, "unit_admin"},
		{"a unit in scope", "governance.membership.write", types.ActorHuman, map[string]any{"unit_scope": ugIn1.String()}, person},
		{"a group decider approves", "governance.elevation.approve", types.ActorHuman, map[string]any{"unit_scope": "group:release-managers"}, "group"},
		{"a group decider rejects", "governance.image_request.reject", types.ActorHuman, map[string]any{"unit_scope": "group:release-managers"}, "group"},
		{"a re-read that found the grant gone", "governance.unit_admin.grant", types.ActorHuman, map[string]any{"unit_scope": ""}, ""},
		{"an elevation request", "governance.elevation.request", types.ActorHuman, nil, person},
		{"an image request", "governance.image_request.request", types.ActorHuman, nil, person},
		{"a person withdraws their elevation request", "governance.elevation.reject", types.ActorHuman, map[string]any{"unit_id": ugIn1.String(), "withdrawn": true}, person},
		{"a person withdraws their image request", "governance.image_request.reject", types.ActorHuman, map[string]any{"unit_id": ugIn1.String(), "withdrawn": true}, person},
		{"the system", "governance.elevation.expire", types.ActorSystem, nil, person},
		{"an envelope write", "governance.envelope.set", types.ActorHuman, map[string]any{"unit_id": ugIn1.String(), "added": []string{"eng"}}, "security"},
	}
	for _, c := range cases {
		ev := ugRow(t, c.action, person, ugIn1.String(), c.data)
		ev.ActorType = c.typ
		got := unitGovernanceView(ev, in)
		if got.Actor != c.want {
			t.Errorf("%s: actor = %q, want %q", c.name, got.Actor, c.want)
		}
		if got.Actor != "" && got.Actor == person && c.want != person {
			t.Errorf("%s: the person's name was served", c.name)
		}
	}
	// A group decider's row never names the group either, nor the person.
	ev := ugRow(t, "governance.elevation.approve", person, "", map[string]any{"unit_scope": "group:release-managers", "unit_id": ugIn1.String()})
	if d := ugData(t, unitGovernanceView(ev, in)); d["unit_scope"] != "group" {
		t.Errorf("a group scope is served as %v, want \"group\"", d["unit_scope"])
	}
	// approver and proposed_by are never served, whatever else is.
	ev = ugRow(t, "governance.unit.write", person, ugIn1.String(), map[string]any{
		"unit_id": ugIn1.String(), "approver": "carol@corp.example", "proposed_by": "dan@corp.example", "actor": "erin@corp.example"})
	got := unitGovernanceView(ev, in)
	for _, k := range []string{"approver", "proposed_by", "actor"} {
		if _, has := ugData(t, got)[k]; has {
			t.Errorf("the governance view served %q", k)
		}
	}
	if bytes.Contains(got.Data, []byte("corp.example")) {
		t.Errorf("the governance view served an address: %s", got.Data)
	}
	if got.SourceIP != "" || got.PrevHash != "" || got.RowHash != "" || got.RunID != nil {
		t.Errorf("the governance view kept the source address, chain hashes or a run: %+v", got)
	}
}

// TestAuditUnitGovernanceUnitsSubsetted: a row naming units outside the reader's keeps the ones inside.
func TestAuditUnitGovernanceUnitsSubsetted(t *testing.T) {
	in := ugScope()
	ev := ugRow(t, "governance.membership.write", "x", "", map[string]any{
		"units": []string{ugIn1.String(), ugOut.String(), "not-a-uuid", ugIn2.String()}})
	d := ugData(t, unitGovernanceView(ev, in))
	got, _ := d["units"].([]any)
	if len(got) != 2 || got[0] != ugIn1.String() || got[1] != ugIn2.String() {
		t.Errorf("units = %v, want the two in scope", d["units"])
	}
	ev = ugRow(t, "governance.membership.write", "x", "", map[string]any{"units": []string{ugOut.String()}})
	if d := ugData(t, unitGovernanceView(ev, in)); len(d) != 0 {
		t.Errorf("a units list wholly outside scope is served: %v", d)
	}
	ev = ugRow(t, "governance.membership.write", "x", "", map[string]any{
		"unit_id": ugOut.String(), "from_unit_id": ugIn1.String(), "parent_id": ugOther.String(), "grant_id": ugOther.String()})
	d = ugData(t, unitGovernanceView(ev, in))
	if _, has := d["unit_id"]; has {
		t.Errorf("unit_id outside scope is served: %v", d)
	}
	if _, has := d["parent_id"]; has {
		t.Errorf("parent_id outside scope is served: %v", d)
	}
	if d["from_unit_id"] != ugIn1.String() || d["grant_id"] != ugOther.String() {
		t.Errorf("from_unit_id in scope and grant_id (an id with no scope) = %v", d)
	}
}

// TestAuditUnitGovernanceTargetOnlyInScope: the target is served for a unit in scope, and for the row's
// own membership or grant id on the families that name one; anywhere else it is empty.
func TestAuditUnitGovernanceTargetOnlyInScope(t *testing.T) {
	in := ugScope()
	member := uuid.NewString()
	cases := []struct {
		name, action, target string
		data                 map[string]any
		want                 string
	}{
		{"a unit in scope", "governance.unit.write", ugIn1.String(), nil, ugIn1.String()},
		{"a unit outside scope", "governance.unit.write", ugOut.String(), map[string]any{"units": []string{ugIn1.String()}}, ""},
		{"a name", "governance.unit.write", "ua-a", nil, ""},
		{"a membership of a unit in scope", "governance.membership.delete", member, map[string]any{"unit_id": ugIn1.String()}, member},
		{"a membership of a unit outside scope", "governance.membership.delete", member, map[string]any{"unit_id": ugOut.String()}, ""},
		{"an elevation approval", "governance.elevation.approve", member, map[string]any{"unit_id": ugIn2.String()}, member},
		{"an elevation expiry", "governance.elevation.expire", member, map[string]any{"unit_id": ugIn2.String()}, member},
		{"an image approval", "governance.image_request.approve", member, map[string]any{"unit_id": ugIn1.String()}, member},
		{"an image rejection names no grant", "governance.image_request.reject", member, map[string]any{"unit_id": ugIn1.String()}, ""},
		{"a unit-admin grant by grant id", "governance.unit_admin.grant", member, map[string]any{"unit_id": ugIn1.String()}, ""},
		{"a membership id that is not an id", "governance.membership.write", "alice@corp.example", map[string]any{"unit_id": ugIn1.String()}, ""},
	}
	for _, c := range cases {
		ev := ugRow(t, c.action, "x", c.target, c.data)
		if got := unitGovernanceView(ev, in).Target; got != c.want {
			t.Errorf("%s: target = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestAuditUnitGovernanceValueShapes: a subject is served when it is a person's address, a group name or a
// user type; an image only as a digest reference; an object, a sentence or a list holding one is left out.
func TestAuditUnitGovernanceValueShapes(t *testing.T) {
	in := ugScope()
	digest := "registry.corp.example/team/app@sha256:" + string(bytes.Repeat([]byte("a"), 64))
	ev := ugRow(t, "governance.image_request.request", "x", "", map[string]any{
		"subject": "alice@corp.example", "subject_type": "user", "priority": 3, "home": true, "image": digest,
		"added": []string{"eng", "release managers"}, "removed": []string{"a\nb"},
		"rules": map[string]any{"x": 1}, "limits": map[string]any{"y": 2}, "name": "secret-team",
		"duration": "8h", "withdrawn": false, "change_id": uuid.NewString(), "changed": []string{"deciders"},
	})
	d := ugData(t, unitGovernanceView(ev, in))
	for _, k := range []string{"subject", "subject_type", "priority", "home", "image", "added", "duration", "withdrawn", "change_id", "changed"} {
		if _, has := d[k]; !has {
			t.Errorf("%s was left out: %v", k, d)
		}
	}
	for _, k := range []string{"removed", "rules", "limits", "name"} {
		if _, has := d[k]; has {
			t.Errorf("%s is served: %v", k, d)
		}
	}
	for _, bad := range []string{"registry.corp.example/app:latest", "user:pw@host/app@sha256:" + string(bytes.Repeat([]byte("a"), 64)), "app@sha256:abc"} {
		ev := ugRow(t, "governance.image_request.request", "x", "", map[string]any{"image": bad})
		if _, has := ugData(t, unitGovernanceView(ev, in))["image"]; has {
			t.Errorf("image %q is served", bad)
		}
	}
}
