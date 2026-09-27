// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// #1197: GET /approvals' opt-in ?view=user|admin.
package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestListApprovals_ViewUserForcesOwnerScope pins #1197's approvals scoping: an ADMIN who passes
// ?view=user sees only approvals on runs THEY created — the same shape
// TestListApprovals_MemberSeesOnlyTheirOwnRuns already proves for a plain
// member, now proven for the tier that ?view=user exists to narrow.
// Counterfactual: dropping the view=user force (so approvalsViewScope always
// returns !isSecurityOperator) must fail this — the admin case would then see
// both rows again.
func TestListApprovals_ViewUserForcesOwnerScope(t *testing.T) {
	const adminSub = "sub-f5-admin"
	srv, ast, aap, _ := newAuthzMatrixServer(t)

	seedRunOwnedBy := func(createdBy string) uuid.UUID {
		id := uuid.New()
		ast.mu.Lock()
		ast.runs[id] = types.AgentRun{ID: id, CreatedBy: createdBy, State: types.RunRunning, Agent: "claude-code"}
		ast.mu.Unlock()
		return id
	}
	mineRun, foreignRun := seedRunOwnedBy(adminSub), seedRunOwnedBy("sub-f5-someone-else")
	mineAP := aap.seed(mineRun)
	foreignAP := aap.seed(foreignRun)

	admin := ssoSession(t, adminSub, "f5-admin@corp.example", oidc.RoleAdmin)

	// Without view=, the admin sees the fleet-wide queue (both rows) — the
	// existing, unchanged behaviour.
	wAll := doSSO(t, srv, http.MethodGet, "/api/v1/approvals", admin, "")
	if wAll.Code != http.StatusOK {
		t.Fatalf("admin, no view: code = %d; body=%s", wAll.Code, wAll.Body.String())
	}
	var all []types.ApprovalRequest
	if err := json.Unmarshal(wAll.Body.Bytes(), &all); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("admin, no view: got %d row(s), want 2 (fixture stopped exercising the split)", len(all))
	}

	// With view=user, the SAME admin is narrowed to their own run's approval.
	wUser := doSSO(t, srv, http.MethodGet, "/api/v1/approvals?view=user", admin, "")
	if wUser.Code != http.StatusOK {
		t.Fatalf("admin, view=user: code = %d; body=%s", wUser.Code, wUser.Body.String())
	}
	var scoped []types.ApprovalRequest
	if err := json.Unmarshal(wUser.Body.Bytes(), &scoped); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := map[uuid.UUID]bool{}
	for _, ap := range scoped {
		ids[ap.ID] = true
	}
	if !ids[mineAP] {
		t.Errorf("admin, view=user: own approval %s missing", mineAP)
	}
	if ids[foreignAP] {
		t.Errorf("admin, view=user: foreign approval %s leaked through (%d rows total) — the view=user owner force did not apply",
			foreignAP, len(scoped))
	}

	// view=admin from a non-operator is coerced to user (fail closed), never
	// escalated: a member still sees only their own run's approval.
	memberRun := seedRunOwnedBy("sub-f5-member")
	memberAP := aap.seed(memberRun)
	member := ssoSession(t, "sub-f5-member", "f5-member@corp.example", oidc.RoleUser)
	wMember := doSSO(t, srv, http.MethodGet, "/api/v1/approvals?view=admin", member, "")
	if wMember.Code != http.StatusOK {
		t.Fatalf("member, view=admin: code = %d; body=%s", wMember.Code, wMember.Body.String())
	}
	var memberGot []types.ApprovalRequest
	if err := json.Unmarshal(wMember.Body.Bytes(), &memberGot); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(memberGot) != 1 || memberGot[0].ID != memberAP {
		t.Errorf("member, view=admin: got %+v, want only their own approval %s (view=admin must coerce to user for a non-operator)",
			memberGot, memberAP)
	}
}

// TestListApprovals_InvalidView pins the 400 on an unrecognised ?view=.
func TestListApprovals_InvalidView(t *testing.T) {
	h := newHarness(t)
	w := do(t, h.srv, http.MethodGet, "/api/v1/approvals?view=nonsense", adminToken, "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}
