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
	seed := func(ws types.Workspace, spec *types.RunPolicySpec, req *createRunRequest) ([]string, int, error) {
		return New(baseTestConfig(h, &workspaceStoreFake{ws: ws})).
			seedRequestWorkspace(context.Background(), spec, req)
	}
	localDirWS := func(id uuid.UUID, path string) types.Workspace {
		return types.Workspace{ID: id, Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: path}}}
	}
	id := uuid.New()

	t.Run("local dir prepends a read-only mount", func(t *testing.T) {
		spec := types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: "/srv/from-policy", Target: "/home/agent/other"}}}
		req := createRunRequest{Agent: "claude-code", WorkspaceID: &id}
		if _, code, err := seed(localDirWS(id, "/srv/app"), &spec, &req); err != nil {
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
		repoWS := types.Workspace{ID: id, Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "acme/widgets"}}}
		if _, code, err := seed(repoWS, &spec, &req); err != nil {
			t.Fatalf("seed: %d %v", code, err)
		}
		if len(spec.WorkspaceRepos) != 1 || spec.WorkspaceRepos[0].Repo != "acme/widgets" {
			t.Errorf("workspace_repos = %+v, want the named repo seeded (the onboarding gate + wsRefs read this, never run.Repo)", spec.WorkspaceRepos)
		}
		if req.Repo != "acme/widgets" {
			t.Errorf("req.Repo = %q, want the run row labelled with the workspace source", req.Repo)
		}
	})

	// A migrated container-kind workspace is now an ephemeral source + a custom
	// BaseImage (0029). seedRequestWorkspace itself no longer refuses it — it
	// seeds req.Image from BaseImage exactly as an explicit --image would set
	// it — but the SAME validateImageBuildRequest gate its real callers
	// (handleCreateRun, handlePreflightRun) run immediately afterward still
	// refuses it when no ImageBuilder is wired, so the end-to-end refusal
	// survives, just split across the two functions.
	t.Run("container-shaped workspace seeds req.Image, still gated on the image builder", func(t *testing.T) {
		spec := types.RunPolicySpec{}
		req := createRunRequest{Agent: "claude-code", WorkspaceID: &id}
		srv := New(baseTestConfig(h, &workspaceStoreFake{ws: types.Workspace{
			ID:        id,
			Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
			BaseImage: &types.WorkspaceBaseImage{Kind: "custom", Image: "ghcr.io/acme/base:1"},
		}}))
		if _, code, err := srv.seedRequestWorkspace(context.Background(), &spec, &req); err != nil {
			t.Fatalf("seed: %d %v", code, err)
		}
		if req.Image != "ghcr.io/acme/base:1" {
			t.Fatalf("req.Image = %q, want the workspace's base image seeded (mirrors an explicit --image)", req.Image)
		}
		if len(spec.WorkspaceMounts) != 0 || len(spec.WorkspaceRepos) != 0 {
			t.Error("an ephemeral-only workspace must seed no mounts/repos")
		}
		if msg := srv.validateImageBuildRequest(req); msg == "" {
			t.Fatal("want a refusal message (no ImageBuilder wired) — a container-shaped workspace's image must still go through the builder gate")
		}
	})

	// A multi-source workspace — only possible under the composition model — must
	// seed ONE policy entry per local_dir/repo source, in order, while its
	// ephemeral source contributes NO policy entry (it surfaces only via the
	// returned ephemeralDirs, for the caller to expose as WARDYN_EPHEMERAL_DIRS).
	t.Run("multi-source workspace seeds one entry per source, ephemeral contributes none", func(t *testing.T) {
		spec := types.RunPolicySpec{}
		req := createRunRequest{Agent: "claude-code", WorkspaceID: &id}
		ws := types.Workspace{ID: id, Sources: []types.WorkspaceSource{
			{Type: types.WorkspaceSourceTypeLocalDir, Path: "/srv/one"},
			{Type: types.WorkspaceSourceTypeRepo, Source: "acme/two"},
			{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/scratch"},
		}}
		dirs, code, err := seed(ws, &spec, &req)
		if err != nil {
			t.Fatalf("seed: %d %v", code, err)
		}
		if len(spec.WorkspaceMounts) != 1 || spec.WorkspaceMounts[0].Source != "/srv/one" {
			t.Errorf("want exactly 1 mount for the local_dir source, got %+v", spec.WorkspaceMounts)
		}
		if len(spec.WorkspaceRepos) != 1 || spec.WorkspaceRepos[0].Repo != "acme/two" {
			t.Errorf("want exactly 1 repo entry for the repo source, got %+v", spec.WorkspaceRepos)
		}
		if len(dirs) != 1 || dirs[0] != "/home/agent/scratch" {
			t.Errorf("ephemeral source must surface as an ephemeralDir, never a policy entry: got %v", dirs)
		}
	})

	t.Run("a target collision with the policy's own mounts is 422, not a driver failure", func(t *testing.T) {
		// The policy already binds composerWorkspaceTarget; the workspace (no
		// default_target) would seed a second mount at the same in-container path.
		// The unique-target invariant must catch the seed's own output.
		spec := types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: "/srv/from-policy", Target: composerWorkspaceTarget}}}
		req := createRunRequest{Agent: "claude-code", WorkspaceID: &id}
		_, code, err := seed(localDirWS(id, "/srv/app"), &spec, &req)
		if err == nil || code != http.StatusUnprocessableEntity {
			t.Fatalf("code = %d, err = %v; want 422 on the duplicate in-container target", code, err)
		}
	})
}

// TestResolveWorkspaceImage_ContainerShapedWorkspaceUsesBaseImage covers the
// OTHER image-resolution path a migrated container-kind workspace exercises:
// resolveWorkspaceImage (workspace_run.go), used by the import-pipeline's
// scan/record/verify runs — distinct from TestSeedRequestWorkspace above, which
// pins the ordinary create-run --image/workspace_id seed. An explicit BaseImage
// choice ("custom"/"registry"/"byo") takes precedence over the detected-
// toolchain devcontainer path unconditionally, before any profile is even
// consulted — the one thing the pre-composition-model "container" kind ever
// did, so a migrated row (ephemeral source + BaseImage) must still resolve to
// that same fixed image.
func TestResolveWorkspaceImage_ContainerShapedWorkspaceUsesBaseImage(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, nil)
	cfg.ImageBuilder = fakeImageBuilder{}
	srv := New(cfg)
	ws := types.Workspace{
		ID:        uuid.New(),
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		BaseImage: &types.WorkspaceBaseImage{Kind: "custom", Image: "ghcr.io/acme/base:1"},
		// No Profile: an unscanned ephemeral-only workspace has none, and the
		// BaseImage branch must return before ever needing one.
	}
	image, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws)
	if !ok || image != "ghcr.io/acme/base:1" {
		t.Fatalf("resolveWorkspaceImage = (%q, %v), want the workspace's BaseImage verbatim, unconditionally", image, ok)
	}
}
