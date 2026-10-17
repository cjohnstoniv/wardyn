// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// POST /workspaces/{id}/env-as-code/write: the refusals that come before any
// disk write, then the write itself and what it leaves alone.
func TestHandleWriteEnvAsCode(t *testing.T) {
	profile := mustJSON(workspacescan.WorkspaceProfile{Languages: []string{"Go"}, PackageManagers: []string{"go"}, Confidence: "high"})
	localDirWS := func(dir string) types.Workspace {
		return types.Workspace{
			ID: uuid.New(), Kind: types.WorkspaceKindLocalDir, Source: dir, Profile: profile,
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: dir, Target: "/home/agent/work"}},
		}
	}
	write := func(t *testing.T, ws types.Workspace) (*harness, *Server, string) {
		t.Helper()
		h := newHarness(t)
		srv := New(baseTestConfig(h, &workspaceStoreFake{ws: ws}))
		return h, srv, "/api/v1/workspaces/" + ws.ID.String() + "/env-as-code/write"
	}

	t.Run("malformed id", func(t *testing.T) {
		srv := New(baseTestConfig(newHarness(t), &workspaceStoreFake{}))
		if w := do(t, srv, http.MethodPost, "/api/v1/workspaces/nope/env-as-code/write", adminToken, ""); w.Code != http.StatusBadRequest || reasonOf(t, w) != reasonInvalidIDParam {
			t.Errorf("= %d %s, want 400 %s", w.Code, w.Body.String(), reasonInvalidIDParam)
		}
	})
	t.Run("a repo-only workspace has no host path", func(t *testing.T) {
		ws := types.Workspace{ID: uuid.New(), Kind: types.WorkspaceKindRepo, Source: "org/repo", Profile: profile}
		_, srv, path := write(t, ws)
		if w := do(t, srv, http.MethodPost, path, adminToken, ""); w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonWorkspaceEnvcodeNoLocalDir {
			t.Errorf("= %d %s, want 422 %s", w.Code, w.Body.String(), reasonWorkspaceEnvcodeNoLocalDir)
		}
	})
	t.Run("an unscanned workspace has nothing to emit", func(t *testing.T) {
		ws := localDirWS(t.TempDir())
		ws.Profile = nil
		_, srv, path := write(t, ws)
		if w := do(t, srv, http.MethodPost, path, adminToken, ""); w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonWorkspaceEnvcodeNoProfile {
			t.Errorf("= %d %s, want 422 %s", w.Code, w.Body.String(), reasonWorkspaceEnvcodeNoProfile)
		}
	})
	t.Run("a source directory that is gone is a server error and writes nothing", func(t *testing.T) {
		h, srv, path := write(t, localDirWS(filepath.Join(t.TempDir(), "gone")))
		if w := do(t, srv, http.MethodPost, path, adminToken, ""); w.Code != http.StatusInternalServerError {
			t.Errorf("= %d %s, want 500", w.Code, w.Body.String())
		}
		if got := h.auditOutcomes("workspace.envcode.write"); len(got) != 0 {
			t.Errorf("audit rows %v, want none for a failed write", got)
		}
	})
	t.Run("writes the emitted files, keeps an operator Dockerfile and says so", func(t *testing.T) {
		dir := t.TempDir()
		const operatorDockerfile = "FROM my-own-base:latest\n"
		dockerfile := filepath.Join(dir, filepath.FromSlash(workspacescan.EnvAsCodeDockerfilePath))
		if err := os.MkdirAll(filepath.Dir(dockerfile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dockerfile, []byte(operatorDockerfile), 0o644); err != nil {
			t.Fatal(err)
		}
		h, srv, path := write(t, localDirWS(dir))
		w := do(t, srv, http.MethodPost, path, adminToken, "")
		if w.Code != http.StatusOK {
			t.Fatalf("= %d %s, want 200", w.Code, w.Body.String())
		}
		var got struct {
			Written map[string]string `json:"written_files"`
			Skipped []string          `json:"skipped_files"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Skipped) != 1 || got.Skipped[0] != workspacescan.EnvAsCodeDockerfilePath {
			t.Errorf("skipped = %v, want only the operator's Dockerfile", got.Skipped)
		}
		if _, listed := got.Written[workspacescan.EnvAsCodeDockerfilePath]; listed {
			t.Error("written_files lists the Dockerfile that was skipped")
		}
		if kept, _ := os.ReadFile(dockerfile); string(kept) != operatorDockerfile {
			t.Errorf("the operator's Dockerfile was changed to %q", kept)
		}
		if len(got.Written) == 0 {
			t.Fatal("nothing was written")
		}
		for rel, want := range got.Written {
			if onDisk, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))); err != nil || string(onDisk) != want {
				t.Errorf("%s on disk = %q (err %v), want the reported content", rel, onDisk, err)
			}
		}
		if outcomes := h.auditOutcomes("workspace.envcode.write"); len(outcomes) != 1 || outcomes[0] != "success" {
			t.Errorf("workspace.envcode.write audit outcomes = %v, want [success]", outcomes)
		}
	})
}
