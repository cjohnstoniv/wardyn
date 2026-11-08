// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The strict-unit pins: a held change to a unit itself (its walls, locks, mode, name or parent) is
// decided by a security admin or by an admin of a unit strictly above it, and above every other unit
// the change touches. The env is in governance_unit_admin_pg_test.go.

// adminOf is a fresh person granted on u, and their session.
func (e *unitAdminEnv) adminOf(u types.GovernanceUnit) (sub string, session *http.Cookie) {
	e.t.Helper()
	sub = e.fresh("sub-ua-strict")
	e.grant(u, types.CapabilitySubjectUser, sub)
	return sub, uaSession(e.t, sub, []string{}, false)
}

// refusedAndPending fails the test unless approving ch as c is a 403 unit_scope_denied that leaves it pending.
func (e *unitAdminEnv) refusedAndPending(what string, c *http.Cookie, ch types.GovernanceChange) {
	e.t.Helper()
	if w := e.call(c, http.MethodPost, approvePath(ch.ID), ""); !scopeRefused(e.t, w) {
		e.t.Errorf("%s approving = %d %s, want 403 unit_scope_denied", what, w.Code, w.Body)
	}
	if got := e.changeState(ch.ID); got != types.GovernanceChangePending {
		e.t.Fatalf("after %s was refused the change is %s, want pending", what, got)
	}
}

// wideA1 is a widening of ua-a1's walls, which its own admin can only hold.
func (e *unitAdminEnv) wideA1() string {
	return uaUnitBody("ua-a1", e.a.ID, uaDomains("pypi.org", "github.com", "npmjs.org"), nil)
}

// TestUnitOwnWallsPeerCannotApprove: ua-a1 has two admins. One proposes a widening of its walls and the
// other's approval is refused 403 unit_scope_denied; the change lists the security admins and the
// grants on ua-a, above the unit, and not the unit's own.
func TestUnitOwnWallsPeerCannotApprove(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	ch := e.pending(e.call(e.adminA1, http.MethodPut, "/api/v1/governance/units/"+e.a1.ID.String(), e.wideA1()))
	e.refusedAndPending("ua-a1's peer admin", e.peerA1, ch)
	if got := e.domainsOf(e.a1); len(got) != 2 {
		t.Errorf("a refused approval changed ua-a1 to %v", got)
	}
	approvers := e.readApprovers(ch.ID)
	if len(approvers) < 2 || approvers[0].Kind != "security" {
		t.Fatalf("approvers %+v, want the security admins first and then the grants on ua-a", approvers)
	}
	for _, a := range approvers[1:] {
		if a.Kind != "unit_admin" || a.UnitName != "ua-a" {
			t.Errorf("approver %+v, want a grant on ua-a", a)
		}
	}
	if !listsUnitAdmin(approvers, uaSubPeerA) || listsUnitAdmin(approvers, uaSubPeerA1) {
		t.Errorf("approvers %+v, want ua-a's peer admin listed and ua-a1's not", approvers)
	}
}

// TestUnitOwnWallsAncestorAdminApproves: the same widening is approved by an admin granted on ua-a1's
// parent.
func TestUnitOwnWallsAncestorAdminApproves(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	ch := e.pending(e.call(e.adminA1, http.MethodPut, "/api/v1/governance/units/"+e.a1.ID.String(), e.wideA1()))
	if w := e.call(e.adminA, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("an admin of ua-a approving = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.domainsOf(e.a1); len(got) != 3 {
		t.Errorf("approved, ua-a1 allows %v, want the three domains", got)
	}
}

// TestUnitOwnLockRemovalAndModePeerCannotApprove: removing a lock on a unit and switching it to
// pass-through are the unit's own changes, so its peer admin is refused and an admin above it decides.
func TestUnitOwnLockRemovalAndModePeerCannotApprove(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	path := "/api/v1/governance/units/" + e.a1.ID.String()

	t.Run("lock removal", func(t *testing.T) {
		if w := e.call(e.adminA1, http.MethodPut, path, e.restate(e.a1, map[string]any{"locked_knobs": []string{"min_confinement_class"}})); w.Code != http.StatusOK {
			t.Fatalf("locking ua-a1 = %d %s, want 200", w.Code, w.Body)
		}
		ch := e.pending(e.call(e.adminA1, http.MethodPut, path, e.restate(e.a1, map[string]any{"locked_knobs": []string{}})))
		e.refusedAndPending("ua-a1's peer admin", e.peerA1, ch)
		if w := e.call(e.adminA, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
			t.Fatalf("an admin of ua-a approving = %d %s, want 200", w.Code, w.Body)
		}
		if locks := e.profile(e.a1.ID).LockedKnobs; len(locks) != 0 {
			t.Errorf("approved, ua-a1 still locks %v", locks)
		}
	})

	t.Run("mode", func(t *testing.T) {
		ch := e.pending(e.call(e.adminA1, http.MethodPut, path,
			uaUnitBody("ua-a1", e.a.ID, map[string]any{}, map[string]any{"mode": types.UnitModePassThrough})))
		e.refusedAndPending("ua-a1's peer admin", e.peerA1, ch)
		if w := e.call(e.adminA, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
			t.Fatalf("an admin of ua-a approving = %d %s, want 200", w.Code, w.Body)
		}
		if got := e.profile(e.a1.ID).Mode; got != types.UnitModePassThrough {
			t.Errorf("approved, ua-a1's mode is %q, want %q", got, types.UnitModePassThrough)
		}
	})
}

// TestUnitReparentStrict: moving X up from p1 to its grandparent m is held, because m's walls are
// wider. X's own admin, an admin of p1 alone and an admin granted on the new parent m itself are each
// refused; an admin of ua-a, above m, approves.
func TestUnitReparentStrict(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	m := e.child(e.a)
	p1 := treeChild(t, e.govEnv, uaUnitBody(e.fresh("ua-p1"), m.ID, uaDomains("pypi.org"), nil))
	x := e.child(p1)
	_, proposer := e.adminOf(e.a)
	ch := e.pending(e.call(proposer, http.MethodPut, "/api/v1/governance/units/"+x.ID.String(), e.restate(x, map[string]any{"parent_id": m.ID})))

	for _, who := range []struct {
		what string
		unit types.GovernanceUnit
	}{{"an admin of the new parent", m}, {"an admin of the old parent alone", p1}, {"an admin of the moving unit", x}} {
		_, c := e.adminOf(who.unit)
		e.refusedAndPending(who.what, c, ch)
	}
	if p := e.profile(x.ID).ParentID; p == nil || *p != p1.ID {
		t.Fatalf("a refused approval moved the unit to %v", p)
	}
	if w := e.call(e.adminA, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("an admin above the new parent approving = %d %s, want 200", w.Code, w.Body)
	}
	if p := e.profile(x.ID).ParentID; p == nil || *p != m.ID {
		t.Errorf("approved, the unit's parent is %v, want %s", p, m.Name)
	}
}

// TestUnitRenameReorderStrict: renaming ua-a1 past ua-a2 reorders two legacy rows and so touches ua-a2.
// An admin granted on both units is not above either and is refused, as ua-a1's peer admin is; an admin
// of ua-a, above both, approves.
func TestUnitRenameReorderStrict(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	group := types.CapabilitySubjectGroup
	setLegacy(t, e.govEnv, treeMember(t, e.govEnv, e.a1, group, "ua-eng", 5, false).ID,
		treeMember(t, e.govEnv, e.a2, group, "ua-research", 5, false).ID)
	ch := e.pending(e.call(e.adminA1, http.MethodPut, "/api/v1/governance/units/"+e.a1.ID.String(), e.restate(e.a1, map[string]any{"name": "ua-zeta"})))
	if got := shiftUnits(t, ch); !slices.Contains(got, e.a2.ID) {
		t.Fatalf("the held rename records %v, want ua-a2 among them", got)
	}
	both := e.fresh("sub-ua-admin-both")
	e.grant(e.a1, types.CapabilitySubjectUser, both)
	e.grant(e.a2, types.CapabilitySubjectUser, both)
	e.refusedAndPending("an admin of both units", uaSession(t, both, []string{}, false), ch)
	e.refusedAndPending("ua-a1's peer admin", e.peerA1, ch)
	if w := e.call(e.adminA, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("an admin of ua-a approving = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.profile(e.a1.ID).Name; got != "ua-zeta" {
		t.Errorf("approved, ua-a1 is named %q, want ua-zeta", got)
	}
}

// TestUnitCreateStillParentAdmin: a held create of a child is decided by an admin granted on its parent.
func TestUnitCreateStillParentAdmin(t *testing.T) {
	e := newUnitAdminEnv(t, true)
	ch := e.pending(e.call(e.alice, http.MethodPost, "/api/v1/governance/units", uaUnitBody("ua-new-child", e.a1.ID, map[string]any{}, nil)))
	if w := e.call(e.peerA1, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("an admin of the new unit's parent approving = %d %s, want 200", w.Code, w.Body)
	}
	all, err := store.ListGovernanceUnitsQ(context.Background(), e.pool)
	if err != nil {
		t.Fatalf("list units: %v", err)
	}
	if !slices.ContainsFunc(all, func(u types.GovernanceUnit) bool { return u.Name == "ua-new-child" }) {
		t.Error("the approved create left no ua-new-child")
	}
}

// TestUnitPendingChangeJudgedByNewRule: a change that has been waiting is decided under the rule in force
// when it is decided: its stored row names no approver, so its peer admin is refused and an admin above
// the unit approves.
func TestUnitPendingChangeJudgedByNewRule(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	ch := e.pending(e.call(e.adminA1, http.MethodPut, "/api/v1/governance/units/"+e.a1.ID.String(), e.wideA1()))
	if _, err := e.pool.Exec(context.Background(), `UPDATE governance_changes SET proposed_at = proposed_at - interval '2 days' WHERE id = $1`, ch.ID); err != nil {
		t.Fatalf("age the change: %v", err)
	}
	e.refusedAndPending("ua-a1's peer admin", e.peerA1, ch)
	if w := e.call(e.adminA, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("an admin of ua-a approving = %d %s, want 200", w.Code, w.Body)
	}
}

// TestUnitOwnNarrowingStillDirect: a unit admin's narrowing of their own unit applies at once, with no
// change held.
func TestUnitOwnNarrowingStillDirect(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	w := e.call(e.adminA1, http.MethodPut, "/api/v1/governance/units/"+e.a1.ID.String(), uaUnitBody("ua-a1", e.a.ID, uaDomains("pypi.org"), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("a narrowing of ua-a1 by its own admin = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.domainsOf(e.a1); !slices.Equal(got, []string{"pypi.org"}) || e.pendingCount() != 0 {
		t.Errorf("ua-a1 allows %v with %d pending, want [pypi.org] and none", got, e.pendingCount())
	}
}
