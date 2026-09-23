// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
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

// TestMemberApprovalListByRun is the ?run_id= arm of handleListApprovals for a
// member, which no test had run (every ?run_id= test used the admin token): a
// member naming ANOTHER member's run gets the byte-identical 404 a missing run
// gets and none of its approvals; naming their own run gets exactly its rows.
func TestMemberApprovalListByRun(t *testing.T) {
	srv, ast, aap, _ := newAuthzMatrixServer(t)
	const mine, theirs = "sub-member", "sub-other-member"
	member := ssoSession(t, mine, "member@corp.example", oidc.RoleMember)
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	ownRun, foreignRun := seedAuthzRun(ast, mine), seedAuthzRun(ast, theirs)
	ownAp, foreignAp := aap.seed(ownRun), aap.seed(foreignRun)

	w := doSSO(t, srv, http.MethodGet, "/api/v1/approvals?run_id="+foreignRun.String(), member, "")
	missing := doSSO(t, srv, http.MethodGet, "/api/v1/approvals?run_id="+uuid.NewString(), member, "")
	if w.Code != http.StatusNotFound || w.Body.String() != missing.Body.String() {
		t.Fatalf("member ?run_id=<foreign run>: %d %q, want the 404 a missing run gets (%d %q)",
			w.Code, w.Body.String(), missing.Code, missing.Body.String())
	}
	if strings.Contains(w.Body.String(), foreignAp.String()) {
		t.Fatalf("the refusal carries the foreign approval: %s", w.Body.String())
	}

	if got := approvalIDs(t, doSSO(t, srv, http.MethodGet, "/api/v1/approvals?run_id="+ownRun.String(), member, "")); len(got) != 1 || got[0] != ownAp {
		t.Fatalf("member ?run_id=<own run> = %v, want exactly [%s]", got, ownAp)
	}
	// The control: the same foreign query from an admin answers, so the 404
	// above is the member's refusal and not a fixture that cannot answer.
	if got := approvalIDs(t, doSSO(t, srv, http.MethodGet, "/api/v1/approvals?run_id="+foreignRun.String(), admin, "")); len(got) != 1 || got[0] != foreignAp {
		t.Fatalf("admin ?run_id=<foreign run> = %v, want [%s]", got, foreignAp)
	}
}

// TestMemberListsFailClosedWithoutCreatorScope pins the fail-CLOSED arms of
// GET /runs and GET /approvals: on a backend that cannot scope by creator, a
// member gets a 500 carrying no row — never the unscoped listing, which both
// doubles here can serve (the admin control proves it).
func TestMemberListsFailClosedWithoutCreatorScope(t *testing.T) {
	srv, ast, aap, _ := newAuthzMatrixServer(t)
	// Each wrapper exposes only the base interface, so the creator pager the
	// real double implements is invisible to the handler's type assertion.
	srv.cfg.Store = struct{ store.Store }{ast}
	srv.cfg.Approvals = struct{ ApprovalService }{aap}
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember)
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

	ownRun, foreignRun := seedAuthzRun(ast, "sub-member"), seedAuthzRun(ast, "sub-other-member")
	ownAp, foreignAp := aap.seed(ownRun), aap.seed(foreignRun)
	for _, tc := range []struct {
		path string
		rows []uuid.UUID
	}{
		{"/api/v1/runs", []uuid.UUID{ownRun, foreignRun}},
		{"/api/v1/approvals", []uuid.UUID{ownAp, foreignAp}},
	} {
		w := doSSO(t, srv, http.MethodGet, tc.path, member, "")
		if w.Code != http.StatusInternalServerError {
			t.Errorf("member GET %s on an unscopable backend = %d, want 500; body=%s", tc.path, w.Code, w.Body.String())
		}
		ctl := doSSO(t, srv, http.MethodGet, tc.path, admin, "")
		for _, id := range tc.rows {
			if strings.Contains(w.Body.String(), id.String()) {
				t.Errorf("member GET %s carries %s: %s", tc.path, id, w.Body.String())
			}
			if !strings.Contains(ctl.Body.String(), id.String()) {
				t.Errorf("admin GET %s lacks %s (%d) — the control proves the fall-through would have served it: %s",
					tc.path, id, ctl.Code, ctl.Body.String())
			}
		}
	}
}

func seedAuthzRun(ast *authzStore, createdBy string) uuid.UUID {
	id := uuid.New()
	ast.mu.Lock()
	ast.runs[id] = types.AgentRun{ID: id, CreatedBy: createdBy, State: types.RunRunning, Agent: "claude-code"}
	ast.mu.Unlock()
	return id
}

func approvalIDs(t *testing.T, w *httptest.ResponseRecorder) []uuid.UUID {
	t.Helper()
	res := w.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", res.StatusCode)
	}
	var got []types.ApprovalRequest
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	ids := make([]uuid.UUID, 0, len(got))
	for _, ap := range got {
		ids = append(ids, ap.ID)
	}
	return ids
}
