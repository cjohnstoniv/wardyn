// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func envelopePath(unit uuid.UUID) string {
	return "/api/v1/governance/units/" + unit.String() + "/envelope"
}

func envelopeBody(groups ...string) string {
	b, _ := json.Marshal(map[string]any{"approved_groups": groups})
	return string(b)
}

func (e *unitAdminEnv) envelopeOf(unit uuid.UUID) (types.GovernanceEnvelope, bool) {
	e.t.Helper()
	env, err := store.GetEnvelopeQ(context.Background(), e.pool, unit)
	if err != nil && err != store.ErrNotFound {
		e.t.Fatalf("read envelope of %s: %v", unit, err)
	}
	return env, err == nil
}

// TestEnvelopeWriteSecurityOnly: only the security tier sets or clears an envelope. A unit admin of the
// unit itself, and a member, are refused before any row or audit event exists.
func TestEnvelopeWriteSecurityOnly(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	for name, c := range map[string]struct {
		method, body string
	}{"set": {http.MethodPut, envelopeBody("plat-a")}, "clear": {http.MethodDelete, ""}} {
		for who, cookie := range map[string]*http.Cookie{"unit admin of the unit": e.adminA, "unit admin of another tree": e.adminB, "member": e.member} {
			w := e.call(cookie, c.method, envelopePath(e.a.ID), c.body)
			if w.Code != http.StatusForbidden || wireReason(t, w) != "security_admin_surface" {
				t.Errorf("%s by %s = %d %s, want 403 security_admin_surface", name, who, w.Code, w.Body)
			}
		}
	}
	if _, ok := e.envelopeOf(e.a.ID); ok || len(e.audits("governance.envelope.set")) != 0 {
		t.Fatal("a refused write reached the store or the audit log")
	}

	w := e.call(e.alice, http.MethodPut, envelopePath(e.a.ID), envelopeBody("Plat-A-Users", "plat-a-builders"))
	var got types.GovernanceEnvelope
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.UnitID != e.a.ID ||
		!slices.Equal(got.ApprovedGroups, []string{"plat-a-builders", "plat-a-users"}) || got.SetBy != govSubAlice {
		t.Fatalf("security PUT = %d %s, want 200 with the folded, sorted list set by the caller", w.Code, w.Body)
	}
	rows := e.audits("governance.envelope.set")
	if len(rows) != 1 || rows[0].Target != e.a.ID.String() {
		t.Fatalf("audit rows = %+v, want one for the unit", rows)
	}
	var d struct {
		Added, Removed []string
		UnitID         string `json:"unit_id"`
	}
	if json.Unmarshal(rows[0].Data, &d) != nil || d.UnitID != e.a.ID.String() || len(d.Added) != 2 || len(d.Removed) != 0 {
		t.Errorf("audit data = %s, want unit_id and both groups added", rows[0].Data)
	}

	// A replacement reports what changed.
	if w := e.call(e.alice, http.MethodPut, envelopePath(e.a.ID), envelopeBody("plat-a-users")); w.Code != http.StatusOK {
		t.Fatalf("replace = %d %s", w.Code, w.Body)
	}
	rows = e.audits("governance.envelope.set")
	if len(rows) != 2 || !strings.Contains(string(rows[1].Data), `"removed":["plat-a-builders"]`) {
		t.Errorf("replacement audit = %+v, want plat-a-builders removed", rows)
	}

	if w := e.call(e.alice, http.MethodDelete, envelopePath(e.a.ID), ""); w.Code != http.StatusNoContent {
		t.Fatalf("security DELETE = %d %s, want 204", w.Code, w.Body)
	}
	if _, ok := e.envelopeOf(e.a.ID); ok || len(e.audits("governance.envelope.delete")) != 1 {
		t.Errorf("after DELETE: envelope present=%v, delete rows=%d, want none and one", ok, len(e.audits("governance.envelope.delete")))
	}
}

// TestEnvelopeWriteHeldUnderSecondHuman: with the switch on, a security admin's set and clear are held
// as governance_envelope changes that a second human applies; nothing is written before then.
func TestEnvelopeWriteHeldUnderSecondHuman(t *testing.T) {
	e := newUnitAdminEnv(t, true)
	ch := e.pending(e.call(e.alice, http.MethodPut, envelopePath(e.a.ID), envelopeBody("plat-a")))
	if ch.TargetKind != types.GovernanceTargetEnvelope {
		t.Fatalf("held kind = %q, want %q", ch.TargetKind, types.GovernanceTargetEnvelope)
	}
	if _, ok := e.envelopeOf(e.a.ID); ok {
		t.Fatal("a held set was written")
	}
	// A unit admin may not decide it: the kind is decided on the security tier.
	if w := e.call(e.adminA, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
		t.Fatalf("unit admin approve = %d %s, want 403 or 404", w.Code, w.Body)
	}
	e.approve(ch.ID)
	if env, ok := e.envelopeOf(e.a.ID); !ok || !slices.Equal(env.ApprovedGroups, []string{"plat-a"}) {
		t.Fatalf("approved set = %+v present=%v", env, ok)
	}
	if len(e.audits("governance.envelope.set")) != 1 {
		t.Errorf("want one governance.envelope.set row at approval, got %d", len(e.audits("governance.envelope.set")))
	}

	del := e.pending(e.call(e.alice, http.MethodDelete, envelopePath(e.a.ID), ""))
	if _, ok := e.envelopeOf(e.a.ID); !ok {
		t.Fatal("a held clear was applied")
	}
	e.approve(del.ID)
	if _, ok := e.envelopeOf(e.a.ID); ok {
		t.Fatal("the approved clear left the envelope")
	}
}

// TestEnvelopeRefusesDefaultAndNested: the Default unit, an unknown unit, a bad group list, and an
// envelope above or below another are refused with their reasons and write nothing.
func TestEnvelopeRefusesDefaultAndNested(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	put := func(unit uuid.UUID, body string) (int, string) {
		w := e.call(e.alice, http.MethodPut, envelopePath(unit), body)
		return w.Code, wireReason(t, w)
	}
	many := make([]string, 65)
	for i := range many {
		many[i] = fmt.Sprintf("g-%d", i)
	}
	for name, c := range map[string]struct {
		unit uuid.UUID
		body string
	}{
		"default unit":    {store.DefaultUnitID, envelopeBody("plat-a")},
		"unknown unit":    {uuid.New(), envelopeBody("plat-a")},
		"empty group":     {e.a.ID, envelopeBody("plat-a", " ")},
		"duplicate group": {e.a.ID, envelopeBody("plat-a", "PLAT-A")},
		"too many groups": {e.a.ID, envelopeBody(many...)},
		"unknown field":   {e.a.ID, `{"approved_groups":[],"x":1}`},
	} {
		if code, reason := put(c.unit, c.body); code != http.StatusBadRequest || (reason != "envelope_invalid" && name != "unknown field") {
			t.Errorf("%s = %d %s, want 400 envelope_invalid", name, code, reason)
		}
	}

	if code, _ := put(e.a.ID, envelopeBody("plat-a")); code != http.StatusOK {
		t.Fatalf("set on A = %d", code)
	}
	for name, unit := range map[string]uuid.UUID{"below an envelope": e.a1.ID, "grandchild": e.child(e.a1).ID} {
		if code, reason := put(unit, envelopeBody("plat-x")); code != http.StatusConflict || reason != "envelope_nested" {
			t.Errorf("%s = %d %s, want 409 envelope_nested", name, code, reason)
		}
	}
	e.call(e.alice, http.MethodDelete, envelopePath(e.a.ID), "")
	if code, _ := put(e.a1.ID, envelopeBody("plat-x")); code != http.StatusOK {
		t.Fatalf("set on A1 = %d", code)
	}
	if code, reason := put(e.a.ID, envelopeBody("plat-y")); code != http.StatusConflict || reason != "envelope_nested" {
		t.Errorf("above an envelope = %d %s, want 409 envelope_nested", code, reason)
	}
	if got := len(e.audits("governance.envelope.set")); got != 2 {
		t.Errorf("%d set rows, want exactly the two accepted writes", got)
	}
}

// TestEnvelopeGroupOnOneEnvelopeOnly: a group sits on one envelope, and the refusal names the unit.
func TestEnvelopeGroupOnOneEnvelopeOnly(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	if w := e.call(e.alice, http.MethodPut, envelopePath(e.a.ID), envelopeBody("shared-group", "plat-a")); w.Code != http.StatusOK {
		t.Fatalf("set on A = %d %s", w.Code, w.Body)
	}
	w := e.call(e.alice, http.MethodPut, envelopePath(e.b.ID), envelopeBody("SHARED-GROUP"))
	if w.Code != http.StatusConflict || wireReason(t, w) != "envelope_group_taken" || !strings.Contains(w.Body.String(), e.a.Name) {
		t.Fatalf("a group on a second envelope = %d %s, want 409 envelope_group_taken naming %s", w.Code, w.Body, e.a.Name)
	}
	if _, ok := e.envelopeOf(e.b.ID); ok {
		t.Fatal("the refused write stored an envelope")
	}
	// Re-listing its own group is not a conflict.
	if w := e.call(e.alice, http.MethodPut, envelopePath(e.a.ID), envelopeBody("shared-group")); w.Code != http.StatusOK {
		t.Fatalf("re-set on A = %d %s", w.Code, w.Body)
	}
}

// TestEnvelopeDeletedWithUnit: deleting a unit removes its envelope.
func TestEnvelopeDeletedWithUnit(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	if w := e.call(e.alice, http.MethodPut, envelopePath(e.b.ID), envelopeBody("plat-b")); w.Code != http.StatusOK {
		t.Fatalf("set on B = %d %s", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodDelete, "/api/v1/governance/units/"+e.b.ID.String(), ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete unit B = %d %s", w.Code, w.Body)
	}
	if _, ok := e.envelopeOf(e.b.ID); ok {
		t.Fatal("the envelope outlived its unit")
	}
}

// TestGetGovernanceEnvelopesScoped: GET /governance lists every envelope to the security tier, and to a
// unit admin only the envelopes at or above the units they govern or below them.
func TestGetGovernanceEnvelopesScoped(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	for unit, g := range map[uuid.UUID]string{e.a.ID: "plat-a", e.b.ID: "plat-b"} {
		if w := e.call(e.alice, http.MethodPut, envelopePath(unit), envelopeBody(g)); w.Code != http.StatusOK {
			t.Fatalf("set %s = %d %s", g, w.Code, w.Body)
		}
	}
	read := func(c *http.Cookie) []uuid.UUID {
		w := e.call(c, http.MethodGet, "/api/v1/governance", "")
		var body struct {
			Envelopes *[]types.GovernanceEnvelope `json:"envelopes"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Envelopes == nil {
			t.Fatalf("GET /governance = %d %s, want an envelopes array", w.Code, w.Body)
		}
		var ids []uuid.UUID
		for _, env := range *body.Envelopes {
			ids = append(ids, env.UnitID)
		}
		slices.SortFunc(ids, func(x, y uuid.UUID) int { return strings.Compare(x.String(), y.String()) })
		return ids
	}
	want := func(ids ...uuid.UUID) []uuid.UUID {
		slices.SortFunc(ids, func(x, y uuid.UUID) int { return strings.Compare(x.String(), y.String()) })
		return ids
	}
	for name, c := range map[string]struct {
		cookie *http.Cookie
		want   []uuid.UUID
	}{
		"security":                {e.alice, want(e.a.ID, e.b.ID)},
		"admin of the root":       {e.adminA, want(e.a.ID)},
		"admin below the root":    {e.adminA1, want(e.a.ID)},
		"admin of the other tree": {e.adminB, want(e.b.ID)},
	} {
		if got := read(c.cookie); !slices.Equal(got, c.want) {
			t.Errorf("%s reads envelopes of %v, want %v", name, got, c.want)
		}
	}
}
