// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Governance four-eyes for the permission-side targets (gov4-b2): capability grants, the enforcement
// map, a value's availability and a user type's priority, against a real Postgres. The two routes that
// widen without a grant or assignment write (lifting a restriction, changing a priority) are pinned
// against the real decision, not against the row: a test that read only the 202 would pass while the
// decision had already changed.
//
// Guarded by WARDYN_TEST_PG (throwawayPGPool): skipped cleanly when unset, must PASS when set.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	govTypeDev  = "developer"
	govTypePM   = "portfolio-manager"
	govAgentVal = "codex"
)

func (e *govEnv) seedUserType(id string, priority int) {
	e.t.Helper()
	if _, err := e.pg.CreateUserType(context.Background(), types.UserType{ID: id, Name: id, Priority: priority, CreatedBy: "seed"}); err != nil {
		e.t.Fatalf("seed user type %s: %v", id, err)
	}
}

// approve approves a held change as bob and fails the test unless it applied.
func (e *govEnv) approve(id uuid.UUID) {
	e.t.Helper()
	w := e.call(e.bob, http.MethodPost, "/api/v1/governance/changes/"+id.String()+"/approve", "")
	if w.Code != http.StatusOK {
		e.t.Fatalf("approving %s = %d %s, want 200", id, w.Code, w.Body)
	}
}

func (e *govEnv) grantRows() []types.CapabilityGrant {
	e.t.Helper()
	g, err := e.pg.ListCapabilityGrants(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return g
}

// agentAllowed is the REAL capability decision for one person of one type asking for the agent.
func (e *govEnv) agentAllowed(userType string) bool {
	e.t.Helper()
	w := httptest.NewRecorder()
	_, denied := e.srv.denyUserRequest(w, typeRequest(oidc.RoleUser, userType), createRunRequest{Agent: govAgentVal})
	return !denied
}

// TestPG_GovernanceChanges_GrantUpsertAndDeleteAreHeld: a grant upsert and a deny-grant DELETE both
// answer 202 and leave the real decision unchanged until a distinct approval applies them.
func TestPG_GovernanceChanges_GrantUpsertAndDeleteAreHeld(t *testing.T) {
	e := newGovEnv(t)
	e.seedUserType(govTypeDev, 1)
	deny := types.CapabilityGrant{
		SubjectType: types.CapabilitySubjectUserType, Subject: govTypeDev, Capability: capAgent, Value: govAgentVal,
		Effect: types.CapabilityDeny, CreatedBy: "seed",
	}
	if _, err := e.pg.UpsertCapabilityGrant(context.Background(), deny); err != nil {
		t.Fatal(err)
	}
	if e.agentAllowed(govTypeDev) {
		t.Fatal("precondition: a developer is denied the agent")
	}
	denyID := e.grantRows()[0].ID

	// A deny-grant delete is held, and the deny keeps applying.
	del := e.pending(e.call(e.alice, http.MethodDelete, "/api/v1/permissions/grants/"+denyID.String(), ""))
	if del.TargetKind != govKindGrant || del.Op != "delete" {
		t.Errorf("held delete = %s %s, want %s delete", del.TargetKind, del.Op, govKindGrant)
	}
	if len(e.grantRows()) != 1 || e.agentAllowed(govTypeDev) {
		t.Fatalf("a held delete of a deny grant changed the real decision (rows %d)", len(e.grantRows()))
	}
	e.approve(del.ID)
	if len(e.grantRows()) != 0 || !e.agentAllowed(govTypeDev) {
		t.Fatalf("the approved delete did not apply (rows %d)", len(e.grantRows()))
	}
	if rows := e.audits("capability.grant.delete"); len(rows) != 1 || rows[0].Actor != govSubBob || rows[0].Target != denyID.String() {
		t.Errorf("capability.grant.delete rows = %+v, want one by the approver naming the grant", rows)
	} else if d := auditData(t, rows[0]); d["proposed_by"] != govSubAlice || d["change_id"] == nil {
		t.Errorf("delete audit data = %v, want change_id and proposed_by", d)
	}

	// An upsert is held, then applies as a create, then a re-grant applies as an update.
	body := fmt.Sprintf(`{"subject_type":"user_type","subject":%q,"capability":"agent","value":%q,"effect":"deny"}`, govTypeDev, govAgentVal)
	up := e.pending(e.call(e.alice, http.MethodPost, "/api/v1/permissions/grants", body))
	if len(e.grantRows()) != 0 || !e.agentAllowed(govTypeDev) {
		t.Fatal("a held upsert changed the real decision")
	}
	e.approve(up.ID)
	if rows := e.grantRows(); len(rows) != 1 || rows[0].Effect != types.CapabilityDeny || rows[0].CreatedBy != govSubAlice || e.agentAllowed(govTypeDev) {
		t.Fatalf("the approved upsert = %+v (agent allowed %v), want a deny created by the proposer", rows, e.agentAllowed(govTypeDev))
	}
	if n := len(e.audits("capability.grant.create")); n != 1 {
		t.Errorf("%d capability.grant.create rows, want 1", n)
	}
	flip := strings.Replace(body, `"deny"`, `"allow"`, 1)
	e.approve(e.pending(e.call(e.alice, http.MethodPost, "/api/v1/permissions/grants", flip)).ID)
	if rows := e.grantRows(); len(rows) != 1 || rows[0].Effect != types.CapabilityAllow {
		t.Errorf("the approved re-grant = %+v, want the one row flipped to allow", rows)
	}
	if n := len(e.audits("capability.grant.update")); n != 1 {
		t.Errorf("%d capability.grant.update rows, want 1", n)
	}

	// A pending delete and a pending upsert of the same grant collide: the delete was resolved to the
	// natural key.
	id := e.grantRows()[0].ID
	e.pending(e.call(e.alice, http.MethodDelete, "/api/v1/permissions/grants/"+id.String(), ""))
	if w := e.call(e.alice, http.MethodPost, "/api/v1/permissions/grants", body); w.Code != http.StatusConflict ||
		wireReason(t, w) != reasonGovernanceChangePending {
		t.Errorf("an upsert of a grant with a held delete = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangePending)
	}
	// The refusals a direct write gives are given, not held.
	if w := e.call(e.alice, http.MethodPost, "/api/v1/permissions/grants", `{"subject_type":"all","capability":"nope","value":"x","effect":"allow"}`); w.Code != http.StatusBadRequest {
		t.Errorf("an invalid grant = %d %s, want the direct 400", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodDelete, "/api/v1/permissions/grants/"+govSubCarol, ""); w.Code != http.StatusBadRequest {
		t.Errorf("a malformed id = %d, want the direct 400", w.Code)
	}
}

// TestPG_GovernanceChanges_EnforcementReplaceIsHeldAndStaleChecked: the whole-map replacement is held
// (with or without If-Match), applies on a distinct approval, and a write to the map in between makes
// the approval stale.
func TestPG_GovernanceChanges_EnforcementReplaceIsHeldAndStaleChecked(t *testing.T) {
	e := newGovEnv(t)
	enf := func() map[string]bool {
		m, err := e.pg.GetCapabilityEnforcement(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	ch := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/permissions/enforcement", `{"agent":true}`))
	if ch.TargetKind != govKindEnforcement || ch.Op != "replace" || len(enf()) != 0 {
		t.Fatalf("held enforcement = %+v, enforcement now %v", ch, enf())
	}
	// If-Match is judged at proposal: a stale one is the direct 412 and is not held.
	if w := doSSOIfMatch(t, e.srv, http.MethodPut, "/api/v1/permissions/enforcement", e.alice, `"stale"`, `{"image":true}`); w.Code != http.StatusPreconditionFailed {
		t.Errorf("a stale If-Match = %d %s, want 412", w.Code, w.Body)
	}
	e.approve(ch.ID)
	if got := enf(); !got["agent"] {
		t.Fatalf("the approved replacement did not apply: %v", got)
	}
	if rows := e.audits("capability.enforcement.write"); len(rows) != 1 || rows[0].Actor != govSubBob {
		t.Errorf("capability.enforcement.write rows = %+v, want one by the approver", rows)
	}

	// A write between proposal and approval changes the base: the approval is stale and applies nothing.
	held := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/permissions/enforcement", `{"agent":false}`))
	if w := e.admin(http.MethodPut, "/api/v1/permissions/enforcement", `{"agent":true,"image":true}`); w.Code != http.StatusOK {
		t.Fatalf("admin-token write = %d %s", w.Code, w.Body)
	}
	if w := e.call(e.bob, http.MethodPost, approvePath(held.ID), ""); w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale {
		t.Fatalf("a stale approval = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeStale)
	}
	if got := enf(); !got["agent"] || !got["image"] {
		t.Errorf("a stale approval overwrote the map: %v", got)
	}
}

// TestPG_GovernanceChanges_AvailabilityLiftIsHeldAgainstTheRealDecision: lifting a restriction admits
// every person with no grant write. The real capability decision for an ungranted person stays refused
// until a distinct approval, and is allowed after it.
func TestPG_GovernanceChanges_AvailabilityLiftIsHeldAgainstTheRealDecision(t *testing.T) {
	e := newGovEnv(t)
	e.seedUserType(govTypeDev, 1)
	e.seedUserType(govTypePM, 1)
	if _, err := e.pg.UpsertCapabilityGrant(context.Background(), types.CapabilityGrant{
		SubjectType: types.CapabilitySubjectUserType, Subject: govTypeDev, Capability: capAgent, Value: govAgentVal,
		Effect: types.CapabilityAllow, CreatedBy: "seed",
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.pg.SetCapabilityRestriction(context.Background(), capAgent, govAgentVal, true, "seed"); err != nil {
		t.Fatal(err)
	}
	if e.agentAllowed(govTypePM) || !e.agentAllowed(govTypeDev) {
		t.Fatal("precondition: the agent is restricted to developers")
	}

	path := "/api/v1/permissions/availability/agent/" + govAgentVal
	ch := e.pending(e.call(e.alice, http.MethodPut, path, `{"restricted":false}`))
	if ch.TargetKind != govKindAvailability || e.agentAllowed(govTypePM) {
		t.Fatalf("a held lift changed the real decision for an ungranted person (kind %s)", ch.TargetKind)
	}
	e.approve(ch.ID)
	if !e.agentAllowed(govTypePM) {
		t.Fatal("the approved lift did not change the real decision")
	}
	if rows := e.audits("capability.availability.write"); len(rows) != 1 || rows[0].Actor != govSubBob || auditData(t, rows[0])["restricted"] != false {
		t.Errorf("capability.availability.write rows = %+v, want one by the approver with restricted false", rows)
	}

	// Restricting is covered too (the bit changes), and applies only on approval.
	re := e.pending(e.call(e.alice, http.MethodPut, path, `{"restricted":true}`))
	if !e.agentAllowed(govTypePM) {
		t.Fatal("a held restriction already applied")
	}
	e.approve(re.ID)
	if e.agentAllowed(govTypePM) {
		t.Fatal("the approved restriction did not apply")
	}
	// A PUT that leaves the bit as it is applies directly, as it always did.
	if w := e.call(e.alice, http.MethodPut, path, `{"restricted":true}`); w.Code != http.StatusOK {
		t.Errorf("an unchanged availability PUT = %d %s, want the direct 200", w.Code, w.Body)
	}
	// The "Only..." refusal is the direct write's, not held.
	if w := e.call(e.alice, http.MethodPut, "/api/v1/permissions/availability/agent/other", `{"restricted":true}`); w.Code != http.StatusBadRequest ||
		wireReason(t, w) != reasonAvailabilityOnlyEmpty {
		t.Errorf("restricting with nobody listed = %d %s, want 400 %s", w.Code, w.Body, reasonAvailabilityOnlyEmpty)
	}
}

// pgOIDCRoleMappings is the role-mapping source the daemon wires: the real table, read at sign-in.
type pgOIDCRoleMappings struct{ pg store.PG }

func (b pgOIDCRoleMappings) ListRoleMappings(ctx context.Context) ([]oidc.RoleMapping, error) {
	rows, err := b.pg.ListRoleMappings(ctx)
	return toOIDCRoleMappings(rows), err
}

// TestPG_GovernanceChanges_PriorityChangeIsHeldAgainstThePickedType: a priority change can move a
// person matching two types onto the other at their next sign-in. The real sign-in derivation
// (pickUserType over the real tables) keeps its outcome until a distinct approval.
func TestPG_GovernanceChanges_PriorityChangeIsHeldAgainstThePickedType(t *testing.T) {
	e := newGovEnv(t)
	e.seedUserType(govTypeDev, 5)
	e.seedUserType(govTypePM, 3)
	auth := newAccessAuth(t, map[string]string{"grp-dev": govTypeDev, "grp-pm": govTypePM}, "", nil, nil,
		func(c *oidc.Config) { c.RoleMappings, c.UserTypes = pgOIDCRoleMappings{e.pg}, e.pg })
	picked := func() string {
		d, err := auth.PreviewRole(context.Background(), nil, []string{"grp-dev", "grp-pm"}, "x@corp.example")
		if err != nil || !d.OK() {
			t.Fatalf("derive: %+v %v", d, err)
		}
		return d.UserType
	}
	if picked() != govTypeDev {
		t.Fatalf("precondition: the higher-priority type %s wins, got %s", govTypeDev, picked())
	}

	path := "/api/v1/user-types/" + govTypeDev
	ch := e.pending(e.call(e.alice, http.MethodPut, path, fmt.Sprintf(`{"name":%q,"priority":1}`, govTypeDev)))
	if ch.TargetKind != govKindUserType || picked() != govTypeDev {
		t.Fatalf("a held priority change moved the real sign-in outcome to %s (kind %s)", picked(), ch.TargetKind)
	}
	e.approve(ch.ID)
	if picked() != govTypePM {
		t.Fatalf("after approval the real sign-in outcome is %s, want %s", picked(), govTypePM)
	}
	if rows := e.audits("user_type.write"); len(rows) != 1 || rows[0].Actor != govSubBob || auditData(t, rows[0])["change_id"] == nil {
		t.Errorf("user_type.write rows = %+v, want one by the approver with a change_id", rows)
	}
}

// TestPG_GovernanceChanges_UserTypeMetadataAppliesDirectly: a name or description edit with the same
// priority is metadata and applies as it always did; a name plus a priority edit is held whole.
func TestPG_GovernanceChanges_UserTypeMetadataAppliesDirectly(t *testing.T) {
	e := newGovEnv(t)
	e.seedUserType(govTypeDev, 5)
	path := "/api/v1/user-types/" + govTypeDev
	if w := e.call(e.alice, http.MethodPut, path, `{"name":"Renamed","description":"d","priority":5}`); w.Code != http.StatusOK {
		t.Fatalf("a metadata edit = %d %s, want the direct 200", w.Code, w.Body)
	}
	if got, _ := e.pg.GetUserType(context.Background(), govTypeDev); got.Name != "Renamed" || got.Priority != 5 {
		t.Fatalf("the metadata edit = %+v", got)
	}
	if e.pendingCount() != 0 {
		t.Fatal("a metadata edit was held")
	}

	ch := e.pending(e.call(e.alice, http.MethodPut, path, `{"name":"Again","description":"d","priority":6}`))
	if got, _ := e.pg.GetUserType(context.Background(), govTypeDev); got.Name != "Renamed" || got.Priority != 5 {
		t.Fatalf("a held name-and-priority edit applied its name: %+v", got)
	}
	e.approve(ch.ID)
	if got, _ := e.pg.GetUserType(context.Background(), govTypeDev); got.Name != "Again" || got.Priority != 6 {
		t.Errorf("the approved edit = %+v, want name and priority both applied", got)
	}
	// A name clash is the direct 409, not held.
	e.seedUserType("other", 1)
	if w := e.call(e.alice, http.MethodPut, "/api/v1/user-types/other", `{"name":"Again","priority":2}`); w.Code != http.StatusConflict {
		t.Errorf("a clashing name = %d %s, want the direct 409", w.Code, w.Body)
	}
}

// TestPG_GovernanceChanges_PermissionKindsHonourTheGates: the admin token applies a b2 write directly
// with a bypass row; local mode refuses a covered write with the 503; a member cannot reach the route.
func TestPG_GovernanceChanges_PermissionKindsHonourTheGates(t *testing.T) {
	e := newGovEnv(t)
	body := `{"subject_type":"all","capability":"agent","value":"codex","effect":"deny"}`
	if w := e.admin(http.MethodPost, "/api/v1/permissions/grants", body); w.Code != http.StatusCreated {
		t.Fatalf("admin-token grant = %d %s, want the direct 201", w.Code, w.Body)
	}
	if w := e.admin(http.MethodPut, "/api/v1/permissions/enforcement", `{"agent":true}`); w.Code != http.StatusOK {
		t.Fatalf("admin-token enforcement = %d %s, want the direct 200", w.Code, w.Body)
	}
	if rows := e.audits("governance.change.bypass"); len(rows) != 2 {
		t.Errorf("%d bypass rows, want 2", len(rows))
	}
	if w := e.call(e.user, http.MethodPost, "/api/v1/permissions/grants", body); w.Code != http.StatusForbidden {
		t.Errorf("a member writing a grant = %d, want 403", w.Code)
	}
	if e.pendingCount() != 0 {
		t.Errorf("%d pending changes after bypass writes", e.pendingCount())
	}
}

// TestPG_GovernanceChanges_PermissionRoutesSwitchOffIsUnchanged: with the switch unset every b2 route
// answers as 0.8.5 did: the same statuses, no 202, no Location, no pending row and no governance.change
// audit row, for the human who would otherwise have been held.
func TestPG_GovernanceChanges_PermissionRoutesSwitchOffIsUnchanged(t *testing.T) {
	pool := throwawayPGPool(t) // the switch is not set
	e := newGovEnvOn(t, pool)
	e.seedUserType(govTypeDev, 5)
	grant := `{"subject_type":"user_type","subject":"developer","capability":"agent","value":"codex","effect":"allow"}`
	etag := func() string {
		m, _ := e.pg.GetCapabilityEnforcement(context.Background())
		return computeETag(m)
	}
	type step struct {
		name, method, path, body, ifMatch string
		want                              int
	}
	steps := []step{
		{name: "grant create", method: http.MethodPost, path: "/api/v1/permissions/grants", body: grant, want: http.StatusCreated},
		{name: "grant re-grant", method: http.MethodPost, path: "/api/v1/permissions/grants", body: grant, want: http.StatusOK},
		{name: "enforcement", method: http.MethodPut, path: "/api/v1/permissions/enforcement", body: `{"agent":true}`, want: http.StatusOK},
		{name: "enforcement matching If-Match", method: http.MethodPut, path: "/api/v1/permissions/enforcement", body: `{"agent":false}`, ifMatch: "etag", want: http.StatusOK},
		{name: "enforcement stale If-Match", method: http.MethodPut, path: "/api/v1/permissions/enforcement", body: `{"agent":true}`, ifMatch: `"stale"`, want: http.StatusPreconditionFailed},
		{name: "availability restrict", method: http.MethodPut, path: "/api/v1/permissions/availability/agent/codex", body: `{"restricted":true}`, want: http.StatusOK},
		{name: "availability lift", method: http.MethodPut, path: "/api/v1/permissions/availability/agent/codex", body: `{"restricted":false}`, want: http.StatusOK},
		{name: "user type priority", method: http.MethodPut, path: "/api/v1/user-types/developer", body: `{"name":"developer","priority":9}`, want: http.StatusOK},
		{name: "user type metadata", method: http.MethodPut, path: "/api/v1/user-types/developer", body: `{"name":"Dev","priority":9}`, want: http.StatusOK},
	}
	for _, s := range steps {
		var w *httptest.ResponseRecorder
		if s.ifMatch != "" {
			im := s.ifMatch
			if im == "etag" {
				im = etag()
			}
			w = doSSOIfMatch(t, e.srv, s.method, s.path, e.alice, im, s.body)
		} else {
			w = e.call(e.alice, s.method, s.path, s.body)
		}
		if w.Code != s.want || w.Header().Get("Location") != "" || strings.Contains(w.Body.String(), "pending_change") {
			t.Errorf("%s: %d %s (Location %q), want %d with no pending_change", s.name, w.Code, w.Body, w.Header().Get("Location"), s.want)
		}
	}
	id := e.grantRows()[0].ID
	if w := e.call(e.alice, http.MethodDelete, "/api/v1/permissions/grants/"+id.String(), ""); w.Code != http.StatusNoContent {
		t.Errorf("grant delete = %d %s, want 204", w.Code, w.Body)
	}
	if e.pendingCount() != 0 {
		t.Errorf("%d pending changes with the switch off", e.pendingCount())
	}
	for _, ev := range e.h.audit.snapshot() {
		if strings.HasPrefix(ev.Action, "governance.change.") {
			t.Errorf("a switch-off write recorded %s", ev.Action)
		}
	}
}

// TestPG_GovernanceChanges_PermissionKindsLocalModeRefuses: local mode authenticates nobody, so a
// covered b2 write answers the 503 that names the incompatibility, and a write that changes nothing
// covered (a metadata edit) is not refused by it.
func TestPG_GovernanceChanges_PermissionKindsLocalModeRefuses(t *testing.T) {
	e := newGovEnv(t, func(c *Config) {
		c.LocalMode = true
		c.LocalOperator = "local:alice"
		c.OIDC = nil
	})
	e.seedUserType(govTypeDev, 5)
	local := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Host = "127.0.0.1:8080"
		req.RemoteAddr = "127.0.0.1:54321"
		w := httptest.NewRecorder()
		panicFails(t, e.srv.Handler()).ServeHTTP(w, req)
		return w
	}
	for name, w := range map[string]*httptest.ResponseRecorder{
		"grant upsert": local(http.MethodPost, "/api/v1/permissions/grants", `{"subject_type":"all","capability":"agent","value":"codex","effect":"deny"}`),
		"grant delete": local(http.MethodDelete, "/api/v1/permissions/grants/"+uuid.NewString(), ""),
		"enforcement":  local(http.MethodPut, "/api/v1/permissions/enforcement", `{"agent":true}`),
		"priority":     local(http.MethodPut, "/api/v1/user-types/"+govTypeDev, `{"name":"developer","priority":1}`),
	} {
		if w.Code != http.StatusServiceUnavailable || wireReason(t, w) != reasonGovernanceSecondHumanLocalMode {
			t.Errorf("%s in local mode: %d %s, want 503 %s", name, w.Code, w.Body, reasonGovernanceSecondHumanLocalMode)
		}
	}
	if w := local(http.MethodPut, "/api/v1/user-types/"+govTypeDev, `{"name":"Renamed","priority":5}`); w.Code != http.StatusOK {
		t.Errorf("a metadata edit in local mode = %d %s, want the direct 200", w.Code, w.Body)
	}
	if e.pendingCount() != 0 {
		t.Errorf("local mode held %d changes", e.pendingCount())
	}
}
