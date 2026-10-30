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
	"github.com/cjohnstoniv/wardyn/internal/policyref"
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

func newUnitAdminEnv(t *testing.T, secondHuman bool, mutate ...func(*Config)) *unitAdminEnv {
	t.Helper()
	t.Setenv(envGovernanceSecondHuman, strconv.FormatBool(secondHuman))
	e := &unitAdminEnv{govEnv: newGovEnvOn(t, throwawayPGPool(t), mutate...)}
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

// TestUnitAdminReadsNoPathProvenance: path_provenance names the units above a person's leaf, a
// security-tier read: a unit admin's GET /governance and POST /governance/check carry none, a
// security admin's carry it.
func TestUnitAdminReadsNoPathProvenance(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	person := e.fresh("sub-ua-person")
	treeMember(t, e.govEnv, e.a1, types.CapabilitySubjectUser, person, 1, false)
	check := `{"user_subjects":["` + person + `"]}`
	for _, tc := range []struct {
		who  string
		c    *http.Cookie
		want bool
	}{
		{"ua-a's admin", e.adminA, false},
		{"a security admin", e.alice, true},
	} {
		for _, req := range [][3]string{{http.MethodGet, "/api/v1/governance", ""}, {http.MethodPost, "/api/v1/governance/check", check}} {
			w := e.call(tc.c, req[0], req[1], req[2])
			if w.Code != http.StatusOK {
				t.Fatalf("%s %s as %s = %d %s", req[0], req[1], tc.who, w.Code, w.Body)
			}
			if got := strings.Contains(w.Body.String(), `"path_provenance"`); got != tc.want {
				t.Errorf("%s %s as %s carries path_provenance = %v, want %v", req[0], req[1], tc.who, got, tc.want)
			}
		}
	}
}

// TestUnitAdminCheckWithinScope: a unit admin's POST /governance/check carries no restrictions (no spec,
// limits or contact) and of the person's workspaces only the group pinned to a unit they govern, so
// no unit outside their units is named; a security admin's check carries all of it. The effective
// walls' provenance is no richer than GET /governance's: the knob the restriction set says only that
// a restriction did, where a security admin's names it. A context's path is GET's shape too: an admin
// of a unit two below ua-a reads the path up to ua-a1, the parent of their unit, and no unit above.
func TestUnitAdminCheckWithinScope(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	person := e.fresh("sub-ua-person")
	treeMember(t, e.govEnv, e.a1, types.CapabilitySubjectUser, person, 1, false)
	r := types.RestrictionWrite{Name: "ua-check-r", Spec: types.RestrictionSpec{DeniedDomains: strs("denied.ua.example")},
		Contact: &policyref.Contact{Email: "restrictions@ua.example"}, Members: []types.RestrictionMember{userMember(person)}}
	if w := e.call(e.alice, http.MethodPost, "/api/v1/governance/restrictions", restrictionJSON(t, r)); w.Code != http.StatusCreated {
		t.Fatalf("create the restriction = %d %s", w.Code, w.Body)
	}
	for name, unit := range map[string]uuid.UUID{"ua-ws-in": e.a1.ID, "ua-ws-out": e.b.ID} {
		ws, err := e.pg.CreateWorkspace(context.Background(), types.Workspace{
			ID: uuid.New(), Name: name, OwnedBy: person, Status: types.WorkspaceScanned,
			Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		})
		if err == nil {
			_, err = e.pg.SetWorkspaceOwningUnit(context.Background(), ws.ID, &unit)
		}
		if err != nil {
			t.Fatalf("seed workspace %s: %v", name, err)
		}
	}
	check := `{"user_subjects":["` + person + `"]}`
	for _, tc := range []struct {
		who  string
		c    *http.Cookie
		want bool
	}{
		{"ua-a's admin", e.adminA, false},
		{"a security admin", e.alice, true},
	} {
		w := e.call(tc.c, http.MethodPost, "/api/v1/governance/check", check)
		var got governanceCheckResponse
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
			t.Fatalf("check as %s = %d %s", tc.who, w.Code, w.Body)
		}
		whole := len(got.Restrictions) == 1 && got.Restrictions[0].Spec.DeniedDomains != nil &&
			got.Restrictions[0].Contact != nil && got.Restrictions[0].Contact.Email == "restrictions@ua.example"
		if whole != tc.want || (!tc.want && strings.Contains(w.Body.String(), `"restrictions":`)) {
			t.Errorf("check as %s: restrictions %+v, want them whole = %v", tc.who, got.Restrictions, tc.want)
		}
		var names []string
		for _, g := range got.Workspaces {
			names = append(names, g.UnitName+":"+strings.Join(g.Names, ","))
		}
		want := []string{"ua-a1:ua-ws-in"}
		if tc.want {
			want = append(want, "ua-b:ua-ws-out")
		}
		if !slices.Equal(names, want) || strings.Contains(w.Body.String(), `"ua-b"`) != tc.want {
			t.Errorf("check as %s: workspaces %v, want %v and ua-b named = %v", tc.who, names, want, tc.want)
		}
		wantRec := policyref.Provenance{Layer: policyref.LayerRestriction}
		if tc.want {
			wantRec.Name = r.Name
		}
		if got.Effective == nil || got.Effective.Provenance["denied_domains"] != wantRec || strings.Contains(w.Body.String(), r.Name) != tc.want {
			t.Errorf("check as %s: denied_domains provenance %+v, want %+v and the restriction named = %v", tc.who, got.Effective, wantRec, tc.want)
		}
	}
	mid := e.child(e.a1)
	leaf := e.child(mid)
	deep, admin := e.fresh("sub-ua-deep"), e.fresh("sub-ua-mid-admin")
	treeMember(t, e.govEnv, leaf, types.CapabilitySubjectUser, deep, 1, false)
	e.grant(mid, types.CapabilitySubjectUser, admin)
	for _, tc := range []struct {
		who  string
		c    *http.Cookie
		want []uuid.UUID
	}{
		{"the admin of the unit below ua-a1", uaSession(t, admin, []string{}, false), []uuid.UUID{leaf.ID, mid.ID, e.a1.ID}},
		{"a security admin", e.alice, []uuid.UUID{leaf.ID, mid.ID, e.a1.ID, e.a.ID}},
	} {
		w := e.call(tc.c, http.MethodPost, "/api/v1/governance/check", `{"user_subjects":["`+deep+`"]}`)
		var got governanceCheckResponse
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Contexts) != 1 {
			t.Fatalf("check as %s = %d %s, want 200 and one context", tc.who, w.Code, w.Body)
		}
		var path []uuid.UUID
		for _, l := range got.Contexts[0].Path {
			path = append(path, l.ID)
		}
		if len(path) < len(tc.want) || !slices.Equal(path[:len(tc.want)], tc.want) || (tc.c != e.alice && len(path) != len(tc.want)) {
			t.Errorf("check as %s: path %v, want it to open with %v (and end there below a security admin)", tc.who, path, tc.want)
		}
		if named := strings.Contains(w.Body.String(), `"ua-a"`); named != (tc.c == e.alice) {
			t.Errorf("check as %s names ua-a = %v", tc.who, named)
		}
	}
}

// TestUnitAdminCheckRunInOutsideScope: a unit admin's run_in of a unit they do not govern answers as a
// unit that is not the person's does, so the check does not tell them whether the person is in it.
// ua-a's admin naming ua-b for a person in ua-a1 and ua-b, for one in ua-a1 only, and naming a unit no
// one holds reads the same status and body; naming ua-a1, which they govern, answers 200.
func TestUnitAdminCheckRunInOutsideScope(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	user, both, only := types.CapabilitySubjectUser, e.fresh("sub-ua-both"), e.fresh("sub-ua-only")
	treeMember(t, e.govEnv, e.a1, user, both, 1, false)
	treeMember(t, e.govEnv, e.b, user, both, 2, false)
	treeMember(t, e.govEnv, e.a1, user, only, 1, false)
	check := func(person, runIn string) *httptest.ResponseRecorder {
		return e.call(e.adminA, http.MethodPost, "/api/v1/governance/check", `{"user_subjects":["`+person+`"],"run_in":"`+runIn+`"}`)
	}
	in, out, none := check(both, "ua-b"), check(only, "ua-b"), check(both, "ua-nowhere")
	if in.Code != http.StatusForbidden || wireReason(t, in) != reasonUnitNotAContext {
		t.Fatalf("run_in ua-b for a person in it = %d %s, want 403 %s", in.Code, in.Body, reasonUnitNotAContext)
	}
	for what, w := range map[string]*httptest.ResponseRecorder{"a person not in it": out, "a unit no one holds": none} {
		if w.Code != in.Code || w.Body.String() != in.Body.String() {
			t.Errorf("run_in for %s = %d %s, want byte for byte the answer for a person in it: %d %s", what, w.Code, w.Body, in.Code, in.Body)
		}
	}
	if w := check(both, "ua-a1"); w.Code != http.StatusOK {
		t.Errorf("run_in ua-a1, a unit they govern = %d %s, want 200", w.Code, w.Body)
	}
}

// TestUnitAdminHeldChangeRedactsSecrets: a held change's diff reads at its reader's tier, as GET
// /governance does. A unit admin lists and reads a held unit change and a held membership change on
// ua-a1 with the secret refs and secret names of each ceiling redacted, the ceiling of ua-a1 in each
// exactly the effective ceiling GET /governance shows them; a security admin reads them whole.
func TestUnitAdminHeldChangeRedactsSecrets(t *testing.T) {
	const secretRef, secretName = "ua-vendor-key", "ua-prod-db"
	e := newUnitAdminEnv(t, false, func(c *Config) {
		p := govTestPolicy
		p.EligibleGrants = []types.GrantSpec{{Kind: types.GrantAPIKey,
			Scope: json.RawMessage(`{"host":"api.vendor.example","secret_name":"` + secretRef + `"}`)}}
		p.LLMInspection = &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, WorkspaceSecretNames: []string{secretName}}
		c.DefaultPolicy = p
	})
	unitCh := e.pending(e.call(e.adminA, http.MethodPut, "/api/v1/governance/units/"+e.a1.ID.String(),
		uaUnitBody("ua-a1", e.a.ID, uaDomains("pypi.org", "github.com", "npmjs.org"), nil)))
	memberCh := e.heldOn(e.a1)
	// at is the JSON value at path in raw, nil when absent.
	at := func(raw json.RawMessage, path ...string) json.RawMessage {
		for _, k := range path {
			var m map[string]json.RawMessage
			if json.Unmarshal(raw, &m) != nil {
				return nil
			}
			raw = m[k]
		}
		return raw
	}
	for _, tc := range []struct {
		who    string
		c      *http.Cookie
		redact bool
	}{
		{"ua-a's admin", e.adminA, true},
		{"a security admin", e.alice, false},
	} {
		var gov struct {
			Units []struct {
				ID        uuid.UUID       `json:"id"`
				Effective json.RawMessage `json:"effective"`
			} `json:"units"`
		}
		w := e.call(tc.c, http.MethodGet, "/api/v1/governance", "")
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &gov) != nil {
			t.Fatalf("GET /governance as %s = %d %s", tc.who, w.Code, w.Body)
		}
		var want json.RawMessage
		for _, u := range gov.Units {
			if u.ID == e.a1.ID {
				want = at(u.Effective, "ceiling")
			}
		}
		if len(want) == 0 || strings.Contains(string(want), secretRef) == tc.redact || strings.Contains(string(want), secretName) == tc.redact {
			t.Fatalf("GET /governance as %s: ua-a1's effective ceiling %s, want the secret ref and name redacted = %v", tc.who, want, tc.redact)
		}
		var queue []types.GovernanceChange
		if w := e.call(tc.c, http.MethodGet, "/api/v1/governance/changes", ""); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &queue) != nil {
			t.Fatalf("GET /governance/changes as %s = %d %s", tc.who, w.Code, w.Body)
		}
		diffs := map[string]json.RawMessage{}
		for _, ch := range queue {
			diffs["GET /governance/changes "+ch.ID.String()] = ch.Diff
		}
		for _, id := range []uuid.UUID{unitCh.ID, memberCh.ID} {
			var ch types.GovernanceChange
			if w := e.call(tc.c, http.MethodGet, "/api/v1/governance/changes/"+id.String(), ""); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &ch) != nil {
				t.Fatalf("GET /governance/changes/%s as %s = %d %s", id, tc.who, w.Code, w.Body)
			}
			diffs["GET /governance/changes/{id} "+id.String()] = ch.Diff
		}
		for _, id := range []uuid.UUID{unitCh.ID, memberCh.ID} {
			for _, route := range []string{"GET /governance/changes ", "GET /governance/changes/{id} "} {
				diff, ok := diffs[route+id.String()]
				if !ok {
					t.Fatalf("%s as %s does not carry change %s", route, tc.who, id)
				}
				path := []string{"before", "effective", "ceiling"}
				if id == memberCh.ID {
					path = []string{"after", "unit", "ceiling"}
				}
				if got := at(diff, path...); !samePayload(got, want) {
					t.Errorf("%s%s as %s: %s = %s, want GET /governance's %s", route, id, tc.who, strings.Join(path, "."), got, want)
				}
				if strings.Contains(string(diff), secretRef) == tc.redact || strings.Contains(string(diff), secretName) == tc.redact {
					t.Errorf("%s%s as %s: diff %s, want the secret ref and name redacted = %v", route, id, tc.who, diff, tc.redact)
				}
			}
		}
	}
}

const (
	uaSecretRef  = "ua-vendor-key" // an eligible grant's secret ref, hidden from a unit admin
	uaSecretName = "ua-prod-db"    // an llm_inspection workspace secret name, hidden from a unit admin
	uaSubAdminS  = "sub-ua-admin-s"
)

func uaSecretGrants() []types.GrantSpec {
	return []types.GrantSpec{{Kind: types.GrantAPIKey,
		Scope: json.RawMessage(`{"header":"Authorization","host":"api.vendor.example","secret_name":"` + uaSecretRef + `"}`)}}
}

func uaSecretInspection() *types.LLMInspectionSpec {
	return &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, WorkspaceSecretNames: []string{uaSecretName}}
}

// newUnitAdminSecretEnv is the unit-admin tree on a deployment that carries a secret-ref grant, a
// workspace secret name and a mount, with two profiles that carry them too: secrets, composed below
// ua-a with them in its overlay, and standalone, with them in its ceiling and adminS its unit admin.
func newUnitAdminSecretEnv(t *testing.T) (e *unitAdminEnv, secrets, standalone types.GovernanceUnit, adminS *http.Cookie) {
	t.Helper()
	e = newUnitAdminEnv(t, false, func(c *Config) {
		p := govTestPolicy
		p.EligibleGrants, p.LLMInspection = uaSecretGrants(), uaSecretInspection()
		p.WorkspaceMounts = []types.WorkspaceMount{{Source: pinBaseSrc, Target: pinTgtA}}
		c.DefaultPolicy = p
	})
	now := time.Now().UTC()
	if _, err := e.pg.CreateWorkspace(context.Background(), types.Workspace{ID: uuid.New(), Name: "ws-ua-secrets", Status: types.WorkspaceScanned,
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: pinBaseSrc}, {Type: types.WorkspaceSourceTypeLocalDir, Path: pinMountSrc}},
		CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("onboard the seeded mount sources: %v", err)
	}
	grants := uaSecretGrants()
	secrets = e.seedComposed("ua-secrets", e.a, types.CeilingOverlay{AllowedDomains: &[]string{"pypi.org", "github.com"},
		EligibleGrants: &grants, LLMInspection: uaSecretInspection(), WorkspaceMounts: &[]types.WorkspaceMount{{Source: pinBaseSrc, Target: pinTgtA}}})
	standalone, err := e.pg.UpsertGovernanceUnit(context.Background(), types.GovernanceUnit{Name: "ua-standalone", CreatedBy: "seed",
		Ceiling: types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "pypi.org"}, MinConfinementClass: types.CC2,
			EligibleGrants: uaSecretGrants(), LLMInspection: uaSecretInspection(), WorkspaceMounts: []types.WorkspaceMount{{Source: pinMountSrc, Target: pinTgtA}}}})
	if err != nil {
		t.Fatalf("seed the standalone profile: %v", err)
	}
	e.grant(standalone, types.CapabilitySubjectUser, uaSubAdminS)
	return e, secrets, standalone, uaSession(t, uaSubAdminS, []string{}, false)
}

// roundTrip is profile id as reader c reads it on GET /governance, sent back as a PUT body; domains,
// when given, replaces its allowed_domains (a narrowing or a widening in the same write).
func (e *unitAdminEnv) roundTrip(c *http.Cookie, id uuid.UUID, domains ...string) string {
	e.t.Helper()
	v, _ := e.view(c, id)
	if v.Effective.Error != "" {
		e.t.Fatalf("profile %s does not compose: %s", id, v.Effective.Error)
	}
	m := map[string]any{"name": v.Name}
	if v.Overlay != nil {
		o := *v.Overlay
		if domains != nil {
			o.AllowedDomains = &domains
		}
		m["parent_id"], m["overlay"] = v.ParentID, o
	} else {
		ceiling := v.Ceiling
		if domains != nil {
			ceiling.AllowedDomains = domains
		}
		m["ceiling"], m["limits"] = ceiling, v.Limits
	}
	b, err := json.Marshal(m)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

// uaJSONAt is the JSON value at path in raw, nil when absent.
func uaJSONAt(raw json.RawMessage, path ...string) json.RawMessage {
	for _, k := range path {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			return nil
		}
		raw = m[k]
	}
	return raw
}

// TestUnitAdminOverlaySecretsRedacted: a unit's overlay reads as its ceiling does at the reader's tier.
// A unit admin's GET /governance, and their list and {id} reads of a held change to the unit, show
// the overlay's eligible-grant secret ref and llm_inspection secret name redacted; a security admin's
// show both.
func TestUnitAdminOverlaySecretsRedacted(t *testing.T) {
	e, secrets, _, _ := newUnitAdminSecretEnv(t)
	held := e.pending(e.call(e.adminA, http.MethodPut, "/api/v1/governance/units/"+secrets.ID.String(),
		e.roundTrip(e.adminA, secrets.ID, "pypi.org", "github.com", "npmjs.org")))
	for _, tc := range []struct {
		who    string
		c      *http.Cookie
		redact bool
	}{
		{"ua-a's admin", e.adminA, true},
		{"a security admin", e.alice, false},
	} {
		leaks := func(raw []byte) bool {
			return strings.Contains(string(raw), uaSecretRef) || strings.Contains(string(raw), uaSecretName)
		}
		v, _ := e.view(tc.c, secrets.ID)
		if got := mustJSON(v.Overlay); leaks(got) == tc.redact || !strings.Contains(string(got), "api.vendor.example") {
			t.Errorf("GET /governance as %s: overlay %s, want the secret ref and name redacted = %v", tc.who, got, tc.redact)
		}
		var queue []types.GovernanceChange
		if w := e.call(tc.c, http.MethodGet, "/api/v1/governance/changes", ""); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &queue) != nil {
			t.Fatalf("GET /governance/changes as %s = %d %s", tc.who, w.Code, w.Body)
		}
		var read types.GovernanceChange
		if w := e.call(tc.c, http.MethodGet, "/api/v1/governance/changes/"+held.ID.String(), ""); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &read) != nil {
			t.Fatalf("GET /governance/changes/%s as %s = %d %s", held.ID, tc.who, w.Code, w.Body)
		}
		i := slices.IndexFunc(queue, func(ch types.GovernanceChange) bool { return ch.ID == held.ID })
		if i < 0 {
			t.Fatalf("GET /governance/changes as %s does not list %s", tc.who, held.ID)
		}
		for route, diff := range map[string]json.RawMessage{"GET /governance/changes": queue[i].Diff, "GET /governance/changes/{id}": read.Diff} {
			for _, side := range []string{"before", "after"} {
				if got := uaJSONAt(diff, side, "overlay"); leaks(got) == tc.redact || !strings.Contains(string(got), "api.vendor.example") {
					t.Errorf("%s as %s: %s.overlay %s, want the secret ref and name redacted = %v", route, tc.who, side, got, tc.redact)
				}
			}
		}
	}
}

// TestUnitAdminRoundTripKeepsSecrets: a unit admin who sends a profile back as they read it, with a
// narrowing, keeps the secret refs, secret names and mount sources the read redacted, in an overlay
// and in a ceiling, and the narrowing applies; a names count that stands for no stored names is
// refused. A security admin's round trip stores the ceiling byte for byte.
func TestUnitAdminRoundTripKeepsSecrets(t *testing.T) {
	e, secrets, standalone, adminS := newUnitAdminSecretEnv(t)
	same := func(what string, got, want any) {
		t.Helper()
		if g, w := mustJSON(got), mustJSON(want); string(g) != string(w) {
			t.Errorf("%s = %s, want %s as stored", what, g, w)
		}
	}
	before := e.profile(secrets.ID)
	if w := e.call(e.adminA, http.MethodPut, "/api/v1/governance/units/"+secrets.ID.String(), e.roundTrip(e.adminA, secrets.ID, "pypi.org")); w.Code != http.StatusOK {
		t.Fatalf("ua-a's admin round trip of the overlay with a narrowing = %d %s, want 200", w.Code, w.Body)
	}
	after := e.profile(secrets.ID)
	same("overlay.eligible_grants", after.Overlay.EligibleGrants, before.Overlay.EligibleGrants)
	same("overlay.llm_inspection", after.Overlay.LLMInspection, before.Overlay.LLMInspection)
	same("overlay.workspace_mounts", after.Overlay.WorkspaceMounts, before.Overlay.WorkspaceMounts)
	same("overlay.allowed_domains", after.Overlay.AllowedDomains, []string{"pypi.org"})

	before = e.profile(standalone.ID)
	if w := e.call(adminS, http.MethodPut, "/api/v1/governance/units/"+standalone.ID.String(), e.roundTrip(adminS, standalone.ID, "api.anthropic.com")); w.Code != http.StatusOK {
		t.Fatalf("the unit admin's round trip of the ceiling with a narrowing = %d %s, want 200", w.Code, w.Body)
	}
	after = e.profile(standalone.ID)
	same("ceiling.eligible_grants", after.Ceiling.EligibleGrants, before.Ceiling.EligibleGrants)
	same("ceiling.llm_inspection", after.Ceiling.LLMInspection, before.Ceiling.LLMInspection)
	same("ceiling.workspace_mounts", after.Ceiling.WorkspaceMounts, before.Ceiling.WorkspaceMounts)
	same("ceiling.allowed_domains", after.Ceiling.AllowedDomains, []string{"api.anthropic.com"})

	// json.Marshal writes < and > escaped.
	body := e.roundTrip(adminS, standalone.ID)
	if !strings.Contains(body, `\u003c1 name(s) redacted\u003e`) {
		t.Fatalf("the unit admin reads no names count: %s", body)
	}
	stale := strings.Replace(body, `\u003c1 name(s) redacted\u003e`, `\u003c2 name(s) redacted\u003e`, 1)
	wantStatusReason(t, e.call(adminS, http.MethodPut, "/api/v1/governance/units/"+standalone.ID.String(), stale),
		http.StatusUnprocessableEntity, reasonGovernanceUnitRequestInvalid)
	same("ceiling.llm_inspection after the refused count", e.profile(standalone.ID).Ceiling.LLMInspection, before.Ceiling.LLMInspection)

	// The security admin reads both whole and stores what they send.
	v, _ := e.view(e.alice, standalone.ID)
	if got := mustJSON(v.Ceiling); !strings.Contains(string(got), uaSecretRef) || !strings.Contains(string(got), uaSecretName) {
		t.Errorf("GET /governance as a security admin: ceiling %s, want the secret ref and name", got)
	}
	stored := e.profile(standalone.ID).Ceiling
	if w := e.call(e.alice, http.MethodPut, "/api/v1/governance/units/"+standalone.ID.String(), e.roundTrip(e.alice, standalone.ID)); w.Code != http.StatusOK {
		t.Fatalf("a security admin's round trip = %d %s, want 200", w.Code, w.Body)
	}
	same("the ceiling after a security admin's round trip", e.profile(standalone.ID).Ceiling, stored)
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
