// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pushContentScopeJSON is a well-formed push_content requested_scope.
func pushContentScopeJSON(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(types.PushContentScope{
		Repo:        "github.com/octocat/hello-world",
		Branch:      "refs/heads/wardyn/run/work",
		ActsAs:      "github_token:" + uuid.NewString(),
		Paths:       []string{".github/workflows/ci.yml"},
		PathsTotal:  1,
		Commits:     []string{strings.Repeat("a", 40)},
		PathsDigest: strings.Repeat("0", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// seedPushContent adds a PENDING push_content row on the fixture's run.
func (f *scopeFixture) seedPushContent(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.approval.mu.Lock()
	f.approval.byID[id] = types.ApprovalRequest{
		ID: id, RunID: f.runID, Kind: types.ApprovalPushContent,
		RequestedScope: json.RawMessage(pushContentScopeJSON(t)),
		State:          types.ApprovalPending, RequestedAt: time.Now().UTC(),
	}
	f.approval.mu.Unlock()
	return id
}

// TestPushContentMemberCannotDecide: push_content is admin-decidable only. A
// member — the run's own owner included — gets the member gate's existing
// byte-identical 404, on approve and on deny, and the row stays PENDING. A
// member approving their own workflow-file edit is the exfiltration the rule
// stops.
func TestPushContentMemberCannotDecide(t *testing.T) {
	f := newScopeFixture(t)
	owner := ssoSession(t, f.memberID, "member@corp.example", oidc.RoleMember)

	for _, verb := range []string{"approve", "deny"} {
		t.Run(verb, func(t *testing.T) {
			id := f.seedPushContent(t)
			w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/"+verb, owner, "")
			if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "approval not found") {
				t.Fatalf("run owner (member) %s on push_content: status = %d body=%s, want 404 approval not found",
					verb, w.Code, w.Body.String())
			}
			f.approval.mu.Lock()
			got := f.approval.byID[id].State
			f.approval.mu.Unlock()
			if got != types.ApprovalPending {
				t.Errorf("state after a refused member decision = %q, want PENDING", got)
			}
		})
	}
}

// TestPushContentAdminDecides: an admin decides one bodyless, and rule 4
// refuses a decision_scope on it — the sidecar reads an approval as covering
// exactly the commits the scope names, so a scope would mean nothing.
func TestPushContentAdminDecides(t *testing.T) {
	f := newScopeFixture(t)
	admin := ssoSession(t, "sub-admin-push", "admin@corp.example", oidc.RoleAdmin)

	id := f.seedPushContent(t)
	if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve",
		admin, decideBody(t, types.ScopeRun, nil)); w.Code != http.StatusBadRequest {
		t.Fatalf("scope on push_content: status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", admin, ""); w.Code != http.StatusOK {
		t.Fatalf("admin approve: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestInternalPushContentRaise drives the sidecar's raise: an attended run's
// well-formed scope becomes a row; an unattended run's, or a malformed one,
// does not.
func TestInternalPushContentRaise(t *testing.T) {
	h := newHarness(t)
	ast := newAuthzStore()
	aap := newAuthzApprovals(ast)
	cfg := baseTestConfig(h, ast)
	cfg.Approvals = aap
	srv := New(cfg)

	attended, unattended := uuid.New(), uuid.New()
	ast.mu.Lock()
	ast.runs[attended] = types.AgentRun{ID: attended, State: types.RunRunning, Interactive: true}
	ast.runs[unattended] = types.AgentRun{ID: unattended, State: types.RunRunning}
	ast.mu.Unlock()
	raise := func(runID uuid.UUID, scope string) *http.Response {
		w := do(t, srv, http.MethodPost, "/api/v1/internal/approvals", h.mintRunToken(t, runID),
			`{"kind":"push_content","requested_scope":`+scope+`}`)
		return w.Result()
	}
	rows := func() int {
		aap.mu.Lock()
		defer aap.mu.Unlock()
		return len(aap.byID)
	}

	if got := raise(attended, pushContentScopeJSON(t)).StatusCode; got != http.StatusCreated {
		t.Fatalf("attended raise: status = %d, want 201", got)
	}
	if rows() != 1 {
		t.Fatalf("rows = %d after an attended raise, want 1", rows())
	}

	for name, c := range map[string]struct {
		run   uuid.UUID
		scope string
		want  int
	}{
		"unattended run":   {unattended, pushContentScopeJSON(t), http.StatusForbidden},
		"unknown field":    {attended, strings.Replace(pushContentScopeJSON(t), `{`, `{"host":"x",`, 1), http.StatusBadRequest},
		"eleven paths":     {attended, strings.Replace(pushContentScopeJSON(t), `"paths":[".github/workflows/ci.yml"]`, `"paths":["a","b","c","d","e","f","g","h","i","j","k"]`, 1), http.StatusBadRequest},
		"not an object id": {attended, strings.Replace(pushContentScopeJSON(t), strings.Repeat("a", 40), "HEAD", 1), http.StatusBadRequest},
	} {
		if got := raise(c.run, c.scope).StatusCode; got != c.want {
			t.Errorf("%s: status = %d, want %d", name, got, c.want)
		}
	}
	if rows() != 1 {
		t.Errorf("rows = %d, want still 1: a refused raise must write nothing", rows())
	}
}

// TestDispatchStampsUnattended: the sidecar learns a run is unattended from
// dispatch, from the same flag that decides whether anyone drives it.
func TestDispatchStampsUnattended(t *testing.T) {
	for _, interactive := range []bool{true, false} {
		fr := &fakeRunner{}
		srv, _, _, run := dispatchTeardownFixture(t, fr, types.RunPending)
		run.Task = ""
		srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}), dispatchParams{
			RunToken: "run-token", Image: "wardyn/claude-code:latest", Interactive: interactive,
		})
		if got := fr.lastSpec.ProxyConfig.Unattended; got == interactive {
			t.Errorf("Interactive=%v dispatched ProxyConfig.Unattended=%v", interactive, got)
		}
	}
}
