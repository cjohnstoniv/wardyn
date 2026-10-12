// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// seedLegacy is seedRequestWorkspace in its older five-value shape, for the
// tests written against it.
func seedLegacy(srv *Server, ctx context.Context, spec *types.RunPolicySpec, req *createRunRequest) ([]string, string, int, string, error) {
	seed, refusal := srv.seedRequestWorkspace(ctx, spec, req)
	if refusal != nil {
		return nil, "", refusal.status, refusal.body.Reason, errors.New(refusal.body.Error)
	}
	return seed.ephemeralDirs, seed.imageOwner, 0, "", nil
}

const (
	helloRepo = "https://github.com/octocat/Hello-World.git"
	spoonRepo = "https://github.com/octocat/Spoon-Knife.git"
)

// attachFixture is a run fixture whose store holds wss and whose site config is
// site; every door is reached as the operator.
func attachFixture(t *testing.T, site types.SiteConfig, wss ...types.Workspace) *Server {
	t.Helper()
	srv := providerRunFixture(t, site, &capStore{}, nil)
	srv.cfg.Store.(*integStore).workspaces = wss
	return srv
}

func repoWS(name string, repos ...string) types.Workspace {
	ws := types.Workspace{ID: uuid.New(), Name: name, Status: types.WorkspaceScanned}
	for _, r := range repos {
		ws.Sources = append(ws.Sources, types.WorkspaceSource{Type: types.WorkspaceSourceTypeRepo, Source: r})
	}
	return ws
}

func folderWS(name, path string) types.Workspace {
	return types.Workspace{ID: uuid.New(), Name: name, Status: types.WorkspaceScanned,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: path}}}
}

// attachBody is a background agent run attaching wss in order, plus extra fields.
func attachBody(wss []types.Workspace, extra string) string {
	sels := make([]string, len(wss))
	for i, ws := range wss {
		sels[i] = `{"workspace_id":"` + ws.ID.String() + `"}`
	}
	return `{"agent":"claude-code","task":"t","workspaces":[` + strings.Join(sels, ",") + `]` + extra + `}`
}

// runDoors are the three doors one fold answers for.
var runDoors = []struct{ name, path string }{
	{"create", "/api/v1/runs"},
	{"preflight", "/api/v1/runs/preflight"},
	{"preview", policyPreviewPath},
}

// createAndDispatch posts body to create and returns the run and the spec the
// runner was handed.
func createAndDispatch(t *testing.T, srv *Server, body string) (types.AgentRun, runner.SandboxSpec) {
	t.Helper()
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	srv.WaitBackground()
	fr := srv.cfg.Runner.(*fakeRunner)
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.createCalls == 0 {
		t.Fatalf("run %s was not dispatched", created.ID)
	}
	return created.AgentRun, fr.lastSpec
}

func attachmentsOf(t *testing.T, body []byte) []client.RunAttachment {
	t.Helper()
	var out struct {
		Attachments []client.RunAttachment `json:"attachments"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out.Attachments
}

// TestMultiWorkspace_EveryEntryAttaches: each workspaces[] entry is attached,
// not only one the run otherwise references, and the first is the primary.
func TestMultiWorkspace_EveryEntryAttaches(t *testing.T) {
	one, two := repoWS("payments", helloRepo), repoWS("docs", spoonRepo)
	srv := attachFixture(t, types.SiteConfig{}, one, two)
	run, spec := createAndDispatch(t, srv, attachBody([]types.Workspace{one, two}, ""))
	if want := []uuid.UUID{one.ID, two.ID}; !slices.Equal(run.WorkspaceIDs, want) {
		t.Fatalf("workspace_ids = %v, want %v (the primary first)", run.WorkspaceIDs, want)
	}
	repos := spec.Env["WARDYN_REPOS"]
	for _, dest := range []string{"/home/agent/work/hello-world", "/home/agent/work/spoon-knife"} {
		if !strings.Contains(repos, dest) {
			t.Errorf("WARDYN_REPOS lacks %s: %q", dest, repos)
		}
	}
	w := do(t, srv, http.MethodPost, policyPreviewPath, adminToken, attachBody([]types.Workspace{one, two}, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	got := attachmentsOf(t, w.Body.Bytes())
	if len(got) != 2 || got[0].Name != "payments" || got[0].Target != "/home/agent/work/hello-world" || got[1].Name != "docs" ||
		got[1].Kind != attachmentRepository || got[1].WorkspaceID != two.ID.String() {
		t.Fatalf("preview attachments = %+v", got)
	}
}

// TestMultiWorkspace_AliasMustBeThePrimary: workspace_id beside workspaces[]
// names workspaces[0], or the request is malformed.
func TestMultiWorkspace_AliasMustBeThePrimary(t *testing.T) {
	one, two := repoWS("payments", helloRepo), repoWS("docs", spoonRepo)
	srv := attachFixture(t, types.SiteConfig{}, one, two)
	w := do(t, srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, attachBody([]types.Workspace{one, two}, `,"workspace_id":"`+two.ID.String()+`"`))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), reasonWorkspaceSelectionInvalid) {
		t.Fatalf("conflicting alias: %d %s", w.Code, w.Body.String())
	}
}

// TestOverlap_ResolvedDefaultTargetsCollide: workspaces that name no target
// still collide on the paths they resolve to, in both orders, at every door.
func TestOverlap_ResolvedDefaultTargetsCollide(t *testing.T) {
	folder := folderWS("site", "/srv/site")         // mounts at /home/agent/work
	repo := repoWS("payments", helloRepo)           // clones to /home/agent/work/hello-world
	twin := repoWS("mirror", "octocat/hello-world") // clones to the same directory
	for _, tc := range []struct {
		name string
		wss  []types.Workspace
		msg  string
	}{
		{"nested, folder first", []types.Workspace{folder, repo},
			"/home/agent/work/hello-world is inside /home/agent/work, where site mounts. Two mounts can't nest."},
		{"nested, repo first", []types.Workspace{repo, folder},
			"/home/agent/work/hello-world is inside /home/agent/work, where site mounts. Two mounts can't nest."},
		{"equal", []types.Workspace{repo, twin},
			"/home/agent/work/hello-world is also where payments mounts. Give one of them another path."},
		{"equal, other order", []types.Workspace{twin, repo},
			"/home/agent/work/hello-world is also where mirror mounts. Give one of them another path."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := attachFixture(t, types.SiteConfig{}, folder, repo, twin)
			for _, door := range runDoors {
				w := do(t, srv, http.MethodPost, door.path, adminToken, attachBody(tc.wss, ""))
				var body errorBody
				_ = json.Unmarshal(w.Body.Bytes(), &body)
				if w.Code != http.StatusUnprocessableEntity || body.Reason != reasonWorkspaceTargetOverlap || body.Error != tc.msg {
					t.Fatalf("%s: %d %s, want 422 %q", door.name, w.Code, w.Body.String(), tc.msg)
				}
			}
		})
	}
}

// TestOverlap_DriveTargetIncluded: the drive's resolved path is one of the
// run's targets, in either order against a workspace.
func TestOverlap_DriveTargetIncluded(t *testing.T) {
	ws := repoWS("payments", helloRepo)
	srv := attachFixture(t, types.SiteConfig{}, ws)
	drive := legacyDriveTarget(&types.DriveMount{Target: runner.DriveTarget}, nil)
	for _, target := range []string{runner.DriveTarget, runner.DriveTarget + "/x"} {
		spec := types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: helloRepo, Target: target}}}
		_, _, refusal := srv.runAttachments(createRunRequest{}, spec, []types.Workspace{ws}, []types.Workspace{ws}, drive)
		if refusal == nil || refusal.body.Reason != reasonWorkspaceTargetOverlap || !strings.Contains(refusal.body.Error, "your drive") {
			t.Fatalf("target %s beside the drive: %+v", target, refusal)
		}
	}
	// The drive's path inside a workspace's mount, the other order.
	home := folderWS("home", "/srv/home")
	spec := types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: "/srv/home", Target: "/home/agent"}}}
	if _, _, refusal := srv.runAttachments(createRunRequest{}, spec, []types.Workspace{home}, nil, drive); refusal == nil ||
		refusal.body.Error != "/home/agent/drive is inside /home/agent, where home mounts. Two mounts can't nest." {
		t.Fatalf("drive inside a workspace mount: %+v", refusal)
	}
}

// TestOverlap_NestingInsideOneWorkspaceAllowed: one workspace's own sources may
// nest, so a folder and a repo cloned inside it attach together.
func TestOverlap_NestingInsideOneWorkspaceAllowed(t *testing.T) {
	ws := folderWS("site", "/srv/site")
	ws.Sources = append(ws.Sources, types.WorkspaceSource{Type: types.WorkspaceSourceTypeRepo, Source: helloRepo})
	srv := attachFixture(t, types.SiteConfig{}, ws)
	for _, door := range runDoors[1:] {
		w := do(t, srv, http.MethodPost, door.path, adminToken, attachBody([]types.Workspace{ws}, ""))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", door.name, w.Code, w.Body.String())
		}
		// A20: the host folder attaches with its workspace, shown read-only.
		want := []client.RunAttachment{
			{Kind: attachmentFolder, WorkspaceID: ws.ID.String(), Name: "site", Target: "/home/agent/work", ReadOnly: true},
			{Kind: attachmentRepository, WorkspaceID: ws.ID.String(), Repo: helloRepo, Name: "site", Target: "/home/agent/work/hello-world"},
		}
		if got := attachmentsOf(t, w.Body.Bytes()); !slices.Equal(got, want) {
			t.Fatalf("%s attachments = %+v, want %+v", door.name, got, want)
		}
	}
}

// TestTwoReposOneDefaultDestRefused: two repositories of one workspace that
// derive one directory were a 201 with one clone dropped; now refused.
func TestTwoReposOneDefaultDestRefused(t *testing.T) {
	ws := repoWS("payments", helloRepo, "acme/hello-world")
	srv := attachFixture(t, types.SiteConfig{}, ws)
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, attachBody([]types.Workspace{ws}, ""))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), reasonWorkspaceTargetOverlap) {
		t.Fatalf("two repos one directory: %d %s", w.Code, w.Body.String())
	}
}

// TestConflicts_PinImageADOOrgRefusedAtEveryDoor: two workspaces that pin two
// providers, choose two base images or use two Azure DevOps organisations are
// refused, with the same sentence at create, preflight and preview.
func TestConflicts_PinImageADOOrgRefusedAtEveryDoor(t *testing.T) {
	anthropic, corp := keyProvider("anthropic", "claude-code"), keyProvider("corp", "claude-code")
	anthropic.Name, corp.Name = "Anthropic API", "Corp gateway"
	pinned := func(name, repo, pin string) types.Workspace {
		ws := repoWS(name, repo)
		ws.LLMCred = &types.WorkspaceLLMCred{ProviderRef: pin}
		return ws
	}
	imaged := func(name, repo, image string) types.Workspace {
		ws := repoWS(name, repo)
		ws.BaseImage = &types.WorkspaceBaseImage{Kind: "custom", Image: image}
		return ws
	}
	ado := types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{{
		ID: "ado-row", Kind: types.GitProviderAzureDevOps, BaseURLs: []string{"https://dev.azure.com/acme", "https://dev.azure.com/globex"},
	}}}}
	for _, tc := range []struct {
		name, reason, msg string
		site              types.SiteConfig
		wss               []types.Workspace
	}{
		{"pin", reasonWorkspacePinConflict, workspacePinConflictMsg("payments", "Anthropic API", "docs", "Corp gateway"),
			types.SiteConfig{ModelProviders: providerBlock(anthropic, corp)},
			[]types.Workspace{pinned("payments", helloRepo, "anthropic"), pinned("docs", spoonRepo, "corp")}},
		{"image", reasonImageConflict, imageConflictMsg("payments", "docs"), types.SiteConfig{},
			[]types.Workspace{imaged("payments", helloRepo, "ghcr.io/acme/a:1"), imaged("docs", spoonRepo, "ghcr.io/acme/b:1")}},
		{"azure devops organisation", reasonWorkspaceADOOrgConflict, workspaceADOOrgConflictMsg("payments", "acme", "docs", "globex"), ado,
			[]types.Workspace{repoWS("payments", "https://dev.azure.com/acme/p/_git/r"), repoWS("docs", "https://dev.azure.com/globex/p/_git/s")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := attachFixture(t, tc.site, tc.wss...)
			for _, door := range runDoors {
				w := do(t, srv, http.MethodPost, door.path, adminToken, attachBody(tc.wss, ""))
				var body errorBody
				_ = json.Unmarshal(w.Body.Bytes(), &body)
				if w.Code != http.StatusUnprocessableEntity || body.Reason != tc.reason || body.Error != tc.msg {
					t.Fatalf("%s: %d %s, want 422 %s %q", door.name, w.Code, w.Body.String(), tc.reason, tc.msg)
				}
			}
		})
	}
}

// TestReview_RequirementSecretNamesItsWorkspace: Review's checklist names the
// workspace each required secret comes from, for every attached workspace, one
// that brings no repository or folder of its own included.
func TestReview_RequirementSecretNamesItsWorkspace(t *testing.T) {
	one := repoWS("payments", helloRepo)
	one.Requirements = map[string]types.WorkspaceRequirement{"secret:STRIPE_KEY": {Level: "required", Provenance: "operator_set"}}
	scratch := types.Workspace{ID: uuid.New(), Name: "notebooks", Status: types.WorkspaceScanned,
		Sources:      []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/notebooks"}},
		Requirements: map[string]types.WorkspaceRequirement{"secret:JUPYTER_TOKEN": {Level: "required", Provenance: "operator_set"}}}
	srv := attachFixture(t, types.SiteConfig{}, one, scratch)
	w := do(t, srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, attachBody([]types.Workspace{one, scratch}, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("preflight: %d %s", w.Code, w.Body.String())
	}
	var resp preflightResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"workspace_secret:STRIPE_KEY":    "workspace payments's requirements contract",
		"workspace_secret:JUPYTER_TOKEN": "workspace notebooks's requirements contract",
	}
	for _, it := range resp.SetupItems {
		if by, ok := want[it.ID]; ok {
			if it.RequiredBy != by {
				t.Errorf("%s required by %q, want %q", it.ID, it.RequiredBy, by)
			}
			delete(want, it.ID)
		}
	}
	if len(want) != 0 {
		t.Fatalf("rows missing: %v (items %+v)", want, resp.SetupItems)
	}
}

// startFolderBody is a 0.9 background run with the given workspaces[] entries
// and start folder.
func startFolderBody(sels, folder string) string {
	return `{"mode_contract":"0.9","experience":"background","workload":{"kind":"agent_task","agent":"claude-code","task":"t"},` +
		`"tools":[{"id":"claude-code","kind":"harness"}],"workspaces":[` + sels + `],"start_folder":` + folder + `}`
}

// TestStartFolder_InsideAnAttachmentResolvesAndRemovalRefuses: a starting
// folder inside one repository of one attached workspace becomes the run's one
// working directory; one whose attachment or repository the run no longer
// carries is refused, never moved to another attachment.
func TestStartFolder_InsideAnAttachmentResolvesAndRemovalRefuses(t *testing.T) {
	one, two := repoWS("payments", helloRepo), repoWS("docs", spoonRepo, "octocat/linguist")
	srv := attachFixture(t, types.SiteConfig{}, one, two)
	both := `{"workspace_id":"` + one.ID.String() + `"},{"workspace_id":"` + two.ID.String() + `"}`
	inDocs := `{"kind":"attachment","attachment":"` + two.ID.String() + `","repo":"` + spoonRepo + `","subpath":"src"}`

	_, spec := createAndDispatch(t, srv, startFolderBody(both, inDocs))
	if got := spec.Env["WARDYN_START_FOLDER"]; got != "/home/agent/work/spoon-knife/src" {
		t.Fatalf("WARDYN_START_FOLDER = %q, want the docs clone's src", got)
	}
	w := do(t, srv, http.MethodPost, policyPreviewPath, adminToken, startFolderBody(both, inDocs))
	if w.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	var starts []string
	for _, a := range attachmentsOf(t, w.Body.Bytes()) {
		if a.StartsHere {
			starts = append(starts, a.Repo)
		}
	}
	if !slices.Equal(starts, []string{spoonRepo}) {
		t.Fatalf("starts_here on %v, want exactly the docs clone", starts)
	}

	onlyOne := `{"workspace_id":"` + one.ID.String() + `"}`
	repoGone := `{"workspace_id":"` + one.ID.String() + `"},{"workspace_id":"` + two.ID.String() + `","repos":[{"repo":"octocat/linguist"}]}`
	for name, sels := range map[string]string{"attachment removed": onlyOne, "repository not picked": repoGone} {
		for _, door := range runDoors {
			w := do(t, srv, http.MethodPost, door.path, adminToken, startFolderBody(sels, inDocs))
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), reasonStartFolderInvalid) {
				t.Fatalf("%s at %s: %d %s, want 400 %s", name, door.name, w.Code, w.Body.String(), reasonStartFolderInvalid)
			}
		}
	}
}

// TestStartFolderRepoAndDrive pins the start folder's other forms: a repo only
// with a workspace attachment, and drive_id refused by name until the server
// attaches several drives.
func TestStartFolderRepoAndDrive(t *testing.T) {
	one := repoWS("payments", helloRepo)
	srv := attachFixture(t, types.SiteConfig{}, one)
	sel := `{"workspace_id":"` + one.ID.String() + `"}`
	for _, tc := range []struct {
		name, folder, reason string
		code                 int
	}{
		{"repo with the drive", `{"kind":"attachment","attachment":"drive","repo":"x/y"}`, reasonStartFolderInvalid, 400},
		{"attachment and drive_id", `{"kind":"attachment","attachment":"` + one.ID.String() + `","drive_id":"d1"}`, reasonStartFolderInvalid, 400},
		{"drive_id waits for the drive list", `{"kind":"attachment","drive_id":"d1"}`, reasonRequestFieldUnavailable, 422},
		{"image default carries no repo", `{"kind":"image_default","repo":"x/y"}`, reasonStartFolderInvalid, 400},
	} {
		w := do(t, srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, startFolderBody(sel, tc.folder))
		if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.reason) {
			t.Fatalf("%s: %d %s, want %d %s", tc.name, w.Code, w.Body.String(), tc.code, tc.reason)
		}
	}
	_, spec := createAndDispatch(t, srv, startFolderBody(sel, `{"kind":"image_default"}`))
	if got := spec.Env["WARDYN_START_FOLDER"]; got != startFolderImageDefault {
		t.Fatalf("WARDYN_START_FOLDER = %q, want %q", got, startFolderImageDefault)
	}
}

// TestRepoSelectionSubsetAndRef: repos picks exactly the named repositories at
// their refs (an explicit [] picks none), and an unknown one is refused.
func TestRepoSelectionSubsetAndRef(t *testing.T) {
	ws := repoWS("payments", helloRepo, spoonRepo)
	ws.Sources[1].Ref = "stable"
	srv := attachFixture(t, types.SiteConfig{}, ws)
	body := func(repos string) string {
		return `{"agent":"claude-code","task":"t","workspaces":[{"workspace_id":"` + ws.ID.String() + `","repos":` + repos + `}]}`
	}
	const spoonRecord = "https://github.com/octocat/spoon-knife.git\t/home/agent/work/spoon-knife\toctocat/spoon-knife\t"
	for _, tc := range []struct{ repos, want string }{
		{`[{"repo":"` + spoonRepo + `","ref":"release/1.2"}]`, spoonRecord + "release/1.2"},
		{`[{"repo":"` + spoonRepo + `"}]`, spoonRecord + "stable"},
		{`[]`, ""},
	} {
		srv.cfg.Runner = &fakeRunner{}
		if _, spec := createAndDispatch(t, srv, body(tc.repos)); spec.Env["WARDYN_REPOS"] != tc.want {
			t.Fatalf("repos %s: WARDYN_REPOS = %q, want %q", tc.repos, spec.Env["WARDYN_REPOS"], tc.want)
		}
	}
	w := do(t, srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, body(`[{"repo":"octocat/other"}]`))
	var refusal errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &refusal)
	if w.Code != http.StatusUnprocessableEntity || refusal.Reason != reasonWorkspaceRepoUnknown || refusal.Error != workspaceRepoUnknownMsg("octocat/other", "payments") {
		t.Fatalf("unknown repo: %d %s", w.Code, w.Body.String())
	}
}

// TestWorkspaceTargetApplied: a selection's target is where its one repository
// clones (R2-0048: it was accepted and not applied); on a workspace with more
// than one it names no single place and is refused.
func TestWorkspaceTargetApplied(t *testing.T) {
	one, two := repoWS("payments", helloRepo), repoWS("docs", spoonRepo, "octocat/linguist")
	srv := attachFixture(t, types.SiteConfig{}, one, two)
	_, spec := createAndDispatch(t, srv, `{"agent":"claude-code","task":"t","workspaces":[{"workspace_id":"`+one.ID.String()+`","target":"/home/agent/payments"}]}`)
	if !strings.Contains(spec.Env["WARDYN_REPOS"], "\t/home/agent/payments\t") {
		t.Fatalf("WARDYN_REPOS = %q, want the clone at the selection's target", spec.Env["WARDYN_REPOS"])
	}
	// DR-0103: Review reads the final target, not the source's default.
	w := do(t, srv, http.MethodPost, policyPreviewPath, adminToken, `{"agent":"claude-code","task":"t","workspaces":[{"workspace_id":"`+one.ID.String()+`","target":"/home/agent/payments"}]}`)
	if got := attachmentsOf(t, w.Body.Bytes()); w.Code != http.StatusOK || len(got) != 1 || got[0].Target != "/home/agent/payments" {
		t.Fatalf("preview: %d attachments %+v, want the clone at /home/agent/payments", w.Code, got)
	}
	w = do(t, srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, `{"agent":"claude-code","task":"t","workspaces":[{"workspace_id":"`+two.ID.String()+`","target":"/home/agent/docs"}]}`)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), reasonWorkspaceTargetInvalid) {
		t.Fatalf("target on two repos: %d %s", w.Code, w.Body.String())
	}
}

// TestCloneTargetProjectionMatchesRecords: a workspace read's clone_target is
// the destination buildRepoRecords clones to, for every locator form.
func TestCloneTargetProjectionMatchesRecords(t *testing.T) {
	for _, repo := range []string{"octocat/Hello-World", helloRepo, "HTTPS://GitHub.com/Octocat/Hello-World/",
		"https://dev.azure.com/acme/p/_git/Card%20Auth", "git@ssh.dev.azure.com:v3/acme/p/r"} {
		ws := stampSourceAdmission(types.SiteConfig{}, repoWS("w", repo))
		records, _ := buildRepoRecords("", []types.WorkspaceRepo{{Repo: repo}})
		if records == "" {
			if ws.Sources[0].CloneTarget != "" {
				t.Errorf("%s: clone_target %q for a repo that clones nowhere", repo, ws.Sources[0].CloneTarget)
			}
			continue
		}
		if dest := strings.Split(records, "\t")[1]; ws.Sources[0].CloneTarget != dest {
			t.Errorf("%s: clone_target %q, records dest %q", repo, ws.Sources[0].CloneTarget, dest)
		}
	}
}

// TestReservedDriveRoots: an authored target at or under a drive root is
// refused, and a neighbour of one is not.
func TestReservedDriveRoots(t *testing.T) {
	for _, tgt := range []string{runner.DrivesRoot, runner.DrivesRoot + "/team", runner.SharedRoot, runner.SharedRoot + "/x/y"} {
		if err := runner.ValidateAuthoredTarget(tgt); err == nil || !strings.Contains(err.Error(), "is reserved for drives") {
			t.Errorf("%s: %v, want reserved for drives", tgt, err)
		}
	}
	if err := runner.ValidateAuthoredTarget("/home/agent/drives-report"); err != nil {
		t.Errorf("a neighbour of the drives root: %v", err)
	}
}

// TestRepoSelectionsPresenceSurvivesMarshal (DR-0099): an explicit empty repos
// list reaches the server as [], "no repositories", never dropped into "all".
func TestRepoSelectionsPresenceSurvivesMarshal(t *testing.T) {
	none := []client.RepoSelection{}
	b, err := json.Marshal(client.WorkspaceSelection{WorkspaceID: "w", Repos: &none})
	if err != nil || !strings.Contains(string(b), `"repos":[]`) {
		t.Fatalf("explicit [] marshals as %s (%v)", b, err)
	}
	if b, _ := json.Marshal(client.WorkspaceSelection{WorkspaceID: "w"}); strings.Contains(string(b), "repos") {
		t.Fatalf("absent repos marshals as %s", b)
	}
}

// TestStartFolderEffectiveWorkdirAfterClone (DR-0134) runs resolve_workdir from
// agent-run-lib.sh the way both start paths do after a successful clone (the
// background task: resolve, then cd; the interactive startup: resolve in prep,
// write the marker, the boot pane reads it and cds), and reads the process's
// working directory.
func TestStartFolderEffectiveWorkdirAfterClone(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	lib, err := filepath.Abs("../../deploy/images/common/agent-run-lib.sh")
	if err != nil {
		t.Fatal(err)
	}
	const script = `set -euo pipefail
. "$LIB"
export HOME="$ROOT/home"
workdir="$HOME/work"; mkdir -p "$workdir/repo/.git" "$workdir/repo/src" "$workdir/other"
ln -s "$workdir/other" "$workdir/repo/link"
if [[ "$PATH_KIND" == task ]]; then
  resolve_workdir; cd "$workdir"
else
  resolve_workdir; mkdir -p "$HOME/.wardyn"; printf '%s\n' "$workdir" > "$HOME/.wardyn/workdir"
  workdir="$(cat "$HOME/.wardyn/workdir")"; cd "$workdir"
fi
pwd -P`
	for _, kind := range []string{"task", "interactive"} {
		for _, tc := range []struct {
			name, folder, want string
			fails              bool
		}{
			{"omitted keeps the clone", "", "/home/work/repo", false},
			{"image default keeps the image's own directory", "image_default", "/home/work", false},
			{"a folder inside the clone", "@/home/work/repo/src", "/home/work/repo/src", false},
			{"a link out of the clone fails", "@/home/work/repo/link", "", true},
			{"a missing folder fails", "@/home/work/repo/gone", "", true},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				root, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command("bash", "-c", script)
				cmd.Env = append(os.Environ(), "LIB="+lib, "ROOT="+root, "PATH_KIND="+kind,
					"WARDYN_START_FOLDER="+strings.Replace(tc.folder, "@", root, 1))
				out, err := cmd.Output()
				if tc.fails {
					if err == nil {
						t.Fatalf("started in %s, want a refusal", out)
					}
					return
				}
				if err != nil {
					t.Fatalf("run: %v", err)
				}
				if got := strings.TrimPrefix(strings.TrimSpace(string(out)), root); got != tc.want {
					t.Fatalf("working directory = %s, want %s", got, tc.want)
				}
			})
		}
	}
}

// TestPG_MultiWorkspace_EveryEntryAttaches is the attach on the real store:
// both workspaces[] entries reach the mount map, the primary first.
func TestPG_MultiWorkspace_EveryEntryAttaches(t *testing.T) {
	h, _ := newRunOwnerPGHarness(t)
	h.srv.cfg.Runner = &fakeRunner{}
	h.srv.cfg.DefaultPolicy = types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2}
	ctx := context.Background()
	var wss []types.Workspace
	for _, ws := range []types.Workspace{repoWS("payments", helloRepo), repoWS("docs", spoonRepo)} {
		created, err := h.srv.cfg.Store.CreateWorkspace(ctx, ws)
		if err != nil {
			t.Fatalf("CreateWorkspace: %v", err)
		}
		wss = append(wss, created)
	}
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, attachBody(wss, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("preflight: %d %s", w.Code, w.Body.String())
	}
	got := attachmentsOf(t, w.Body.Bytes())
	if len(got) != 2 || got[0].WorkspaceID != wss[0].ID.String() || got[1].WorkspaceID != wss[1].ID.String() {
		t.Fatalf("attachments = %+v, want both workspaces, the primary first", got)
	}
}
