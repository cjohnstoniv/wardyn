// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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

// addedTo reports whether subject holds a membership in unit u.
func (e *unitAdminEnv) addedTo(subject string, u types.GovernanceUnit) bool {
	e.t.Helper()
	ms, err := e.pg.ListGovernanceMemberships(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return slices.ContainsFunc(ms, func(m types.GovernanceMembership) bool { return m.Subject == subject && m.UnitID == u.ID })
}

// qualifierChange is what a belonging test does, after the change is held, to the unit q below ua-a
// whose membership admitted it: nothing, remove the person's row there, or re-parent q under ua-b,
// out of ua-a's units with the row unchanged.
type qualifierChange int

const (
	qualifierKept qualifierChange = iota
	qualifierRemoved
	qualifierMovedOut
)

// changeQualifier applies c to the person's row in unit q, as a security admin.
func (e *unitAdminEnv) changeQualifier(c qualifierChange, person string, q types.GovernanceUnit) {
	e.t.Helper()
	switch c {
	case qualifierRemoved:
		if w := e.admin(http.MethodDelete, "/api/v1/governance/memberships/"+e.rowOf(person, q).ID.String(), ""); w.Code != http.StatusNoContent {
			e.t.Fatalf("remove the row in %s = %d %s, want 204", q.Name, w.Code, w.Body)
		}
	case qualifierMovedOut:
		if w := e.admin(http.MethodPut, "/api/v1/governance/units/"+q.ID.String(), e.restate(q, map[string]any{"parent_id": e.b.ID})); w.Code != http.StatusOK {
			e.t.Fatalf("re-parent %s under ua-b = %d %s, want 200", q.Name, w.Code, w.Body)
		}
	}
}

// qualifierCases are the belonging tests' cases: the admitting row changed (stale unless the person's
// ua-a row also admits the change) or not.
var qualifierCases = []struct {
	name    string
	change  qualifierChange
	alsoInA bool
	stale   bool
}{
	{"the admitting row unchanged", qualifierKept, false, false},
	{"the admitting row removed since", qualifierRemoved, false, true},
	{"the admitting row's unit moved out from under ua-a since", qualifierMovedOut, false, true},
	{"the admitting row removed and another remaining", qualifierRemoved, true, false},
	{"the admitting row's unit moved out and another remaining", qualifierMovedOut, true, false},
}

// TestUnitAdminAddRechecksBelonging: a unit admin's add is admitted because the person already belongs
// in their units, and the decision reads that again under its locks, against the units below ua-a as
// they stand. The person's row in q, a unit below ua-a, admits ua-a's admin's add to ua-a2. With that
// row removed since, or q re-parented out from under ua-a, ua-a's peer admin approving is stale and
// creates no row, and a fresh proposal of the same add is refused. With it unchanged, or while a ua-a
// row that admits the add too remains, the approval applies.
func TestUnitAdminAddRechecksBelonging(t *testing.T) {
	const person = "sub-ua-placed"
	user := types.CapabilitySubjectUser
	for _, tc := range qualifierCases {
		t.Run(tc.name, func(t *testing.T) {
			e := newUnitAdminEnv(t, false)
			q := e.child(e.a)
			treeMember(t, e.govEnv, q, user, person, 1, false)
			if tc.alsoInA {
				treeMember(t, e.govEnv, e.a, user, person, 1, false)
			}
			add := membershipBody(person, e.a2.ID)
			ch := e.pending(e.call(e.adminA, http.MethodPost, "/api/v1/governance/memberships", add))
			e.changeQualifier(tc.change, person, q)
			w := e.call(e.peerA, http.MethodPost, approvePath(ch.ID), "")
			if tc.stale {
				if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale || e.addedTo(person, e.a2) {
					t.Fatalf("approving = %d %s with the row added = %v, want 409 %s and no row", w.Code, w.Body, e.addedTo(person, e.a2), reasonGovernanceChangeStale)
				}
				if w := e.call(e.adminA, http.MethodPost, "/api/v1/governance/memberships", add); !scopeRefused(t, w) {
					t.Errorf("a fresh proposal since = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
				}
				return
			}
			if w.Code != http.StatusOK || !e.addedTo(person, e.a2) {
				t.Fatalf("approving = %d %s with the row added = %v, want 200 and the row", w.Code, w.Body, e.addedTo(person, e.a2))
			}
		})
	}
}

// TestSecurityAddFirstPlacementApplies: a security admin's held add may place a person who holds no
// membership yet. It is not admitted by belonging, so its approval applies with nothing to recheck.
func TestSecurityAddFirstPlacementApplies(t *testing.T) {
	e := newUnitAdminEnv(t, true)
	const person = "sub-ua-first"
	ch := e.pending(e.call(e.alice, http.MethodPost, "/api/v1/governance/memberships", membershipBody(person, e.a2.ID)))
	if w := e.call(e.peerA, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK || !e.addedTo(person, e.a2) {
		t.Fatalf("approving = %d %s with the row added = %v, want 200 and the row", w.Code, w.Body, e.addedTo(person, e.a2))
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

// TestLegacyDeleteRechecksKeeping: a unit admin's delete is admitted because the person keeps another
// membership in their units, and the decision reads that again under its locks, against the units
// below ua-a as they stand. The person's legacy rows are in ua-a1, ua-b and q, a unit below ua-a, at
// falling priority (and in ua-a too, lowest, when another remains), and ua-a's admin deletes the ua-a1
// row, held since ua-b takes over. With the q row removed since, or q re-parented out from under ua-a,
// a security admin's approval is stale and keeps the row, and a fresh proposal is refused. With it
// unchanged, or while the ua-a row remains, the approval applies.
func TestLegacyDeleteRechecksKeeping(t *testing.T) {
	const person = "sub-ua-legacy"
	for _, tc := range qualifierCases {
		t.Run(tc.name, func(t *testing.T) {
			e := newUnitAdminEnv(t, false)
			q := e.child(e.a)
			rest := []types.GovernanceUnit{e.b, q}
			if tc.alsoInA {
				rest = append(rest, e.a)
			}
			ch := e.legacyDelete(person, rest...)
			deleted := e.rowOf(person, e.a1)
			e.changeQualifier(tc.change, person, q)
			w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), "")
			kept := e.addedTo(person, e.a1)
			if tc.stale {
				if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale || !kept {
					t.Fatalf("approving = %d %s with the row kept = %v, want 409 %s and the row kept", w.Code, w.Body, kept, reasonGovernanceChangeStale)
				}
				if w := e.call(e.adminA, http.MethodDelete, "/api/v1/governance/memberships/"+deleted.ID.String(), ""); !scopeRefused(t, w) {
					t.Errorf("a fresh proposal since = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
				}
				return
			}
			if w.Code != http.StatusOK || kept {
				t.Fatalf("approving = %d %s with the row kept = %v, want 200 and the row deleted", w.Code, w.Body, kept)
			}
		})
	}
}

// legacyRow is a legacy membership a move test seeds.
type legacyRow struct {
	unit     types.GovernanceUnit
	st       types.CapabilitySubjectType
	subject  string
	priority int
}

// legacyMove seeds legacy rows moved and other and has ua-a's admin move moved to a new, narrower unit
// below its own named name, which it returns with the answer and the two rows.
func (e *unitAdminEnv) legacyMove(moved, other legacyRow, name string) (*httptest.ResponseRecorder, types.GovernanceUnit, [2]types.GovernanceMembership) {
	e.t.Helper()
	rows := [2]types.GovernanceMembership{
		treeMember(e.t, e.govEnv, moved.unit, moved.st, moved.subject, moved.priority, false),
		treeMember(e.t, e.govEnv, other.unit, other.st, other.subject, other.priority, false),
	}
	setLegacy(e.t, e.govEnv, rows[0].ID, rows[1].ID)
	to := treeChild(e.t, e.govEnv, uaUnitBody(name, moved.unit.ID, uaDomains("pypi.org"), nil))
	return e.call(e.adminA, http.MethodPut, "/api/v1/governance/memberships/"+rows[0].ID.String(), `{"unit_id":"`+to.ID.String()+`"}`), to, rows
}

// TestLegacyMoveUnitAdmin: a unit admin's narrowing move of a legacy row is held when it changes which
// legacy row a person is selected by, and applies directly when no selection changes but the row's own
// move. Every case seeds the moved row in a unit of ua-a's and the other in ua-b. The rows are two
// groups one person is in, or one person's two rows; at equal priority the unit name breaks the tie,
// so the destination's name decides. When the moved row falls behind, ua-b takes over; when it takes
// over, the person leaves ua-b. Either way only an approver who governs ua-b too may decide: ua-a's
// peer admin is refused, and a security admin (behind) or an admin of both ua-a and ua-b (takes over)
// approves. In the witness ua-a1 refuses exec runs and ua-b does not: the person's runs keep the
// refusal until the approval.
func TestLegacyMoveUnitAdmin(t *testing.T) {
	group, user := types.CapabilitySubjectGroup, types.CapabilitySubjectUser
	for _, tc := range []struct {
		name string
		// from is the moved row's unit, ua-a1 unless set (a unit below ua-a named ua-c-from).
		belowC        bool
		moved, other  legacyRow
		to            string
		held, behind  bool
		denyExec      bool   // ua-a1 refuses exec runs
		before, after string // the unit names the person is selected by before and after the move
	}{
		{name: "overlapping groups, the moved row falls behind (the witness)",
			moved: legacyRow{st: group, subject: "ua-eng", priority: 5}, other: legacyRow{st: group, subject: "ua-research", priority: 5},
			to: "ua-zeta", held: true, behind: true, denyExec: true, before: "ua-a1", after: "ua-b"},
		{name: "overlapping groups, the moved row still selected",
			moved: legacyRow{st: group, subject: "ua-eng", priority: 5}, other: legacyRow{st: group, subject: "ua-research", priority: 5},
			to: "ua-a1-leaf", before: "ua-a1", after: "ua-a1-leaf"},
		{name: "overlapping groups, the moved row takes over", belowC: true,
			moved: legacyRow{st: group, subject: "ua-x", priority: 5}, other: legacyRow{st: group, subject: "ua-y", priority: 5},
			to: "ua-a-to", held: true, before: "ua-b", after: "ua-a-to"},
		{name: "overlapping groups, the moved row still behind", belowC: true,
			moved: legacyRow{st: group, subject: "ua-x", priority: 5}, other: legacyRow{st: group, subject: "ua-y", priority: 5},
			to: "ua-d-to", before: "ua-b", after: "ua-b"},
		{name: "an equal-priority tie the unit name breaks, the moved row falls behind",
			moved: legacyRow{st: user, subject: "sub-ua-tie", priority: 5}, other: legacyRow{st: user, subject: "sub-ua-tie", priority: 5},
			to: "ua-zeta", held: true, behind: true, before: "ua-a1", after: "ua-b"},
		{name: "an equal-priority tie the unit name breaks, the moved row still selected",
			moved: legacyRow{st: user, subject: "sub-ua-tie", priority: 5}, other: legacyRow{st: user, subject: "sub-ua-tie", priority: 5},
			to: "ua-a1-leaf", before: "ua-a1", after: "ua-a1-leaf"},
		{name: "a lower priority, the moved row behind on both sides",
			moved: legacyRow{st: group, subject: "ua-eng", priority: 1}, other: legacyRow{st: group, subject: "ua-research", priority: 5},
			to: "ua-zeta", before: "ua-b", after: "ua-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newUnitAdminEnv(t, false)
			tc.moved.unit, tc.other.unit = e.a1, e.b
			if tc.belowC {
				tc.moved.unit = treeChild(t, e.govEnv, uaUnitBody("ua-c-from", e.a.ID, uaDomains("pypi.org", "github.com"), nil))
			}
			if tc.denyExec {
				if w := e.admin(http.MethodPut, "/api/v1/governance/units/"+e.a1.ID.String(),
					e.restate(e.a1, map[string]any{"overlay_limits": map[string]any{"deny_task_mode_exec": true}})); w.Code != http.StatusOK {
					t.Fatalf("ua-a1 refusing exec runs = %d %s", w.Code, w.Body)
				}
			}
			who := treeCtx("sub-ua-tie", []string{tc.moved.subject, tc.other.subject}, "")
			if tc.moved.st == user {
				who = treeCtx("sub-ua-tie", nil, "")
			}
			names := map[uuid.UUID]string{e.a1.ID: "ua-a1", e.b.ID: "ua-b", tc.moved.unit.ID: tc.moved.unit.Name}
			selected := func() string {
				t.Helper()
				return names[unitIDOf(treeCeiling(t, e.govEnv, who))]
			}
			w, to, _ := e.legacyMove(tc.moved, tc.other, tc.to)
			names[to.ID] = to.Name
			if !tc.held {
				if w.Code != http.StatusOK || e.pendingCount() != 0 {
					t.Fatalf("move = %d %s with %d pending, want 200 and none", w.Code, w.Body, e.pendingCount())
				}
				if got := selected(); got != tc.after {
					t.Errorf("selected by %s after the move, want %s", got, tc.after)
				}
				return
			}
			ch := e.pending(w)
			if got := selected(); got != tc.before {
				t.Errorf("selected by %s before approval, want %s", got, tc.before)
			}
			if tc.denyExec && !treeCeiling(t, e.govEnv, who).Limits.DenyTaskModeExec {
				t.Errorf("before approval the person's runs are not refused exec")
			}
			if w := e.call(e.peerA, http.MethodPost, approvePath(ch.ID), ""); !scopeRefused(t, w) {
				t.Errorf("ua-a's peer admin approving = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
			}
			approver := e.alice
			if !tc.behind {
				both := e.fresh("sub-ua-admin-ab")
				e.grant(e.a, types.CapabilitySubjectUser, both)
				e.grant(e.b, types.CapabilitySubjectUser, both)
				approver = uaSession(t, both, []string{}, false)
			}
			if w := e.call(approver, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
				t.Fatalf("approving = %d %s, want 200", w.Code, w.Body)
			}
			if got := selected(); got != tc.after {
				t.Errorf("selected by %s after approval, want %s", got, tc.after)
			}
			if tc.denyExec && treeCeiling(t, e.govEnv, who).Limits.DenyTaskModeExec {
				t.Errorf("after approval the person's runs are still refused exec, want ua-b's walls")
			}
		})
	}
}

// TestLegacyMoveHeldGoesStale: a held legacy move is decided over the units it would activate when
// held, ua-b in each case (the witness: groups in ua-a1 and ua-b at one priority, the ua-a1 row moved
// below it to ua-zeta). An edit since that changes what the move would do makes the approval stale,
// even a security admin's, and leaves the row where it was: the ua-b row moved to ua-s, which sorts
// between ua-b and ua-zeta, or raised above the moved row's priority. An unrelated edit since leaves
// the approval to apply.
func TestLegacyMoveHeldGoesStale(t *testing.T) {
	group := types.CapabilitySubjectGroup
	for _, tc := range []struct {
		name  string
		edit  func(e *unitAdminEnv, other types.GovernanceMembership)
		stale bool
	}{
		{"the ua-b row moved to a unit the move now activates", func(e *unitAdminEnv, other types.GovernanceMembership) {
			s := treeChild(e.t, e.govEnv, uaUnitBody("ua-s", store.DefaultUnitID, uaDomains("pypi.org"), nil))
			if w := e.admin(http.MethodPut, "/api/v1/governance/memberships/"+other.ID.String(), `{"unit_id":"`+s.ID.String()+`"}`); w.Code != http.StatusOK {
				e.t.Fatalf("move the ua-b row to ua-s = %d %s", w.Code, w.Body)
			}
		}, true},
		{"the ua-b row's priority raised above the moved row's", func(e *unitAdminEnv, other types.GovernanceMembership) {
			if _, err := e.pool.Exec(context.Background(), `UPDATE governance_memberships SET priority = 9 WHERE id = $1`, other.ID); err != nil {
				e.t.Fatalf("raise the ua-b row's priority: %v", err)
			}
		}, true},
		{"an unrelated unit edited", func(e *unitAdminEnv, _ types.GovernanceMembership) {
			if w := e.admin(http.MethodPut, "/api/v1/governance/units/"+e.a2.ID.String(),
				e.restate(e.a2, map[string]any{"overlay_limits": map[string]any{"deny_interactive": true}})); w.Code != http.StatusOK {
				e.t.Fatalf("edit ua-a2 = %d %s", w.Code, w.Body)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newUnitAdminEnv(t, false)
			w, _, rows := e.legacyMove(legacyRow{unit: e.a1, st: group, subject: "ua-eng", priority: 5},
				legacyRow{unit: e.b, st: group, subject: "ua-research", priority: 5}, "ua-zeta")
			ch := e.pending(w)
			tc.edit(e, rows[1])
			w = e.call(e.alice, http.MethodPost, approvePath(ch.ID), "")
			ms, err := e.pg.ListGovernanceMemberships(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(ms, func(m types.GovernanceMembership) bool { return m.ID == rows[0].ID })
			if tc.stale {
				if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale {
					t.Fatalf("approving = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeStale)
				}
				if i < 0 || ms[i].UnitID != e.a1.ID {
					t.Errorf("the stale approval moved the row: %+v", ms)
				}
				return
			}
			if w.Code != http.StatusOK || i < 0 || ms[i].UnitID == e.a1.ID {
				t.Fatalf("approving = %d %s with the row moved = %v, want 200 and the row moved", w.Code, w.Body, i >= 0 && ms[i].UnitID != e.a1.ID)
			}
		})
	}
}

// rowOf is subject's membership in unit u.
func (e *unitAdminEnv) rowOf(subject string, u types.GovernanceUnit) types.GovernanceMembership {
	e.t.Helper()
	ms, err := e.pg.ListGovernanceMemberships(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	i := slices.IndexFunc(ms, func(m types.GovernanceMembership) bool { return m.Subject == subject && m.UnitID == u.ID })
	if i < 0 {
		e.t.Fatalf("%s has no membership in %s", subject, u.Name)
	}
	return ms[i]
}

// TestLegacyDeleteHeldGoesStale: a held legacy delete is decided over the units it would activate when
// held (the person's rows in ua-a1, ua-b and ua-a2 at falling priority, the ua-a1 row deleted, so ua-b
// takes over). An edit since that changes which row takes over makes the approval stale, even a
// security admin's, and keeps the row: the ua-b row moved to ua-s, or lowered below the ua-a2 row. An
// unrelated edit since leaves the approval to apply.
func TestLegacyDeleteHeldGoesStale(t *testing.T) {
	const person = "sub-ua-legacy"
	for _, tc := range []struct {
		name  string
		edit  func(e *unitAdminEnv)
		stale bool
	}{
		{"the ua-b row moved to another unit", func(e *unitAdminEnv) {
			s := treeChild(e.t, e.govEnv, uaUnitBody("ua-s", store.DefaultUnitID, uaDomains("pypi.org"), nil))
			if w := e.admin(http.MethodPut, "/api/v1/governance/memberships/"+e.rowOf(person, e.b).ID.String(), `{"unit_id":"`+s.ID.String()+`"}`); w.Code != http.StatusOK {
				e.t.Fatalf("move the ua-b row to ua-s = %d %s", w.Code, w.Body)
			}
		}, true},
		{"the ua-b row lowered below the ua-a2 row", func(e *unitAdminEnv) {
			if _, err := e.pool.Exec(context.Background(), `UPDATE governance_memberships SET priority = 1 WHERE id = $1`, e.rowOf(person, e.b).ID); err != nil {
				e.t.Fatalf("lower the ua-b row's priority: %v", err)
			}
		}, true},
		{"an unrelated unit edited", func(e *unitAdminEnv) {
			if w := e.admin(http.MethodPut, "/api/v1/governance/units/"+e.a2.ID.String(),
				e.restate(e.a2, map[string]any{"overlay_limits": map[string]any{"deny_interactive": true}})); w.Code != http.StatusOK {
				e.t.Fatalf("edit ua-a2 = %d %s", w.Code, w.Body)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newUnitAdminEnv(t, false)
			ch := e.legacyDelete(person, e.b, e.a2)
			deleted := e.rowOf(person, e.a1)
			tc.edit(e)
			w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), "")
			ms, err := e.pg.ListGovernanceMemberships(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			kept := slices.ContainsFunc(ms, func(m types.GovernanceMembership) bool { return m.ID == deleted.ID })
			if tc.stale {
				if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale || !kept {
					t.Fatalf("approving = %d %s with the row kept = %v, want 409 %s and the row kept", w.Code, w.Body, kept, reasonGovernanceChangeStale)
				}
				return
			}
			if w.Code != http.StatusOK || kept {
				t.Fatalf("approving = %d %s with the row kept = %v, want 200 and the row deleted", w.Code, w.Body, kept)
			}
			if c := treeCeiling(t, e.govEnv, treeCtx(person, nil, "")); unitIDOf(c) != e.b.ID {
				t.Errorf("the person's context is %s after the delete, want ua-b", unitIDOf(c))
			}
		})
	}
}

// TestLegacyMoveFollowsTheDatabaseOrder: which legacy row a move hands a person is read in the
// resolver's own order, so the database's collation sorts the unit names, not Go's byte order. The
// person's rows are in ua-a1 and UA-M at one priority, and the ua-a1 row moves to ua-n below it. Under
// a case-folding collation ua-a1 < UA-M < ua-n, so the move hands the person UA-M and is held; in byte
// order UA-M sorts first on both sides and the move changes nothing. The move's answer and the unit
// recorded as activated must match the order the database applies, and the resolver's pick after it.
func TestLegacyMoveFollowsTheDatabaseOrder(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	m := treeChild(t, e.govEnv, uaUnitBody("UA-M", store.DefaultUnitID, uaDomains("pypi.org"), nil))
	group := types.CapabilitySubjectGroup
	w, to, _ := e.legacyMove(legacyRow{unit: e.a1, st: group, subject: "ua-g1", priority: 5},
		legacyRow{unit: m, st: group, subject: "ua-g2", priority: 5}, "ua-n")
	var order []uuid.UUID
	rows, err := e.pool.Query(context.Background(), `SELECT id FROM governance_units WHERE id = ANY($1) ORDER BY name`, []uuid.UUID{e.a1.ID, m.ID, to.ID})
	if err == nil {
		order, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	}
	if err != nil {
		t.Fatalf("read the database's order of the unit names: %v", err)
	}
	var collation string
	_ = e.pool.QueryRow(context.Background(), `SELECT datcollate FROM pg_database WHERE datname = current_database()`).Scan(&collation)
	held := slices.Equal(order, []uuid.UUID{e.a1.ID, m.ID, to.ID})
	t.Logf("collation %q orders the names %v: the move is held = %v (byte order would never hold it)", collation, order, held)
	who := treeCtx("sub-ua-collation", []string{"ua-g1", "ua-g2"}, "")
	if !held {
		if w.Code != http.StatusOK || e.pendingCount() != 0 {
			t.Fatalf("move = %d %s with %d pending, want 200 and none", w.Code, w.Body, e.pendingCount())
		}
		if c := treeCeiling(t, e.govEnv, who); unitIDOf(c) != m.ID {
			t.Errorf("the person's context is %s after the move, want UA-M as before it", unitIDOf(c))
		}
		return
	}
	ch := e.pending(w)
	stored, err := e.pg.GetGovernanceChange(context.Background(), ch.ID)
	var diff governanceDiffBody
	if err != nil || json.Unmarshal(stored.Diff, &diff) != nil || len(diff.Activates) != 1 || diff.Activates[0].UnitID != m.ID {
		t.Fatalf("the held move records activates %+v (%v), want UA-M alone", diff.Activates, err)
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("a security admin approving = %d %s, want 200", w.Code, w.Body)
	}
	if c := treeCeiling(t, e.govEnv, who); unitIDOf(c) != m.ID {
		t.Errorf("the resolver picks %s after the move, want UA-M, the unit the move recorded", unitIDOf(c))
	}
}

// legacyRename seeds two legacy rows at priority 5, one in ua-a1 and one in ua-b, of two groups one
// person is in or of one person (st), and has ua-a1's admin rename ua-a1 to name. It returns the
// answer and that person.
func (e *unitAdminEnv) legacyRename(st types.CapabilitySubjectType, name string) (*httptest.ResponseRecorder, context.Context) {
	e.t.Helper()
	subjects, who := [2]string{"ua-eng", "ua-research"}, treeCtx("sub-ua-tie", []string{"ua-eng", "ua-research"}, "")
	if st == types.CapabilitySubjectUser {
		subjects, who = [2]string{"sub-ua-tie", "sub-ua-tie"}, treeCtx("sub-ua-tie", nil, "")
	}
	setLegacy(e.t, e.govEnv, treeMember(e.t, e.govEnv, e.a1, st, subjects[0], 5, false).ID,
		treeMember(e.t, e.govEnv, e.b, st, subjects[1], 5, false).ID)
	return e.call(e.adminA1, http.MethodPut, "/api/v1/governance/units/"+e.a1.ID.String(), e.restate(e.a1, map[string]any{"name": name})), who
}

// TestLegacyRenameUnitAdmin: at equal priority the unit name breaks a tie between legacy rows, so a
// unit admin's rename of ua-a1 past ua-b hands the person ua-b. It is held, with the rows two groups
// the person is in or the person's own two rows, and ua-a1's peer admin, who does not govern ua-b, is
// refused; a security admin approves. A rename that keeps the order is decided over ua-a1 alone: its
// peer admin approves the groups' rename, held as an equal-priority group tie, and the person's own
// rows' rename applies directly.
func TestLegacyRenameUnitAdmin(t *testing.T) {
	group, user := types.CapabilitySubjectGroup, types.CapabilitySubjectUser
	for _, tc := range []struct {
		what         string
		st           types.CapabilitySubjectType
		name         string
		held, shifts bool
	}{
		{"overlapping groups renamed past ua-b (the witness)", group, "ua-zeta", true, true},
		{"one person's two rows renamed past ua-b", user, "ua-zeta", true, true},
		{"overlapping groups renamed with the order kept", group, "ua-a0", true, false},
		{"one person's two rows renamed with the order kept", user, "ua-a0", false, false},
	} {
		t.Run(tc.what, func(t *testing.T) {
			e := newUnitAdminEnv(t, false)
			w, who := e.legacyRename(tc.st, tc.name)
			selected := func() uuid.UUID {
				t.Helper()
				return unitIDOf(treeCeiling(t, e.govEnv, who))
			}
			want := e.a1.ID
			if tc.shifts {
				want = e.b.ID
			}
			if !tc.held {
				if w.Code != http.StatusOK || e.pendingCount() != 0 || e.profile(e.a1.ID).Name != tc.name {
					t.Fatalf("rename = %d %s with %d pending, want 200, none and the name %s", w.Code, w.Body, e.pendingCount(), tc.name)
				}
				if got := selected(); got != want {
					t.Errorf("selected by %s after the rename, want ua-a1", got)
				}
				return
			}
			ch := e.pending(w)
			if got := selected(); got != e.a1.ID || e.profile(e.a1.ID).Name != "ua-a1" {
				t.Fatalf("before approval the person is selected by %s with ua-a1 named %q, want ua-a1 unrenamed", got, e.profile(e.a1.ID).Name)
			}
			approver := e.peerA1
			if tc.shifts {
				if w := e.call(e.peerA1, http.MethodPost, approvePath(ch.ID), ""); !scopeRefused(t, w) {
					t.Errorf("ua-a1's peer admin approving = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
				}
				approver = e.alice
			}
			if w := e.call(approver, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
				t.Fatalf("approving = %d %s, want 200", w.Code, w.Body)
			}
			if got := selected(); got != want || e.profile(e.a1.ID).Name != tc.name {
				t.Errorf("after approval the person is selected by %s with ua-a1 named %q, want %s and %s", got, e.profile(e.a1.ID).Name, want, tc.name)
			}
		})
	}
}

// TestLegacyRenameHeldGoesStale: a held rename is decided over the units it would activate when held
// (the witness: groups in ua-a1 and ua-b at one priority, ua-a1 renamed ua-zeta, so ua-b). An edit
// since that changes what the rename would do makes the approval stale, even a security admin's, and
// keeps the name: the ua-b row raised above the ua-a1 row's priority. An unrelated edit since leaves
// the approval to apply.
func TestLegacyRenameHeldGoesStale(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(e *unitAdminEnv)
		stale bool
	}{
		{"the ua-b row's priority raised above the ua-a1 row's", func(e *unitAdminEnv) {
			if _, err := e.pool.Exec(context.Background(), `UPDATE governance_memberships SET priority = 9 WHERE id = $1`, e.rowOf("ua-research", e.b).ID); err != nil {
				e.t.Fatalf("raise the ua-b row's priority: %v", err)
			}
		}, true},
		{"an unrelated unit edited", func(e *unitAdminEnv) {
			if w := e.admin(http.MethodPut, "/api/v1/governance/units/"+e.a2.ID.String(),
				e.restate(e.a2, map[string]any{"overlay_limits": map[string]any{"deny_interactive": true}})); w.Code != http.StatusOK {
				e.t.Fatalf("edit ua-a2 = %d %s", w.Code, w.Body)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newUnitAdminEnv(t, false)
			w, _ := e.legacyRename(types.CapabilitySubjectGroup, "ua-zeta")
			ch := e.pending(w)
			tc.edit(e)
			w = e.call(e.alice, http.MethodPost, approvePath(ch.ID), "")
			name := e.profile(e.a1.ID).Name
			if tc.stale {
				if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale || name != "ua-a1" {
					t.Fatalf("approving = %d %s with ua-a1 named %q, want 409 %s and the name kept", w.Code, w.Body, name, reasonGovernanceChangeStale)
				}
				return
			}
			if w.Code != http.StatusOK || name != "ua-zeta" {
				t.Fatalf("approving = %d %s with ua-a1 named %q, want 200 and ua-zeta", w.Code, w.Body, name)
			}
		})
	}
}

// legacyRepost seeds legacy rows of two groups one person is in, ua-eng in ua-a1 at priority a1 and
// ua-research in ua-b at priority b, and has ua-a1's admin re-post the ua-a1 row at priority to. It
// returns the answer and that person.
func (e *unitAdminEnv) legacyRepost(a1, b, to int) (*httptest.ResponseRecorder, context.Context) {
	e.t.Helper()
	group := types.CapabilitySubjectGroup
	setLegacy(e.t, e.govEnv, treeMember(e.t, e.govEnv, e.a1, group, "ua-eng", a1, false).ID,
		treeMember(e.t, e.govEnv, e.b, group, "ua-research", b, false).ID)
	body, _ := json.Marshal(map[string]any{"subject_type": group, "subject": "ua-eng", "unit_id": e.a1.ID, "priority": to})
	return e.call(e.adminA1, http.MethodPost, "/api/v1/governance/memberships", string(body)),
		treeCtx("sub-ua-tie", []string{"ua-eng", "ua-research"}, "")
}

// shiftUnits is the units a held change's diff records as activated and vacated.
func shiftUnits(t *testing.T, ch types.GovernanceChange) []uuid.UUID {
	t.Helper()
	var d governanceDiffBody
	if err := json.Unmarshal(ch.Diff, &d); err != nil {
		t.Fatal(err)
	}
	var out []uuid.UUID
	for _, a := range slices.Concat(d.Activates, d.Vacates) {
		out = append(out, a.UnitID)
	}
	return out
}

// TestLegacyRepostUnitAdmin: a legacy row's priority orders it against another legacy row, so a unit
// admin's re-post of a person's ua-a1 row at a new priority can hand them ua-b (lowered below the ua-b
// row) or take them out of it (raised above it). Either is held with ua-b recorded, ua-a1's peer
// admin, who does not govern ua-b, is refused, and an admin of both ua-a1 and ua-b approves. A re-post
// that keeps the order, the same priority included, is held with nothing recorded and decided over
// ua-a1 alone: its peer admin approves.
func TestLegacyRepostUnitAdmin(t *testing.T) {
	for _, tc := range []struct {
		what          string
		a1, b, to     int
		before, after string
	}{
		{"lowered below the ua-b row", 5, 5, 1, "ua-a1", "ua-b"},
		{"raised above the ua-b row", 5, 6, 9, "ua-b", "ua-a1"},
		{"raised with the order kept", 5, 5, 7, "ua-a1", "ua-a1"},
		{"re-posted at the same priority", 5, 5, 5, "ua-a1", "ua-a1"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			e := newUnitAdminEnv(t, false)
			w, who := e.legacyRepost(tc.a1, tc.b, tc.to)
			names := map[uuid.UUID]string{e.a1.ID: "ua-a1", e.b.ID: "ua-b"}
			selected := func() string {
				t.Helper()
				return names[unitIDOf(treeCeiling(t, e.govEnv, who))]
			}
			ch := e.pending(w)
			shifts := tc.before != tc.after
			if got := shiftUnits(t, ch); slices.Contains(got, e.b.ID) != shifts || (!shifts && len(got) > 0) {
				t.Errorf("the held re-post records %v, want ua-b recorded = %v and nothing else without a shift", got, shifts)
			}
			if got := selected(); got != tc.before {
				t.Errorf("selected by %s before approval, want %s", got, tc.before)
			}
			approver := e.peerA1
			if shifts {
				if w := e.call(e.peerA1, http.MethodPost, approvePath(ch.ID), ""); !scopeRefused(t, w) {
					t.Errorf("ua-a1's peer admin approving = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
				}
				both := e.fresh("sub-ua-admin-ab")
				e.grant(e.a1, types.CapabilitySubjectUser, both)
				e.grant(e.b, types.CapabilitySubjectUser, both)
				approver = uaSession(t, both, []string{}, false)
			}
			if w := e.call(approver, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
				t.Fatalf("approving = %d %s, want 200", w.Code, w.Body)
			}
			if got, p := selected(), e.rowOf("ua-eng", e.a1).Priority; got != tc.after || p != tc.to {
				t.Errorf("after approval selected by %s with the row at priority %d, want %s and %d", got, p, tc.after, tc.to)
			}
		})
	}
}

// TestLegacyRepostHeldGoesStale: a held re-post is decided over the units it would activate when held
// (the ua-a1 row lowered below the ua-b row at one priority, so ua-b). An edit since that changes what
// the re-post would do makes the approval stale, even a security admin's, and keeps the priority: the
// ua-b row lowered below the re-posted priority, or ua-b renamed to sort before ua-a1, which hands the
// person ua-b already. An unrelated edit since leaves the approval to apply.
func TestLegacyRepostHeldGoesStale(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(e *unitAdminEnv)
		stale bool
	}{
		{"the ua-b row lowered below the re-posted priority", func(e *unitAdminEnv) {
			if _, err := e.pool.Exec(context.Background(), `UPDATE governance_memberships SET priority = 0 WHERE id = $1`, e.rowOf("ua-research", e.b).ID); err != nil {
				e.t.Fatalf("lower the ua-b row's priority: %v", err)
			}
		}, true},
		{"ua-b renamed to sort before ua-a1", func(e *unitAdminEnv) {
			if w := e.admin(http.MethodPut, "/api/v1/governance/units/"+e.b.ID.String(), e.restate(e.b, map[string]any{"name": "ua-0"})); w.Code != http.StatusOK {
				e.t.Fatalf("rename ua-b = %d %s", w.Code, w.Body)
			}
		}, true},
		{"an unrelated unit edited", func(e *unitAdminEnv) {
			if w := e.admin(http.MethodPut, "/api/v1/governance/units/"+e.a2.ID.String(),
				e.restate(e.a2, map[string]any{"overlay_limits": map[string]any{"deny_interactive": true}})); w.Code != http.StatusOK {
				e.t.Fatalf("edit ua-a2 = %d %s", w.Code, w.Body)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newUnitAdminEnv(t, false)
			w, _ := e.legacyRepost(5, 5, 1)
			ch := e.pending(w)
			tc.edit(e)
			w = e.call(e.alice, http.MethodPost, approvePath(ch.ID), "")
			p := e.rowOf("ua-eng", e.a1).Priority
			if tc.stale {
				if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale || p != 5 {
					t.Fatalf("approving = %d %s with the row at priority %d, want 409 %s and 5 kept", w.Code, w.Body, p, reasonGovernanceChangeStale)
				}
				return
			}
			if w.Code != http.StatusOK || p != 1 {
				t.Fatalf("approving = %d %s with the row at priority %d, want 200 and 1", w.Code, w.Body, p)
			}
		})
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
