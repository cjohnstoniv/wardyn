// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// This file covers Task 1 — PUT /workspaces/{id}/requirements — plus the pure
// key-grammar helpers (splitRequirementKey, validateWorkspaceRequirement) the
// endpoint and the fold in runs_create.go both consult.

// ─── splitRequirementKey / validateWorkspaceRequirement (pure helpers) ──────

func TestSplitRequirementKey(t *testing.T) {
	cases := []struct {
		key      string
		wantTyp  string
		wantRest string
		wantOK   bool
	}{
		{"secret:acme-key", "secret", "acme-key", true},
		{"egress:api.github.com", "egress", "api.github.com", true},
		{"write:/home/user/repo", "write", "/home/user/repo", true},
		// FIRST colon only — a write path may itself legally contain a colon.
		{"write:/home/user:repo", "write", "/home/user:repo", true},
		{"nocolon", "", "", false},
		{"secret:", "", "", false},   // empty suffix
		{"unknown:x", "", "", false}, // not one of the three known tokens
		{"", "", "", false},
	}
	for _, c := range cases {
		typ, rest, ok := splitRequirementKey(c.key)
		if typ != c.wantTyp || rest != c.wantRest || ok != c.wantOK {
			t.Errorf("splitRequirementKey(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.key, typ, rest, ok, c.wantTyp, c.wantRest, c.wantOK)
		}
	}
}

func TestValidateWorkspaceRequirement(t *testing.T) {
	valid := types.WorkspaceRequirement{Level: "required", Provenance: "operator_set"}
	cases := []struct {
		name string
		key  string
		req  types.WorkspaceRequirement
		ok   bool
	}{
		{"valid secret", "secret:acme-key", valid, true},
		{"valid egress", "egress:api.github.com", valid, true},
		{"valid write", "write:/home/user/repo", valid, true},
		{"valid optional/scan_seeded", "secret:acme-key", types.WorkspaceRequirement{Level: "optional", Provenance: "scan_seeded"}, true},
		{"bad key grammar", "nocolon", valid, false},
		{"unknown type prefix", "env:FOO", valid, false},
		{"bad secret name (uppercase+space)", "secret:BAD NAME", valid, false},
		{"reserved secret name", "secret:wardyn-signing-key", valid, false},
		{"bad egress (wildcard)", "egress:*.example.com", valid, false},
		{"bad egress (scheme)", "egress:https://example.com", valid, false},
		{"bad egress (uppercase)", "egress:Example.com", valid, false},
		{"bad write (relative)", "write:relative/path", valid, false},
		{"bad write (denied prefix)", "write:/etc/passwd", valid, false},
		{"bad level", "secret:acme-key", types.WorkspaceRequirement{Level: "nope", Provenance: "operator_set"}, false},
		{"bad provenance", "secret:acme-key", types.WorkspaceRequirement{Level: "required", Provenance: "nope"}, false},
	}
	for _, c := range cases {
		if msg := validateWorkspaceRequirement(c.key, c.req); (msg == "") != c.ok {
			t.Errorf("%s: validateWorkspaceRequirement(%q, %+v) = %q, want ok=%v", c.name, c.key, c.req, msg, c.ok)
		}
	}
}

// ─── HTTP-level: auth gating ─────────────────────────────────────────────────

func TestWorkspaceRequirementsRouteRequiresAdminAuth(t *testing.T) {
	h := newHarness(t)
	path := "/api/v1/workspaces/" + uuid.New().String() + "/requirements"
	body := `{"requirements":{}}`
	if w := do(t, h.srv, http.MethodPut, path, "", body); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: code = %d, want 401", w.Code)
	}
	if w := do(t, h.srv, http.MethodPut, path, "wrong", body); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: code = %d, want 401", w.Code)
	}
}

// ─── HTTP-level: validation (fails BEFORE any store call, so a no-Store
// harness is sufficient — parseIDParam only needs a well-formed UUID). ───────

func TestSetWorkspaceRequirements_Validation(t *testing.T) {
	h := newHarness(t)
	path := "/api/v1/workspaces/" + uuid.New().String() + "/requirements"
	cases := []struct {
		name, body string
	}{
		{"invalid json", `{not json`},
		{"unknown field (typo)", `{"requirement":{}}`},
		{"no colon in key", `{"requirements":{"secretfoo":{"level":"required","provenance":"operator_set"}}}`},
		{"unknown type prefix", `{"requirements":{"env:FOO":{"level":"required","provenance":"operator_set"}}}`},
		{"empty suffix", `{"requirements":{"secret:":{"level":"required","provenance":"operator_set"}}}`},
		{"invalid secret name (uppercase)", `{"requirements":{"secret:BAD":{"level":"required","provenance":"operator_set"}}}`},
		{"reserved secret name", `{"requirements":{"secret:wardyn-signing-key":{"level":"required","provenance":"operator_set"}}}`},
		{"invalid egress host (wildcard)", `{"requirements":{"egress:*.example.com":{"level":"required","provenance":"operator_set"}}}`},
		{"invalid egress host (scheme)", `{"requirements":{"egress:https://example.com":{"level":"required","provenance":"operator_set"}}}`},
		{"invalid write path (relative)", `{"requirements":{"write:relative/path":{"level":"required","provenance":"operator_set"}}}`},
		{"invalid write path (denied prefix)", `{"requirements":{"write:/etc/passwd":{"level":"required","provenance":"operator_set"}}}`},
		{"invalid level", `{"requirements":{"secret:ok":{"level":"nope","provenance":"operator_set"}}}`},
		{"invalid provenance", `{"requirements":{"secret:ok":{"level":"required","provenance":"nope"}}}`},
	}
	for _, c := range cases {
		w := do(t, h.srv, http.MethodPut, path, adminToken, c.body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400; body=%s", c.name, w.Code, w.Body.String())
		}
	}
}

func TestSetWorkspaceRequirements_TooManyEntriesRejected(t *testing.T) {
	h := newHarness(t)
	reqs := map[string]map[string]string{}
	for i := 0; i < maxWorkspaceRequirements+1; i++ {
		reqs[fmt.Sprintf("secret:req-%d", i)] = map[string]string{"level": "required", "provenance": "operator_set"}
	}
	body, err := json.Marshal(map[string]any{"requirements": reqs})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/workspaces/" + uuid.New().String() + "/requirements"
	w := do(t, h.srv, http.MethodPut, path, adminToken, string(body))
	if w.Code != http.StatusBadRequest {
		t.Errorf("over-cap requirements: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestSetWorkspaceRequirements_BadID(t *testing.T) {
	h := newHarness(t)
	w := do(t, h.srv, http.MethodPut, "/api/v1/workspaces/not-a-uuid/requirements", adminToken, `{"requirements":{}}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad id: code = %d, want 400", w.Code)
	}
}

// ─── HTTP-level: happy path ──────────────────────────────────────────────────

// requirementsStoreFake serves one workspace and captures what
// SetWorkspaceRequirements was called with — a narrower sibling of
// workspaceStoreFake (workspaces_test.go), which does not implement this
// scoped setter.
type requirementsStoreFake struct {
	store.Store
	ws  types.Workspace
	set map[string]types.WorkspaceRequirement
}

func (s *requirementsStoreFake) GetWorkspace(context.Context, uuid.UUID) (types.Workspace, error) {
	return s.ws, nil
}

func (s *requirementsStoreFake) SetWorkspaceRequirements(_ context.Context, _ uuid.UUID, reqs map[string]types.WorkspaceRequirement) (types.Workspace, error) {
	s.set = reqs
	s.ws.Requirements = reqs
	return s.ws, nil
}

func TestSetWorkspaceRequirements_HappyPath(t *testing.T) {
	h := newHarness(t)
	id := uuid.New()
	fake := &requirementsStoreFake{ws: types.Workspace{ID: id, Name: "w"}}
	srv := New(baseTestConfig(h, fake))
	body := `{"requirements":{` +
		`"secret:acme-key":{"level":"required","provenance":"operator_set"},` +
		`"egress:api.stripe.com":{"level":"optional","provenance":"operator_set"},` +
		`"write:/srv/app":{"level":"required","provenance":"scan_seeded"}}}`
	w := do(t, srv, http.MethodPut, "/api/v1/workspaces/"+id.String()+"/requirements", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(fake.set) != 3 {
		t.Fatalf("stored requirements = %+v, want 3 entries", fake.set)
	}
	if got := fake.set["secret:acme-key"]; got.Level != "required" || got.Provenance != "operator_set" {
		t.Errorf("secret:acme-key = %+v", got)
	}
	if got := fake.set["write:/srv/app"]; got.Level != "required" || got.Provenance != "scan_seeded" {
		t.Errorf("write:/srv/app = %+v", got)
	}
	var got types.Workspace
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Requirements) != 3 {
		t.Errorf("response requirements = %+v, want 3 entries", got.Requirements)
	}
	found := false
	for _, ev := range h.audit.events {
		if ev.Action == "workspace.requirements.write" {
			found = true
		}
	}
	if !found {
		t.Error("want a workspace.requirements.write audit event")
	}
}

func TestSetWorkspaceRequirements_UnknownWorkspaceIs404(t *testing.T) {
	h := newHarness(t)
	fake := &requirementsStoreFake{}
	// The embedded nil store.Store surfaces as a generic error unless
	// SetWorkspaceRequirements itself reports store.ErrNotFound; mirror
	// handleSetApprovedEgress's sibling test shape by overriding it here.
	srv := New(baseTestConfig(h, notFoundRequirementsStore{fake}))
	w := do(t, srv, http.MethodPut, "/api/v1/workspaces/"+uuid.New().String()+"/requirements",
		adminToken, `{"requirements":{}}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// notFoundRequirementsStore makes SetWorkspaceRequirements report
// store.ErrNotFound, for the 404 test above.
type notFoundRequirementsStore struct{ *requirementsStoreFake }

func (n notFoundRequirementsStore) SetWorkspaceRequirements(context.Context, uuid.UUID, map[string]types.WorkspaceRequirement) (types.Workspace, error) {
	return types.Workspace{}, store.ErrNotFound
}
