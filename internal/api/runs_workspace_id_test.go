// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
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
// generate-devcontainer path: SetWorkspaceBuiltImage (the cache write on a
// successful build) is the only method it needs — every other method panics
// if a test here ever starts needing it (embedded interface, nil by default).
type resolveImageStoreFake struct {
	store.Store
	// builtHash records what the WRITER stored in built_profile_hash, so a
	// test can assert the READER keys on the same expression.
	builtHash string
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

// TestResolveWorkspaceImage_AlwaysBakesStandardAgentTool pins the semantic
// target of removing the "an integration bakes a tool" coupling: the
// generated devcontainer bakes the standard agent-tool install (claude-code)
// into EVERY build, unconditionally — independent of whatever (if anything)
// the workspace names in its requirements.
func TestResolveWorkspaceImage_AlwaysBakesStandardAgentTool(t *testing.T) {
	h := newHarness(t)
	profile := workspacescan.WorkspaceProfile{
		Languages: []string{"Go", "JavaScript"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
	}

	for _, tc := range []struct {
		name string
		reqs map[string]types.WorkspaceRequirement
	}{
		{name: "no named integration"},
		{name: "named anthropic integration", reqs: map[string]types.WorkspaceRequirement{
			"integration:acme-claude": {Level: "optional", Provenance: "operator_set"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			builder := &capturingImageBuilder{}
			cfg := baseTestConfig(h, &resolveImageStoreFake{})
			cfg.ImageBuilder = builder
			srv := New(cfg)
			ws := types.Workspace{ID: uuid.New(), Profile: mustJSON(profile), Requirements: tc.reqs}
			if _, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil); !ok {
				t.Fatal("resolveWorkspaceImage failed")
			}
			if len(builder.calls) != 1 {
				t.Fatalf("want exactly one build, got %d", len(builder.calls))
			}
			dc := builder.calls[0].files[".devcontainer/devcontainer.json"]
			df := builder.calls[0].files[".devcontainer/Dockerfile"]
			if !strings.Contains(df, "downloads.claude.ai") || !strings.Contains(dc, `"dockerfile"`) {
				t.Errorf("every generated devcontainer must bake claude-code via a built Dockerfile:\n%s\n%s", dc, df)
			}
			// A lifecycle hook runs after envbuilder's build+push and reaches no
			// delivered layer — the whole reason this is a Dockerfile RUN.
			if strings.Contains(dc, "CreateCommand") {
				t.Errorf("the bake must be a build step, never a lifecycle command: %s", dc)
			}
		})
	}
}

// TestResolveWorkspaceImage_RebuildReclaimsSupersededTag is the bug-workspace-1
// regression: every build lane in resolveWorkspaceImage mints a fresh,
// uniquely-named local docker tag on every cache miss, but nothing ever
// called ImageRemove on the tag it superseded — every rescan/edit leaked a
// full docker image forever. A cache-miss rebuild (a stale ImageRef whose
// BuiltProfileHash no longer matches p.CacheKey()) must reclaim the OLD tag
// once the new one has actually built.
func TestResolveWorkspaceImage_RebuildReclaimsSupersededTag(t *testing.T) {
	h := newHarness(t)
	profile := workspacescan.WorkspaceProfile{
		Languages: []string{"Go"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
	}
	builder := &capturingImageBuilder{}
	cfg := baseTestConfig(h, &resolveImageStoreFake{})
	cfg.ImageBuilder = builder
	rr := &imageRemoverRunner{fakeRunner: &fakeRunner{}}
	cfg.Runner = rr
	srv := New(cfg)
	const oldRef = "wardyn-workspace/old-id:stale-hash"
	ws := types.Workspace{ID: uuid.New(), Profile: mustJSON(profile),
		ImageRef: oldRef, BuiltProfileHash: "a-stale-hash-that-will-never-match"}

	built, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil)
	if !ok {
		t.Fatal("resolveWorkspaceImage failed")
	}
	if built == oldRef {
		t.Fatalf("expected a freshly-built tag distinct from the stale one, got %q for both", built)
	}
	rr.mu.Lock()
	removed := append([]string(nil), rr.removed...)
	rr.mu.Unlock()
	if len(removed) != 1 || removed[0] != oldRef {
		t.Errorf("ImageRemove calls = %v, want exactly [%s] (the superseded tag reclaimed)", removed, oldRef)
	}
}

// TestResolveWorkspaceImage_CacheHitNeverReclaimsItsOwnTag is the
// counterfactual: a genuine cache HIT (the stored ref is still valid and
// present) must never call ImageRemove on the ref it's about to return —
// that would delete the very image the run is about to launch.
func TestResolveWorkspaceImage_CacheHitNeverReclaimsItsOwnTag(t *testing.T) {
	h := newHarness(t)
	profile := workspacescan.WorkspaceProfile{
		Languages: []string{"Go"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
	}
	builder := &capturingImageBuilder{}
	cfg := baseTestConfig(h, &resolveImageStoreFake{})
	cfg.ImageBuilder = builder
	rr := &imageRemoverRunner{fakeRunner: &fakeRunner{}}
	cfg.Runner = rr
	srv := New(cfg)
	const cachedRef = "wardyn-workspace/w:cached-hash"
	ws := types.Workspace{ID: uuid.New(), Profile: mustJSON(profile),
		ImageRef: cachedRef, BuiltProfileHash: profile.CacheKey()}

	built, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil)
	if !ok || built != cachedRef {
		t.Fatalf("resolveWorkspaceImage = (%q, %v), want the cache hit (%q, true)", built, ok, cachedRef)
	}
	if len(builder.calls) != 0 {
		t.Fatalf("cache hit must not trigger a rebuild, got %d builds", len(builder.calls))
	}
	rr.mu.Lock()
	removed := len(rr.removed)
	rr.mu.Unlock()
	if removed != 0 {
		t.Errorf("ImageRemove calls = %d, want 0 on a cache hit", removed)
	}
}

// TestResolveWorkspaceImage_CacheKeyInvalidatesPreUnconditionalBake pins
// CacheKey's salt bump (profile.go's cacheKeySalt): a workspace image cached
// under the pre-bump formula (a bare ProfileHash — the old "no tools named"
// case, which may predate the standard agent-tool install becoming
// unconditional and so may lack claude-code) must NOT read as a cache hit; a
// workspace already cached under the CURRENT p.CacheKey() must.
func TestResolveWorkspaceImage_CacheKeyInvalidatesPreUnconditionalBake(t *testing.T) {
	h := newHarness(t)
	profile := workspacescan.WorkspaceProfile{
		Languages: []string{"Go", "JavaScript"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
	}

	t.Run("cache holds when the stored hash already reflects CacheKey()", func(t *testing.T) {
		builder := &capturingImageBuilder{}
		cfg := baseTestConfig(h, &resolveImageStoreFake{})
		cfg.ImageBuilder = builder
		srv := New(cfg)
		ws := types.Workspace{
			ID: uuid.New(), Profile: mustJSON(profile),
			ImageRef: "wardyn-workspace/cached:abc", BuiltProfileHash: profile.CacheKey(),
		}
		image, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil)
		if !ok || image != "wardyn-workspace/cached:abc" {
			t.Fatalf("resolveWorkspaceImage = (%q, %v), want the cached image reused", image, ok)
		}
		if len(builder.calls) != 0 {
			t.Errorf("want no build when the cache already reflects CacheKey(), got %d", len(builder.calls))
		}
	})

	t.Run("a bare-ProfileHash stale hash (pre-salt-bump) busts the cache", func(t *testing.T) {
		builder := &capturingImageBuilder{}
		cfg := baseTestConfig(h, &resolveImageStoreFake{})
		cfg.ImageBuilder = builder
		srv := New(cfg)
		ws := types.Workspace{
			ID: uuid.New(), Profile: mustJSON(profile),
			// A pre-salt-bump image may lack claude-code entirely (it predates
			// the standard install becoming unconditional) — its cached hash
			// must never be trusted.
			ImageRef: "wardyn-workspace/stale:abc", BuiltProfileHash: profile.ProfileHash(),
		}
		image, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil)
		if !ok {
			t.Fatal("resolveWorkspaceImage failed")
		}
		if image == "wardyn-workspace/stale:abc" {
			t.Error("must not reuse a pre-salt-bump cached image — it may predate the unconditional agent-tool bake")
		}
		if len(builder.calls) != 1 {
			t.Errorf("want exactly one rebuild when the cached hash predates the salt bump, got %d", len(builder.calls))
		}
	})
}

// TestResolveWorkspaceImage_RepoOwnDevcontainerWinsVerbatim pins the R5
// medium fix: a workspace whose PRIMARY source is a repo carrying its OWN
// devcontainer builds that devcontainer AS-IS — resolveWorkspaceImage must
// never rewrite it to layer in the generated Dockerfile (gen.go's package
// comment: neither mechanism that could add a RUN without rewriting the
// operator's own devcontainer survived a real build), so claude-code is never
// baked here, regardless of anything named on the workspace.
func TestResolveWorkspaceImage_RepoOwnDevcontainerWinsVerbatim(t *testing.T) {
	h := newHarness(t)
	profile := workspacescan.WorkspaceProfile{
		Languages: []string{"Go"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
		HasDevcontainer: true,
	}

	t.Run("repo build wins, generator never runs", func(t *testing.T) {
		builder := &capturingImageBuilder{}
		cfg := baseTestConfig(h, &resolveImageStoreFake{})
		cfg.ImageBuilder = builder
		srv := New(cfg)
		ws := types.Workspace{
			ID:      uuid.New(),
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "https://github.com/acme/widgets", Ref: "main"}},
			Profile: mustJSON(profile),
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

		// resolveBuildView must report the repo-own-devcontainer image as done
		// once the tracker knows about it (exactly what handleBuildWorkspace's
		// goroutine does on success) — nothing about this lane's caveat-free
		// build should confuse the ordinary "done" reporting.
		srv.builds.finish(ws.ID, built, "")
		view := srv.resolveBuildView(ws)
		if view.State != "done" || view.Image != built {
			t.Errorf("resolveBuildView = %+v, want state=done image=%q", view, built)
		}
	})

	t.Run("SSH source falls through to the generator instead, which bakes claude-code", func(t *testing.T) {
		builder := &capturingImageBuilder{}
		cfg := baseTestConfig(h, &resolveImageStoreFake{})
		cfg.ImageBuilder = builder
		srv := New(cfg)
		ws := types.Workspace{
			ID:      uuid.New(),
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "git@github.com:acme/widgets.git"}},
			Profile: mustJSON(profile),
		}
		// The image builder cannot clone an SSH source (no minted key in this
		// lane) — resolveWorkspaceImage falls through to the generator, which
		// bakes claude-code unconditionally.
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

// imageCheckerRunner wraps fakeRunner with runner.ImageChecker, so a test can
// simulate a docker-like substrate that can confirm/deny a cached image_ref
// is still actually present.
type imageCheckerRunner struct {
	*fakeRunner
	present map[string]bool // ref -> present; absent key = "not present"
}

func (r *imageCheckerRunner) ImagePresent(_ context.Context, ref string) (bool, error) {
	return r.present[ref], nil
}

// imageRemoverRunner wraps fakeRunner with runner.ImageRemover, so a test can
// assert which refs resolveWorkspaceImage/handleUpdateWorkspace/
// handleDeleteWorkspace reclaimed (bug-workspace-1).
type imageRemoverRunner struct {
	*fakeRunner
	mu      sync.Mutex
	removed []string
	failRef string // ImageRemove errors for this ref, if set
}

func (r *imageRemoverRunner) ImageRemove(_ context.Context, ref string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ref == r.failRef {
		return fmt.Errorf("docker: image in use")
	}
	r.removed = append(r.removed, ref)
	return nil
}

// TestResolveWorkspaceImage_StaleCacheFallsThroughToRebuild is
// W20-W20-record-image-5: a cached image_ref the daemon no longer has used to
// be a permanent dead end — resolveWorkspaceImage trusted BuiltProfileHash
// alone and never verified the ref was still real. With an ImageChecker
// Runner wired, a cache "hit" whose ref the runner reports ABSENT must fall
// through to a rebuild instead of returning the dead ref.
func TestResolveWorkspaceImage_StaleCacheFallsThroughToRebuild(t *testing.T) {
	h := newHarness(t)
	profile := workspacescan.WorkspaceProfile{
		Languages: []string{"Go"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
	}
	builder := &capturingImageBuilder{}
	cfg := baseTestConfig(h, &resolveImageStoreFake{})
	cfg.ImageBuilder = builder
	cfg.Runner = &imageCheckerRunner{fakeRunner: &fakeRunner{}, present: map[string]bool{}} // empty: nothing is present
	srv := New(cfg)
	ws := types.Workspace{
		ID: uuid.New(), Profile: mustJSON(profile),
		ImageRef: "wardyn-workspace/gone:abc", BuiltProfileHash: profile.CacheKey(),
	}

	image, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil)
	if !ok {
		t.Fatal("resolveWorkspaceImage failed")
	}
	if image == "wardyn-workspace/gone:abc" {
		t.Error("must not reuse a cached image_ref the runner reports absent — permanent dead end otherwise")
	}
	if len(builder.calls) != 1 {
		t.Errorf("want exactly one rebuild when the cached image is confirmed absent, got %d", len(builder.calls))
	}
}

// TestResolveBuildView_AgreesWithBuiltHash pins the reader/writer contract the
// /build endpoints depend on: resolveBuildView must key the cache-hit "done"
// branch on the SAME expression resolveWorkspaceImage stores in
// built_profile_hash (p.CacheKey(), not a bare ProfileHash()). Keying the
// reader on anything else makes the branch unreachable by construction, which
// reports state:"none" for a workspace that IS built as soon as the in-memory
// tracker no longer knows about it: after a restart, or from a second
// replica.
func TestResolveBuildView_AgreesWithBuiltHash(t *testing.T) {
	h := newHarness(t)
	profile := workspacescan.WorkspaceProfile{
		Languages: []string{"Go", "JavaScript"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
	}
	st := &resolveImageStoreFake{}
	cfg := baseTestConfig(h, st)
	cfg.ImageBuilder = &capturingImageBuilder{}
	srv := New(cfg)
	ws := types.Workspace{ID: uuid.New(), Profile: mustJSON(profile)}
	built, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil)
	if !ok {
		t.Fatal("resolveWorkspaceImage failed")
	}
	if st.builtHash == "" {
		t.Fatal("writer stored no built_profile_hash")
	}

	// A FRESH server: the in-memory build tracker knows nothing, exactly
	// like the process that reads this row after a restart.
	fresh := New(baseTestConfig(h, &resolveImageStoreFake{}))
	ws.ImageRef, ws.BuiltProfileHash = built, st.builtHash
	view := fresh.resolveBuildView(ws)
	if view.State != "done" || view.Image != built {
		t.Errorf("resolveBuildView = %+v, want state=done image=%q — the reader disagrees with the writer's hash", view, built)
	}
}

// TestResolveWorkspaceImage_ByoiCachesAcrossSessions is
// W20-W20-record-image-3: the byoi lane used to tag EVERY wrap with the run
// id (`wardyn-byoi/<runid>:latest`), so two record/replay sessions against
// the identical base image always rebuilt — a multi-minute FinalizeBase call
// on every single launch. A cache hit must reuse the workspace's stored
// ImageRef without calling FinalizeBase again; a base ref CHANGE must still
// rebuild (and re-cache under the new key).
func TestResolveWorkspaceImage_ByoiCachesAcrossSessions(t *testing.T) {
	h := newHarness(t)
	builder := &capturingByoiImageBuilder{}
	st := &resolveImageStoreFake{}
	cfg := baseTestConfig(h, st)
	cfg.ImageBuilder = builder
	srv := New(cfg)
	ws := types.Workspace{
		ID:      uuid.New(),
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		BaseImage: &types.WorkspaceBaseImage{
			Kind: "custom", Image: "ghcr.io/acme/base:1",
		},
	}

	first, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil)
	if !ok {
		t.Fatal("resolveWorkspaceImage failed (first launch)")
	}
	if len(builder.calls) != 1 {
		t.Fatalf("want exactly one FinalizeBase call on the first launch, got %d", len(builder.calls))
	}
	if st.builtHash == "" {
		t.Fatal("first launch stored no built_profile_hash — nothing to cache against")
	}

	// A SECOND session for the SAME workspace+base ref: simulate what the
	// workspace row now holds (ImageRef/BuiltProfileHash persisted by the
	// first launch's SetWorkspaceBuiltImage call).
	ws.ImageRef, ws.BuiltProfileHash = first, st.builtHash
	second, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil)
	if !ok {
		t.Fatal("resolveWorkspaceImage failed (second launch)")
	}
	if second != first {
		t.Errorf("second launch = %q, want the cached image %q reused", second, first)
	}
	if len(builder.calls) != 1 {
		t.Errorf("want NO additional FinalizeBase call on a cache hit, got %d total calls", len(builder.calls))
	}

	// Changing the base ref must still rebuild — the cache is per (kind, ref),
	// not blanket-sticky on the workspace.
	ws.BaseImage = &types.WorkspaceBaseImage{Kind: "custom", Image: "ghcr.io/acme/base:2"}
	third, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil)
	if !ok {
		t.Fatal("resolveWorkspaceImage failed (third launch, changed base ref)")
	}
	if third == first {
		t.Errorf("a changed base ref must not reuse the old base's cached image")
	}
	if len(builder.calls) != 2 {
		t.Errorf("want exactly one rebuild after the base ref changed, got %d total calls", len(builder.calls))
	}
}

// capturingByoiImageBuilder records every FinalizeBase call and returns a
// tag derived from the CALL COUNT (not the output tag) — a real Docker image
// digest is deterministic per input, unlike the old runID-suffixed tag this
// test's cache-hit assertion depends on distinguishing from a genuine rebuild.
type capturingByoiImageBuilder struct {
	fakeImageBuilder
	calls []string // base refs FinalizeBase was called with, in order
}

func (b *capturingByoiImageBuilder) FinalizeBase(_ context.Context, baseRef, _ string, _ io.Writer) (string, error) {
	b.calls = append(b.calls, baseRef)
	return "wardyn-byoi/cached:" + baseRef, nil
}

// TestResolveWorkspaceImage_RepoDevcontainerCachesAcrossSessions is
// W20-W20-record-image-3's repo-own-devcontainer half: this lane built the
// repo's OWN devcontainer unconditionally on every session (a fixed tag, but
// no cache-hit check before calling BuildDevcontainer again).
func TestResolveWorkspaceImage_RepoDevcontainerCachesAcrossSessions(t *testing.T) {
	h := newHarness(t)
	profile := workspacescan.WorkspaceProfile{
		Languages: []string{"Go"}, Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic,
		HasDevcontainer: true,
	}
	builder := &capturingImageBuilder{}
	st := &resolveImageStoreFake{}
	cfg := baseTestConfig(h, st)
	cfg.ImageBuilder = builder
	srv := New(cfg)
	ws := types.Workspace{
		ID:      uuid.New(),
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "https://github.com/acme/widgets", Ref: "main"}},
		Profile: mustJSON(profile),
	}

	first, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil)
	if !ok {
		t.Fatal("resolveWorkspaceImage failed (first launch)")
	}
	if len(builder.repoBuilds) != 1 {
		t.Fatalf("want exactly one BuildDevcontainer call on the first launch, got %d", len(builder.repoBuilds))
	}
	if st.builtHash == "" {
		t.Fatal("first launch stored no built_profile_hash — nothing to cache against")
	}

	ws.ImageRef, ws.BuiltProfileHash = first, st.builtHash
	second, ok := srv.resolveWorkspaceImage(context.Background(), uuid.New(), ws, nil)
	if !ok {
		t.Fatal("resolveWorkspaceImage failed (second launch)")
	}
	if second != first {
		t.Errorf("second launch = %q, want the cached image %q reused", second, first)
	}
	if len(builder.repoBuilds) != 1 {
		t.Errorf("want NO additional BuildDevcontainer call on a cache hit, got %d total calls", len(builder.repoBuilds))
	}
}
