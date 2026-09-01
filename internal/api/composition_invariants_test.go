// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// composition_invariants_test.go pins the shape of the Workspace Composition
// model (migration 0029; types.go's Workspace/WorkspaceSource/WorkspaceBaseImage):
// a workspace is a COMPOSITION of one-or-more sources (local_dir | repo |
// ephemeral) rather than a single source with a kind.
//
// NOT duplicated here: "the mount gate refuses an un-onboarded path even when
// the workspace holds several sources" is already pinned — including the exact
// multi-source case — by TestValidateWorkspaceSources in workspace_refs_test.go
// (see its "multi-source workspace: both its own sources pass" and "... an
// un-onboarded third source still rejected" cases). Re-asserting the identical
// fact here would be redundant coverage, not a new invariant.

// ─── the ≥1-source floor ─────────────────────────────────────────────────────

// createCaptureStore is a minimal store.Store for handleCreateWorkspace: it
// embeds the interface (nil — any other method would panic if called) and
// implements only CreateWorkspace, capturing exactly what the handler
// persists. workspaceStoreFake (workspaces_test.go) has no CreateWorkspace, so
// this is a separate, single-purpose fake rather than growing that one.
type createCaptureStore struct {
	store.Store
	lib     sourceLibraryFake
	created types.Workspace
}

func (s *createCaptureStore) CreateWorkspace(_ context.Context, ws types.Workspace) (types.Workspace, error) {
	s.created = ws
	return ws, nil
}
func (s *createCaptureStore) UpsertSource(ctx context.Context, src types.Source) (types.Source, error) {
	return s.lib.UpsertSource(ctx, src)
}
func (s *createCaptureStore) UpsertBaseImage(ctx context.Context, b types.BaseImageEntry) (types.BaseImageEntry, error) {
	return s.lib.UpsertBaseImage(ctx, b)
}
func (s *createCaptureStore) GetSourcesByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]types.Source, error) {
	return s.lib.GetSourcesByIDs(ctx, ids)
}

// TestWorkspaceComposition_EmptySourcesFloorsToEphemeral is Task 1(a): a
// workspace created with no sources and none of the legacy kind/source fields
// gets exactly one ephemeral source — never an empty list. An empty Sources
// would violate migration 0029's "one-or-more sources" invariant and give
// runs_create.go's seedRequestWorkspace nothing to attach a run to.
func TestWorkspaceComposition_EmptySourcesFloorsToEphemeral(t *testing.T) {
	h := newHarness(t)
	cs := &createCaptureStore{}
	srv := New(baseTestConfig(h, cs))

	w := do(t, srv, http.MethodPost, "/api/v1/workspaces", adminToken, `{"name":"floor-test"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create with no sources: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}

	if len(cs.created.Sources) != 1 {
		t.Fatalf("persisted Sources = %+v, want exactly one entry (the ephemeral floor), never empty", cs.created.Sources)
	}
	got := cs.created.Sources[0]
	if got.Type != types.WorkspaceSourceTypeEphemeral {
		t.Errorf("Sources[0].Type = %q, want ephemeral", got.Type)
	}
	if got.Target != defaultEphemeralTarget {
		t.Errorf("Sources[0].Target = %q, want %q", got.Target, defaultEphemeralTarget)
	}

	// The response body (not just what's persisted) must reflect the floor too.
	var resp types.Workspace
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Sources) != 1 || resp.Sources[0].Type != types.WorkspaceSourceTypeEphemeral {
		t.Errorf("response Sources = %+v, want the same one-ephemeral floor", resp.Sources)
	}
}

// ─── a migrated container-shaped workspace resolves its image ───────────────

// TestSeedRequestWorkspace_MigratedContainerShapeResolvesImage is Task 1(c): a
// workspace migration 0029 rewrote from the old "container" kind — an
// ephemeral source plus a custom base_image pointing at the old image ref (see
// 0029's `WHERE kind = 'container'` backfill) — still resolves to that exact
// image when a run attaches it, exactly as a freshly-onboarded container-shaped
// workspace would.
func TestSeedRequestWorkspace_MigratedContainerShapeResolvesImage(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	ws := types.Workspace{
		ID: wsID,
		Sources: []types.WorkspaceSource{
			{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"},
		},
		BaseImage: &types.WorkspaceBaseImage{Kind: "custom", Image: "registry.example.com/legacy-container:v3"},
	}
	srv := New(baseTestConfig(h, &workspaceStoreFake{ws: ws}))
	spec := &types.RunPolicySpec{}
	req := &createRunRequest{Agent: "claude-code", WorkspaceID: &wsID}

	ephemeralDirs, _, code, err := srv.seedRequestWorkspace(context.Background(), spec, req)
	if err != nil {
		t.Fatalf("seed: %d %v", code, err)
	}

	if req.Image != "registry.example.com/legacy-container:v3" {
		t.Errorf("req.Image = %q, want the migrated workspace's base_image.image", req.Image)
	}
	if len(spec.WorkspaceMounts) != 0 || len(spec.WorkspaceRepos) != 0 {
		t.Errorf("an ephemeral-only (container-shaped) workspace must add no mount/repo entries, got mounts=%+v repos=%+v",
			spec.WorkspaceMounts, spec.WorkspaceRepos)
	}
	if len(ephemeralDirs) != 1 || ephemeralDirs[0] != "/home/agent/work" {
		t.Errorf("ephemeralDirs = %v, want [/home/agent/work] (surfaced as WARDYN_EPHEMERAL_DIRS)", ephemeralDirs)
	}
}

// TestSeedRequestWorkspace_RecommendedBaseImageNeverOverridesImage guards the
// negative next to the migrated-container case above: BaseImage.Kind
// "recommended" means "no override" and must NOT set req.Image, even though a
// non-nil BaseImage is present.
func TestSeedRequestWorkspace_RecommendedBaseImageNeverOverridesImage(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	ws := types.Workspace{
		ID:        wsID,
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		BaseImage: &types.WorkspaceBaseImage{Kind: "recommended"},
	}
	srv := New(baseTestConfig(h, &workspaceStoreFake{ws: ws}))
	spec := &types.RunPolicySpec{}
	req := &createRunRequest{Agent: "claude-code", WorkspaceID: &wsID}

	if _, _, code, err := srv.seedRequestWorkspace(context.Background(), spec, req); err != nil {
		t.Fatalf("seed: %d %v", code, err)
	}
	if req.Image != "" {
		t.Errorf("req.Image = %q, want empty — kind=recommended means \"no override\"", req.Image)
	}
}

// ─── one policy entry per local_dir/repo source, none for ephemeral ─────────

// TestSeedRequestWorkspace_OnePolicyEntryPerSource is Task 1(d): a mixed,
// multi-source workspace folds into the resolved spec with exactly one
// WorkspaceMount per local_dir source, one WorkspaceRepo per repo source, in
// Sources order, and NO policy entry at all for its ephemeral source (which
// instead surfaces only via ephemeralDirs / WARDYN_EPHEMERAL_DIRS at dispatch).
func TestSeedRequestWorkspace_OnePolicyEntryPerSource(t *testing.T) {
	h := newHarness(t)
	wsID := uuid.New()
	ws := types.Workspace{
		ID: wsID,
		Sources: []types.WorkspaceSource{
			{Type: types.WorkspaceSourceTypeLocalDir, Path: "/srv/one", Target: "/home/agent/one"},
			{Type: types.WorkspaceSourceTypeRepo, Source: "acme/repo-a", Target: "/home/agent/repo-a"},
			{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/scratch"},
			{Type: types.WorkspaceSourceTypeLocalDir, Path: "/srv/two", Target: "/home/agent/two", Writable: true},
			{Type: types.WorkspaceSourceTypeRepo, Source: "acme/repo-b"},
		},
	}
	srv := New(baseTestConfig(h, &workspaceStoreFake{ws: ws}))
	spec := &types.RunPolicySpec{}
	req := &createRunRequest{Agent: "claude-code", WorkspaceID: &wsID}

	ephemeralDirs, _, code, err := srv.seedRequestWorkspace(context.Background(), spec, req)
	if err != nil {
		t.Fatalf("seed: %d %v", code, err)
	}

	if len(spec.WorkspaceMounts) != 2 {
		t.Fatalf("WorkspaceMounts = %+v, want exactly 2 (one per local_dir source)", spec.WorkspaceMounts)
	}
	if spec.WorkspaceMounts[0].Source != "/srv/one" || spec.WorkspaceMounts[1].Source != "/srv/two" {
		t.Errorf("mount sources = [%q, %q], want [/srv/one, /srv/two] in Sources order",
			spec.WorkspaceMounts[0].Source, spec.WorkspaceMounts[1].Source)
	}
	if spec.WorkspaceMounts[0].ReadOnlyOrDefault() != true {
		t.Error("the non-Writable local_dir source must mount read-only")
	}
	if spec.WorkspaceMounts[1].ReadOnlyOrDefault() != false {
		t.Error("the Writable local_dir source must mount read-write")
	}

	if len(spec.WorkspaceRepos) != 2 {
		t.Fatalf("WorkspaceRepos = %+v, want exactly 2 (one per repo source)", spec.WorkspaceRepos)
	}
	if spec.WorkspaceRepos[0].Repo != "acme/repo-a" || spec.WorkspaceRepos[1].Repo != "acme/repo-b" {
		t.Errorf("repo entries = [%q, %q], want [acme/repo-a, acme/repo-b] in Sources order",
			spec.WorkspaceRepos[0].Repo, spec.WorkspaceRepos[1].Repo)
	}

	if len(ephemeralDirs) != 1 || ephemeralDirs[0] != "/home/agent/scratch" {
		t.Errorf("ephemeralDirs = %v, want exactly [/home/agent/scratch] — the ephemeral source gets NO mount/repo entry", ephemeralDirs)
	}
}
