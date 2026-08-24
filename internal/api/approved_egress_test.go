// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// approvedEgressStore is a minimal store.Store for the approved-egress
// handler: SetWorkspaceApprovedEgress applies the scoped write to the fixture
// (mirroring the real single-column UPDATE — scan-owned fields untouched by
// construction). Every other method would panic (embedded nil interface).
type approvedEgressStore struct {
	store.Store
	ws      types.Workspace
	updated *types.Workspace
}

func (s *approvedEgressStore) SetWorkspaceApprovedEgress(_ context.Context, _ uuid.UUID, domains []string) (types.Workspace, error) {
	ws := s.ws
	ws.ApprovedEgress = domains
	s.updated = &ws
	return ws, nil
}

func TestSetApprovedEgressValidation(t *testing.T) {
	h := newHarness(t)
	id := uuid.New().String()
	cases := []struct {
		name, path, body string
	}{
		{"bad id", "/api/v1/workspaces/not-a-uuid/approved-egress", `{"domains":[]}`},
		{"invalid json", "/api/v1/workspaces/" + id + "/approved-egress", `{not json`},
		{"unknown field", "/api/v1/workspaces/" + id + "/approved-egress", `{"domain":["x.io"]}`},
		{"scheme", "/api/v1/workspaces/" + id + "/approved-egress", `{"domains":["https://ghcr.io"]}`},
		{"port", "/api/v1/workspaces/" + id + "/approved-egress", `{"domains":["ghcr.io:443"]}`},
		{"wildcard", "/api/v1/workspaces/" + id + "/approved-egress", `{"domains":["*.ghcr.io"]}`},
		{"bare word (no dot)", "/api/v1/workspaces/" + id + "/approved-egress", `{"domains":["postgres"]}`},
		{"path", "/api/v1/workspaces/" + id + "/approved-egress", `{"domains":["ghcr.io/acme"]}`},
		// W19-W19b-3: a git-broker-managed forge host, an SSH-over-443
		// forge host, and the control plane's own host are all routed
		// SPECIALLY at dispatch — never as a plain ApprovedEgress entry — so
		// approving one here would succeed with a toast and be dead by
		// construction on every future run. Reject it explicitly instead.
		{"git-broker-managed host", "/api/v1/workspaces/" + id + "/approved-egress", `{"domains":["github.com"]}`},
		{"git-broker-managed host (api)", "/api/v1/workspaces/" + id + "/approved-egress", `{"domains":["api.github.com"]}`},
	}
	for _, c := range cases {
		if w := do(t, h.srv, http.MethodPut, c.path, adminToken, c.body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400; body=%s", c.name, w.Code, w.Body.String())
		}
	}

	// Over the cap: 65 valid domains.
	doms := make([]string, 65)
	for i := range doms {
		doms[i] = "h" + strconv.Itoa(i) + ".example.com"
	}
	body, _ := json.Marshal(map[string]any{"domains": doms})
	if w := do(t, h.srv, http.MethodPut, "/api/v1/workspaces/"+id+"/approved-egress", adminToken, string(body)); w.Code != http.StatusBadRequest {
		t.Errorf("over cap: code = %d, want 400", w.Code)
	}
}

// TestSetApprovedEgressRoundTrip: the handler normalizes (lowercase, dedupe,
// sort), persists OUTSIDE the profile blob, preserves the scan-owned fields,
// and returns the FULL updated workspace (the UI adopts the response verbatim).
func TestSetApprovedEgressRoundTrip(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	profile := mustJSON(workspacescan.WorkspaceProfile{SuggestedEgress: []string{"ghcr.io"}, Confidence: "high", Source: "deterministic"})
	fake := &approvedEgressStore{ws: types.Workspace{
		ID: wsID, Name: "w",
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/home/u/repo", Target: "/home/agent/work"}},
		Profile: profile, Status: types.WorkspaceScanned,
	}}
	srv := New(baseTestConfig(h, fake))

	w := do(t, srv, http.MethodPut, "/api/v1/workspaces/"+wsID.String()+"/approved-egress",
		adminToken, `{"domains":["GHCR.io","docker.io","ghcr.io"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got types.Workspace
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"docker.io", "ghcr.io"}
	if strings.Join(got.ApprovedEgress, ",") != strings.Join(want, ",") {
		t.Errorf("response ApprovedEgress = %v, want %v", got.ApprovedEgress, want)
	}
	if fake.updated == nil || strings.Join(fake.updated.ApprovedEgress, ",") != strings.Join(want, ",") {
		t.Errorf("persisted ApprovedEgress = %+v, want %v", fake.updated, want)
	}
	// Approval must NOT touch the scan-owned fields.
	if fake.updated.Status != types.WorkspaceScanned || string(fake.updated.Profile) != string(profile) {
		t.Errorf("approval mutated scan-owned fields: %+v", fake.updated)
	}

	// Full-replacement semantics: an empty list un-approves everything.
	fake.ws.ApprovedEgress = want
	if w := do(t, srv, http.MethodPut, "/api/v1/workspaces/"+wsID.String()+"/approved-egress",
		adminToken, `{"domains":[]}`); w.Code != http.StatusOK {
		t.Fatalf("empty replacement: code = %d; body=%s", w.Code, w.Body.String())
	}
	if len(fake.updated.ApprovedEgress) != 0 {
		t.Errorf("empty replacement persisted %v, want none", fake.updated.ApprovedEgress)
	}
}

// TestUnionWorkspaceEgress_ApprovedYesSuggestedNever is the trust boundary in
// one test: profile EgressDomains (filename-keyed) and operator ApprovedEgress
// union into the run allowlist; content-derived SuggestedEgress NEVER does.
func TestUnionWorkspaceEgress_ApprovedYesSuggestedNever(t *testing.T) {
	ws := types.Workspace{
		ID: uuid.New(), Name: "w",
		Sources:        []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/w", Target: "/home/agent/work"}},
		Status:         types.WorkspaceScanned,
		ApprovedEgress: []string{"ghcr.io"},
		Profile: mustJSON(workspacescan.WorkspaceProfile{
			EgressDomains:   []string{"registry.npmjs.org"},
			SuggestedEgress: []string{"evil.example.com"},
			Confidence:      "high", Source: "deterministic",
		}),
	}
	spec := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}
	added := unionWorkspaceEgress(&spec, []types.Workspace{ws})
	got := strings.Join(spec.AllowedDomains, ",")
	if got != "api.anthropic.com,registry.npmjs.org,ghcr.io" {
		t.Errorf("AllowedDomains = %v", spec.AllowedDomains)
	}
	if strings.Join(added, ",") != "registry.npmjs.org,ghcr.io" {
		t.Errorf("added = %v", added)
	}
	for _, d := range spec.AllowedDomains {
		if d == "evil.example.com" {
			t.Fatal("content-derived SuggestedEgress leaked into AllowedDomains")
		}
	}
}

// observedEgressStore serves a fixed workspace, run list, and per-run audit
// events for the observed-egress telemetry handler.
type observedEgressStore struct {
	store.Store
	ws     types.Workspace
	runs   []types.AgentRun
	events map[uuid.UUID][]types.AuditEvent
}

func (s *observedEgressStore) GetWorkspace(context.Context, uuid.UUID) (types.Workspace, error) {
	return s.ws, nil
}
func (s *observedEgressStore) ListRuns(context.Context) ([]types.AgentRun, error) { return s.runs, nil }
func (s *observedEgressStore) QueryAuditEvents(_ context.Context, runID uuid.UUID, _ int) ([]types.AuditEvent, error) {
	return s.events[runID], nil
}

// Observed egress = denied hosts from runs that used the workspace, minus what
// the workspace already allows/approved, promotable by the operator.
func TestObservedEgress(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	runA := uuid.New()
	runOther := uuid.New()
	fake := &observedEgressStore{
		ws: types.Workspace{
			ID: wsID, Name: "w",
			Sources:        []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/home/u/repo", Target: "/home/agent/work"}},
			Status:         types.WorkspaceScanned,
			ApprovedEgress: []string{"already.example.com"},
			Profile: mustJSON(workspacescan.WorkspaceProfile{
				EgressDomains: []string{"registry.npmjs.org"}, Confidence: "high", Source: "deterministic",
			}),
		},
		runs: []types.AgentRun{
			{ID: runA, WorkspacePath: "/home/u/repo"},      // uses the workspace
			{ID: runOther, WorkspacePath: "/home/u/other"}, // does not
		},
		events: map[uuid.UUID][]types.AuditEvent{
			runA: {
				{Action: "egress.deny", Target: "feeds.datagolf.com"},
				{Action: "egress.deny", Target: "registry.npmjs.org"},   // already allowed → excluded
				{Action: "egress.deny", Target: "already.example.com"},  // already approved → excluded
				{Action: "egress.allow", Target: "api.anthropic.com"},   // not a deny → excluded
				{Action: "egress.deny", Target: "https://bad-scheme/x"}, // invalid host → excluded
			},
			runOther: {{Action: "egress.deny", Target: "should-not-appear.example.com"}},
		},
	}
	srv := New(baseTestConfig(h, fake))
	w := do(t, srv, http.MethodGet, "/api/v1/workspaces/"+wsID.String()+"/observed-egress", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Denied       []string `json:"denied"`
		RunsExamined int      `json:"runs_examined"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Denied, ",") != "feeds.datagolf.com" {
		t.Errorf("denied = %v, want [feeds.datagolf.com] (allowed/approved/allow/invalid all excluded)", got.Denied)
	}
	if got.RunsExamined != 1 {
		t.Errorf("runs_examined = %d, want 1 (only the run using this workspace)", got.RunsExamined)
	}
}

// W25-S1-2: the observed-egress route is member-reachable, and every host it
// returns is another run's audit-trail telemetry. A member must see only the
// runs they created — the same owner-or-admin line /runs/{id} draws — while an
// admin still sees the whole workspace's telemetry for the promote flow.
func TestObservedEgress_MemberSeesOnlyOwnRuns(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	mine, theirs := uuid.New(), uuid.New()
	fake := &observedEgressStore{
		ws: types.Workspace{
			ID: wsID, Name: "shared",
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/srv/shared", Target: "/home/agent/work"}},
			Status:  types.WorkspaceScanned,
		},
		runs: []types.AgentRun{
			{ID: mine, WorkspacePath: "/srv/shared", CreatedBy: "sub-bob"},
			{ID: theirs, WorkspacePath: "/srv/shared", CreatedBy: "sub-alice"},
		},
		events: map[uuid.UUID][]types.AuditEvent{
			mine:   {{Action: "egress.deny", Target: "bobs-host.example.com"}},
			theirs: {{Action: "egress.deny", Target: "alices-host.example.com"}},
		},
	}
	srv := New(baseTestConfig(h, fake))

	call := func(ctxMod func(context.Context) context.Context) (denied []string, examined int) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/"+wsID.String()+"/observed-egress", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", wsID.String())
		ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
		if ctxMod != nil {
			ctx = ctxMod(ctx)
		}
		w := httptest.NewRecorder()
		srv.handleObservedEgress(w, r.WithContext(ctx))
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		var got struct {
			Denied       []string `json:"denied"`
			RunsExamined int      `json:"runs_examined"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got.Denied, got.RunsExamined
	}

	denied, examined := call(func(ctx context.Context) context.Context {
		return withOIDCRole(withOIDCHuman(ctx, "sub-bob"), oidc.RoleMember)
	})
	if strings.Join(denied, ",") != "bobs-host.example.com" {
		t.Errorf("member denied = %v, want only their own run's host", denied)
	}
	if examined != 1 {
		t.Errorf("member runs_examined = %d, want 1", examined)
	}

	denied, examined = call(nil) // no OIDC session on ctx == operator
	if strings.Join(denied, ",") != "alices-host.example.com,bobs-host.example.com" {
		t.Errorf("admin denied = %v, want both runs' hosts", denied)
	}
	if examined != 2 {
		t.Errorf("admin runs_examined = %d, want 2", examined)
	}
}
