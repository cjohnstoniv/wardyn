// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

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
	}
	for _, c := range cases {
		if w := do(t, h.srv, http.MethodPut, c.path, adminToken, c.body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400; body=%s", c.name, w.Code, w.Body.String())
		}
	}

	// Over the cap: 65 valid domains.
	doms := make([]string, 65)
	for i := range doms {
		doms[i] = "h" + itoaTest(i) + ".example.com"
	}
	body, _ := json.Marshal(map[string]any{"domains": doms})
	if w := do(t, h.srv, http.MethodPut, "/api/v1/workspaces/"+id+"/approved-egress", adminToken, string(body)); w.Code != http.StatusBadRequest {
		t.Errorf("over cap: code = %d, want 400", w.Code)
	}
}

func itoaTest(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// TestSetApprovedEgressRoundTrip: the handler normalizes (lowercase, dedupe,
// sort), persists OUTSIDE the profile blob, preserves the scan-owned fields,
// and returns the FULL updated workspace (the UI adopts the response verbatim).
func TestSetApprovedEgressRoundTrip(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	profile := mustJSON(workspacescan.WorkspaceProfile{SuggestedEgress: []string{"ghcr.io"}, Confidence: "high", Source: "deterministic"})
	fake := &approvedEgressStore{ws: types.Workspace{
		ID: wsID, Name: "w", Kind: types.WorkspaceKindLocalDir, Source: "/home/u/repo",
		Profile: profile, Status: types.WorkspaceReady,
	}}
	srv := New(Config{
		Identity:        h.idp,
		Audit:           h.audit,
		AdminToken:      adminToken,
		TrustDomain:     "wardyn.local",
		ControlPlaneURL: "http://wardynd:8080",
		Store:           fake,
	})

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
	if fake.updated.Status != types.WorkspaceReady || string(fake.updated.Profile) != string(profile) {
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
		ID: uuid.New(), Name: "w", Kind: types.WorkspaceKindLocalDir, Source: "/w",
		Status:         types.WorkspaceReady,
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
			ID: wsID, Name: "w", Kind: types.WorkspaceKindLocalDir, Source: "/home/u/repo",
			Status:         types.WorkspaceReady,
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
	srv := New(Config{
		Identity: h.idp, Audit: h.audit, AdminToken: adminToken,
		TrustDomain: "wardyn.local", ControlPlaneURL: "http://wardynd:8080", Store: fake,
	})
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

// setupCmdStore captures the operator-approved setup-commands write.
type setupCmdStore struct {
	store.Store
	ws      types.Workspace
	updated json.RawMessage
}

func (s *setupCmdStore) SetWorkspaceSetupCommands(_ context.Context, _ uuid.UUID, cmds json.RawMessage) (types.Workspace, error) {
	s.updated = cmds
	ws := s.ws
	ws.SetupCommands = cmds
	return ws, nil
}

func TestSetSetupCommands(t *testing.T) {
	h := newHarness(t)
	id := uuid.New().String()
	// Validation rejects: unknown stage, empty command, newline injection, over-cap.
	bad := []string{
		`{"commands":[{"stage":"deploy","command":"x"}]}`,          // unknown stage
		`{"commands":[{"stage":"build","command":""}]}`,            // empty
		`{"commands":[{"stage":"build","command":"a\nrm -rf /"}]}`, // newline
		`{"command":[]}`, // unknown field
	}
	for _, b := range bad {
		if w := do(t, h.srv, http.MethodPut, "/api/v1/workspaces/"+id+"/setup-commands", adminToken, b); w.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for %s, got %d", b, w.Code)
		}
	}

	wsID := uuid.New()
	fake := &setupCmdStore{ws: types.Workspace{ID: wsID, Name: "w", Kind: types.WorkspaceKindLocalDir, Source: "/w", Status: types.WorkspaceScanned}}
	srv := New(Config{Identity: h.idp, Audit: h.audit, AdminToken: adminToken, TrustDomain: "wardyn.local", ControlPlaneURL: "http://wardynd:8080", Store: fake})
	w := do(t, srv, http.MethodPut, "/api/v1/workspaces/"+wsID.String()+"/setup-commands",
		adminToken, `{"commands":[{"stage":"install","command":"pnpm install --frozen-lockfile","source":"convention:node"},{"stage":"test","command":"pnpm run test"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got []workspacescan.SetupCommand
	if err := json.Unmarshal(fake.updated, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Stage != "install" || got[1].Command != "pnpm run test" {
		t.Errorf("persisted setup commands = %+v", got)
	}
}
