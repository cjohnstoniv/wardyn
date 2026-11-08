// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The envelope pins: below an envelope's root, a unit admin's edit within the root's walls and the add
// or move of an approved group apply directly; the root's own changes, and unit admin grants on it,
// are decided by the security tier alone; and an envelope that cannot be read proves nothing. Every
// test seeds the envelope on ua-a1 (adminA1 and peerA1 its admins, adminA and peerA above it) unless
// it says otherwise.

const envGroup = "plat-a-users"

// seedEnvelope makes groups (already canonical) the envelope of u, as the store writes it.
func (e *unitAdminEnv) seedEnvelope(u types.GovernanceUnit, groups ...string) {
	e.t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		e.t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	if _, err := store.SetEnvelopeQ(ctx, tx, u.ID, groups, "seed"); err != nil {
		e.t.Fatalf("seed the envelope of %s: %v", u.Name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		e.t.Fatalf("commit the envelope of %s: %v", u.Name, err)
	}
}

// envelopesUnreadable renames the envelope table away (on) or back (off), so every read of it fails.
func (e *unitAdminEnv) envelopesUnreadable(on bool) {
	e.t.Helper()
	from, to := "governance_envelopes", "governance_envelopes_unreadable"
	if !on {
		from, to = to, from
	}
	if _, err := e.pool.Exec(context.Background(), `ALTER TABLE `+from+` RENAME TO `+to); err != nil {
		e.t.Fatalf("rename %s: %v", from, err)
	}
}

// envBelow is a new unit under parent with overlay, written by the admin token.
func (e *unitAdminEnv) envBelow(parent types.GovernanceUnit, overlay map[string]any) types.GovernanceUnit {
	e.t.Helper()
	return treeChild(e.t, e.govEnv, uaUnitBody(e.fresh("env-unit"), parent.ID, overlay, nil))
}

func envUnitPath(u types.GovernanceUnit) string { return "/api/v1/governance/units/" + u.ID.String() }

// envUnitBody is u's write body under parent with overlay.
func envUnitBody(u types.GovernanceUnit, parent uuid.UUID, overlay map[string]any) string {
	return uaUnitBody(u.Name, parent, overlay, nil)
}

func envGroupBody(group string, unit uuid.UUID) string {
	return `{"subject_type":"group","subject":"` + group + `","unit_id":"` + unit.String() + `","priority":1}`
}

func envMove(m types.GovernanceMembership) string {
	return "/api/v1/governance/memberships/" + m.ID.String()
}

func envMoveBody(to uuid.UUID) string { return `{"unit_id":"` + to.String() + `"}` }

// readApprovers is who may decide change id, as a security admin reads it.
func (e *unitAdminEnv) readApprovers(id uuid.UUID) []changeApprover {
	e.t.Helper()
	var read governanceChangeRead
	w := e.call(e.alice, http.MethodGet, "/api/v1/governance/changes/"+id.String(), "")
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &read) != nil {
		e.t.Fatalf("read change %s = %d %s", id, w.Code, w.Body)
	}
	return read.Approvers
}

// dryRunApprovers is who a unit dry run of body (a unit write body) on u, asked by c, says could decide it.
func (e *unitAdminEnv) dryRunApprovers(c *http.Cookie, u types.GovernanceUnit, body string) []changeApprover {
	e.t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		e.t.Fatalf("body %s: %v", body, err)
	}
	m["target"], m["id"] = dryRunTargetUnit, u.ID
	raw, _ := json.Marshal(m)
	var rep treeDryRunReport
	w := e.call(c, http.MethodPost, "/api/v1/governance/dry-run", string(raw))
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rep) != nil {
		e.t.Fatalf("dry run on %s = %d %s", u.Name, w.Code, w.Body)
	}
	return rep.Approvers
}

// reject has a security admin reject change ch, so its target takes another proposal.
func (e *unitAdminEnv) reject(ch types.GovernanceChange) {
	e.t.Helper()
	if w := e.call(e.bob, http.MethodPost, rejectPath(ch.ID), ""); w.Code != http.StatusOK {
		e.t.Fatalf("rejecting %s = %d %s, want 200", ch.ID, w.Code, w.Body)
	}
}

// securitySurfaceRefused reports a 403 security_admin_surface: the refusal of a caller who governs the
// unit but whose change is the security tier's alone to decide.
func securitySurfaceRefused(t *testing.T, w *httptest.ResponseRecorder) bool {
	t.Helper()
	return w.Code == http.StatusForbidden && wireReason(t, w) == string(authz.ReasonSecurityAdminSurface)
}

func securityOnlyList(a []changeApprover) bool { return len(a) == 1 && a[0].Kind == "security" }

func listsUnitAdmin(a []changeApprover, sub string) bool {
	return slices.ContainsFunc(a, func(x changeApprover) bool { return x.Kind == "unit_admin" && x.Name == sub })
}

// membershipAt reports whether subject has a membership of subject type st in u.
func (e *unitAdminEnv) membershipAt(st types.CapabilitySubjectType, subject string, u types.GovernanceUnit) bool {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM governance_memberships WHERE subject_type = $1 AND subject = $2 AND unit_id = $3`, st, subject, u.ID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n == 1
}

// envTree is a unit c below ua-a1 allowing both of ua-a1's domains and g below c allowing one: g can be
// widened back to ua-a1's walls.
func (e *unitAdminEnv) envTree() (c, g types.GovernanceUnit) {
	e.t.Helper()
	c = e.envBelow(e.a1, uaDomains("pypi.org", "github.com"))
	g = e.envBelow(c, uaDomains("pypi.org"))
	return c, g
}

// ── row 1: walls inside the envelope ─────────────────────────────────────────

// TestEnvelopeDescendantWideningApplies: a unit admin widening a grandchild of the root back to its
// parent's walls applies directly once ua-a1 is an envelope's root, with no change row. Without the
// envelope it is held, and so is the same widening of a unit outside it.
func TestEnvelopeDescendantWideningApplies(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	c, g := e.envTree()
	wide := envUnitBody(g, c.ID, uaDomains("pypi.org", "github.com"))
	held := e.pending(e.call(e.adminA1, http.MethodPut, envUnitPath(g), wide)) // no envelope yet: held
	if got := e.domainsOf(g); len(got) != 1 {
		t.Fatalf("a held widening changed %s to %v", g.Name, got)
	}
	e.reject(held)

	e.seedEnvelope(e.a1)
	before := e.pendingCount()
	if w := e.call(e.adminA1, http.MethodPut, envUnitPath(g), wide); w.Code != http.StatusOK {
		t.Fatalf("a widening within the root's walls = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.domainsOf(g); len(got) != 2 || e.pendingCount() != before {
		t.Errorf("%s allows %v with %d pending (was %d), want both domains and no new change", g.Name, got, e.pendingCount(), before)
	}

	outside := e.envBelow(e.a, uaDomains("pypi.org"))
	e.pending(e.call(e.adminA, http.MethodPut, envUnitPath(outside), envUnitBody(outside, e.a.ID, uaDomains("pypi.org", "github.com"))))
}

// TestEnvelopeReparentWithinApplies: moving a unit from a narrower parent to a wider one, both below the
// root, applies directly; the same move with no envelope is held.
func TestEnvelopeReparentWithinApplies(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	narrow := e.envBelow(e.a1, uaDomains("pypi.org"))
	wide := e.envBelow(e.a1, uaDomains("pypi.org", "github.com"))
	g := e.envBelow(narrow, map[string]any{})
	e.reject(e.pending(e.call(e.adminA1, http.MethodPut, envUnitPath(g), envUnitBody(g, wide.ID, map[string]any{}))))

	e.seedEnvelope(e.a1)
	if w := e.call(e.adminA1, http.MethodPut, envUnitPath(g), envUnitBody(g, wide.ID, map[string]any{})); w.Code != http.StatusOK {
		t.Fatalf("a re-parent within the envelope = %d %s, want 200", w.Code, w.Body)
	}
	if p := e.profile(g.ID).ParentID; p == nil || *p != wide.ID {
		t.Errorf("%s's parent is %v, want %s", g.Name, p, wide.Name)
	}
}

// TestEnvelopeReparentOutHeld: moving a unit from below the root to a wider unit outside it is held,
// though the caller governs both.
func TestEnvelopeReparentOutHeld(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	narrow := e.envBelow(e.a1, uaDomains("pypi.org"))
	g := e.envBelow(narrow, map[string]any{})
	e.seedEnvelope(e.a1)
	e.pending(e.call(e.adminA, http.MethodPut, envUnitPath(g), envUnitBody(g, e.a.ID, map[string]any{})))
	if p := e.profile(g.ID).ParentID; p == nil || *p != narrow.ID {
		t.Errorf("a held re-parent moved %s to %v", g.Name, p)
	}
}

// TestEnvelopeRootWideningSecurityOnly: the root's own widening is held, as without an envelope, but only
// a security admin may decide it: a peer admin of the root and an admin above it are refused. With no
// envelope an admin above the root's approval applies it.
func TestEnvelopeRootWideningSecurityOnly(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	narrow := envUnitBody(e.a1, e.a.ID, uaDomains("pypi.org"))
	wide := envUnitBody(e.a1, e.a.ID, uaDomains("pypi.org", "github.com"))
	if w := e.call(e.adminA1, http.MethodPut, envUnitPath(e.a1), narrow); w.Code != http.StatusOK {
		t.Fatalf("narrowing the root = %d %s, want 200", w.Code, w.Body)
	}
	ch := e.pending(e.call(e.adminA1, http.MethodPut, envUnitPath(e.a1), wide))
	if w := e.call(e.peerA, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("no envelope: an admin above the root approving = %d %s, want 200", w.Code, w.Body)
	}

	e.seedEnvelope(e.a1)
	if w := e.call(e.adminA1, http.MethodPut, envUnitPath(e.a1), narrow); w.Code != http.StatusOK {
		t.Fatalf("narrowing the envelope's root = %d %s, want 200", w.Code, w.Body)
	}
	ch = e.pending(e.call(e.adminA1, http.MethodPut, envUnitPath(e.a1), wide))
	for who, c := range map[string]*http.Cookie{"a peer admin of the root": e.peerA1, "an admin above the root": e.adminA} {
		if w := e.call(c, http.MethodPost, approvePath(ch.ID), ""); !securitySurfaceRefused(t, w) {
			t.Errorf("%s approving = %d %s, want 403 security_admin_surface", who, w.Code, w.Body)
		}
	}
	if got := e.domainsOf(e.a1); len(got) != 1 || e.changeState(ch.ID) != types.GovernanceChangePending {
		t.Fatalf("a refused approval changed the root to %v (change %s)", got, e.changeState(ch.ID))
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("a security admin approving = %d %s, want 200", w.Code, w.Body)
	}
	if got := e.domainsOf(e.a1); len(got) != 2 {
		t.Errorf("approved, the root allows %v, want both domains", got)
	}
}

// TestEnvelopeRootLockRemovalSecurityOnly: removing a lock on the root is held for a security admin; the
// root's peer admin is refused.
func TestEnvelopeRootLockRemovalSecurityOnly(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	if w := e.call(e.adminA1, http.MethodPut, envUnitPath(e.a1), e.restate(e.a1, map[string]any{"locked_knobs": []string{"min_confinement_class"}})); w.Code != http.StatusOK {
		t.Fatalf("locking the root = %d %s, want 200", w.Code, w.Body)
	}
	e.seedEnvelope(e.a1)
	ch := e.pending(e.call(e.adminA1, http.MethodPut, envUnitPath(e.a1), e.restate(e.a1, map[string]any{"locked_knobs": []string{}})))
	if w := e.call(e.peerA1, http.MethodPost, approvePath(ch.ID), ""); !securitySurfaceRefused(t, w) {
		t.Errorf("the root's peer approving a lock removal = %d %s, want 403 security_admin_surface", w.Code, w.Body)
	}
	if !securityOnlyList(e.readApprovers(ch.ID)) {
		t.Errorf("approvers %+v, want the security admins alone", e.readApprovers(ch.ID))
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("a security admin approving = %d %s, want 200", w.Code, w.Body)
	}
	if locks := e.profile(e.a1.ID).LockedKnobs; len(locks) != 0 {
		t.Errorf("approved, the root still locks %v", locks)
	}
}

// TestEnvelopeRootContactSecurityOnly: a new contact on the root is held for a security admin.
func TestEnvelopeRootContactSecurityOnly(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	e.seedEnvelope(e.a1)
	ch := e.pending(e.call(e.adminA1, http.MethodPut, envUnitPath(e.a1),
		e.restate(e.a1, map[string]any{"contact": map[string]any{"owner": "Platform A", "email": "plat-a@example.com"}})))
	if w := e.call(e.peerA1, http.MethodPost, approvePath(ch.ID), ""); !securitySurfaceRefused(t, w) {
		t.Errorf("the root's peer approving a contact = %d %s, want 403 security_admin_surface", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("a security admin approving = %d %s, want 200", w.Code, w.Body)
	}
}

// TestEnvelopeDescendantLockRemovalPeerHeld: a lock set below the root keeps today's rule: its removal is
// held and a peer admin of the unit decides it.
func TestEnvelopeDescendantLockRemovalPeerHeld(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	c := e.envBelow(e.a1, map[string]any{})
	if w := e.call(e.adminA1, http.MethodPut, envUnitPath(c), e.restate(c, map[string]any{"locked_knobs": []string{"min_confinement_class"}})); w.Code != http.StatusOK {
		t.Fatalf("locking %s = %d %s, want 200", c.Name, w.Code, w.Body)
	}
	e.seedEnvelope(e.a1)
	ch := e.pending(e.call(e.adminA1, http.MethodPut, envUnitPath(c), e.restate(c, map[string]any{"locked_knobs": []string{}})))
	if a := e.readApprovers(ch.ID); !listsUnitAdmin(a, uaSubPeerA1) {
		t.Errorf("approvers %+v, want ua-a1's peer admin listed", a)
	}
	if w := e.call(e.peerA1, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("the peer approving = %d %s, want 200", w.Code, w.Body)
	}
}

// TestEnvelopeSecurityWriteFollowsSwitch: with the second-human switch on, a security admin's widening
// inside the envelope is held as any of theirs is, while a unit admin's applies directly.
func TestEnvelopeSecurityWriteFollowsSwitch(t *testing.T) {
	e := newUnitAdminEnv(t, true)
	c, g := e.envTree()
	e.seedEnvelope(e.a1)
	wide := envUnitBody(g, c.ID, uaDomains("pypi.org", "github.com"))
	e.reject(e.pending(e.call(e.alice, http.MethodPut, envUnitPath(g), wide)))
	if w := e.call(e.adminA1, http.MethodPut, envUnitPath(g), wide); w.Code != http.StatusOK {
		t.Fatalf("the unit admin's widening = %d %s, want 200", w.Code, w.Body)
	}
}

// TestEnvelopeApproversListSecurityOnly: a held change of the root lists the security admins alone, in
// the change read and in the dry run, while a held change below it lists the peer admins in both.
func TestEnvelopeApproversListSecurityOnly(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	e.seedEnvelope(e.a1)
	contact := e.restate(e.a1, map[string]any{"contact": map[string]any{"owner": "Platform A"}})
	if a := e.dryRunApprovers(e.adminA1, e.a1, contact); !securityOnlyList(a) {
		t.Errorf("dry run of a root change lists %+v, want the security admins alone", a)
	}
	ch := e.pending(e.call(e.adminA1, http.MethodPut, envUnitPath(e.a1), contact))
	if a := e.readApprovers(ch.ID); !securityOnlyList(a) {
		t.Errorf("the root change lists %+v, want the security admins alone", a)
	}

	c := e.envBelow(e.a1, map[string]any{})
	below := e.restate(c, map[string]any{"contact": map[string]any{"owner": "Team C"}})
	if a := e.dryRunApprovers(e.adminA1, c, below); !listsUnitAdmin(a, uaSubPeerA1) {
		t.Errorf("dry run of a change below the root lists %+v, want ua-a1's peer admin", a)
	}
	ch = e.pending(e.call(e.adminA1, http.MethodPut, envUnitPath(c), below))
	if a := e.readApprovers(ch.ID); !listsUnitAdmin(a, uaSubPeerA1) {
		t.Errorf("the change below the root lists %+v, want ua-a1's peer admin", a)
	}
}

// TestEnvelopeRootProposerSeesOwnChange: the unit admin who proposed a change of the root keeps seeing
// it though they cannot decide it: it is in their list, readable by id, and named in the 409 a repeat
// proposal gets. A peer admin of the same root, who proposed nothing, gets none of the three.
func TestEnvelopeRootProposerSeesOwnChange(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	e.seedEnvelope(e.a1)
	contact := e.restate(e.a1, map[string]any{"contact": map[string]any{"owner": "Platform A"}})
	ch := e.pending(e.call(e.adminA1, http.MethodPut, envUnitPath(e.a1), contact))

	listed := func(c *http.Cookie) bool {
		w := e.call(c, http.MethodGet, "/api/v1/governance/changes", "")
		var out []types.GovernanceChange
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("list = %d %s", w.Code, w.Body)
		}
		return slices.ContainsFunc(out, func(x types.GovernanceChange) bool { return x.ID == ch.ID })
	}
	if !listed(e.adminA1) {
		t.Error("the proposer's list omits their own change of the root")
	}
	if listed(e.peerA1) {
		t.Error("a peer admin's list shows a change they did not propose and cannot decide")
	}

	if w := e.call(e.adminA1, http.MethodGet, "/api/v1/governance/changes/"+ch.ID.String(), ""); w.Code != http.StatusOK {
		t.Errorf("the proposer reading their change = %d %s, want 200", w.Code, w.Body)
	}
	if w := e.call(e.peerA1, http.MethodGet, "/api/v1/governance/changes/"+ch.ID.String(), ""); w.Code != http.StatusNotFound {
		t.Errorf("a peer admin reading the change = %d %s, want 404", w.Code, w.Body)
	}

	repeat := func(c *http.Cookie) errorBody {
		w := e.call(c, http.MethodPut, envUnitPath(e.a1), contact)
		var body errorBody
		if w.Code != http.StatusConflict || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("a repeat proposal = %d %s, want 409", w.Code, w.Body)
		}
		return body
	}
	if body := repeat(e.adminA1); body.PendingChange == nil || body.PendingChange.ID != ch.ID {
		t.Errorf("the proposer's 409 carries pending_change %+v, want change %s", body.PendingChange, ch.ID)
	}
	if body := repeat(e.peerA1); body.PendingChange != nil {
		t.Errorf("a peer admin's 409 carries pending_change %+v, want none", body.PendingChange)
	}
}

// TestEnvelopeDryRunMatchesWrite: the dry run lists exactly the approvers the held write then lists, for
// the root with and without an envelope and for a unit below it.
func TestEnvelopeDryRunMatchesWrite(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	c := e.envBelow(e.a1, map[string]any{})
	same := func(what string, u types.GovernanceUnit, body string) []changeApprover {
		t.Helper()
		dry := e.dryRunApprovers(e.adminA1, u, body)
		ch := e.pending(e.call(e.adminA1, http.MethodPut, envUnitPath(u), body))
		held := e.readApprovers(ch.ID)
		if !slices.Equal(dry, held) {
			t.Errorf("%s: the dry run lists %+v, the held change %+v", what, dry, held)
		}
		e.approve(ch.ID)
		return held
	}
	owner := func(u types.GovernanceUnit, name string) string {
		return e.restate(u, map[string]any{"contact": map[string]any{"owner": name}})
	}
	if a := same("the root, no envelope", e.a1, owner(e.a1, "One")); !listsUnitAdmin(a, uaSubPeerA) || listsUnitAdmin(a, uaSubPeerA1) {
		t.Errorf("no envelope: %+v, want the admins above the root listed and its peer admin not", a)
	}
	e.seedEnvelope(e.a1)
	if a := same("the envelope's root", e.a1, owner(e.a1, "Two")); !securityOnlyList(a) {
		t.Errorf("the envelope's root: %+v, want the security admins alone", a)
	}
	if a := same("a unit below the root", c, owner(c, "Three")); !listsUnitAdmin(a, uaSubPeerA1) {
		t.Errorf("below the root: %+v, want the root's peer admin listed", a)
	}
}

// TestEnvelopeRootUnitAdminGrantSecurityOnly: a unit admin grant on the root, held for its proposer above
// the root, is decided by a security admin alone; with no envelope an admin above the root decides it.
func TestEnvelopeRootUnitAdminGrantSecurityOnly(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	grant := func() types.GovernanceChange {
		return e.pending(e.call(e.adminA, http.MethodPost, envUnitPath(e.a1)+"/admins",
			`{"subject_type":"user","subject":"`+e.fresh("sub-env-grantee")+`"}`))
	}
	ch := grant()
	if w := e.call(e.peerA, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("no envelope: an admin above approving = %d %s, want 200", w.Code, w.Body)
	}

	e.seedEnvelope(e.a1)
	ch = grant()
	if w := e.call(e.peerA, http.MethodPost, approvePath(ch.ID), ""); !securitySurfaceRefused(t, w) {
		t.Errorf("an admin above the root approving = %d %s, want 403 security_admin_surface", w.Code, w.Body)
	}
	if a := e.readApprovers(ch.ID); !securityOnlyList(a) {
		t.Errorf("approvers %+v, want the security admins alone", a)
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("a security admin approving = %d %s, want 200", w.Code, w.Body)
	}
}

// ── row 3: approved groups ───────────────────────────────────────────────────

// TestEnvelopeApprovedGroupAddApplies: a unit admin adds an approved group, which has no membership
// anywhere, to the root and to a unit below it directly, in any case spelling; the audit row names the
// envelope. Without the group on the list the same add is refused, as today.
func TestEnvelopeApprovedGroupAddApplies(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	c := e.envBelow(e.a1, map[string]any{})
	if w := e.call(e.adminA1, http.MethodPost, "/api/v1/governance/memberships", envGroupBody(envGroup, e.a1.ID)); !scopeRefused(t, w) {
		t.Fatalf("no envelope: adding a group nobody placed = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
	}
	e.seedEnvelope(e.a1, envGroup)
	for _, u := range []types.GovernanceUnit{e.a1, c} {
		if w := e.call(e.adminA1, http.MethodPost, "/api/v1/governance/memberships", envGroupBody("Plat-A-Users", u.ID)); w.Code != http.StatusCreated {
			t.Fatalf("adding the approved group to %s = %d %s, want 201", u.Name, w.Code, w.Body)
		}
		if !e.membershipAt(types.CapabilitySubjectGroup, envGroup, u) {
			t.Errorf("the approved group is not a member of %s", u.Name)
		}
	}
	if e.pendingCount() != 0 {
		t.Errorf("%d changes pending, want none", e.pendingCount())
	}
	rows := e.audits("governance.membership.write")
	if len(rows) == 0 {
		t.Fatal("no governance.membership.write row")
	}
	var d map[string]any
	if json.Unmarshal(rows[len(rows)-1].Data, &d) != nil || d["envelope"] != e.a1.ID.String() || d["unit_scope"] != e.a1.ID.String() {
		t.Errorf("audit data = %s, want envelope and unit_scope naming ua-a1", rows[len(rows)-1].Data)
	}
}

// TestEnvelopeApprovedGroupMoveWithinApplies: moving an approved group's membership to a wider unit
// below the root applies directly; with no envelope the same move is held.
func TestEnvelopeApprovedGroupMoveWithinApplies(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	narrow := e.envBelow(e.a1, uaDomains("pypi.org"))
	m := treeMember(t, e.govEnv, narrow, types.CapabilitySubjectGroup, envGroup, 1, false)
	e.reject(e.pending(e.call(e.adminA1, http.MethodPut, envMove(m), envMoveBody(e.a1.ID))))

	e.seedEnvelope(e.a1, envGroup)
	if w := e.call(e.adminA1, http.MethodPut, envMove(m), envMoveBody(e.a1.ID)); w.Code != http.StatusOK {
		t.Fatalf("a widening move of the approved group = %d %s, want 200", w.Code, w.Body)
	}
	if !e.membershipAt(types.CapabilitySubjectGroup, envGroup, e.a1) {
		t.Error("the move did not land in ua-a1")
	}
}

// TestEnvelopeUnapprovedBelongingGroupHeld: a group that belongs below the root but is not on the list
// is held, as today.
func TestEnvelopeUnapprovedBelongingGroupHeld(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	treeMember(t, e.govEnv, e.envBelow(e.a1, map[string]any{}), types.CapabilitySubjectGroup, "ua-eng", 1, false)
	e.seedEnvelope(e.a1, envGroup)
	e.pending(e.call(e.adminA1, http.MethodPost, "/api/v1/governance/memberships", envGroupBody("ua-eng", e.a1.ID)))
	if e.membershipAt(types.CapabilitySubjectGroup, "ua-eng", e.a1) {
		t.Error("a held add wrote the membership")
	}
}

// TestEnvelopeNewGroupRefused: a group nobody placed and not on the list is refused, as today.
func TestEnvelopeNewGroupRefused(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	e.seedEnvelope(e.a1, envGroup)
	if w := e.call(e.adminA1, http.MethodPost, "/api/v1/governance/memberships", envGroupBody("ua-ops", e.a1.ID)); !scopeRefused(t, w) {
		t.Errorf("adding a new group = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
	}
}

// TestEnvelopeApprovedGroupOutsideEnvelopeUnitNotDirect: the approved group's add to a unit outside the
// envelope, and its move out to a wider unit, keep today's rules: refused and held.
func TestEnvelopeApprovedGroupOutsideEnvelopeUnitNotDirect(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	e.seedEnvelope(e.a1, envGroup)
	if w := e.call(e.adminA, http.MethodPost, "/api/v1/governance/memberships", envGroupBody(envGroup, e.a2.ID)); !scopeRefused(t, w) {
		t.Errorf("adding the approved group outside the envelope = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
	}
	m := treeMember(t, e.govEnv, e.a1, types.CapabilitySubjectGroup, envGroup, 1, false)
	e.pending(e.call(e.adminA, http.MethodPut, envMove(m), envMoveBody(e.a.ID)))
	if !e.membershipAt(types.CapabilitySubjectGroup, envGroup, e.a1) {
		t.Error("a held move moved the membership")
	}
}

// TestEnvelopeApprovedGroupLegacyShiftHeld: an approved group's widening move inside the envelope is
// still held when it changes which legacy row a person is selected by, and applies directly when it
// changes none. ua-a is the root here; the other legacy row is in ua-b, and the destination's name
// breaks the equal-priority tie.
func TestEnvelopeApprovedGroupLegacyShiftHeld(t *testing.T) {
	for _, tc := range []struct {
		to   string
		held bool
	}{{to: "ua-zeta", held: true}, {to: "ua-a1-leaf", held: false}} {
		t.Run(tc.to, func(t *testing.T) {
			e := newUnitAdminEnv(t, false)
			e.seedEnvelope(e.a, "ua-eng")
			moved := treeMember(t, e.govEnv, e.a1, types.CapabilitySubjectGroup, "ua-eng", 5, false)
			other := treeMember(t, e.govEnv, e.b, types.CapabilitySubjectGroup, "ua-research", 5, false)
			setLegacy(t, e.govEnv, moved.ID, other.ID)
			to := treeChild(t, e.govEnv, uaUnitBody(tc.to, e.a.ID, map[string]any{}, nil)) // ua-a's three domains: wider than ua-a1
			w := e.call(e.adminA, http.MethodPut, envMove(moved), envMoveBody(to.ID))
			if tc.held {
				e.pending(w)
				return
			}
			if w.Code != http.StatusOK {
				t.Fatalf("a move that shifts no legacy row = %d %s, want 200", w.Code, w.Body)
			}
		})
	}
}

// TestEnvelopeUserSubjectStillHeld: a person's add and widening move inside the envelope are held, as
// today: the list approves groups only.
func TestEnvelopeUserSubjectStillHeld(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	narrow := e.envBelow(e.a1, uaDomains("pypi.org"))
	m := treeMember(t, e.govEnv, narrow, types.CapabilitySubjectUser, "sub-env-person", 1, false)
	e.seedEnvelope(e.a1, envGroup, "sub-env-person")
	e.pending(e.call(e.adminA1, http.MethodPost, "/api/v1/governance/memberships", membershipBody("sub-env-person", e.a1.ID)))
	e.pending(e.call(e.adminA1, http.MethodPut, envMove(m), envMoveBody(e.a1.ID)))
}

// beforeWriteGovernanceStore runs *hook once, before the next governance write transaction begins.
type beforeWriteGovernanceStore struct {
	store.PG
	hook *func()
}

func (s beforeWriteGovernanceStore) WriteGovernance(ctx context.Context, fn func(store.Querier) error) error {
	if f := *s.hook; f != nil {
		*s.hook = nil
		f()
	}
	return s.PG.WriteGovernance(ctx, fn)
}

// TestEnvelopeGroupRemovedFromListRaceRefused: the list is read again on the write's transaction, so a
// group taken off it after the first read is not added directly: the add takes today's path, which
// refuses a group nobody placed.
func TestEnvelopeGroupRemovedFromListRaceRefused(t *testing.T) {
	var hook func()
	e := newUnitAdminEnv(t, false, func(c *Config) {
		c.Store = beforeWriteGovernanceStore{PG: store.NewPG(c.Store.(store.PG).Pool), hook: &hook}
	})
	e.seedEnvelope(e.a1, envGroup)
	hook = func() { e.seedEnvelope(e.a1) }
	if w := e.call(e.adminA1, http.MethodPost, "/api/v1/governance/memberships", envGroupBody(envGroup, e.a1.ID)); !scopeRefused(t, w) {
		t.Errorf("an add whose group left the list = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
	}
	if hook != nil {
		t.Fatal("the add never opened a governance write transaction")
	}
	if e.membershipAt(types.CapabilitySubjectGroup, envGroup, e.a1) {
		t.Error("the group was added after it left the list")
	}
}

// TestEnvelopeApprovedGroupDeleteUnchanged: deleting an approved group's membership keeps today's rule:
// refused when it is the group's last one in the caller's units, direct when another stays.
func TestEnvelopeApprovedGroupDeleteUnchanged(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	m := treeMember(t, e.govEnv, e.a1, types.CapabilitySubjectGroup, envGroup, 1, false)
	e.seedEnvelope(e.a1, envGroup)
	if w := e.call(e.adminA1, http.MethodDelete, envMove(m), ""); !scopeRefused(t, w) {
		t.Errorf("deleting the group's last membership = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
	}
	treeMember(t, e.govEnv, e.envBelow(e.a1, map[string]any{}), types.CapabilitySubjectGroup, envGroup, 2, false)
	if w := e.call(e.adminA1, http.MethodDelete, envMove(m), ""); w.Code != http.StatusNoContent {
		t.Errorf("deleting with another membership kept = %d %s, want 204", w.Code, w.Body)
	}
}

// ── fail closed and back-compat ──────────────────────────────────────────────

// TestEnvelopeUnreadableFailsClosed: with the envelope table unreadable, the widening below the root is
// held, its change lists and accepts the security admins alone, and the approved group's add takes
// today's path (refused, as nobody placed it). Readable again, the same add applies.
func TestEnvelopeUnreadableFailsClosed(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	c, g := e.envTree()
	e.seedEnvelope(e.a1, envGroup)
	e.envelopesUnreadable(true)

	ch := e.pending(e.call(e.adminA1, http.MethodPut, envUnitPath(g), envUnitBody(g, c.ID, uaDomains("pypi.org", "github.com"))))
	if a := e.readApprovers(ch.ID); !securityOnlyList(a) {
		t.Errorf("approvers %+v, want the security admins alone", a)
	}
	if w := e.call(e.peerA1, http.MethodPost, approvePath(ch.ID), ""); !securitySurfaceRefused(t, w) {
		t.Errorf("a peer approving = %d %s, want 403 security_admin_surface", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Errorf("a security admin approving = %d %s, want 200", w.Code, w.Body)
	}
	if w := e.call(e.adminA1, http.MethodPost, "/api/v1/governance/memberships", envGroupBody(envGroup, e.a1.ID)); !scopeRefused(t, w) {
		t.Errorf("the approved group's add, unreadable = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
	}

	e.envelopesUnreadable(false)
	if w := e.call(e.adminA1, http.MethodPost, "/api/v1/governance/memberships", envGroupBody(envGroup, e.a1.ID)); w.Code != http.StatusCreated {
		t.Errorf("the approved group's add, readable = %d %s, want 201", w.Code, w.Body)
	}
}

// TestNoEnvelopeVerdictsGolden: with no envelope row, every unit-admin verdict these tests change is the
// one before envelopes: a widening below and of ua-a1 is held and lists the peers, an unplaced group's
// add is refused, a group's widening move is held, and a unit admin grant on ua-a1 lists the admins above.
func TestNoEnvelopeVerdictsGolden(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	c, g := e.envTree()
	ch := e.pending(e.call(e.adminA1, http.MethodPut, envUnitPath(g), envUnitBody(g, c.ID, uaDomains("pypi.org", "github.com"))))
	if a := e.readApprovers(ch.ID); !listsUnitAdmin(a, uaSubPeerA1) || !listsUnitAdmin(a, uaSubAdminA) {
		t.Errorf("a widening below ua-a1 lists %+v, want its peer and the admin above", a)
	}
	ch = e.pending(e.call(e.adminA1, http.MethodPut, envUnitPath(e.a1), e.restate(e.a1, map[string]any{"contact": map[string]any{"owner": "x"}})))
	if a := e.readApprovers(ch.ID); !listsUnitAdmin(a, uaSubPeerA1) {
		t.Errorf("a change of ua-a1 lists %+v, want its peer", a)
	}
	if w := e.call(e.adminA1, http.MethodPost, "/api/v1/governance/memberships", envGroupBody(envGroup, e.a1.ID)); !scopeRefused(t, w) {
		t.Errorf("an unplaced group's add = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
	}
	m := treeMember(t, e.govEnv, g, types.CapabilitySubjectGroup, envGroup, 1, false)
	e.pending(e.call(e.adminA1, http.MethodPut, envMove(m), envMoveBody(e.a1.ID)))
	ch = e.pending(e.call(e.adminA, http.MethodPost, envUnitPath(e.a1)+"/admins", `{"subject_type":"user","subject":"sub-env-golden"}`))
	if a := e.readApprovers(ch.ID); !listsUnitAdmin(a, uaSubPeerA) {
		t.Errorf("a grant on ua-a1 lists %+v, want the peer admin above", a)
	}
}
