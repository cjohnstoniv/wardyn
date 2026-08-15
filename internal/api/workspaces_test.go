// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// TestDecodeWorkspaceRequest_DuplicateExplicitTargetsRejected is the
// W8-S1-3 regression: two sources sharing an explicit target both resolve to
// the SAME in-sandbox mount/clone path — validatePolicyWorkspaces (policy.go)
// then 422s that composition on every subsequent run. Rejecting it here, at
// onboarding time, catches it before it's even possible to run — and before a
// wizard's own basename collision (fixed client-side in wizard-types.ts's
// defaultTargetFor) could ever reach the server in the first place. Exercises
// decodeWorkspaceRequest directly (not the full handler) so it needs no store.
func TestDecodeWorkspaceRequest_DuplicateExplicitTargetsRejected(t *testing.T) {
	body := `{"name":"w","sources":[
		{"type":"repo","source":"acme/api","target":"/home/agent/work/api"},
		{"type":"repo","source":"other-org/api","target":"/home/agent/work/api"}
	]}`
	r := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces", strings.NewReader(body))
	w := httptest.NewRecorder()
	_, msg := decodeWorkspaceRequest(w, r)
	if msg == "" {
		t.Fatal("two sources with the same explicit target: got no rejection, want one")
	}
	if !strings.Contains(msg, "sources[1]") || !strings.Contains(msg, "duplicates") {
		t.Errorf("message = %q, want it to name sources[1] and the duplicate", msg)
	}
}

// Distinct explicit targets, or an empty (default-derived) target repeated on
// several sources, must NOT be rejected here — the empty case isn't resolved
// until attach/clone time (buildRepoRecords), same as validatePolicyWorkspaces.
func TestDecodeWorkspaceRequest_DistinctOrEmptyTargetsAccepted(t *testing.T) {
	for _, body := range []string{
		`{"name":"w","sources":[
			{"type":"repo","source":"acme/api","target":"/home/agent/work/api"},
			{"type":"repo","source":"other-org/payments","target":"/home/agent/work/payments"}
		]}`,
		`{"name":"w","sources":[
			{"type":"repo","source":"acme/api"},
			{"type":"repo","source":"other-org/api"}
		]}`,
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces", strings.NewReader(body))
		w := httptest.NewRecorder()
		if _, msg := decodeWorkspaceRequest(w, r); msg != "" {
			t.Errorf("body=%s: got rejection %q, want none", body, msg)
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
// sourceLibraryFake is the in-memory tier-1/tier-2 mixin the workspace fakes
// embed: upsertAndAttach routes every dir/repo create/update through
// UpsertSource + UpsertBaseImage now, so any fake serving those handlers must
// answer them. Dedupe mirrors the store's identity rule.
type sourceLibraryFake struct {
	sources map[string]types.Source
	images  map[string]types.BaseImageEntry
}

func (f *sourceLibraryFake) UpsertSource(_ context.Context, src types.Source) (types.Source, error) {
	if f.sources == nil {
		f.sources = map[string]types.Source{}
	}
	key := string(src.Kind) + "|" + src.Locator + "|" + src.Ref
	if existing, ok := f.sources[key]; ok {
		return existing, nil
	}
	f.sources[key] = src
	return src, nil
}

func (f *sourceLibraryFake) UpsertBaseImage(_ context.Context, b types.BaseImageEntry) (types.BaseImageEntry, error) {
	if f.images == nil {
		f.images = map[string]types.BaseImageEntry{}
	}
	key := b.Kind + "|" + b.Image + "|" + strings.Join(b.Steps, "\n")
	if existing, ok := f.images[key]; ok {
		return existing, nil
	}
	f.images[key] = b
	return b, nil
}

func (f *sourceLibraryFake) GetSourcesByIDs(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]types.Source, error) {
	out := map[uuid.UUID]types.Source{}
	for _, src := range f.sources {
		for _, id := range ids {
			if src.ID == id {
				out[id] = src
			}
		}
	}
	return out, nil
}

type workspaceStoreFake struct {
	store.Store
	lib     sourceLibraryFake // named, not embedded: embedding beside the interface makes every shared method ambiguous
	ws      types.Workspace
	updated types.Workspace
}

func (s *workspaceStoreFake) UpsertSource(ctx context.Context, src types.Source) (types.Source, error) {
	return s.lib.UpsertSource(ctx, src)
}
func (s *workspaceStoreFake) UpsertBaseImage(ctx context.Context, b types.BaseImageEntry) (types.BaseImageEntry, error) {
	return s.lib.UpsertBaseImage(ctx, b)
}
func (s *workspaceStoreFake) GetSourcesByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]types.Source, error) {
	return s.lib.GetSourcesByIDs(ctx, ids)
}

func (s *workspaceStoreFake) GetWorkspace(context.Context, uuid.UUID) (types.Workspace, error) {
	return s.ws, nil
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
// everything reviewed against the OLD source — image, approvals, recorded
// evidence and the "PROVEN to install/build/test" stamp — must be gone when
// source/kind/ref changes, since the store UPDATE rewrites every column.
//
// Profile/Status are deliberately NOT asserted here (STORE-4): every workspace
// reaching this handler now has a non-empty Attachments (upsertAndAttach always
// attaches at least the composition floor), so the REAL store's hydrate pass
// unconditionally RE-DERIVES both from the fresh sources on every read — a
// handler-side reset is provably discarded before this request's own response
// leaves the store. workspaceStoreFake.UpdateWorkspace is a bare echo (no
// hydrate simulation), so asserting them here would pin the handler to writing
// bytes the real store never reads back, not any observable behavior; the fold
// itself (an attachment-backed, freshly-attached-and-unscanned source derives
// Profile=nil / Status=pending_scan) is store_sources.go's hydrateWorkspace,
// covered at the store layer.
func TestUpdateWorkspace_ContentChangeClearsEveryReviewedField(t *testing.T) {
	h := newHarness(t)
	id := uuid.New()
	fake := &workspaceStoreFake{ws: types.Workspace{
		ID: id, Name: "w",
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/home/u/old"}},
		Status:  types.WorkspaceScanned, Profile: mustJSON(workspacescan.WorkspaceProfile{Confidence: "high"}),
		ImageRef: "wardyn/ws:abc", BuiltProfileHash: "abc", ApprovedEgress: []string{"example.com"},
		Requirements:  map[string]types.WorkspaceRequirement{"secret:acme-key": {Level: "required", Provenance: "scan_seeded"}},
		RecordResults: mustJSON(map[string]any{"t": 1}),
	}}
	srv := New(baseTestConfig(h, fake))
	w := do(t, srv, http.MethodPut, "/api/v1/workspaces/"+id.String(), adminToken,
		`{"name":"w","kind":"local_dir","source":"/home/u/new"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got := fake.updated
	if got.ImageRef != "" || got.BuiltProfileHash != "" || got.ApprovedEgress != nil ||
		got.Requirements != nil || got.RecordResults != nil {
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

	_, err := writeEnvAsCode(root, map[string]string{"AGENTS.md": "generated content"})

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
// guard must not break the normal emit (a nested .devcontainer/ path), and a
// FRESH directory (nothing pre-existing) gets the full emit including the
// generated Dockerfile — the O_EXCL guard below must not turn into a
// never-write.
func TestWriteEnvAsCode_WritesNestedFiles(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		".devcontainer/devcontainer.json": `{"name":"x"}`,
		".devcontainer/Dockerfile":        "FROM mcr.microsoft.com/devcontainers/base:ubuntu\n",
		"AGENTS.md":                       "# agents",
	}
	skipped, err := writeEnvAsCode(root, files)
	if err != nil {
		t.Fatalf("writeEnvAsCode: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want none — nothing pre-existed", skipped)
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

// TestWriteEnvAsCode_PreservesExistingDockerfile pins the R5 high-severity
// fix: 342da88 added a generated .devcontainer/Dockerfile to EmitEnvAsCode's
// output, and writeEnvAsCode used to O_TRUNC every emitted key unconditionally
// — silently destroying an operator's own hand-authored Dockerfile the first
// time they clicked "Write into the directory". A pre-existing Dockerfile is
// now left alone and reported in the skipped list; every OTHER emitted key
// (Wardyn's own regenerate-on-demand output) still refreshes as before, so the
// fix does not turn the whole feature into a first-write-only no-op.
func TestWriteEnvAsCode_PreservesExistingDockerfile(t *testing.T) {
	root := t.TempDir()
	const operatorDockerfile = "FROM my-own-base:latest\n# hand-authored, do not touch\n"
	if err := os.MkdirAll(filepath.Join(root, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".devcontainer", "Dockerfile"), []byte(operatorDockerfile), 0o644); err != nil {
		t.Fatal(err)
	}

	files := map[string]string{
		".devcontainer/devcontainer.json": `{"build":{"dockerfile":"Dockerfile"}}`,
		".devcontainer/Dockerfile":        "FROM mcr.microsoft.com/devcontainers/base:ubuntu\n# wardyn generated\n",
		"AGENTS.md":                       "# agents",
	}
	skipped, err := writeEnvAsCode(root, files)
	if err != nil {
		t.Fatalf("writeEnvAsCode: %v", err)
	}
	if len(skipped) != 1 || skipped[0] != ".devcontainer/Dockerfile" {
		t.Fatalf("skipped = %v, want exactly [.devcontainer/Dockerfile]", skipped)
	}
	got, err := os.ReadFile(filepath.Join(root, ".devcontainer", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != operatorDockerfile {
		t.Errorf("operator's Dockerfile was overwritten: got %q, want preserved %q", got, operatorDockerfile)
	}
	// Every other key still refreshes — the guard is Dockerfile-specific, not
	// a blanket "never overwrite" that would defeat the card's own documented
	// "regenerate after a rescan" contract.
	for _, rel := range []string{".devcontainer/devcontainer.json", "AGENTS.md"} {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(got) != files[rel] {
			t.Errorf("%s = %q, want %q (still regenerated)", rel, got, files[rel])
		}
	}
}

// TestWriteEnvAsCode_RefreshesOwnStub pins the R6 fix: the O_EXCL guard used
// to key on existence alone, so the SECOND "Write into the directory"
// click — the card's own "regenerate after a rescan" contract invites exactly
// this — always found the FIRST click's own Dockerfile in the way and
// reported it skipped, permanently closing the regenerate path for this one
// file and making the card's "won't include the agent CLI unless you add
// that yourself" copy false about a file that DOES bake it. A pre-existing
// Dockerfile whose content is byte-identical to what Wardyn would write right
// now (genAgentToolDockerfile is a pure function of tools, so identical
// content can only be Wardyn's own previous stub, never an operator's
// coincidence) now refreshes silently and is never reported skipped.
func TestWriteEnvAsCode_RefreshesOwnStub(t *testing.T) {
	root := t.TempDir()
	const wardynStub = "FROM mcr.microsoft.com/devcontainers/base:ubuntu\n\nRUN set -eu; \\\n    echo installing\n"
	if err := os.MkdirAll(filepath.Join(root, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Simulates an earlier "Write into the directory" click that already
	// planted Wardyn's own stub.
	if err := os.WriteFile(filepath.Join(root, ".devcontainer", "Dockerfile"), []byte(wardynStub), 0o644); err != nil {
		t.Fatal(err)
	}

	files := map[string]string{
		".devcontainer/devcontainer.json": `{"build":{"dockerfile":"Dockerfile"}}`,
		".devcontainer/Dockerfile":        wardynStub, // same tools -> byte-identical regeneration
		"AGENTS.md":                       "# agents",
	}
	skipped, err := writeEnvAsCode(root, files)
	if err != nil {
		t.Fatalf("writeEnvAsCode: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want none — this is Wardyn's own stub, not the operator's", skipped)
	}
	got, err := os.ReadFile(filepath.Join(root, ".devcontainer", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != wardynStub {
		t.Errorf("Dockerfile = %q, want refreshed %q", got, wardynStub)
	}
}
