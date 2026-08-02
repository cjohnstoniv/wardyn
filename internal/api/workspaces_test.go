// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// These tests exercise the admin-gated workspace routes WITHOUT a Postgres
// pool, mirroring policies_test.go: only paths that fail closed BEFORE any
// store call (auth gating, body/source validation 400s, id parsing 400s) are
// covered here. A happy-path store round-trip needs a real Store and is out of
// scope for this pool-free harness.

func TestWorkspaceRoutesRequireAdminAuth(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/workspaces"},
		{http.MethodPost, "/api/v1/workspaces"},
		{http.MethodGet, "/api/v1/workspaces/" + uuid.New().String()},
		{http.MethodPut, "/api/v1/workspaces/" + uuid.New().String()},
		{http.MethodDelete, "/api/v1/workspaces/" + uuid.New().String()},
		{http.MethodPost, "/api/v1/workspaces/" + uuid.New().String() + "/scan"},
		{http.MethodGet, "/api/v1/workspaces/" + uuid.New().String() + "/env-as-code"},
	}
	for _, c := range cases {
		if w := do(t, h.srv, c.method, c.path, "", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s no token: code = %d, want 401", c.method, c.path, w.Code)
		}
		if w := do(t, h.srv, c.method, c.path, "wrong", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s wrong token: code = %d, want 401", c.method, c.path, w.Code)
		}
	}
}

func TestCreateWorkspaceValidation(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name string
		body string
	}{
		{"invalid json", `{not json`},
		{"missing name", `{"kind":"local_dir","source":"/home/u/repo"}`},
		{"blank name", `{"name":"  ","kind":"local_dir","source":"/home/u/repo"}`},
		{"missing source", `{"name":"w","kind":"local_dir"}`},
		{"unknown kind", `{"name":"w","kind":"weird","source":"/home/u/repo"}`},
		{"unknown field (typo)", `{"name":"w","kind":"local_dir","sourc":"/home/u/repo"}`},
		{"local_dir denied source", `{"name":"w","kind":"local_dir","source":"/etc"}`},
		{"local_dir non-absolute source", `{"name":"w","kind":"local_dir","source":"relative"}`},
		{"repo source with whitespace", `{"name":"w","kind":"repo","source":"org/name; rm -rf"}`},
		{"repo source not a recognized slug/URL", `{"name":"w","kind":"repo","source":"not a repo"}`},
		{"bad default_target", `{"name":"w","kind":"local_dir","source":"/home/u/repo","default_target":"/etc"}`},
	}
	for _, c := range cases {
		w := do(t, h.srv, http.MethodPost, "/api/v1/workspaces", adminToken, c.body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("create %q: code = %d, want 400; body=%s", c.name, w.Code, w.Body.String())
		}
	}
}

func TestUpdateWorkspaceValidation(t *testing.T) {
	h := newHarness(t)
	id := uuid.New().String()
	if w := do(t, h.srv, http.MethodPut, "/api/v1/workspaces/"+id,
		adminToken, `{"name":"w","kind":"weird","source":"/home/u/repo"}`); w.Code != http.StatusBadRequest {
		t.Errorf("update invalid spec: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if w := do(t, h.srv, http.MethodPut, "/api/v1/workspaces/not-a-uuid",
		adminToken, `{"name":"w","kind":"local_dir","source":"/home/u/repo"}`); w.Code != http.StatusBadRequest {
		t.Errorf("update bad id: code = %d, want 400", w.Code)
	}
}

func TestGetDeleteScanWorkspaceBadID(t *testing.T) {
	h := newHarness(t)
	if w := do(t, h.srv, http.MethodGet, "/api/v1/workspaces/not-a-uuid", adminToken, ""); w.Code != http.StatusBadRequest {
		t.Errorf("get bad id: code = %d, want 400", w.Code)
	}
	if w := do(t, h.srv, http.MethodDelete, "/api/v1/workspaces/not-a-uuid", adminToken, ""); w.Code != http.StatusBadRequest {
		t.Errorf("delete bad id: code = %d, want 400", w.Code)
	}
	if w := do(t, h.srv, http.MethodPost, "/api/v1/workspaces/not-a-uuid/scan", adminToken, ""); w.Code != http.StatusBadRequest {
		t.Errorf("scan bad id: code = %d, want 400", w.Code)
	}
}

// ─── single-workspace store fake ─────────────────────────────────────────────

// workspaceStoreFake serves one workspace and captures the row written back.
// GetSiteConfig must be implemented: the env-as-code generator folds in the
// operator's artifact-registry redirects, and the embedded nil store.Store
// would panic there.
type workspaceStoreFake struct {
	store.Store
	ws      types.Workspace
	updated types.Workspace
}

func (s *workspaceStoreFake) GetWorkspace(context.Context, uuid.UUID) (types.Workspace, error) {
	return s.ws, nil
}
func (s *workspaceStoreFake) GetWorkspaceBySource(_ context.Context, kind types.WorkspaceKind, source string) (types.Workspace, error) {
	if kind == s.ws.Kind && source == s.ws.Source {
		return s.ws, nil
	}
	return types.Workspace{}, store.ErrNotFound
}
func (s *workspaceStoreFake) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	return []types.Workspace{s.ws}, nil
}
func (s *workspaceStoreFake) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, nil
}
func (s *workspaceStoreFake) UpdateWorkspace(_ context.Context, _ uuid.UUID, ws types.Workspace) (types.Workspace, error) {
	s.updated = ws
	return ws, nil
}

// TestUpdateWorkspace_ContentChangeClearsEveryReviewedField pins the reset a new
// field is easy to forget (this is exactly how the verified_* stamp survived it):
// everything reviewed against the OLD source — profile, image, approvals,
// recorded evidence and the "PROVEN to install/build/test" stamp — must be gone
// when source/kind/ref changes, since the store UPDATE rewrites every column.
func TestUpdateWorkspace_ContentChangeClearsEveryReviewedField(t *testing.T) {
	h := newHarness(t)
	id := uuid.New()
	at := time.Now().UTC()
	fake := &workspaceStoreFake{ws: types.Workspace{
		ID: id, Name: "w", Kind: types.WorkspaceKindLocalDir, Source: "/home/u/old",
		Status: types.WorkspaceReady, Profile: mustJSON(workspacescan.WorkspaceProfile{Confidence: "high"}),
		ImageRef: "wardyn/ws:abc", BuiltProfileHash: "abc", ApprovedEgress: []string{"example.com"},
		SetupCommands: mustJSON([]workspacescan.SetupCommand{{Stage: "install", Command: "npm ci"}}),
		VerifyResult:  mustJSON(map[string]any{"ok": true}), RecordResults: mustJSON(map[string]any{"t": 1}),
		VerifiedProfileHash: "abc", VerifiedAt: &at,
	}}
	srv := New(baseTestConfig(h, fake))
	w := do(t, srv, http.MethodPut, "/api/v1/workspaces/"+id.String(), adminToken,
		`{"name":"w","kind":"local_dir","source":"/home/u/new"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got := fake.updated
	if got.Profile != nil || got.ImageRef != "" || got.BuiltProfileHash != "" || got.ApprovedEgress != nil ||
		got.SetupCommands != nil || got.VerifyResult != nil || got.RecordResults != nil ||
		got.VerifiedProfileHash != "" || got.VerifiedAt != nil || got.Status != types.WorkspacePendingScan {
		t.Errorf("source change must clear every field reviewed against the old source; got %+v", got)
	}
}

// ─── env-as-code re-fetch ────────────────────────────────────────────────────

// TestGetEnvAsCode_RegeneratesFromProfile pins the re-fetch path: finalize hands
// a repo workspace's committable files back exactly once and writes them
// nowhere, so the GET must reproduce them from stored state (422 while there is
// no profile to generate from).
func TestGetEnvAsCode_RegeneratesFromProfile(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	fake := &workspaceStoreFake{ws: types.Workspace{ID: wsID, Kind: types.WorkspaceKindRepo, Source: "org/repo"}}
	srv := New(baseTestConfig(h, fake))
	path := "/api/v1/workspaces/" + wsID.String() + "/env-as-code"

	if w := do(t, srv, http.MethodGet, path, adminToken, ""); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unscanned: code = %d, want 422; body=%s", w.Code, w.Body.String())
	}

	fake.ws.Profile = mustJSON(workspacescan.WorkspaceProfile{
		Languages: []string{"Go"}, PackageManagers: []string{"go"}, Confidence: "high",
	})
	w := do(t, srv, http.MethodGet, path, adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got struct {
		EmittedFiles map[string]string `json:"emitted_files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.EmittedFiles[".devcontainer/devcontainer.json"] == "" {
		t.Errorf("no devcontainer.json regenerated; got %v", got.EmittedFiles)
	}
}

// ─── writeEnvAsCode containment ──────────────────────────────────────────────

// TestWriteEnvAsCode_RefusesSymlinkEscape pins the containment guarantee the
// finalize step's env-as-code emit depends on. The tree it writes into is
// exactly the tree an in-sandbox (prompt-injectable) agent can write to on a
// Writable local_dir — and a poisoned repo checked out into a local_dir needs no
// Writable at all, since git happily carries symlinks. A lexical
// filepath.Join/HasPrefix check passes for any path whose STRING stays under
// root, so an `AGENTS.md -> <outside>` symlink would be FOLLOWED and truncate an
// operator file (wardynd runs as the operator in host mode). The counterfactual:
// with os.WriteFile restored, this overwrites `outside` and the test fails.
func TestWriteEnvAsCode_RefusesSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "bashrc")
	const sacred = "# the operator's real file"
	if err := os.WriteFile(outside, []byte(sacred), 0o644); err != nil {
		t.Fatal(err)
	}
	// The sandbox plants the symlink before finalize runs.
	if err := os.Symlink(outside, filepath.Join(root, "AGENTS.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := writeEnvAsCode(root, map[string]string{"AGENTS.md": "generated content"})

	if err == nil {
		t.Error("writeEnvAsCode must REFUSE to write through a symlink escaping the workspace")
	}
	got, rerr := os.ReadFile(outside)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != sacred {
		t.Errorf("host file outside the workspace was overwritten through a symlink: got %q, want %q", got, sacred)
	}
}

// TestWriteEnvAsCode_WritesNestedFiles keeps the fix honest: the containment
// guard must not break the normal emit (a nested .devcontainer/ path).
func TestWriteEnvAsCode_WritesNestedFiles(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		".devcontainer/devcontainer.json": `{"name":"x"}`,
		"AGENTS.md":                       "# agents",
	}
	if err := writeEnvAsCode(root, files); err != nil {
		t.Fatalf("writeEnvAsCode: %v", err)
	}
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
}
