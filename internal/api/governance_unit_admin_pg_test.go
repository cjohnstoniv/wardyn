// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The unit-admin pins: a grant on a unit governs that unit and every unit below it, each route checks
// its target against that, a unit admin's widening is held for a second human, and locks bind every
// writer.

const (
	uaSubAdminA  = "sub-ua-admin-a"
	uaSubPeerA   = "sub-ua-peer-a"
	uaSubAdminA1 = "sub-ua-admin-a1"
	uaSubPeerA1  = "sub-ua-peer-a1"
	uaSubAdminB  = "sub-ua-admin-b"
	uaSubGroupA  = "sub-ua-group-a"
	uaGroupA     = "ua-admins-a"
)

// unitAdminEnv is a tree under the Default unit, ua-a with ua-a1 and ua-a2 below it and ua-b beside
// it, and the people the pins are about: adminA and peerA (user grants on ua-a), groupA (a member of
// the group granted on ua-a), adminA1 and peerA1 (grants on ua-a1), adminB (the one grant on ua-b),
// and member (no grant). alice and bob, the security admins, come from govEnv.
type unitAdminEnv struct {
	*govEnv
	a, a1, a2, b                                           types.GovernanceUnit
	adminA, peerA, groupA, adminA1, peerA1, adminB, member *http.Cookie
	n                                                      int
}

// uaSession is a member's session with a group snapshot; nil groups is a snapshot never taken.
func uaSession(t *testing.T, sub string, groups []string, truncated bool) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(oidc.Session{
		V: oidc.SessionCodecVersion, Sub: sub, Email: sub + "@corp.example", Role: oidc.RoleUser, UserType: types.UserTypeStandard,
		Expiry: time.Now().UTC().Add(time.Hour), Groups: groups, GroupsTruncated: truncated,
	})
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	return signedSessionCookie(payload)
}

// uaUnitBody is a composed unit's write body: its name, parent and overlay, plus any extra fields.
func uaUnitBody(name string, parent uuid.UUID, overlay map[string]any, extra map[string]any) string {
	m := map[string]any{"name": name, "parent_id": parent, "overlay": overlay}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func uaDomains(ds ...string) map[string]any { return map[string]any{"allowed_domains": ds} }

func newUnitAdminEnv(t *testing.T, secondHuman bool) *unitAdminEnv {
	t.Helper()
	t.Setenv(envGovernanceSecondHuman, strconv.FormatBool(secondHuman))
	e := &unitAdminEnv{govEnv: newGovEnvOn(t, throwawayPGPool(t))}
	e.a = treeChild(t, e.govEnv, uaUnitBody("ua-a", store.DefaultUnitID, uaDomains("pypi.org", "github.com", "npmjs.org"), nil))
	e.a1 = treeChild(t, e.govEnv, uaUnitBody("ua-a1", e.a.ID, uaDomains("pypi.org", "github.com"), nil))
	e.a2 = treeChild(t, e.govEnv, uaUnitBody("ua-a2", e.a.ID, uaDomains("pypi.org"), nil))
	e.b = treeChild(t, e.govEnv, uaUnitBody("ua-b", store.DefaultUnitID, uaDomains("pypi.org"), nil))
	e.grant(e.a, types.CapabilitySubjectUser, uaSubAdminA)
	e.grant(e.a, types.CapabilitySubjectUser, uaSubPeerA)
	e.grant(e.a, types.CapabilitySubjectGroup, uaGroupA)
	e.grant(e.a1, types.CapabilitySubjectUser, uaSubAdminA1)
	e.grant(e.a1, types.CapabilitySubjectUser, uaSubPeerA1)
	e.grant(e.b, types.CapabilitySubjectUser, uaSubAdminB)
	e.adminA = uaSession(t, uaSubAdminA, []string{}, false)
	e.peerA = uaSession(t, uaSubPeerA, []string{}, false)
	e.groupA = uaSession(t, uaSubGroupA, []string{uaGroupA}, false)
	e.adminA1 = uaSession(t, uaSubAdminA1, []string{}, false)
	e.peerA1 = uaSession(t, uaSubPeerA1, []string{}, false)
	e.adminB = uaSession(t, uaSubAdminB, []string{}, false)
	e.member = uaSession(t, "sub-ua-member", []string{}, false)
	return e
}

// grant seeds a unit-admin grant as the store writes it.
func (e *unitAdminEnv) grant(u types.GovernanceUnit, st types.CapabilitySubjectType, subject string) types.GovernanceUnitAdmin {
	e.t.Helper()
	g, err := store.GrantUnitAdminQ(context.Background(), e.pool, types.GovernanceUnitAdmin{UnitID: u.ID, SubjectType: st, Subject: subject, GrantedBy: "seed"})
	if err != nil {
		e.t.Fatalf("seed grant %s %q on %s: %v", st, subject, u.Name, err)
	}
	return g
}

// fresh is a name no other call of the test has used.
func (e *unitAdminEnv) fresh(prefix string) string {
	e.n++
	return fmt.Sprintf("%s-%d", prefix, e.n)
}

// child is a new empty unit below u, written by the admin token.
func (e *unitAdminEnv) child(u types.GovernanceUnit) types.GovernanceUnit {
	e.t.Helper()
	return treeChild(e.t, e.govEnv, uaUnitBody(e.fresh("ua-child"), u.ID, map[string]any{}, nil))
}

// restate is u's stored row as a PUT body that changes nothing, plus extra fields.
func (e *unitAdminEnv) restate(u types.GovernanceUnit, extra map[string]any) string {
	e.t.Helper()
	row := e.profile(u.ID)
	m := map[string]any{"name": row.Name, "parent_id": row.ParentID, "overlay": row.Overlay}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func membershipBody(subject string, unit uuid.UUID) string {
	return `{"subject_type":"user","subject":"` + subject + `","unit_id":"` + unit.String() + `","priority":1}`
}

// heldOn is a change, pending, that touches unit u alone: a membership add proposed by a fresh admin
// of u, for a person who already belongs below it.
func (e *unitAdminEnv) heldOn(u types.GovernanceUnit) types.GovernanceChange {
	e.t.Helper()
	proposer := e.fresh("sub-ua-proposer")
	e.grant(u, types.CapabilitySubjectUser, proposer)
	person := e.fresh("sub-ua-person")
	treeMember(e.t, e.govEnv, e.child(u), types.CapabilitySubjectUser, person, 1, false)
	return e.pending(e.call(uaSession(e.t, proposer, []string{}, false), http.MethodPost, "/api/v1/governance/memberships", membershipBody(person, u.ID)))
}

// scopeRefused reports a 403 unit_scope_denied.
func scopeRefused(t *testing.T, w *httptest.ResponseRecorder) bool {
	t.Helper()
	return w.Code == http.StatusForbidden && wireReason(t, w) == string(authz.ReasonUnitScopeDenied)
}

// uaProbe is one unit-admin route asked about target unit u. req seeds what the request needs (as the
// admin token) and returns it. strict: the route needs a grant strictly above u. hidden: a target
// outside the caller's units answers the hidden 404 instead of 403.
type uaProbe struct {
	route          string
	strict, hidden bool
	req            func(e *unitAdminEnv, u types.GovernanceUnit) (method, path, body string)
}

var uaProbes = []uaProbe{
	{route: "POST /governance/units", req: func(e *unitAdminEnv, u types.GovernanceUnit) (string, string, string) {
		return http.MethodPost, "/api/v1/governance/units", uaUnitBody(e.fresh("ua-new"), u.ID, map[string]any{}, nil)
	}},
	{route: "PUT /governance/units/{id}", req: func(e *unitAdminEnv, u types.GovernanceUnit) (string, string, string) {
		return http.MethodPut, "/api/v1/governance/units/" + u.ID.String(), e.restate(u, nil)
	}},
	{route: "DELETE /governance/units/{id} (a unit below)", req: func(e *unitAdminEnv, u types.GovernanceUnit) (string, string, string) {
		return http.MethodDelete, "/api/v1/governance/units/" + e.child(u).ID.String(), ""
	}},
	{route: "POST /governance/units/{id}/admins", strict: true, req: func(e *unitAdminEnv, u types.GovernanceUnit) (string, string, string) {
		return http.MethodPost, "/api/v1/governance/units/" + u.ID.String() + "/admins", `{"subject_type":"user","subject":"` + e.fresh("sub-ua-grantee") + `"}`
	}},
	{route: "DELETE /governance/units/{id}/admins/{admin}", strict: true, req: func(e *unitAdminEnv, u types.GovernanceUnit) (string, string, string) {
		g := e.grant(u, types.CapabilitySubjectUser, e.fresh("sub-ua-revoked"))
		return http.MethodDelete, "/api/v1/governance/units/" + u.ID.String() + "/admins/" + g.ID.String(), ""
	}},
	{route: "POST /governance/memberships", req: func(e *unitAdminEnv, u types.GovernanceUnit) (string, string, string) {
		person := e.fresh("sub-ua-person")
		treeMember(e.t, e.govEnv, e.child(u), types.CapabilitySubjectUser, person, 1, false)
		return http.MethodPost, "/api/v1/governance/memberships", membershipBody(person, u.ID)
	}},
	{route: "PUT /governance/memberships/{id}", req: func(e *unitAdminEnv, u types.GovernanceUnit) (string, string, string) {
		m := treeMember(e.t, e.govEnv, u, types.CapabilitySubjectUser, e.fresh("sub-ua-person"), 1, false)
		return http.MethodPut, "/api/v1/governance/memberships/" + m.ID.String(), `{"unit_id":"` + e.child(u).ID.String() + `"}`
	}},
	{route: "DELETE /governance/memberships/{id}", req: func(e *unitAdminEnv, u types.GovernanceUnit) (string, string, string) {
		person := e.fresh("sub-ua-person")
		m := treeMember(e.t, e.govEnv, u, types.CapabilitySubjectUser, person, 1, false)
		treeMember(e.t, e.govEnv, e.child(u), types.CapabilitySubjectUser, person, 2, false)
		return http.MethodDelete, "/api/v1/governance/memberships/" + m.ID.String(), ""
	}},
	{route: "POST /governance/check", req: func(e *unitAdminEnv, u types.GovernanceUnit) (string, string, string) {
		person := e.fresh("sub-ua-person")
		treeMember(e.t, e.govEnv, u, types.CapabilitySubjectUser, person, 1, false)
		return http.MethodPost, "/api/v1/governance/check", `{"user_subjects":["` + person + `"]}`
	}},
	{route: "POST /governance/dry-run (unit)", req: func(e *unitAdminEnv, u types.GovernanceUnit) (string, string, string) {
		return http.MethodPost, "/api/v1/governance/dry-run", `{"target":"unit","id":"` + u.ID.String() + `",` + strings.TrimPrefix(e.restate(u, nil), "{")
	}},
	{route: "GET /governance/changes/{id}", hidden: true, req: func(e *unitAdminEnv, u types.GovernanceUnit) (string, string, string) {
		return http.MethodGet, "/api/v1/governance/changes/" + e.heldOn(u).ID.String(), ""
	}},
	{route: "POST /governance/changes/{id}/approve", req: func(e *unitAdminEnv, u types.GovernanceUnit) (string, string, string) {
		return http.MethodPost, approvePath(e.heldOn(u).ID), ""
	}},
	{route: "POST /governance/changes/{id}/reject", req: func(e *unitAdminEnv, u types.GovernanceUnit) (string, string, string) {
		return http.MethodPost, rejectPath(e.heldOn(u).ID), ""
	}},
}

// TestUnitScopeMatrix: on every unit-admin route, a plain member is refused at the door, an admin of
// ua-a is refused on its sibling ua-b and admitted on ua-a1 below it, an admin of ua-a1 is admitted on
// ua-a1 itself unless the route needs a grant strictly above, and a security admin is admitted on any
// unit. GET /governance is TestUnitAdminGovernanceRead's.
func TestUnitScopeMatrix(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	admitted := func(t *testing.T, who string, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code >= 500 || w.Code == http.StatusUnauthorized || scopeRefused(t, w) || w.Code == http.StatusNotFound {
			t.Errorf("%s: status = %d %s, want admitted", who, w.Code, w.Body)
		}
	}
	for _, p := range uaProbes {
		t.Run(p.route, func(t *testing.T) {
			e.t = t
			m, path, body := p.req(e, e.a1)
			if w := e.call(e.member, m, path, body); !scopeRefused(t, w) || !strings.Contains(w.Body.String(), unitScopeDoorSentence) {
				t.Errorf("a plain member: status = %d %s, want 403 unit_scope_denied at the door", w.Code, w.Body)
			}
			m, path, body = p.req(e, e.b)
			w := e.call(e.adminA, m, path, body)
			switch {
			case p.hidden && (w.Code != http.StatusNotFound || wireReason(t, w) != reasonGovernanceChangeNotFound):
				t.Errorf("ua-a's admin on ua-b: status = %d %s, want the hidden 404", w.Code, w.Body)
			case !p.hidden && (!scopeRefused(t, w) || strings.Contains(w.Body.String(), unitScopeDoorSentence)):
				t.Errorf("ua-a's admin on ua-b: status = %d %s, want 403 unit_scope_denied past the door", w.Code, w.Body)
			}
			m, path, body = p.req(e, e.a1)
			admitted(t, "ua-a's admin on ua-a1", e.call(e.adminA, m, path, body))
			m, path, body = p.req(e, e.a1)
			if w := e.call(e.adminA1, m, path, body); p.strict {
				if !scopeRefused(t, w) {
					t.Errorf("ua-a1's admin on ua-a1: status = %d %s, want 403 unit_scope_denied (strictly above only)", w.Code, w.Body)
				}
			} else {
				admitted(t, "ua-a1's admin on ua-a1", w)
			}
			m, path, body = p.req(e, e.b)
			admitted(t, "a security admin on ua-b", e.call(e.alice, m, path, body))
		})
	}
	e.t = t
	// A unit admin deletes only below their own unit, never the unit itself.
	if w := e.call(e.adminA1, http.MethodDelete, "/api/v1/governance/units/"+e.child(e.a2).ID.String(), ""); !scopeRefused(t, w) {
		t.Errorf("ua-a1's admin deleting below ua-a2 = %d %s, want 403", w.Code, w.Body)
	}
	empty := treeChild(t, e.govEnv, uaUnitBody("ua-empty", store.DefaultUnitID, map[string]any{}, nil))
	e.grant(empty, types.CapabilitySubjectUser, "sub-ua-empty")
	if w := e.call(uaSession(t, "sub-ua-empty", []string{}, false), http.MethodDelete, "/api/v1/governance/units/"+empty.ID.String(), ""); !scopeRefused(t, w) {
		t.Errorf("an admin deleting their own unit = %d %s, want 403", w.Code, w.Body)
	}
	// A new unit with no parent below the caller's units, and the boundary dry run, stay the security tier's.
	if w := e.call(e.adminA, http.MethodPost, "/api/v1/governance/units", uaUnitBody("ua-top", store.DefaultUnitID, map[string]any{}, nil)); !scopeRefused(t, w) {
		t.Errorf("ua-a's admin creating a top-level unit = %d %s, want 403", w.Code, w.Body)
	}
	if w := e.call(e.adminA, http.MethodPost, "/api/v1/governance/dry-run", `{"target":"boundary","mode":"warn"}`); w.Code != http.StatusForbidden {
		t.Errorf("ua-a's admin on the boundary dry run = %d %s, want 403", w.Code, w.Body)
	}
}

// TestUnitAdminGovernanceRead: GET /governance gives a unit admin the units of their subtree, the
// memberships and grants on them and the walls their subtree hangs from, never the boundary or a unit
// beside it; the change queue lists only changes inside their units.
func TestUnitAdminGovernanceRead(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	treeMember(t, e.govEnv, e.a1, types.CapabilitySubjectUser, "sub-ua-in", 1, false)
	treeMember(t, e.govEnv, e.b, types.CapabilitySubjectUser, "sub-ua-out", 1, false)
	inA, inB := e.heldOn(e.a2), e.heldOn(e.b)
	var got struct {
		Units []struct {
			ID uuid.UUID `json:"id"`
		} `json:"units"`
		Memberships []types.GovernanceMembership `json:"memberships"`
		UnitAdmins  []types.GovernanceUnitAdmin  `json:"unit_admins"`
		Boundary    json.RawMessage              `json:"boundary"`
		ParentWalls []unitParentWalls            `json:"parent_walls"`
	}
	w := e.call(e.adminA, http.MethodGet, "/api/v1/governance", "")
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("GET /governance as ua-a's admin = %d %s", w.Code, w.Body)
	}
	inSubtree := func(id uuid.UUID) bool {
		for cur := &id; cur != nil; cur = e.profile(*cur).ParentID {
			if *cur == e.a.ID {
				return true
			}
		}
		return false
	}
	var ids []uuid.UUID
	for _, u := range got.Units {
		ids = append(ids, u.ID)
		if !inSubtree(u.ID) {
			t.Errorf("unit %s outside ua-a's subtree is listed", u.ID)
		}
	}
	for _, want := range []uuid.UUID{e.a.ID, e.a1.ID, e.a2.ID} {
		if !slices.Contains(ids, want) {
			t.Errorf("unit %s of ua-a's subtree is missing from %v", want, ids)
		}
	}
	for _, m := range got.Memberships {
		if !inSubtree(m.UnitID) {
			t.Errorf("membership %s %q outside the subtree is listed", m.UnitID, m.Subject)
		}
	}
	for _, g := range got.UnitAdmins {
		if !inSubtree(g.UnitID) {
			t.Errorf("grant %q on %s outside the subtree is listed", g.Subject, g.UnitID)
		}
	}
	if len(got.Boundary) != 0 {
		t.Errorf("a unit admin reads the boundary: %s", got.Boundary)
	}
	if len(got.ParentWalls) != 1 || got.ParentWalls[0].UnitID != e.a.ID || got.ParentWalls[0].ParentID != store.DefaultUnitID ||
		got.ParentWalls[0].Effective.Error != "" || len(got.ParentWalls[0].Provenance) == 0 {
		t.Errorf("parent_walls = %+v, want ua-a's, hanging from the Default unit, with provenance", got.ParentWalls)
	}
	var queue []types.GovernanceChange
	if w := e.call(e.adminA, http.MethodGet, "/api/v1/governance/changes", ""); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &queue) != nil {
		t.Fatalf("GET /governance/changes = %d %s", w.Code, w.Body)
	}
	var listed []uuid.UUID
	for _, ch := range queue {
		listed = append(listed, ch.ID)
	}
	if !slices.Contains(listed, inA.ID) || slices.Contains(listed, inB.ID) {
		t.Errorf("queue %v, want ua-a2's change %s and not ua-b's %s", listed, inA.ID, inB.ID)
	}
	// The security admin's read is whole.
	w = e.call(e.alice, http.MethodGet, "/api/v1/governance", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"boundary"`) || !strings.Contains(w.Body.String(), e.b.ID.String()) {
		t.Errorf("GET /governance as a security admin = %d, want the boundary and ua-b listed", w.Code)
	}
}

// TestUnitAdminStaleGroups: a group-granted admin whose snapshot is stale or truncated holds no group
// grant: refused at the door (403, not a 5xx), with their member reach intact; a fresh snapshot passes.
func TestUnitAdminStaleGroups(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	for _, tc := range []struct {
		what      string
		groups    []string
		truncated bool
		want      int
	}{
		{"a snapshot never taken", nil, false, http.StatusForbidden},
		{"a truncated snapshot", []string{uaGroupA}, true, http.StatusForbidden},
		{"a fresh snapshot", []string{uaGroupA}, false, http.StatusOK},
	} {
		w := e.call(uaSession(t, uaSubGroupA, tc.groups, tc.truncated), http.MethodGet, "/api/v1/governance", "")
		if w.Code != tc.want || (tc.want == http.StatusForbidden && !scopeRefused(t, w)) {
			t.Errorf("%s: GET /governance = %d %s, want %d", tc.what, w.Code, w.Body, tc.want)
		}
		if w := e.call(uaSession(t, uaSubGroupA, tc.groups, tc.truncated), http.MethodGet, "/api/v1/me", ""); w.Code != http.StatusOK {
			t.Errorf("%s: GET /me = %d %s, want the member's reach intact", tc.what, w.Code, w.Body)
		}
	}
}

// TestMeUnitAdminOf: GET /me lists the units the caller administers by a grant of theirs or a group's,
// and none for a plain member.
func TestMeUnitAdminOf(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	for _, tc := range []struct {
		what string
		c    *http.Cookie
		want []uuid.UUID
	}{
		{"a user grant", e.adminA, []uuid.UUID{e.a.ID}},
		{"a group grant", e.groupA, []uuid.UUID{e.a.ID}},
		{"a plain member", e.member, []uuid.UUID{}},
	} {
		var body struct {
			UnitAdminOf []meUnitAdmin `json:"unit_admin_of"`
		}
		w := e.call(tc.c, http.MethodGet, "/api/v1/me", "")
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.UnitAdminOf == nil {
			t.Fatalf("%s: GET /me = %d %s, want unit_admin_of", tc.what, w.Code, w.Body)
		}
		got := []uuid.UUID{}
		for _, u := range body.UnitAdminOf {
			got = append(got, u.UnitID)
			if u.UnitID == e.a.ID && u.UnitName != e.a.Name {
				t.Errorf("%s: unit_name %q, want %q", tc.what, u.UnitName, e.a.Name)
			}
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: unit_admin_of %v, want %v", tc.what, got, tc.want)
		}
	}
}

// TestUnitAdminGrantOnlyByStrictAncestor: an admin of ua-a grants on ua-a1 below it, held until a
// security admin approves, and the grantee then governs ua-a1; an admin of ua-a1 cannot grant on it.
func TestUnitAdminGrantOnlyByStrictAncestor(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	path := "/api/v1/governance/units/" + e.a1.ID.String() + "/admins"
	if w := e.call(e.adminA1, http.MethodPost, path, `{"subject_type":"user","subject":"sub-ua-new"}`); !scopeRefused(t, w) {
		t.Errorf("ua-a1's admin granting on ua-a1 = %d %s, want 403 unit_scope_denied", w.Code, w.Body)
	}
	ch := e.pending(e.call(e.adminA, http.MethodPost, path, `{"subject_type":"user","subject":"sub-ua-new"}`))
	grantee := uaSession(t, "sub-ua-new", []string{}, false)
	if w := e.call(grantee, http.MethodGet, "/api/v1/governance", ""); !scopeRefused(t, w) {
		t.Errorf("the grantee before approval = %d %s, want 403", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("security approval = %d %s", w.Code, w.Body)
	}
	if w := e.call(grantee, http.MethodPut, "/api/v1/governance/units/"+e.a1.ID.String(), e.restate(e.a1, nil)); w.Code != http.StatusOK {
		t.Errorf("the grantee writing ua-a1 = %d %s, want 200", w.Code, w.Body)
	}
	if w := e.call(grantee, http.MethodPut, "/api/v1/governance/units/"+e.a2.ID.String(), e.restate(e.a2, nil)); !scopeRefused(t, w) {
		t.Errorf("the grantee writing ua-a2 = %d %s, want 403", w.Code, w.Body)
	}
}

// TestUnitAdminAuditFields: a unit admin's direct write records the actor, the unit their grant is on
// as unit_scope and no approver; an approved grant records its proposer, the approver and "security"
// for a security admin's approval.
func TestUnitAdminAuditFields(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	if w := e.call(e.adminA, http.MethodPut, "/api/v1/governance/units/"+e.a1.ID.String(),
		uaUnitBody("ua-a1", e.a.ID, uaDomains("pypi.org"), nil)); w.Code != http.StatusOK {
		t.Fatalf("a narrowing by ua-a's admin = %d %s", w.Code, w.Body)
	}
	ch := e.pending(e.call(e.adminA, http.MethodPost, "/api/v1/governance/units/"+e.a1.ID.String()+"/admins", `{"subject_type":"group","subject":"ua-new-group"}`))
	if w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("approval = %d %s", w.Code, w.Body)
	}
	for _, tc := range []struct {
		action                 string
		actor, scope, approver string
	}{
		{"governance.unit.write", uaSubAdminA, e.a.ID.String(), ""},
		{"governance.unit_admin.grant", uaSubAdminA, "security", govSubAlice},
	} {
		rows := e.audits(tc.action)
		if len(rows) == 0 {
			t.Errorf("%s: no audit row", tc.action)
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(rows[len(rows)-1].Data, &data); err != nil {
			t.Fatal(err)
		}
		if data["actor"] != tc.actor || data["unit_scope"] != tc.scope || data["approver"] != tc.approver {
			t.Errorf("%s: actor %v unit_scope %v approver %v, want %q %q %q", tc.action, data["actor"], data["unit_scope"], data["approver"],
				tc.actor, tc.scope, tc.approver)
		}
	}
}
