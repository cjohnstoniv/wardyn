// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestUnitAdminGovernanceReadNamesAncestors: a unit admin of a child unit reads, in unit_names, the name of
// every unit above their subtree (its parent and the Default unit), so an envelope's root or the unit a rule
// comes from can be named. Those units are still not listed, and neither their memberships nor their grants are
// served. A sibling unit is not named, and a security admin's read carries no unit_names.
func TestUnitAdminGovernanceReadNamesAncestors(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	treeMember(t, e.govEnv, e.a, types.CapabilitySubjectUser, "sub-ua-above", 1, false)
	var got struct {
		Units []struct {
			ID uuid.UUID `json:"id"`
		} `json:"units"`
		Memberships []struct {
			UnitID uuid.UUID `json:"unit_id"`
		} `json:"memberships"`
		UnitAdmins []struct {
			UnitID uuid.UUID `json:"unit_id"`
		} `json:"unit_admins"`
		UnitNames map[string]json.RawMessage `json:"unit_names"`
	}
	w := e.call(e.adminA1, http.MethodGet, "/api/v1/governance", "")
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("GET /governance as ua-a1's admin = %d %s", w.Code, w.Body)
	}
	want := map[string]string{e.a.ID.String(): e.a.Name, store.DefaultUnitID.String(): e.profile(store.DefaultUnitID).Name}
	if len(got.UnitNames) != len(want) {
		t.Errorf("unit_names = %v, want exactly %v", got.UnitNames, want)
	}
	for id, name := range want {
		var have string
		if err := json.Unmarshal(got.UnitNames[id], &have); err != nil || have != name {
			t.Errorf("unit_names[%s] = %s, want the name %q and nothing else", id, got.UnitNames[id], name)
		}
	}
	if _, has := got.UnitNames[e.b.ID.String()]; has {
		t.Errorf("unit_names names the sibling ua-b")
	}
	above := []uuid.UUID{e.a.ID, store.DefaultUnitID}
	for _, u := range got.Units {
		if slices.Contains(above, u.ID) {
			t.Errorf("unit %s above the subtree is listed", u.ID)
		}
	}
	for _, m := range got.Memberships {
		if slices.Contains(above, m.UnitID) {
			t.Errorf("a membership on %s above the subtree is served", m.UnitID)
		}
	}
	for _, a := range got.UnitAdmins {
		if slices.Contains(above, a.UnitID) {
			t.Errorf("a unit admin grant on %s above the subtree is served", a.UnitID)
		}
	}

	var sec map[string]json.RawMessage
	w = e.call(e.alice, http.MethodGet, "/api/v1/governance", "")
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &sec) != nil {
		t.Fatalf("GET /governance as a security admin = %d %s", w.Code, w.Body)
	}
	if _, has := sec["unit_names"]; has {
		t.Errorf("a security admin's read carries unit_names: %s", sec["unit_names"])
	}
}
