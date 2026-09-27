// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// #1197 L1a: GET /runs' opt-in view/owner/status/ended_within/include_killed/
// workspace/q params, and the H-4 fix (view=user forces owner=me for EVERY
// caller, admins and security operators included). Attention projection is
// L1b's — nothing here asserts on it.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ListRunsFiltered/CountHiddenRuns on authzStore, so the existing route-matrix
// fixture (authz_test.go) can drive the new filtered branch. Owner is the only
// predicate exercised here (H-4 is an ownership question); the SQL-level
// predicates (status/ended_within/include_killed/workspace/q/ordering) are
// pinned against real Postgres in store_runs_filtered_pg_test.go, which is
// where they actually run.
func (s *authzStore) ListRunsFiltered(_ context.Context, f store.RunFilter, p store.Page) ([]types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []types.AgentRun{}
	for _, r := range s.runs {
		if f.Owner != "" && r.CreatedBy != f.Owner {
			continue
		}
		out = append(out, r)
	}
	return pagerSlice(out, p), nil
}

func (s *authzStore) CountHiddenRuns(context.Context, store.RunFilter) (int, int, error) {
	return 0, 0, nil
}

var _ store.RunsFilteredPager = (*authzStore)(nil)

// noRunsFilterStore is store.Store with nothing else — genuinely absent
// store.RunsFilteredPager, the shape TestHandleListRuns_FailClosed drives.
type noRunsFilterStore struct{ store.Store }

func getRunsJSON(t *testing.T, w *httptest.ResponseRecorder) []types.AgentRun {
	t.Helper()
	var got []types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode runs: %v (body=%s)", err, w.Body.String())
	}
	return got
}

// TestHandleListRuns_H4_ViewUserForcesOwnerMe is the H-4 pin: every caller
// tier — an admin-token caller, an SSO admin, an SSO security_admin, and an
// SSO member (which was already forced, and must stay so) — sees ONLY their
// own runs under ?view=user&owner=all, even though `owner=all` on its own
// would otherwise mean "everyone" for the two operator tiers. Counterfactual:
// dropping the view=user force from parseRunsListParams (so only the
// pre-existing non-operator branch narrows) must fail the admin/security_admin
// cases here — the authz route matrix cannot see this, since it only checks
// status codes, never which rows came back.
func TestHandleListRuns_H4_ViewUserForcesOwnerMe(t *testing.T) {
	srv, ast, _, _ := newAuthzMatrixServer(t)
	seed := func(createdBy string) uuid.UUID {
		id := uuid.New()
		ast.mu.Lock()
		ast.runs[id] = types.AgentRun{ID: id, CreatedBy: createdBy, State: types.RunRunning, Agent: "claude-code"}
		ast.mu.Unlock()
		return id
	}
	foreign := seed("someone-else")

	cases := []struct {
		name      string
		principal string
		request   func() *httptest.ResponseRecorder
	}{
		{"admin-bearer-token", adminTokenPrincipal, func() *httptest.ResponseRecorder {
			return do(t, srv, http.MethodGet, "/api/v1/runs?view=user&owner=all", adminToken, "")
		}},
		{"sso-admin", "sub-h4-admin", func() *httptest.ResponseRecorder {
			c := ssoSession(t, "sub-h4-admin", "h4-admin@corp.example", oidc.RoleAdmin)
			return doSSO(t, srv, http.MethodGet, "/api/v1/runs?view=user&owner=all", c, "")
		}},
		{"sso-security-admin", "sub-h4-secadmin", func() *httptest.ResponseRecorder {
			c := ssoSession(t, "sub-h4-secadmin", "h4-secadmin@corp.example", oidc.RoleSecurityAdmin)
			return doSSO(t, srv, http.MethodGet, "/api/v1/runs?view=user&owner=all", c, "")
		}},
		{"sso-member", "sub-h4-member", func() *httptest.ResponseRecorder {
			c := ssoSession(t, "sub-h4-member", "h4-member@corp.example", oidc.RoleUser)
			return doSSO(t, srv, http.MethodGet, "/api/v1/runs?view=user&owner=all", c, "")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mine := seed(c.principal)
			defer func() {
				ast.mu.Lock()
				delete(ast.runs, mine)
				ast.mu.Unlock()
			}()
			w := c.request()
			if w.Code != http.StatusOK {
				t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			got := getRunsJSON(t, w)
			ids := map[uuid.UUID]bool{}
			for _, r := range got {
				ids[r.ID] = true
			}
			if !ids[mine] {
				t.Errorf("%s: own run %s missing from view=user&owner=all", c.name, mine)
			}
			if ids[foreign] {
				t.Errorf("%s: foreign run %s leaked through owner=all under view=user (%d rows total) — the H-4 force did not apply",
					c.name, foreign, len(got))
			}
		})
	}
}

// TestHandleListRuns_NoView_OperatorSeesEverything is the control for the test
// above: WITHOUT view=user, an operator's owner=all still means everyone (the
// pre-existing admin scope, now reached through the filtered branch because
// ?owner= alone is one of the opt-in params). Proves the H-4 force is
// view-gated, not an unconditional new narrowing of every operator read.
func TestHandleListRuns_NoView_OperatorSeesEverything(t *testing.T) {
	srv, ast, _, _ := newAuthzMatrixServer(t)
	a, b := uuid.New(), uuid.New()
	ast.mu.Lock()
	ast.runs[a] = types.AgentRun{ID: a, CreatedBy: "op-a", State: types.RunRunning, Agent: "claude-code"}
	ast.runs[b] = types.AgentRun{ID: b, CreatedBy: "op-b", State: types.RunRunning, Agent: "claude-code"}
	ast.mu.Unlock()

	w := do(t, srv, http.MethodGet, "/api/v1/runs?owner=all", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := getRunsJSON(t, w); len(got) != 2 {
		t.Errorf("owner=all with no view = %d row(s), want 2 (both creators)", len(got))
	}
}

// TestHandleListRuns_FailClosed_NoFilteredCapability pins the fail-CLOSED
// posture RunsFilteredPager's own doc promises: a store that cannot answer
// the filtered/scoped read must 500, never silently fall back to the
// unscoped admin listing (which handleListRuns' OLD branch would otherwise
// reach once the member branch above it is skipped).
func TestHandleListRuns_FailClosed_NoFilteredCapability(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, noRunsFilterStore{})
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)

	member := ssoSession(t, "sub-nocap-member", "nocap@corp.example", oidc.RoleUser)
	w := doSSO(t, srv, http.MethodGet, "/api/v1/runs?status=active", member, "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("member, no RunsFilteredPager: code = %d, want 500; body=%s", w.Code, w.Body.String())
	}

	w = do(t, srv, http.MethodGet, "/api/v1/runs?status=active", adminToken, "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("admin, no RunsFilteredPager: code = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleListRuns_NoParams_OldScopeUnchanged proves the branch guard
// itself: no opt-in param present takes handleListRuns' PRE-#1197 body, so a
// store implementing ONLY the old capabilities (RunsByCreatorPager/Pager, not
// RunsFilteredPager) still answers exactly as it always did.
func TestHandleListRuns_NoParams_OldScopeUnchanged(t *testing.T) {
	h := newHarness(t)
	fake := &pagerFake{runs: makeRuns(3)}
	// RunsByCreatorPager, but genuinely no RunsFilteredPager: proves the old
	// path is taken without depending on the new capability at all.
	byCreator := &creatorOnlyFake{pagerFake: fake}
	srv := New(baseTestConfig(h, byCreator))

	w := do(t, srv, http.MethodGet, "/api/v1/runs", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := getRunsJSON(t, w); len(got) != 3 {
		t.Errorf("no-param admin list = %d row(s), want 3 (unfiltered, old ListRunsPage path)", len(got))
	}
}

type creatorOnlyFake struct{ *pagerFake }

func (f *creatorOnlyFake) ListRunsPageByCreator(_ context.Context, createdBy string, p store.Page) ([]types.AgentRun, error) {
	var out []types.AgentRun
	for _, r := range f.runs {
		if r.CreatedBy == createdBy {
			out = append(out, r)
		}
	}
	return pagerSlice(out, p), nil
}

var _ store.RunsByCreatorPager = (*creatorOnlyFake)(nil)

// TestParseRunsListParams_Validation pins the 400 on every malformed opt-in
// value — each one proven red by deleting its case from the switch (the
// default in every switch already writes the 400; there is no separate
// "unreachable" branch to accidentally satisfy this without validating).
func TestParseRunsListParams_Validation(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, noRunsFilterStore{}))
	for _, q := range []string{
		"view=nonsense",
		"owner=nonsense",
		"status=needs", // L1b's, not yet servable — rejected, not silently ignored
		"status=bogus",
		"ended_within=nonsense",
		"include_killed=maybe",
	} {
		t.Run(q, func(t *testing.T) {
			w := do(t, srv, http.MethodGet, "/api/v1/runs?"+q, adminToken, "")
			if w.Code != http.StatusBadRequest {
				t.Errorf("?%s: code = %d, want 400; body=%s", q, w.Code, w.Body.String())
			}
		})
	}
}

// TestHasRunsListFilterParams pins the branch guard: presence of ANY opt-in
// key trips it, ?limit=/?offset= alone (every existing caller) must not.
func TestHasRunsListFilterParams(t *testing.T) {
	cases := []struct {
		query string
		want  bool
	}{
		{"", false},
		{"limit=10&offset=5", false},
		{"view=user", true},
		{"owner=me", true},
		{"status=active", true},
		{"ended_within=24h", true},
		{"include_killed=1", true},
		{"workspace=org/repo", true},
		{"q=refund", true},
	}
	for _, c := range cases {
		q, err := url.ParseQuery(c.query)
		if err != nil {
			t.Fatalf("parse %q: %v", c.query, err)
		}
		if got := hasRunsListFilterParams(q); got != c.want {
			t.Errorf("hasRunsListFilterParams(%q) = %v, want %v", c.query, got, c.want)
		}
	}
}
