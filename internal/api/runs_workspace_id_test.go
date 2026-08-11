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

	// A composed launch's create-run request never carries workspace_id (it
	// carries workspaces: [...] / inline_policy instead — compose.go's
	// applyWorkspaces seeds the mounts/repos directly). This is the KNOWN,
	// documented gap that leaves: a composed run's base_image never sets
	// req.Image, unlike the manual/workspace_id case just above (same
	// container-shaped-workspace scenario, base_image and all). See
	// seedRequestWorkspace's doc comment and reconcile-workspace-first.md item 2
	// for why re-running this function afterward (deriving a workspace_id for
	// the primary composed selection) is not a safe fix: applyWorkspaces
	// already seeded the same sources, and this function would seed them a
	// second time.
	t.Run("nil WorkspaceID no-ops entirely, regardless of any workspace's base_image (the composed-launch shape)", func(t *testing.T) {
		spec := types.RunPolicySpec{}
		req := createRunRequest{Agent: "claude-code"} // no WorkspaceID: the compose path's shape
		// A bare *Server{}, deliberately: the WorkspaceID==nil guard returns
		// before this function ever reads s.cfg (Store included), so there is no
		// workspace/store fixture to construct — that IS the property under
		// test. (TestIsOperator above uses the same bare-literal pattern for the
		// identical reason; New(...) is for tests that need its full wiring —
		// e.g. a live runner/background reconciliation — which this does not.)
		srv := &Server{}
		dirs, code, err := srv.seedRequestWorkspace(context.Background(), &spec, &req)
		if err != nil || code != 0 {
			t.Fatalf("seed: %d %v, want a clean no-op", code, err)
		}
		if req.Image != "" {
			t.Errorf("req.Image = %q, want empty — a composed launch does not carry workspace_id, so the workspace's base_image is never consulted", req.Image)
		}
		if len(dirs) != 0 || len(spec.WorkspaceMounts) != 0 || len(spec.WorkspaceRepos) != 0 {
			t.Errorf("want a total no-op with WorkspaceID nil: dirs=%v mounts=%v repos=%v", dirs, spec.WorkspaceMounts, spec.WorkspaceRepos)
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

// wrapEchoImageBuilder echoes FinalizeBase's output tag (baseRef -> the wrapped
// wardyn-byoi/<runid> tag), so a test can assert the base image was WRAPPED, not
// dispatched verbatim.
type wrapEchoImageBuilder struct{ fakeImageBuilder }

func (wrapEchoImageBuilder) FinalizeBase(_ context.Context, baseRef, outputTag string, _ io.Writer) (string, error) {
	return outputTag, nil
}

// TestResolveWorkspaceImage_ContainerShapedWorkspaceUsesBaseImage covers the
// OTHER image-resolution path a migrated container-kind workspace exercises:
// resolveWorkspaceImage (workspace_run.go), used by the import-pipeline's
// scan/record/verify runs — distinct from TestSeedRequestWorkspace above, which
// pins the ordinary create-run --image/workspace_id seed. An explicit BaseImage
// choice ("custom"/"registry"/"byo") takes precedence over the detected-
// toolchain devcontainer path unconditionally, before any profile is even
// consulted. PARITY-4: that base image is now FinalizeBase-WRAPPED into a
// wardyn-byoi/<runid> tag (the same wrap the workspace_id create-run door does),
// which arms the fail-closed harness selftest — it is NOT dispatched verbatim
// (that shipped a raw image with no agent-run to a UI-door run).
func TestResolveWorkspaceImage_ContainerShapedWorkspaceUsesBaseImage(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, nil)
	cfg.ImageBuilder = wrapEchoImageBuilder{}
	srv := New(cfg)
	runID := uuid.New()
	ws := types.Workspace{
		ID:        uuid.New(),
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		BaseImage: &types.WorkspaceBaseImage{Kind: "custom", Image: "ghcr.io/acme/base:1"},
		// No Profile: an unscanned ephemeral-only workspace has none, and the
		// BaseImage branch must return before ever needing one.
	}
	image, ok := srv.resolveWorkspaceImage(context.Background(), runID, ws, nil)
	want := "wardyn-byoi/" + runID.String() + ":latest"
	if !ok || image != want {
		t.Fatalf("resolveWorkspaceImage = (%q, %v), want the FinalizeBase-wrapped tag %q (PARITY-4)", image, ok, want)
	}
}

// TestResolveWorkspaceImage_BaseImageNoBuilderDrops pins PARITY-4's no-builder
// arm: with a base image chosen but no ImageBuilder wired, the UI-door lane must
// NOT silently swap in the convention image — it returns ("", false) (falling
// back to the convention image) and audits the drop, rather than dispatching the
// raw base image or claiming success.
func TestResolveWorkspaceImage_BaseImageNoBuilderDrops(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, nil)
	cfg.ImageBuilder = nil
	srv := New(cfg)
	ws := types.Workspace{
		ID:        uuid.New(),
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		BaseImage: &types.WorkspaceBaseImage{Kind: "custom", Image: "ghcr.io/acme/base:1"},
	}
	if image, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil); ok || image != "" {
		t.Fatalf("resolveWorkspaceImage = (%q, %v), want (\"\", false) with no builder wired", image, ok)
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
	// builtHash records what the WRITER stored in built_profile_hash, so a
	// test can assert the READER keys on the same expression.
	builtHash string
}

func (s *resolveImageStoreFake) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.sc, nil
}
func (s *resolveImageStoreFake) SetWorkspaceBuiltImage(_ context.Context, _ uuid.UUID, _, hash string) (types.Workspace, error) {
	s.builtHash = hash
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
	calls      []capturedBuild
	repoBuilds []capturedRepoBuild
}

func (b *capturingImageBuilder) BuildFromDevcontainerFiles(_ context.Context, files map[string]string, tag string, _ io.Writer) (string, error) {
	b.calls = append(b.calls, capturedBuild{files: files, tag: tag})
	return "built/" + tag, nil
}

// capturedRepoBuild is one BuildDevcontainer call capturingImageBuilder
// recorded (the repo-own-devcontainer lane).
type capturedRepoBuild struct {
	repoURL, ref, tag string
}

func (b *capturingImageBuilder) BuildDevcontainer(_ context.Context, repoURL, ref, tag string, _ io.Writer) (string, error) {
	b.repoBuilds = append(b.repoBuilds, capturedRepoBuild{repoURL: repoURL, ref: ref, tag: tag})
	return "built/" + tag, nil
}

// TestResolveWorkspaceImage_NamedIntegrationBakesAgentTool pins moving part 1
// of the "integration bakes the CLI" wave end to end through
// resolveWorkspaceImage: a workspace that names an anthropic integration in
// its requirements must have claude-code's install baked into a generated
// .devcontainer/Dockerfile that devcontainer.json builds; a workspace that
// names nothing must not.
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
		df := builder.calls[0].files[".devcontainer/Dockerfile"]
		if !strings.Contains(df, "downloads.claude.ai") || !strings.Contains(dc, `"dockerfile"`) {
			t.Errorf("a named anthropic integration must bake claude-code via a built Dockerfile:\n%s\n%s", dc, df)
		}
		// A lifecycle hook runs after envbuilder's build+push and reaches no
		// delivered layer — the whole reason this is a Dockerfile RUN.
		if strings.Contains(dc, "CreateCommand") {
			t.Errorf("the bake must be a build step, never a lifecycle command: %s", dc)
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
		if _, ok := builder.calls[0].files[".devcontainer/Dockerfile"]; ok {
			t.Errorf("no named integration must bake no agent CLI: %v", builder.calls[0].files)
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

// TestResolveWorkspaceImage_RepoOwnDevcontainerNeverBakesButIsVisible pins the
// R5 medium fix: a workspace whose PRIMARY source is a repo carrying its OWN
// devcontainer builds that devcontainer AS-IS — resolveWorkspaceImage must
// never rewrite it to layer in the generated Dockerfile (gen.go's package
// comment: neither mechanism that could add a RUN without rewriting the
// operator's own devcontainer survived a real build), so a named
// integration's agent CLI is not baked here. That must not be SILENT: this
// pins the "recorded" half (repoOwnDevcontainerURL/AgentToolsForIntegrationTypes
// agree with what resolveWorkspaceImage actually does) and the "visible" half
// (resolveBuildView's caveat, both pre-build and immediately post-build).
func TestResolveWorkspaceImage_RepoOwnDevcontainerNeverBakesButIsVisible(t *testing.T) {
	h := newHarness(t)
	profile := workspacescan.WorkspaceProfile{
		Languages: []string{"Go"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
		HasDevcontainer: true,
	}
	sc := types.SiteConfig{Integrations: []types.Integration{
		{ID: "acme-claude", Category: types.IntegrationAIProvider, Type: "anthropic_api_key"},
	}}
	reqs := map[string]types.WorkspaceRequirement{
		"integration:acme-claude": {Level: "optional", Provenance: "operator_set"},
	}

	t.Run("named integration: repo build wins, generator never runs, caveat visible before and after", func(t *testing.T) {
		builder := &capturingImageBuilder{}
		cfg := baseTestConfig(h, &resolveImageStoreFake{sc: sc})
		cfg.ImageBuilder = builder
		srv := New(cfg)
		ws := types.Workspace{
			ID:           uuid.New(),
			Sources:      []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "https://github.com/acme/widgets", Ref: "main"}},
			Profile:      mustJSON(profile),
			Requirements: reqs,
		}

		// Pre-build: resolveBuildView must already warn, before anyone clicks Build.
		if d := srv.resolveBuildView(context.Background(), ws).Detail; d == "" || !strings.Contains(d, "does not add") {
			t.Errorf("pre-build Detail = %q, want the repo-own-devcontainer caveat", d)
		}

		built, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil)
		if !ok {
			t.Fatal("resolveWorkspaceImage failed")
		}
		if len(builder.repoBuilds) != 1 || builder.repoBuilds[0].repoURL != "https://github.com/acme/widgets" {
			t.Fatalf("repoBuilds = %+v, want exactly one BuildDevcontainer call for the repo's own devcontainer", builder.repoBuilds)
		}
		if len(builder.calls) != 0 {
			t.Errorf("a repo-own-devcontainer build must never ALSO call the generator (that would bake claude-code into a DIFFERENT image nothing points at): got %d generator calls", len(builder.calls))
		}

		// Post-build: the in-memory tracker now knows about this build (exactly
		// what handleBuildWorkspace's goroutine does on success) — the caveat
		// must still be visible in the "done" state, not just pre-build "none".
		srv.builds.finish(ws.ID, built, "")
		view := srv.resolveBuildView(context.Background(), ws)
		if view.State != "done" {
			t.Fatalf("state = %q, want done", view.State)
		}
		if !strings.Contains(view.Detail, "does not add") || !strings.Contains(view.Detail, "claude-code") {
			t.Errorf("done Detail = %q, want the repo-own-devcontainer caveat naming claude-code", view.Detail)
		}
	})

	t.Run("no named integration: repo build wins, no caveat (nothing was going to be baked anyway)", func(t *testing.T) {
		builder := &capturingImageBuilder{}
		cfg := baseTestConfig(h, &resolveImageStoreFake{sc: sc})
		cfg.ImageBuilder = builder
		srv := New(cfg)
		ws := types.Workspace{
			ID:      uuid.New(),
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "https://github.com/acme/widgets"}},
			Profile: mustJSON(profile),
		}
		if d := srv.resolveBuildView(context.Background(), ws).Detail; d != "" {
			t.Errorf("Detail = %q, want none — no named integration means nothing was going to be baked anyway", d)
		}
		if _, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil); !ok {
			t.Fatal("resolveWorkspaceImage failed")
		}
		if len(builder.repoBuilds) != 1 {
			t.Fatalf("repoBuilds = %+v, want exactly one BuildDevcontainer call", builder.repoBuilds)
		}
	})

	t.Run("SSH source falls through to the generator instead, so no caveat applies", func(t *testing.T) {
		builder := &capturingImageBuilder{}
		cfg := baseTestConfig(h, &resolveImageStoreFake{sc: sc})
		cfg.ImageBuilder = builder
		srv := New(cfg)
		ws := types.Workspace{
			ID:           uuid.New(),
			Sources:      []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "git@github.com:acme/widgets.git"}},
			Profile:      mustJSON(profile),
			Requirements: reqs,
		}
		// The image builder cannot clone an SSH source (no minted key in this
		// lane) — resolveWorkspaceImage falls through to the generator, which
		// DOES bake the named tool, so no "not baked" caveat may fire here.
		if d := srv.resolveBuildView(context.Background(), ws).Detail; d != "" {
			t.Errorf("Detail = %q, want none — an SSH source falls through to the generated (tool-baking) path", d)
		}
		if _, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil); !ok {
			t.Fatal("resolveWorkspaceImage failed")
		}
		if len(builder.repoBuilds) != 0 {
			t.Errorf("an SSH source must never reach BuildDevcontainer: %+v", builder.repoBuilds)
		}
		if len(builder.calls) != 1 {
			t.Errorf("an SSH source must fall through to the generator exactly once, got %d", len(builder.calls))
		}
	})
}

// TestResolveBuildView_AgreesWithBuiltHash pins the reader/writer contract the
// /build endpoints depend on: resolveBuildView must key the cache-hit "done"
// branch on the SAME expression resolveWorkspaceImage stores in
// built_profile_hash. It cannot use a bare ProfileHash(), which the writer has
// never stored — that made the branch unreachable, so a workspace that IS built
// read back as state:"none" (and POST re-entered the async build machinery, and
// audited a run.build) from any process whose in-memory tracker had not itself
// run the build: after a restart, or from a second replica.
//
// Both tool sets matter: with a named integration the hash folds tools, and
// without one it must still be the plain ProfileHash the pre-upgrade rows carry.
func TestResolveBuildView_AgreesWithBuiltHash(t *testing.T) {
	h := newHarness(t)
	profile := workspacescan.WorkspaceProfile{
		Languages: []string{"Go", "JavaScript"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
	}
	sc := types.SiteConfig{Integrations: []types.Integration{
		{ID: "acme-claude", Category: types.IntegrationAIProvider, Type: "anthropic_api_key"},
	}}
	cases := []struct {
		name string
		reqs map[string]types.WorkspaceRequirement
	}{
		{name: "no named integration", reqs: nil},
		{name: "named anthropic integration", reqs: map[string]types.WorkspaceRequirement{
			"integration:acme-claude": {Level: "optional", Provenance: "operator_set"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &resolveImageStoreFake{sc: sc}
			cfg := baseTestConfig(h, st)
			cfg.ImageBuilder = &capturingImageBuilder{}
			srv := New(cfg)
			ws := types.Workspace{ID: uuid.New(), Profile: mustJSON(profile), Requirements: tc.reqs}
			built, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil)
			if !ok {
				t.Fatal("resolveWorkspaceImage failed")
			}
			if st.builtHash == "" {
				t.Fatal("writer stored no built_profile_hash")
			}

			// A FRESH server: the in-memory build tracker knows nothing, exactly
			// like the process that reads this row after a restart.
			fresh := New(baseTestConfig(h, &resolveImageStoreFake{sc: sc}))
			ws.ImageRef, ws.BuiltProfileHash = built, st.builtHash
			view := fresh.resolveBuildView(context.Background(), ws)
			if view.State != "done" || view.Image != built {
				t.Errorf("resolveBuildView = %+v, want state=done image=%q — the reader disagrees with the writer's hash", view, built)
			}
		})
	}
}
