// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Governance four-eyes for role mappings (gov4-b2) against a real Postgres and a real
// oidc.Authenticator reading the real tables. A role mapping is written on the operatorOnly tier, so
// only a super admin approves one, and the lockout guard is judged against the approver.
//
// Guarded by WARDYN_TEST_PG (throwawayPGPool): skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// roleMapEnv is a deployment with SSO whose derivation reads the real role_mappings table.
type roleMapEnv struct {
	*govEnv
	// proposer and approver are super admins; losing holds admin only through the console row
	// "ops-admins"; sec is a security admin.
	proposer, approver, losing, sec *http.Cookie
}

func newRoleMapEnv(t *testing.T, switchOn bool, chart map[string]string) *roleMapEnv {
	t.Helper()
	if switchOn {
		t.Setenv(envGovernanceSecondHuman, "true")
	}
	pool := throwawayPGPool(t)
	pg := store.NewPG(pool)
	auth := newAccessAuth(t, chart, "", nil, nil, func(c *oidc.Config) {
		c.RoleMappings, c.UserTypes = pgOIDCRoleMappings{pg}, pg
	})
	e := newGovEnvOn(t, pool, func(c *Config) { c.OIDC = auth })
	return &roleMapEnv{
		govEnv:   e,
		proposer: accessSession(t, "sub-proposer", "proposer@corp.example", oidc.RoleAdmin, []string{"chart-admins"}),
		approver: accessSession(t, "sub-approver", "approver@corp.example", oidc.RoleAdmin, []string{"chart-admins"}),
		losing:   accessSession(t, "sub-losing", "losing@corp.example", oidc.RoleAdmin, []string{"ops-admins"}),
		sec:      accessSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin, []string{"chart-admins"}),
	}
}

var govRoleMapChart = map[string]string{"chart-admins": oidc.RoleAdmin}

func (e *roleMapEnv) mappings() []types.RoleMapping {
	e.t.Helper()
	rows, err := e.pg.ListRoleMappings(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

func (e *roleMapEnv) seedMapping(value, role string) types.RoleMapping {
	e.t.Helper()
	m, err := e.pg.UpsertRoleMapping(context.Background(), types.RoleMapping{Value: value, Role: role, CreatedBy: "seed"})
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *roleMapEnv) changeIDs(c *http.Cookie, path string) []uuid.UUID {
	e.t.Helper()
	w := e.call(c, http.MethodGet, path, "")
	var out []types.GovernanceChange
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK {
		e.t.Fatalf("list = %d %s (%v)", w.Code, w.Body, err)
	}
	var ids []uuid.UUID
	for _, ch := range out {
		ids = append(ids, ch.ID)
	}
	return ids
}

// TestPG_GovernanceChanges_RoleMappingNeedsASuperAdmin: an upsert and a delete are held, a security
// admin neither sees them nor can approve them (admin_surface), and a distinct super admin applies
// them with the approver as the target row's actor.
func TestPG_GovernanceChanges_RoleMappingNeedsASuperAdmin(t *testing.T) {
	e := newRoleMapEnv(t, true, govRoleMapChart)

	up := e.pending(e.call(e.proposer, http.MethodPost, "/api/v1/access/mappings", `{"value":"Eng","role":"user"}`))
	if up.TargetKind != govKindRoleMapping || up.TargetKey != "eng" || len(e.mappings()) != 0 {
		t.Fatalf("held upsert = %+v, rows %v", up, e.mappings())
	}

	// Visibility: the queue a security admin reads has no role-mapping change, and one read is a 404.
	if ids := e.changeIDs(e.sec, "/api/v1/governance/changes"); len(ids) != 0 {
		t.Errorf("a security admin's change list holds %v, want no role-mapping change", ids)
	}
	if ids := e.changeIDs(e.approver, "/api/v1/governance/changes"); len(ids) != 1 || ids[0] != up.ID {
		t.Errorf("a super admin's change list = %v, want %s", ids, up.ID)
	}
	if w := e.call(e.sec, http.MethodGet, "/api/v1/governance/changes/"+up.ID.String(), ""); w.Code != http.StatusNotFound {
		t.Errorf("a security admin reading a role-mapping change = %d, want 404", w.Code)
	}

	// Approval tier: a security admin is refused admin_surface and the change stays pending.
	w := e.call(e.sec, http.MethodPost, approvePath(up.ID), "")
	if w.Code != http.StatusForbidden || wireReason(t, w) != "admin_surface" {
		t.Fatalf("a security admin approving = %d %s, want 403 admin_surface", w.Code, w.Body)
	}
	if e.changeState(up.ID) != types.GovernanceChangePending || len(e.mappings()) != 0 {
		t.Fatal("a refused security-admin approval changed the change or the table")
	}
	denied := 0
	for _, ev := range e.audits("authz.denied") {
		if d := auditData(t, ev); d["reason"] == "admin_surface" && ev.Target == "governance.change" {
			denied++
		}
	}
	if denied != 1 {
		t.Errorf("%d authz.denied rows for admin_surface at governance.change, want 1", denied)
	}
	// The proposer cannot approve their own, even as a super admin.
	if w := e.call(e.proposer, http.MethodPost, approvePath(up.ID), ""); w.Code != http.StatusForbidden || wireReason(t, w) != "second_human_required" {
		t.Errorf("self-approval = %d %s, want 403 second_human_required", w.Code, w.Body)
	}

	if w := e.call(e.approver, http.MethodPost, approvePath(up.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("a distinct super admin approving = %d %s, want 200", w.Code, w.Body)
	}
	rows := e.mappings()
	if len(rows) != 1 || rows[0].Value != "eng" || rows[0].Role != oidc.RoleUser || rows[0].CreatedBy != "sub-proposer" {
		t.Fatalf("the approved mapping = %+v, want eng/user created by the proposer", rows)
	}
	audit := e.audits("access.role_mapping.write")
	if len(audit) != 1 || audit[0].Actor != "sub-approver" {
		t.Fatalf("access.role_mapping.write rows = %+v, want one by the approver", audit)
	}
	d := auditData(t, audit[0])
	if d["proposed_by"] != "sub-proposer" || d["change_id"] == nil || d["tokens_revoked"] == nil || d["stale_token_snapshots"] == nil {
		t.Errorf("role-mapping audit data = %v, want change_id, proposed_by and the revocation counts", d)
	}

	del := e.pending(e.call(e.proposer, http.MethodDelete, "/api/v1/access/mappings/"+rows[0].ID.String(), ""))
	if del.Op != "delete" || del.TargetKey != "eng" || len(e.mappings()) != 1 {
		t.Fatalf("held delete = %+v, rows %v", del, e.mappings())
	}
	if w := e.call(e.sec, http.MethodPost, approvePath(del.ID), ""); w.Code != http.StatusForbidden || wireReason(t, w) != "admin_surface" {
		t.Errorf("a security admin approving a delete = %d %s, want 403 admin_surface", w.Code, w.Body)
	}
	if w := e.call(e.approver, http.MethodPost, approvePath(del.ID), ""); w.Code != http.StatusOK || len(e.mappings()) != 0 {
		t.Fatalf("the approved delete = %d %s, rows %v", w.Code, w.Body, e.mappings())
	}
	if rows := e.audits("access.role_mapping.delete"); len(rows) != 1 || rows[0].Actor != "sub-approver" {
		t.Errorf("access.role_mapping.delete rows = %+v, want one by the approver", rows)
	}
	// A delete of an unknown id is the direct 404, not held.
	if w := e.call(e.proposer, http.MethodDelete, "/api/v1/access/mappings/"+uuid.NewString(), ""); w.Code != http.StatusNotFound {
		t.Errorf("deleting an unknown mapping = %d, want the direct 404", w.Code)
	}
}

// TestPG_GovernanceChanges_RoleMappingLockoutIsTheApproversNotTheProposers: the lockout guard is run
// when the write is APPLIED, against the claim snapshot of the human applying it. A proposer who would
// lose admin does not block the proposal or the approval; an approver who would lose admin is refused
// and the change stays pending.
func TestPG_GovernanceChanges_RoleMappingLockoutIsTheApproversNotTheProposers(t *testing.T) {
	e := newRoleMapEnv(t, true, govRoleMapChart)
	ops := e.seedMapping("ops-admins", oidc.RoleAdmin)

	// "losing" holds admin only through the ops-admins row; deleting it would lock them out. A direct
	// delete by them is refused by the guard; as a proposal it is held, because the proposer's own facts
	// are not consulted.
	ch := e.pending(e.call(e.losing, http.MethodDelete, "/api/v1/access/mappings/"+ops.ID.String(), ""))

	// An approver whose own snapshot would lose admin is refused, and the change stays pending.
	other := accessSession(t, "sub-losing-too", "losing2@corp.example", oidc.RoleAdmin, []string{"ops-admins"})
	w := e.call(other, http.MethodPost, approvePath(ch.ID), "")
	if w.Code != http.StatusBadRequest || wireReason(t, w) != reasonAccessLockout {
		t.Fatalf("an approver who would lose admin = %d %s, want 400 %s", w.Code, w.Body, reasonAccessLockout)
	}
	if e.changeState(ch.ID) != types.GovernanceChangePending || len(e.mappings()) != 1 {
		t.Fatal("a lockout-refused approval applied or left pending")
	}

	// A distinct approver who keeps admin applies it, though the proposer would have lost theirs.
	if w := e.call(e.approver, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("an approver who keeps admin = %d %s, want 200", w.Code, w.Body)
	}
	if len(e.mappings()) != 0 {
		t.Fatalf("the approved delete did not apply: %v", e.mappings())
	}
	// The admin token is exempt from the guard, as it is on a direct write.
	again := e.seedMapping("ops-admins", oidc.RoleAdmin)
	held := e.pending(e.call(e.losing, http.MethodDelete, "/api/v1/access/mappings/"+again.ID.String(), ""))
	if w := e.admin(http.MethodPost, approvePath(held.ID), ""); w.Code != http.StatusOK {
		t.Errorf("the admin token approving = %d %s, want 200", w.Code, w.Body)
	}
	if rows := e.audits("governance.change.bypass"); len(rows) != 1 {
		t.Errorf("%d bypass rows for the admin-token approval, want 1", len(rows))
	}
}

// TestPG_GovernanceChanges_RoleMappingPostureFlipIsReEvaluatedOnApproval: with no chart, the first
// console row flips every unmatched human from admin to refused, which needs an acknowledgement. The
// proposal is refused without it, exactly as a direct write is; a flip that appears between proposal
// and approval is refused when the change is applied.
func TestPG_GovernanceChanges_RoleMappingPostureFlipIsReEvaluatedOnApproval(t *testing.T) {
	e := newRoleMapEnv(t, true, nil)

	w := e.admin(http.MethodPost, "/api/v1/access/mappings", `{"value":"eng","role":"user"}`)
	var flip accessPostureFlipBody
	if w.Code != http.StatusBadRequest || json.Unmarshal(w.Body.Bytes(), &flip) != nil || !flip.RequiredAcknowledgement {
		t.Fatalf("an unacknowledged flip = %d %s, want the direct 400 with required_acknowledgement", w.Code, w.Body)
	}
	if e.pendingCount() != 0 {
		t.Fatal("an unacknowledged posture flip was held")
	}

	// With one row already stored, a second adds no flip: held.
	r0 := e.seedMapping("ops-admins", oidc.RoleAdmin)
	approver := accessSession(t, "sub-approver", "approver@corp.example", oidc.RoleAdmin, []string{"ops-admins"})
	proposer := accessSession(t, "sub-proposer", "proposer@corp.example", oidc.RoleAdmin, []string{"ops-admins"})
	ch := e.pending(e.call(proposer, http.MethodPost, "/api/v1/access/mappings", `{"value":"eng","role":"user"}`))
	// The only other row goes (acknowledged, by the admin token): applying the held row now would flip
	// the unmatched outcome, which its proposal never acknowledged.
	if w := e.admin(http.MethodDelete, "/api/v1/access/mappings/"+r0.ID.String()+"?acknowledge_access_change=true", ""); w.Code != http.StatusNoContent {
		t.Fatalf("admin-token delete = %d %s", w.Code, w.Body)
	}
	w = e.call(approver, http.MethodPost, approvePath(ch.ID), "")
	if w.Code != http.StatusBadRequest || json.Unmarshal(w.Body.Bytes(), &flip) != nil || !flip.RequiredAcknowledgement {
		t.Fatalf("approving into a flip = %d %s, want the 400 with required_acknowledgement", w.Code, w.Body)
	}
	if e.changeState(ch.ID) != types.GovernanceChangePending || len(e.mappings()) != 0 {
		t.Fatal("a refused approval applied or left pending")
	}
}

// TestPG_GovernanceChanges_RoleMappingSwitchOffIsUnchanged: with the switch unset the role-mapping
// routes answer as 0.8.5 did, the posture-flip acknowledgement refusal included.
func TestPG_GovernanceChanges_RoleMappingSwitchOffIsUnchanged(t *testing.T) {
	e := newRoleMapEnv(t, false, nil)
	check := func(name string, w *httptest.ResponseRecorder, want int) {
		t.Helper()
		if w.Code != want || w.Header().Get("Location") != "" || strings.Contains(w.Body.String(), "pending_change") {
			t.Errorf("%s: %d %s (Location %q), want %d with no pending_change", name, w.Code, w.Body, w.Header().Get("Location"), want)
		}
	}
	w := e.call(e.proposer, http.MethodPost, "/api/v1/access/mappings", `{"value":"eng","role":"user"}`)
	var flip accessPostureFlipBody
	if json.Unmarshal(w.Body.Bytes(), &flip) != nil || !flip.RequiredAcknowledgement {
		t.Errorf("the unacknowledged flip body = %s, want required_acknowledgement", w.Body)
	}
	check("posture flip refusal", w, http.StatusBadRequest)
	// The admin token is exempt from the lockout guard, so the table is writable through it.
	created := e.admin(http.MethodPost, "/api/v1/access/mappings", `{"value":"eng","role":"user","acknowledge_access_change":true}`)
	check("upsert", created, http.StatusCreated)
	check("re-add", e.admin(http.MethodPost, "/api/v1/access/mappings", `{"value":"ENG","role":"admin"}`), http.StatusOK)
	var saved types.RoleMapping
	if err := json.Unmarshal(created.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	check("delete", e.admin(http.MethodDelete, "/api/v1/access/mappings/"+saved.ID.String()+"?acknowledge_access_change=true", ""), http.StatusNoContent)
	if e.pendingCount() != 0 {
		t.Errorf("%d pending changes with the switch off", e.pendingCount())
	}
	for _, ev := range e.h.audit.snapshot() {
		if strings.HasPrefix(ev.Action, "governance.change.") {
			t.Errorf("a switch-off write recorded %s", ev.Action)
		}
	}
}
