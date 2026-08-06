// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
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
	image, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil)
	if !ok || image != "ghcr.io/acme/base:1" {
		t.Fatalf("resolveWorkspaceImage = (%q, %v), want the workspace's BaseImage verbatim, unconditionally", image, ok)
	}
}

// resolveImageStoreFake is a minimal store.Store for resolveWorkspaceImage's
// generate-devcontainer path: GetSiteConfig (integration resolution, via
// namedIntegrationTypes/resolveIntegrationRef) and SetWorkspaceBuiltImage
// (the cache write on a successful build) — every other method panics if a
// test here ever starts needing it (embedded interface, nil by default).
type resolveImageStoreFake struct {
	store.Store
	sc types.SiteConfig
}

func (s *resolveImageStoreFake) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.sc, nil
}
func (s *resolveImageStoreFake) SetWorkspaceBuiltImage(context.Context, uuid.UUID, string, string) (types.Workspace, error) {
	return types.Workspace{}, nil
}

// capturedBuild is one BuildFromDevcontainerFiles call capturingImageBuilder
// recorded.
type capturedBuild struct {
	files map[string]string
	tag   string
}

// capturingImageBuilder wraps fakeImageBuilder (byoi_test.go) to record every
// BuildFromDevcontainerFiles call, so a test can assert on the emitted
// devcontainer.json content and on how many times a build actually happened
// (the cache-hit assertion).
type capturingImageBuilder struct {
	fakeImageBuilder
	calls []capturedBuild
}

func (b *capturingImageBuilder) BuildFromDevcontainerFiles(_ context.Context, files map[string]string, tag string, _ io.Writer) (string, error) {
	b.calls = append(b.calls, capturedBuild{files: files, tag: tag})
	return "built/" + tag, nil
}

// TestResolveWorkspaceImage_NamedIntegrationBakesAgentTool pins moving part 1
// of the "integration bakes the CLI" wave end to end through
// resolveWorkspaceImage: a workspace that names an anthropic integration in
// its requirements must have claude-code's install baked into the generated
// devcontainer's onCreateCommand; a workspace that names nothing must not.
func TestResolveWorkspaceImage_NamedIntegrationBakesAgentTool(t *testing.T) {
	h := newHarness(t)
	profile := workspacescan.WorkspaceProfile{
		Languages: []string{"Go", "JavaScript"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
	}
	sc := types.SiteConfig{Integrations: []types.Integration{
		{ID: "acme-claude", Category: types.IntegrationAIProvider, Type: "anthropic_api_key"},
	}}

	t.Run("named anthropic integration bakes claude-code", func(t *testing.T) {
		builder := &capturingImageBuilder{}
		cfg := baseTestConfig(h, &resolveImageStoreFake{sc: sc})
		cfg.ImageBuilder = builder
		srv := New(cfg)
		ws := types.Workspace{ID: uuid.New(), Profile: mustJSON(profile), Requirements: map[string]types.WorkspaceRequirement{
			"integration:acme-claude": {Level: "optional", Provenance: "operator_set"},
		}}
		if _, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil); !ok {
			t.Fatal("resolveWorkspaceImage failed")
		}
		if len(builder.calls) != 1 {
			t.Fatalf("want exactly one build, got %d", len(builder.calls))
		}
		dc := builder.calls[0].files[".devcontainer/devcontainer.json"]
		if !strings.Contains(dc, "onCreateCommand") || !strings.Contains(dc, "@anthropic-ai/claude-code") {
			t.Errorf("a named anthropic integration must bake claude-code's install into onCreateCommand: %s", dc)
		}
	})

	t.Run("no named integration bakes nothing", func(t *testing.T) {
		builder := &capturingImageBuilder{}
		cfg := baseTestConfig(h, &resolveImageStoreFake{sc: sc})
		cfg.ImageBuilder = builder
		srv := New(cfg)
		ws := types.Workspace{ID: uuid.New(), Profile: mustJSON(profile)}
		if _, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil); !ok {
			t.Fatal("resolveWorkspaceImage failed")
		}
		if len(builder.calls) != 1 {
			t.Fatalf("want exactly one build, got %d", len(builder.calls))
		}
		dc := builder.calls[0].files[".devcontainer/devcontainer.json"]
		if strings.Contains(dc, "onCreateCommand") {
			t.Errorf("no named integration must bake no onCreateCommand: %s", dc)
		}
	})
}

// TestResolveWorkspaceImage_CacheKeyFoldsNamedIntegration pins moving part 2:
// the cached BuiltProfileHash must be TRUSTED only when it already reflects
// the workspace's current named-tool set, or toggling an integration would
// never trigger a rebuild (and, symmetrically, an unchanged tool set must
// still hit the cache — this feature must not force a rebuild on every run).
func TestResolveWorkspaceImage_CacheKeyFoldsNamedIntegration(t *testing.T) {
	h := newHarness(t)
	profile := workspacescan.WorkspaceProfile{
		Languages: []string{"Go", "JavaScript"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
	}
	sc := types.SiteConfig{Integrations: []types.Integration{
		{ID: "acme-claude", Category: types.IntegrationAIProvider, Type: "anthropic_api_key"},
	}}
	reqs := map[string]types.WorkspaceRequirement{
		"integration:acme-claude": {Level: "optional", Provenance: "operator_set"},
	}
	tools := workspacescan.AgentToolsForIntegrationTypes([]string{"anthropic_api_key"})
	hashWithTool := profile.CacheKey(tools)
	hashNoTool := profile.CacheKey(nil)
	if hashWithTool == hashNoTool {
		t.Fatal("test setup invalid: CacheKey must differ with/without the named tool for this test to mean anything")
	}

	t.Run("cache holds when the stored hash already reflects the named tool", func(t *testing.T) {
		builder := &capturingImageBuilder{}
		cfg := baseTestConfig(h, &resolveImageStoreFake{sc: sc})
		cfg.ImageBuilder = builder
		srv := New(cfg)
		ws := types.Workspace{
			ID: uuid.New(), Profile: mustJSON(profile), Requirements: reqs,
			ImageRef: "wardyn-workspace/cached:abc", BuiltProfileHash: hashWithTool,
		}
		image, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil)
		if !ok || image != "wardyn-workspace/cached:abc" {
			t.Fatalf("resolveWorkspaceImage = (%q, %v), want the cached image reused", image, ok)
		}
		if len(builder.calls) != 0 {
			t.Errorf("want no build when the cache already reflects the named tool, got %d", len(builder.calls))
		}
	})

	t.Run("a stale hash predating the named integration busts the cache", func(t *testing.T) {
		builder := &capturingImageBuilder{}
		cfg := baseTestConfig(h, &resolveImageStoreFake{sc: sc})
		cfg.ImageBuilder = builder
		srv := New(cfg)
		ws := types.Workspace{
			ID: uuid.New(), Profile: mustJSON(profile), Requirements: reqs,
			// Cached BEFORE the integration was named: the stale hash must not
			// be trusted once the tool set has changed.
			ImageRef: "wardyn-workspace/stale:abc", BuiltProfileHash: hashNoTool,
		}
		image, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil)
		if !ok {
			t.Fatal("resolveWorkspaceImage failed")
		}
		if image == "wardyn-workspace/stale:abc" {
			t.Error("must not reuse the stale-hash image once the named integration changes the tool set")
		}
		if len(builder.calls) != 1 {
			t.Errorf("want exactly one rebuild when the cached hash predates the named tool, got %d", len(builder.calls))
		}
	})
}
