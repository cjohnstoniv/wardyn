// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestSeedRequestWorkspace pins the three things `workspace_id` has to get right:
// the named workspace becomes the PRIMARY (PREPENDED, so wsRefs[0] — which drives
// the run's cred binding and image — is this workspace even when the caller also
// passed a policy), a local dir stays read-only unless the workspace itself is
// writable, and a container workspace is REFUSED rather than quietly becoming the
// image behind decodeAndValidateCreateRun's image/builder checks.
func TestSeedRequestWorkspace(t *testing.T) {
	h := newHarness(t)
	seed := func(ws types.Workspace, spec *types.RunPolicySpec, req *createRunRequest) (int, error) {
		return New(baseTestConfig(h, &workspaceStoreFake{ws: ws})).
			seedRequestWorkspace(context.Background(), spec, req)
	}
	id := uuid.New()

	t.Run("local dir prepends a read-only mount", func(t *testing.T) {
		spec := types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: "/srv/from-policy", Target: "/home/agent/other"}}}
		req := createRunRequest{Agent: "claude-code", WorkspaceID: &id}
		if code, err := seed(types.Workspace{ID: id, Kind: types.WorkspaceKindLocalDir, Source: "/srv/app"}, &spec, &req); err != nil {
			t.Fatalf("seed: %d %v", code, err)
		}
		if got := spec.WorkspaceMounts[0]; got.Source != "/srv/app" || got.Target != composerWorkspaceTarget {
			t.Errorf("mount[0] = %+v, want the named workspace prepended at %s", got, composerWorkspaceTarget)
		}
		if ro := spec.WorkspaceMounts[0].ReadOnly; ro == nil || !*ro {
			t.Error("a non-writable workspace must mount read-only")
		}
		if len(spec.WorkspaceMounts) != 2 || spec.WorkspaceMounts[1].Source != "/srv/from-policy" {
			t.Errorf("the caller's own mounts must survive alongside: %+v", spec.WorkspaceMounts)
		}
	})

	t.Run("repo prepends a workspace_repos entry and labels the run", func(t *testing.T) {
		spec := types.RunPolicySpec{}
		req := createRunRequest{Agent: "claude-code", WorkspaceID: &id}
		if code, err := seed(types.Workspace{ID: id, Kind: types.WorkspaceKindRepo, Source: "acme/widgets"}, &spec, &req); err != nil {
			t.Fatalf("seed: %d %v", code, err)
		}
		if len(spec.WorkspaceRepos) != 1 || spec.WorkspaceRepos[0].Repo != "acme/widgets" {
			t.Errorf("workspace_repos = %+v, want the named repo seeded (the onboarding gate + wsRefs read this, never run.Repo)", spec.WorkspaceRepos)
		}
		if req.Repo != "acme/widgets" {
			t.Errorf("req.Repo = %q, want the run row labelled with the workspace source", req.Repo)
		}
	})

	t.Run("container is refused", func(t *testing.T) {
		spec := types.RunPolicySpec{}
		req := createRunRequest{Agent: "claude-code", WorkspaceID: &id}
		code, err := seed(types.Workspace{ID: id, Kind: types.WorkspaceKindContainer, Source: "ghcr.io/acme/base:1"}, &spec, &req)
		if err == nil || code != http.StatusBadRequest {
			t.Fatalf("code = %d, err = %v; want 400 (a container workspace must go through `image`, which is builder-gated)", code, err)
		}
		if req.Image != "" || len(spec.WorkspaceMounts) != 0 {
			t.Error("a refused container workspace must seed nothing")
		}
	})
}
