// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The unit-admin write pins: four-eyes for a unit admin's writes, the membership scope rules and the
// knob locks (the env and probes are in governance_unit_admin_pg_test.go).

func (e *unitAdminEnv) domainsOf(u types.GovernanceUnit) []string {
	e.t.Helper()
	row := e.profile(u.ID)
	if row.Overlay == nil || row.Overlay.AllowedDomains == nil {
		return nil
	}
	return *row.Overlay.AllowedDomains
}

// TestUnitAdminNarrowingDirect: a unit admin's edit that only narrows the walls applies directly, with
// the second-human switch off and on.
func TestUnitAdminNarrowingDirect(t *testing.T) {
	for _, on := range []bool{false, true} {
		e := newUnitAdminEnv(t, on)
		w := e.call(e.adminA, http.MethodPut, "/api/v1/governance/units/"+e.a1.ID.String(), uaUnitBody("ua-a1", e.a.ID, uaDomains("pypi.org"), nil))
		if w.Code != http.StatusOK {
			t.Fatalf("switch %v: a narrowing = %d %s, want 200", on, w.Code, w.Body)
		}
		if got := e.domainsOf(e.a1); !slices.Equal(got, []string{"pypi.org"}) || e.pendingCount() != 0 {
			t.Errorf("switch %v: ua-a1 allows %v with %d pending, want [pypi.org] and none", on, got, e.pendingCount())
		}
	}
}

// TestUnitAdminWideningHeld: with the switch off, a unit admin's widening is held. Its proposer and an
// admin beside the unit cannot approve it; a peer admin of the unit, an admin above it and a security
// admin each can. A lone admin's widening waits for a security admin, the only approver listed.
func TestUnitAdminWideningHeld(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	path := "/api/v1/governance/units/" + e.a1.ID.String()
	wide := uaUnitBody("ua-a1", e.a.ID, uaDomains("pypi.org", "github.com", "npmjs.org"), nil)
	narrow := uaUnitBody("ua-a1", e.a.ID, uaDomains("pypi.org", "github.com"), nil)
	for _, approver := range []struct {
		who string
		c   *http.Cookie
	}{{"a peer admin of ua-a1", e.peerA1}, {"an admin of ua-a above it", e.adminA}, {"a security admin", e.alice}} {
		ch := e.pending(e.call(e.adminA1, http.MethodPut, path, wide))
		if got := e.domainsOf(e.a1); len(got) != 2 {
			t.Fatalf("a held widening changed ua-a1 to %v before approval", got)
		}
		if w := e.call(e.adminA1, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusForbidden || wireReason(t, w) != "second_human_required" {
			t.Errorf("the proposer approving = %d %s, want 403 second_human_required", w.Code, w.Body)
		}
		if w := e.call(e.adminB, http.MethodPost, approvePath(ch.ID), ""); !scopeRefused(t, w) {
			t.Errorf("ua-b's admin approving = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
		}
		if w := e.call(approver.c, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
			t.Fatalf("%s approving = %d %s, want 200", approver.who, w.Code, w.Body)
		}
		if got := e.domainsOf(e.a1); len(got) != 3 {
			t.Errorf("%s approved, ua-a1 allows %v, want the three domains", approver.who, got)
		}
		if w := e.call(e.adminA1, http.MethodPut, path, narrow); w.Code != http.StatusOK { // back, directly, for the next round
			t.Fatalf("narrowing back = %d %s", w.Code, w.Body)
		}
	}
	// ua-b has one admin: their widening lists only the security admins and waits for one.
	ch := e.pending(e.call(e.adminB, http.MethodPut, "/api/v1/governance/units/"+e.b.ID.String(),
		uaUnitBody("ua-b", store.DefaultUnitID, uaDomains("pypi.org", "github.com"), nil)))
	var read governanceChangeRead
	if w := e.call(e.alice, http.MethodGet, "/api/v1/governance/changes/"+ch.ID.String(), ""); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &read) != nil {
		t.Fatalf("read the lone admin's change = %d %s", w.Code, w.Body)
	}
	if len(read.Approvers) != 1 || read.Approvers[0].Kind != "security" {
		t.Errorf("approvers %+v, want the security admins alone", read.Approvers)
	}
	if w := e.call(e.adminB, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusForbidden {
		t.Errorf("the lone admin approving their own change = %d %s, want 403", w.Code, w.Body)
	}
	if e.changeState(ch.ID) != types.GovernanceChangePending {
		t.Errorf("the lone admin's change is %s, want still pending", e.changeState(ch.ID))
	}
	e.approve(ch.ID)
}

// TestUnitAdminOutsideSubtreeAdd: a unit admin adds to their units only people who already belong in
// them: a person or group with no membership there is refused; one with a membership there is held.
func TestUnitAdminOutsideSubtreeAdd(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	treeMember(t, e.govEnv, e.b, types.CapabilitySubjectUser, "sub-ua-outside", 1, false)
	treeMember(t, e.govEnv, e.a2, types.CapabilitySubjectUser, "sub-ua-inside", 1, false)
	treeMember(t, e.govEnv, e.a2, types.CapabilitySubjectGroup, "ua-eng", 1, false)
	add := func(st, subject string) string {
		return `{"subject_type":"` + st + `","subject":"` + subject + `","unit_id":"` + e.a1.ID.String() + `","priority":1}`
	}
	for _, tc := range []struct {
		what, body string
		held       bool
	}{
		{"a person with no membership in ua-a's units", add("user", "sub-ua-outside"), false},
		{"a person nobody placed", add("user", "sub-ua-nobody"), false},
		{"a group with no membership in ua-a's units", add("group", "ua-ops"), false},
		{"a person who belongs in ua-a2", add("user", "sub-ua-inside"), true},
		{"a group that belongs in ua-a2", add("group", "ua-eng"), true},
	} {
		w := e.call(e.adminA, http.MethodPost, "/api/v1/governance/memberships", tc.body)
		if tc.held {
			e.pending(w)
		} else if !scopeRefused(t, w) {
			t.Errorf("%s: add = %d %s, want 403 unit_scope_denied", tc.what, w.Code, w.Body)
		}
	}
}

// legacyDelete seeds person's legacy rows, ua-a1's first by priority and then rest's in order, and has
// ua-a's admin delete the ua-a1 row: held, since the next row takes over as the person's context.
func (e *unitAdminEnv) legacyDelete(person string, rest ...types.GovernanceUnit) types.GovernanceChange {
	e.t.Helper()
	first := treeMember(e.t, e.govEnv, e.a1, types.CapabilitySubjectUser, person, 9, false)
	ids := []uuid.UUID{first.ID}
	for i, u := range rest {
		ids = append(ids, treeMember(e.t, e.govEnv, u, types.CapabilitySubjectUser, person, 8-i, false).ID)
	}
	setLegacy(e.t, e.govEnv, ids...)
	return e.pending(e.call(e.adminA, http.MethodDelete, "/api/v1/governance/memberships/"+first.ID.String(), ""))
}

// TestLegacyDeleteUnitAdminScoped: a unit admin's delete of a legacy row whose successor is in their
// units is held; a peer admin approves it and the successor becomes the person's context.
func TestLegacyDeleteUnitAdminScoped(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	ch := e.legacyDelete("sub-ua-legacy", e.a2)
	if w := e.call(e.peerA, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("the peer admin approving = %d %s, want 200", w.Code, w.Body)
	}
	if c := treeCeiling(t, e.govEnv, treeCtx("sub-ua-legacy", nil, "")); unitIDOf(c) != e.a2.ID {
		t.Errorf("the person's context is %s after the delete, want ua-a2", unitIDOf(c))
	}
}

// TestLegacyDeleteUnitAdminOutOfScope: when the successor is outside the admin's units the delete is
// still held, a peer admin's approval is refused unit_scope_denied, and a security admin approves.
func TestLegacyDeleteUnitAdminOutOfScope(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	ch := e.legacyDelete("sub-ua-legacy", e.b, e.a2)
	if w := e.call(e.peerA, http.MethodPost, approvePath(ch.ID), ""); !scopeRefused(t, w) {
		t.Errorf("the peer admin approving = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("a security admin approving = %d %s, want 200", w.Code, w.Body)
	}
	if c := treeCeiling(t, e.govEnv, treeCtx("sub-ua-legacy", nil, "")); unitIDOf(c) != e.b.ID {
		t.Errorf("the person's context is %s after the delete, want ua-b", unitIDOf(c))
	}
}

// TestUnitLockRefusesBothWays: locking a knob that a unit below states otherwise is refused, naming
// those units, and their values are kept. Once a knob is locked, a write below that changes it is
// refused, a narrowing included and for a security admin too; adding a lock applies directly, and a
// unit admin's removal of one is held.
func TestUnitLockRefusesBothWays(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	pathA, pathA1 := "/api/v1/governance/units/"+e.a.ID.String(), "/api/v1/governance/units/"+e.a1.ID.String()
	w := e.call(e.alice, http.MethodPut, pathA, e.restate(e.a, map[string]any{"locked_knobs": []string{"allowed_domains"}}))
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonUnitKnobLocked ||
		!strings.Contains(w.Body.String(), "ua-a1") || !strings.Contains(w.Body.String(), "ua-a2") {
		t.Errorf("locking allowed_domains over units that differ = %d %s, want 409 naming ua-a1 and ua-a2", w.Code, w.Body)
	}
	if got := e.domainsOf(e.a1); len(got) != 2 {
		t.Errorf("the refused lock changed ua-a1 to %v", got)
	}
	if w := e.call(e.adminA, http.MethodPut, pathA, e.restate(e.a, map[string]any{"locked_knobs": []string{"min_confinement_class"}})); w.Code != http.StatusOK {
		t.Fatalf("ua-a's admin adding a lock = %d %s, want 200", w.Code, w.Body)
	}
	tighter := uaUnitBody("ua-a1", e.a.ID, map[string]any{"allowed_domains": []string{"pypi.org", "github.com"}, "min_confinement_class": "CC3"}, nil)
	for _, c := range []struct {
		who string
		c   *http.Cookie
	}{{"ua-a1's admin", e.adminA1}, {"a security admin", e.alice}} {
		w := e.call(c.c, http.MethodPut, pathA1, tighter)
		if w.Code != http.StatusConflict || wireReason(t, w) != reasonUnitKnobLocked ||
			!strings.Contains(w.Body.String(), "min_confinement_class is locked by ua-a; ask an admin of ua-a") {
			t.Errorf("%s raising ua-a1's floor = %d %s, want 409 naming the lock", c.who, w.Code, w.Body)
		}
	}
	if w := e.call(e.adminA, http.MethodPost, "/api/v1/governance/units", uaUnitBody("ua-a1-cc3", e.a1.ID, map[string]any{"min_confinement_class": "CC3"}, nil)); w.Code != http.StatusConflict {
		t.Errorf("a new unit stating the locked knob = %d %s, want 409", w.Code, w.Body)
	}
	e.pending(e.call(e.adminA, http.MethodPut, pathA, e.restate(e.a, map[string]any{"locked_knobs": []string{}})))
}

// TestUnitLockDerivedLocked: a person in ua-a1, where ua-a locks min_confinement_class, has that knob
// marked locked and named for ua-a in the check's provenance, the run's own steps; a knob nothing locks
// is not marked.
func TestUnitLockDerivedLocked(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	if w := e.call(e.alice, http.MethodPut, "/api/v1/governance/units/"+e.a.ID.String(),
		e.restate(e.a, map[string]any{"locked_knobs": []string{"min_confinement_class"}})); w.Code != http.StatusOK {
		t.Fatalf("lock = %d %s", w.Code, w.Body)
	}
	treeMember(t, e.govEnv, e.a1, types.CapabilitySubjectUser, "sub-ua-runner", 1, true)
	got := e.check(`{"user_subjects":["sub-ua-runner"]}`)
	if got.Effective == nil {
		t.Fatalf("the check answered no effective walls: %+v", got)
	}
	if p, want := got.Effective.Provenance["min_confinement_class"], (policyref.Provenance{Layer: policyref.LayerUnit, Name: "ua-a", Locked: true}); p != want {
		t.Errorf("min_confinement_class provenance %+v, want %+v", p, want)
	}
	if p := got.Effective.Provenance["allowed_domains"]; p.Locked {
		t.Errorf("allowed_domains is marked locked: %+v", p)
	}
}

// TestUnitLockedRefusalSaysLocked: ua-a denies interactive runs and locks deny_interactive; ua-b denies
// them unlocked. POST /runs refuses an interactive run for a member of ua-a1 with a policy that says
// locked, and for a member of ua-b with none; the record launch gate's refusal, as its door writes it,
// says the same.
func TestUnitLockedRefusalSaysLocked(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	deny := map[string]any{"deny_interactive": true}
	for _, put := range []struct {
		u     types.GovernanceUnit
		extra map[string]any
	}{
		{e.a, map[string]any{"overlay_limits": deny, "locked_knobs": []string{"deny_interactive"}}},
		{e.b, map[string]any{"overlay_limits": deny}},
	} {
		if w := e.call(e.alice, http.MethodPut, "/api/v1/governance/units/"+put.u.ID.String(), e.restate(put.u, put.extra)); w.Code != http.StatusOK {
			t.Fatalf("PUT %s = %d %s", put.u.Name, w.Code, w.Body)
		}
	}
	for _, tc := range []struct {
		sub    string
		unit   types.GovernanceUnit
		locked bool
	}{{"sub-ua-locked", e.a1, true}, {"sub-ua-open", e.b, false}} {
		treeMember(t, e.govEnv, tc.unit, types.CapabilitySubjectUser, tc.sub, 1, true)
		policyOf := func(raw []byte) map[string]any {
			var b struct {
				Policy map[string]any `json:"policy"`
			}
			if err := json.Unmarshal(raw, &b); err != nil || b.Policy == nil {
				t.Fatalf("%s: refusal %s names no policy", tc.sub, raw)
			}
			return b.Policy
		}
		check := func(door string, p map[string]any) {
			got, has := p["locked"]
			if p["name"] != tc.unit.Name || (tc.locked && got != true) || (!tc.locked && has) {
				t.Errorf("%s under %s: %s policy %v, want name %q and locked %v", tc.sub, tc.unit.Name, door, p, tc.unit.Name, tc.locked)
			}
		}
		w := e.call(uaSession(t, tc.sub, []string{}, false), http.MethodPost, "/api/v1/runs", `{"agent":"claude-code"}`)
		if w.Code != http.StatusForbidden || wireReason(t, w) != string(authz.ReasonGovernanceUnit) {
			t.Fatalf("%s: POST /runs = %d %s, want 403 governance_unit", tc.sub, w.Code, w.Body)
		}
		check("POST /runs", policyOf(w.Body.Bytes()))

		ctx := treeCtx(tc.sub, nil, "")
		err := e.srv.stepRunCeilingLimits(ctx, tc.sub, recordRunGovernance(treeCeiling(t, e.govEnv, ctx)))
		if !errors.Is(err, errRecordCeilingLimit) {
			t.Fatalf("%s: record gate = %v, want the ceiling refusal", tc.sub, err)
		}
		rec := httptest.NewRecorder()
		writeErrorReasonPolicy(rec, http.StatusForbidden, reasonRecordCeilingLimit, err.Error(), recordCeilingRef(err), recordCeilingKnob(err))
		check("record launch", policyOf(rec.Body.Bytes()))
	}
}
