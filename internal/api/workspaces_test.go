// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
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
