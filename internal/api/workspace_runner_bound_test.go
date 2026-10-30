// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// A member's local_dir source may name one of their own runners (runner_id): the folder lives on that
// machine, not on the control plane's host. The control plane checks its text only and places a run with
// it on that runner alone; every door that would read or bind it on the control plane's host refuses it.

// boundLaptopRoot is a member root that exists only on the member's runner, never on the test host.
const (
	boundLaptopRoot = "/Users/wardyn-bound-member/src"
	boundFolder     = boundLaptopRoot + "/payments-api"
)

// boundSource is a local_dir source at path bound to runnerID.
func boundSource(path string, runnerID uuid.UUID) types.WorkspaceSource {
	return types.WorkspaceSource{Type: types.WorkspaceSourceTypeLocalDir, Path: path, RunnerID: &runnerID}
}

// workspaceBody is a POST /workspaces body named name with srcs.
func workspaceBody(t *testing.T, name string, srcs ...types.WorkspaceSource) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"name": name, "sources": srcs})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// advertiseRunnerRoots registers a session for id whose caps advertise roots, the runner owner's
// --drive-root, as a connected runner's caps event does, from a runnerd that binds bound folders.
func advertiseRunnerRoots(t *testing.T, s *Server, id uuid.UUID, roots ...string) {
	t.Helper()
	advertiseRunnerCaps(t, s, id, true, roots...)
}

// advertiseRunnerCaps is advertiseRunnerRoots with bound_folders as given: false is an older runnerd.
func advertiseRunnerCaps(t *testing.T, s *Server, id uuid.UUID, boundFolders bool, roots ...string) {
	t.Helper()
	caps := laptopCaps(types.CC1, types.CC2)
	caps.AllowedRoots = roots
	caps.BoundFolders = boundFolders
	data, err := json.Marshal(caps)
	if err != nil {
		t.Fatal(err)
	}
	sub := remote.New(id.String(), &capsTransport{online: true}, remote.Options{})
	sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
	s.runnerStreams.subsMu.Lock()
	defer s.runnerStreams.subsMu.Unlock()
	if s.runnerStreams.subs == nil {
		s.runnerStreams.subs = map[uuid.UUID]runnerEndpoint{}
	}
	s.runnerStreams.subs[id] = runnerEndpoint{sub: sub, link: &remote.Link{}}
}

func assertNoFolderOnThisHost(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(boundLaptopRoot); err == nil {
		t.Skipf("%s exists on this host; these tests need a folder only the runner has", boundLaptopRoot)
	}
}

// A member's folder that exists only on their own runner is accepted when the source names that runner,
// read-only and writable, and the library row carries the binding as part of its identity.
func TestWorkspaceOwnership_RunnerBoundFolderNotOnThisHost(t *testing.T) {
	assertNoFolderOnThisHost(t)
	srv, st, _ := ownerRunnerHarness(t, runner.UserMountPolicy{Roots: []string{boundLaptopRoot}, WritableRoots: []string{boundLaptopRoot}})
	mine := st.add(ownerMemberSub, types.RunnerOwnerPerson, types.RunnerClaimed)
	member := ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleUser)

	for name, writable := range map[string]bool{"read-only": false, "writable": true} {
		src := boundSource(boundFolder, mine)
		src.Writable = writable
		w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", member, workspaceBody(t, name, src))
		if w.Code != http.StatusCreated {
			t.Fatalf("%s folder on the member's own runner: code = %d, want 201; body=%s", name, w.Code, w.Body.String())
		}
		var created types.Workspace
		if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		if len(created.Sources) != 1 || created.Sources[0].RunnerID == nil || *created.Sources[0].RunnerID != mine {
			t.Errorf("%s: created sources = %+v, want the folder bound to %s", name, created.Sources, mine)
		}
	}
	if !slices.ContainsFunc(st.upserted, func(s types.Source) bool {
		return s.Locator == boundFolder && s.RunnerID != nil && *s.RunnerID == mine
	}) {
		t.Errorf("library rows written = %+v, want %s bound to %s", st.upserted, boundFolder, mine)
	}
}

// A binding is accepted only to the caller's own claimed, unrevoked runner, for a local_dir source whose
// text clears the member rules; an operator never binds. Nothing is written for a refusal.
func TestWorkspaceOwnership_RunnerBoundRefusals(t *testing.T) {
	assertNoFolderOnThisHost(t)
	srv, st, _ := ownerRunnerHarness(t, runner.UserMountPolicy{Roots: []string{boundLaptopRoot}, WritableRoots: []string{boundLaptopRoot + "/w"}})
	mine := st.add(ownerMemberSub, types.RunnerOwnerPerson, types.RunnerClaimed)
	theirs := st.add(ownerOtherSub, types.RunnerOwnerPerson, types.RunnerClaimed)
	org := st.add("", types.RunnerOwnerOrg, types.RunnerClaimed)
	revoked := st.add(ownerMemberSub, types.RunnerOwnerPerson, types.RunnerRevoked)
	unclaimed := st.add(ownerMemberSub, types.RunnerOwnerPerson, types.RunnerUnclaimed)
	member := ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleUser)
	admin := ssoSession(t, "sub-owner-admin", "admin@corp.example", oidc.RoleAdmin)

	writable := boundSource(boundFolder, mine)
	writable.Writable = true
	scratch := types.WorkspaceSource{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/scratch", RunnerID: &mine}
	for _, c := range []struct {
		name    string
		session *http.Cookie
		src     types.WorkspaceSource
		reason  string // "" = the status alone, with a sentence naming runner_id
	}{
		{"another person's runner", member, boundSource(boundFolder, theirs), reasonWorkspaceSourcesNotAllowed},
		{"an organisation's runner", member, boundSource(boundFolder, org), reasonWorkspaceSourcesNotAllowed},
		{"a revoked runner of their own", member, boundSource(boundFolder, revoked), reasonWorkspaceSourcesNotAllowed},
		{"an unclaimed runner of their own", member, boundSource(boundFolder, unclaimed), reasonWorkspaceSourcesNotAllowed},
		{"a runner that does not exist", member, boundSource(boundFolder, uuid.New()), reasonWorkspaceSourcesNotAllowed},
		{"a credential folder", member, boundSource(boundLaptopRoot+"/.ssh", mine), reasonWorkspaceSourcesNotAllowed},
		{"outside the roots", member, boundSource("/Users/someone-else/src/app", mine), reasonWorkspaceSourcesNotAllowed},
		{"writable outside the writable roots", member, writable, reasonWorkspaceSourcesNotAllowed},
		{"an operator setting runner_id", admin, boundSource(boundFolder, mine), ""},
		{"runner_id on a source that is not a folder", member, scratch, ""},
	} {
		before, _ := st.ListWorkspaces(context.Background())
		w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", c.session, workspaceBody(t, "refused", c.src))
		switch {
		case w.Code != http.StatusBadRequest:
			t.Errorf("%s: code = %d, want 400; body=%s", c.name, w.Code, w.Body.String())
		case c.reason != "" && reasonOf(t, w) != c.reason:
			t.Errorf("%s: reason = %q, want %q; body=%s", c.name, reasonOf(t, w), c.reason, w.Body.String())
		case c.reason == "" && !strings.Contains(w.Body.String(), "runner_id"):
			t.Errorf("%s: body = %s, want the refusal to name runner_id", c.name, w.Body.String())
		}
		if after, _ := st.ListWorkspaces(context.Background()); len(after) != len(before) {
			t.Errorf("%s: a refused binding stored a workspace", c.name)
		}
	}

	// An operator editing a member's workspace adds or moves no binding; a binding the workspace already
	// has, unchanged, round-trips.
	spare := st.add(ownerMemberSub, types.RunnerOwnerPerson, types.RunnerClaimed)
	create := func(name string, src types.WorkspaceSource) uuid.UUID {
		t.Helper()
		w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", member, workspaceBody(t, name, src))
		if w.Code != http.StatusCreated {
			t.Fatalf("create %s = %d %s", name, w.Code, w.Body.String())
		}
		return decodeJSON[types.Workspace](t, w).ID
	}
	unbound := create("scratch-only", types.WorkspaceSource{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/scratch"})
	bound := create("bound", boundSource(boundFolder, mine))
	for _, c := range []struct {
		name string
		id   uuid.UUID
		src  types.WorkspaceSource
		want int
	}{
		{"an operator PUT adding a binding", unbound, boundSource(boundFolder, mine), http.StatusBadRequest},
		{"an operator PUT keeping the binding unchanged", bound, boundSource(boundFolder, mine), http.StatusOK},
		{"an operator PUT moving the binding to another of the member's runners", bound, boundSource(boundFolder, spare), http.StatusBadRequest},
	} {
		w := doSSO(t, srv, http.MethodPut, "/api/v1/workspaces/"+c.id.String(), admin, workspaceBody(t, "edited", c.src))
		if w.Code != c.want {
			t.Errorf("%s: code = %d, want %d; body=%s", c.name, w.Code, c.want, w.Body.String())
		}
		if c.want == http.StatusBadRequest && !strings.Contains(w.Body.String(), "runner_id") {
			t.Errorf("%s: body = %s, want the refusal to name runner_id", c.name, w.Body.String())
		}
	}
}

// Guard: a folder that names no runner is checked on the control plane's host exactly as before, so a
// path that does not resolve here is still refused, and one that does is accepted.
func TestWorkspaceOwnership_UnboundFolderKeepsHostCheck(t *testing.T) {
	assertNoFolderOnThisHost(t)
	root, project := memberProjectRoot(t)
	srv, _, _ := ownerHarness(t, runner.UserMountPolicy{Roots: []string{root, boundLaptopRoot}})
	member := ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleUser)

	unbound := types.WorkspaceSource{Type: types.WorkspaceSourceTypeLocalDir, Path: boundFolder}
	w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", member, workspaceBody(t, "nowhere", unbound))
	if w.Code != http.StatusBadRequest || reasonOf(t, w) != reasonWorkspaceSourcesNotAllowed || !strings.Contains(w.Body.String(), "resolve on this host") {
		t.Errorf("an unbound folder missing on this host: code = %d, want 400 %s from the host check; body=%s", w.Code, reasonWorkspaceSourcesNotAllowed, w.Body.String())
	}
	here := types.WorkspaceSource{Type: types.WorkspaceSourceTypeLocalDir, Path: project}
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", member, workspaceBody(t, "here", here)); w.Code != http.StatusCreated {
		t.Errorf("an unbound folder on this host: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
}

// The workspace read says how each binding stands now, from the runner row: claimed, revoked, missing,
// or not the owner's. It is a read projection, never stored.
func TestWorkspaceRead_ShowsRunnerState(t *testing.T) {
	srv, st, _ := ownerRunnerHarness(t, runner.UserMountPolicy{Roots: []string{boundLaptopRoot}})
	mine := st.add(ownerMemberSub, types.RunnerOwnerPerson, types.RunnerClaimed)
	revoked := st.add(ownerMemberSub, types.RunnerOwnerPerson, types.RunnerRevoked)
	theirs := st.add(ownerOtherSub, types.RunnerOwnerPerson, types.RunnerClaimed)
	org := st.add("", types.RunnerOwnerOrg, types.RunnerClaimed)
	want := map[string]string{
		boundLaptopRoot + "/claimed": "claimed",
		boundLaptopRoot + "/revoked": "revoked",
		boundLaptopRoot + "/missing": "missing",
		boundLaptopRoot + "/theirs":  "not_owner",
		boundLaptopRoot + "/org":     "not_owner",
		boundLaptopRoot + "/unbound": "",
	}
	id := st.put(types.Workspace{OwnedBy: ownerMemberSub, Status: types.WorkspaceScanned, Sources: []types.WorkspaceSource{
		boundSource(boundLaptopRoot+"/claimed", mine),
		boundSource(boundLaptopRoot+"/revoked", revoked),
		boundSource(boundLaptopRoot+"/missing", uuid.New()),
		boundSource(boundLaptopRoot+"/theirs", theirs),
		boundSource(boundLaptopRoot+"/org", org),
		{Type: types.WorkspaceSourceTypeLocalDir, Path: boundLaptopRoot + "/unbound"},
	}})
	member := ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleUser)

	w := doSSO(t, srv, http.MethodGet, "/api/v1/workspaces/"+id.String(), member, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", w.Code, w.Body.String())
	}
	var got types.Workspace
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sources) != len(want) {
		t.Fatalf("sources = %+v, want %d", got.Sources, len(want))
	}
	// runner_name is the bound runner's name, empty only when its row is gone.
	wantName := map[string]string{boundLaptopRoot + "/revoked": "laptop", boundLaptopRoot + "/missing": ""}
	for _, src := range got.Sources {
		if src.RunnerState != want[src.Path] {
			t.Errorf("%s: runner_state = %q, want %q", src.Path, src.RunnerState, want[src.Path])
		}
		if name, ok := wantName[src.Path]; ok && src.RunnerName != name {
			t.Errorf("%s: runner_name = %q, want %q", src.Path, src.RunnerName, name)
		}
	}
	stored, _ := st.GetWorkspace(context.Background(), id)
	for _, src := range stored.Sources {
		if src.RunnerState != "" || src.RunnerName != "" {
			t.Errorf("the stored row carries runner_state %q, runner_name %q for %s; they are a read projection", src.RunnerState, src.RunnerName, src.Path)
		}
	}
}

// runner_state and runner_name are read projections: a PUT that round-trips a read, or carries any value
// for them, is neither refused for them nor changes anything by them.
func TestWorkspaceUpdate_IgnoresRunnerStateInBody(t *testing.T) {
	assertNoFolderOnThisHost(t)
	srv, st, _ := ownerRunnerHarness(t, runner.UserMountPolicy{Roots: []string{boundLaptopRoot}})
	mine := st.add(ownerMemberSub, types.RunnerOwnerPerson, types.RunnerClaimed)
	member := ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleUser)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", member, workspaceBody(t, "laptop-folder", boundSource(boundFolder, mine)))
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	var created types.Workspace
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	storedSources := func() string {
		t.Helper()
		ws, err := st.GetWorkspace(context.Background(), created.ID)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(ws.Sources)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	before := storedSources()

	src := boundSource(boundFolder, mine)
	src.RunnerState, src.RunnerName = types.WorkspaceSourceRunnerRevoked, "someone-else"
	w = doSSO(t, srv, http.MethodPut, "/api/v1/workspaces/"+created.ID.String(), member, workspaceBody(t, "laptop-folder", src))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT with runner_state %q = %d %s, want 200", src.RunnerState, w.Code, w.Body.String())
	}
	if after := storedSources(); after != before {
		t.Errorf("stored sources after the PUT = %s, want them unchanged: %s", after, before)
	}
	if strings.Contains(before, "runner_state") || strings.Contains(before, "runner_name") {
		t.Errorf("stored sources = %s, want no read projection persisted", before)
	}
}

// A runner whose wardyn-runnerd does not say bound_folders is older than the checks that bind a bound
// folder, so the folder is never verified on it and the run is refused, saying to update the runner.
func TestRunCreate_RunnerBoundFolderNeedsBoundFoldersCap(t *testing.T) {
	mine := uuid.New()
	s := &Server{}
	advertiseRunnerCaps(t, s, mine, false, boundLaptopRoot)
	mounts := []runner.Mount{{Source: boundFolder, Target: composerWorkspaceTarget, ReadOnly: true, MemberAuthored: true, BoundRunner: mine.String()}}
	if got := s.mountPathsOn(mine, mounts); got["SandboxSpec.Mounts[0]"] {
		t.Errorf("mountPathsOn(a runner without bound_folders) = %v, want the folder unverified", got)
	}
	refusal := s.boundFolderPlacementRefusal(&mine, types.PlacementLocal, mounts)
	if refusal == nil || refusal.body.Reason != string(placement.ReasonPlacementLocalPath) || !strings.Contains(refusal.body.Error, "update the runner") {
		t.Errorf("placement on a runner without bound_folders = %+v, want %s saying to update the runner", refusal, placement.ReasonPlacementLocalPath)
	}
	advertiseRunnerCaps(t, s, mine, true, boundLaptopRoot)
	if refusal := s.boundFolderPlacementRefusal(&mine, types.PlacementLocal, mounts); refusal != nil {
		t.Errorf("placement on the updated runner = %+v, want none", refusal)
	}
}

// boundRefStore is wsRefStore with the runners its sources are bound to.
type boundRefStore struct {
	wsRefStore
	boundRunners
}

func readOnlyMount(src string) types.RunPolicySpec {
	ro := true
	return types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: src, Target: composerWorkspaceTarget, ReadOnly: &ro}}}
}

// The run gate (validateWorkspaceSources -> userMountAllowed) checks a bound folder's text against its
// owner's roots and never looks for it on the control plane's host: the folder passes, and what text can
// show (a credential folder, writable outside the writable roots) is still refused.
func TestValidateWorkspaceSources_RunnerBoundIsTextOnly(t *testing.T) {
	assertNoFolderOnThisHost(t)
	runners := boundRunners{rows: map[uuid.UUID]types.Runner{}}
	mine := runners.add(ownerMemberSub, types.RunnerOwnerPerson, types.RunnerClaimed)
	writableDir := boundLaptopRoot + "/w/app"
	ws := types.Workspace{ID: uuid.New(), OwnedBy: ownerMemberSub, Sources: []types.WorkspaceSource{
		boundSource(boundFolder, mine), boundSource(boundLaptopRoot+"/.ssh", mine), boundSource(writableDir, mine),
	}}
	srv := &Server{cfg: Config{
		Store:      boundRefStore{wsRefStore: wsRefStore{ws: []types.Workspace{ws}}, boundRunners: runners},
		UserMounts: runner.UserMountPolicy{Roots: []string{boundLaptopRoot}, WritableRoots: []string{boundLaptopRoot + "/w"}},
	}}
	writable := func(src string) types.RunPolicySpec {
		rw := false
		return types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: src, Target: composerWorkspaceTarget, ReadOnly: &rw}}}
	}
	for name, spec := range map[string]types.RunPolicySpec{
		"the folder, read-only":                  readOnlyMount(boundFolder),
		"a folder inside the writable roots, rw": writable(writableDir),
	} {
		if code, reason, err := srv.validateWorkspaceSources(context.Background(), spec, runCarrierRule{}); err != nil {
			t.Errorf("%s: %d %s %v, want it to pass on its text", name, code, reason, err)
		}
	}
	for name, spec := range map[string]types.RunPolicySpec{
		"a credential folder":                 readOnlyMount(boundLaptopRoot + "/.ssh"),
		"writable outside the writable roots": writable(boundFolder),
	} {
		code, reason, err := srv.validateWorkspaceSources(context.Background(), spec, runCarrierRule{})
		if err == nil || code != http.StatusUnprocessableEntity || reason != reasonWorkspaceSourceMountNotAllowed {
			t.Errorf("%s: %d %q %v, want 422 %s", name, code, reason, err, reasonWorkspaceSourceMountNotAllowed)
		}
	}
}

// One path held by a bound and an unbound source, or bound to two runners, has no one place to run:
// the run is refused as a placement it cannot have, and the posture binds the path only when every
// holder agrees.
func TestValidateWorkspaceSources_SamePathBoundAndUnboundRefused(t *testing.T) {
	assertNoFolderOnThisHost(t)
	runners := boundRunners{rows: map[uuid.UUID]types.Runner{}}
	mine := runners.add(ownerMemberSub, types.RunnerOwnerPerson, types.RunnerClaimed)
	second := runners.add(ownerMemberSub, types.RunnerOwnerPerson, types.RunnerClaimed)
	bound := types.Workspace{ID: uuid.New(), Name: "bound", OwnedBy: ownerMemberSub, Sources: []types.WorkspaceSource{boundSource(boundFolder, mine)}}
	unbound := types.Workspace{ID: uuid.New(), Name: "unbound", OwnedBy: ownerMemberSub,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: boundFolder}}}
	elsewhere := types.Workspace{ID: uuid.New(), Name: "elsewhere", OwnedBy: ownerMemberSub, Sources: []types.WorkspaceSource{boundSource(boundFolder, second)}}
	server := func(all ...types.Workspace) *Server {
		return &Server{cfg: Config{
			Store:      boundRefStore{wsRefStore: wsRefStore{ws: all}, boundRunners: runners},
			UserMounts: runner.UserMountPolicy{Roots: []string{boundLaptopRoot}},
		}}
	}

	if code, reason, err := server(bound).validateWorkspaceSources(context.Background(), readOnlyMount(boundFolder), runCarrierRule{attached: []types.Workspace{bound}}); err != nil {
		t.Fatalf("one bound holder: %d %s %v, want it to pass", code, reason, err)
	}
	for name, attached := range map[string][]types.Workspace{
		"bound and unbound": {bound, unbound},
		"two runners":       {bound, elsewhere},
	} {
		code, reason, err := server(attached...).validateWorkspaceSources(context.Background(), readOnlyMount(boundFolder), runCarrierRule{attached: attached})
		if err == nil || code != http.StatusUnprocessableEntity || reason != string(placement.ReasonPlacementLocalPath) {
			t.Errorf("%s: %d %q %v, want 422 %s", name, code, reason, err, placement.ReasonPlacementLocalPath)
		}
		if got, ok := server(attached...).userMountPosture(attached).Bound[boundFolder]; ok {
			t.Errorf("%s: posture binds the path to %s; holders disagree, so it must bind none", name, got)
		}
	}
	if got := server(bound).userMountPosture([]types.Workspace{bound}).Bound[boundFolder]; got != mine {
		t.Errorf("one bound holder: posture binds the path to %s, want %s", got, mine)
	}
}

// A record or verify session runs on the organisation's own executor, so wiring a workspace for one never
// turns a folder bound to a runner into a mount there; an unbound folder still mounts.
func TestWireWorkspaceSource_RefusesRunnerBound(t *testing.T) {
	var run types.AgentRun
	var policy types.RunPolicySpec
	ws := types.Workspace{ID: uuid.New(), Sources: []types.WorkspaceSource{boundSource(boundFolder, uuid.New())}}
	if _, _, err := wireWorkspaceSource(&run, &policy, ws); err == nil {
		t.Error("wiring a session over a runner-bound folder succeeded")
	}
	if len(policy.WorkspaceMounts) != 0 || run.WorkspacePath != "" {
		t.Errorf("mounts = %+v, workspace path = %q; want neither for a runner-bound folder", policy.WorkspaceMounts, run.WorkspacePath)
	}

	run, policy = types.AgentRun{}, types.RunPolicySpec{}
	ws.Sources = []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: boundFolder}}
	if _, _, err := wireWorkspaceSource(&run, &policy, ws); err != nil || len(policy.WorkspaceMounts) != 1 {
		t.Errorf("an unbound folder: err %v, mounts %+v; want it mounted", err, policy.WorkspaceMounts)
	}
}

// boundRecordStore is the record door's store with the runners its sources are bound to.
type boundRecordStore struct {
	recordListStore
	boundRunners
}

// The record and verify doors refuse every runner-bound folder before anything launches: by the stale
// reason when the binding no longer holds, and otherwise by the session's own reason.
func TestWorkspaceSession_RefusesRunnerBoundSource(t *testing.T) {
	for _, c := range []struct {
		name   string
		state  types.RunnerState
		reason string
	}{
		{"bound to their claimed runner", types.RunnerClaimed, "workspace_session_runner_bound"},
		{"bound to their revoked runner", types.RunnerRevoked, "workspace_source_runner_unavailable"},
	} {
		for door, body := range map[string]string{"record": `{"name":"capture"}`, "verify": `{"name":"capture","confined":true}`} {
			t.Run(c.name+"/"+door, func(t *testing.T) {
				h := newHarness(t)
				runners := boundRunners{rows: map[uuid.UUID]types.Runner{}}
				rid := runners.add(ownerMemberSub, types.RunnerOwnerPerson, c.state)
				ws := types.Workspace{ID: uuid.New(), Status: types.WorkspaceScanned, OwnedBy: ownerMemberSub,
					Sources: []types.WorkspaceSource{boundSource(boundFolder, rid)}}
				fake := newRecordLLMModeStore(ws)
				cfg := baseTestConfig(h, boundRecordStore{recordListStore{fake}, runners})
				fr := &fakeRunner{}
				cfg.Runner = fr
				cfg.Broker = h.broker
				cfg.UserMounts = runner.UserMountPolicy{Roots: []string{boundLaptopRoot}}
				srv := New(cfg)

				w := do(t, srv, http.MethodPost, "/api/v1/workspaces/"+ws.ID.String()+"/record", adminToken, body)
				if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != c.reason {
					t.Fatalf("%s: code = %d, want 422 %s; body=%s", door, w.Code, c.reason, w.Body.String())
				}
				if fr.createCalls != 0 || len(fake.runs) != 0 {
					t.Errorf("CreateSandbox calls = %d, run rows = %d; want none for a refused session", fr.createCalls, len(fake.runs))
				}
			})
		}
	}
}

// boundScanStore is partialScanStore with the runners its sources are bound to.
type boundScanStore struct {
	*partialScanStore
	boundRunners
}

// The workspace scan reads folders on the control plane's host, so it never reads a runner-bound one:
// a stale binding refuses the scan by name, and a live one is skipped with the reason audited while the
// workspace's other folders are scanned.
func TestScan_SkipsRunnerBoundSource(t *testing.T) {
	assertNoFolderOnThisHost(t)
	root, inside := memberProjectRoot(t)
	for _, c := range []struct {
		name   string
		state  types.RunnerState
		code   int
		reason string
	}{
		{"bound to their claimed runner", types.RunnerClaimed, http.StatusOK, ""},
		{"bound to their revoked runner", types.RunnerRevoked, http.StatusUnprocessableEntity, "workspace_source_runner_unavailable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			runners := boundRunners{rows: map[uuid.UUID]types.Runner{}}
			rid := runners.add(ownerMemberSub, types.RunnerOwnerPerson, c.state)
			boundID, hostID := uuid.New(), uuid.New()
			fake := &partialScanStore{sources: map[uuid.UUID]types.Source{
				boundID: {ID: boundID, Kind: types.SourceLocalDir, Locator: boundFolder, RunnerID: &rid, Status: types.WorkspacePendingScan},
				hostID:  {ID: hostID, Kind: types.SourceLocalDir, Locator: inside},
			}}
			cfg := baseTestConfig(h, boundScanStore{fake, runners})
			cfg.UserMounts = runner.UserMountPolicy{Roots: []string{root, boundLaptopRoot}}
			srv := New(cfg)
			ws := types.Workspace{ID: uuid.New(), OwnedBy: ownerMemberSub, Attachments: []types.WorkspaceAttachment{{SourceID: &boundID}, {SourceID: &hostID}}}
			rec := httptest.NewRecorder()
			srv.scanAttachedSources(rec, httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/"+ws.ID.String()+"/scan", nil), ws)

			if rec.Code != c.code || (c.reason != "" && reasonOf(t, rec) != c.reason) {
				t.Fatalf("code = %d, want %d %s; body=%s", rec.Code, c.code, c.reason, rec.Body.String())
			}
			if slices.Contains(fake.scanned, boundID) {
				t.Errorf("scanned = %v: the runner-bound folder was read on the control plane's host", fake.scanned)
			}
			if c.code != http.StatusOK {
				return
			}
			if !slices.Equal(fake.scanned, []uuid.UUID{hostID}) {
				t.Errorf("scanned = %v, want the host folder alone", fake.scanned)
			}
			named := slices.ContainsFunc(h.audit.snapshot(), func(ev types.AuditEvent) bool {
				return ev.Action == "workspace.scan" && strings.Contains(string(ev.Data), boundID.String())
			})
			if !named {
				t.Error("no workspace.scan audit row names the skipped runner-bound source")
			}
		})
	}
}

// boundSourceStore is scanSourceStore with the runners its sources are bound to.
type boundSourceStore struct {
	*scanSourceStore
	boundRunners
}

// The library scan door refuses every runner-bound source, stale or not, and leaves its status alone,
// even when a folder exists at that path on the control plane's host. The same folder unbound scans.
func TestScan_LibraryDoorRefusesRunnerBound(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runners := boundRunners{rows: map[uuid.UUID]types.Runner{}}
	for name, rid := range map[string]uuid.UUID{
		"claimed": runners.add(ownerMemberSub, types.RunnerOwnerPerson, types.RunnerClaimed),
		"revoked": runners.add(ownerMemberSub, types.RunnerOwnerPerson, types.RunnerRevoked),
		"missing": uuid.New(),
	} {
		t.Run(name, func(t *testing.T) {
			id := uuid.New()
			st := &scanSourceStore{sources: map[uuid.UUID]types.Source{
				id: {ID: id, Kind: types.SourceLocalDir, Locator: dir, RunnerID: &rid, Status: types.WorkspacePendingScan},
			}}
			srv := New(baseTestConfig(newHarness(t), boundSourceStore{st, runners}))
			w := do(t, srv, http.MethodPost, "/api/v1/sources/"+id.String()+"/scan", adminToken, "")
			if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != "source_scan_runner_bound" || !strings.Contains(w.Body.String(), rid.String()) {
				t.Fatalf("= %d %s, want 422 source_scan_runner_bound naming the runner", w.Code, w.Body.String())
			}
			if len(st.statuses) != 0 || st.sources[id].Status != types.WorkspacePendingScan {
				t.Errorf("status writes %v, status %q; want the source left pending_scan", st.statuses, st.sources[id].Status)
			}
		})
	}
	id := uuid.New()
	st := &scanSourceStore{sources: map[uuid.UUID]types.Source{id: {ID: id, Kind: types.SourceLocalDir, Locator: dir}}}
	srv := New(baseTestConfig(newHarness(t), boundSourceStore{st, runners}))
	if w := do(t, srv, http.MethodPost, "/api/v1/sources/"+id.String()+"/scan", adminToken, ""); w.Code != http.StatusOK {
		t.Errorf("the same folder unbound = %d %s, want 200", w.Code, w.Body.String())
	}
}

// Env-as-code writes into a folder on the control plane's host, so it drops runner-bound sources: with
// none left it is the no-folder refusal, and otherwise it writes into the first unbound folder.
func TestWriteEnvAsCode_SkipsRunnerBoundSource(t *testing.T) {
	profile := mustJSON(workspacescan.WorkspaceProfile{Languages: []string{"Go"}, PackageManagers: []string{"go"}, Confidence: "high"})
	bound, host := t.TempDir(), t.TempDir()
	rid := uuid.New()
	boundOnly := types.Workspace{ID: uuid.New(), Kind: types.WorkspaceKindLocalDir, Source: bound, Profile: profile,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: bound, Target: "/home/agent/work", RunnerID: &rid}}}
	write := func(ws types.Workspace) *httptest.ResponseRecorder {
		srv := New(baseTestConfig(newHarness(t), &workspaceStoreFake{ws: ws}))
		return do(t, srv, http.MethodPost, "/api/v1/workspaces/"+ws.ID.String()+"/env-as-code/write", adminToken, "")
	}

	if w := write(boundOnly); w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonWorkspaceEnvcodeNoLocalDir {
		t.Errorf("only a runner-bound folder = %d %s, want 422 %s", w.Code, w.Body.String(), reasonWorkspaceEnvcodeNoLocalDir)
	}
	mixed := boundOnly
	mixed.ID = uuid.New()
	mixed.Sources = append(slices.Clone(boundOnly.Sources), types.WorkspaceSource{Type: types.WorkspaceSourceTypeLocalDir, Path: host, Target: "/home/agent/host"})
	w := write(mixed)
	if w.Code != http.StatusOK {
		t.Fatalf("a bound and a host folder = %d %s, want 200", w.Code, w.Body.String())
	}
	var got struct {
		Written map[string]string `json:"written_files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Written) == 0 {
		t.Fatalf("written = %+v (err %v), want the files", got.Written, err)
	}
	for rel := range got.Written {
		if _, err := os.Stat(filepath.Join(host, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s is not in the host folder: %v", rel, err)
		}
	}
	if entries, _ := os.ReadDir(bound); len(entries) != 0 {
		t.Errorf("the runner-bound path on this host holds %d entries, want none written", len(entries))
	}
}

// Dispatch on the organisation's executor never binds a runner-bound folder, whatever placement the run
// row says: it fails the run before the sandbox. The same mount with no binding dispatches.
func TestDispatch_RemoteNeverBindsRunnerBoundFolder(t *testing.T) {
	ro := true
	policy := types.RunPolicySpec{MinConfinementClass: types.CC1,
		WorkspaceMounts: []types.WorkspaceMount{{Source: boundFolder, Target: composerWorkspaceTarget, ReadOnly: &ro}}}
	posture := func(bound bool) userMountPosture {
		p := userMountPosture{Roots: []string{boundLaptopRoot}, Sources: map[string]bool{boundFolder: true}}
		if bound {
			p.Bound = map[string]uuid.UUID{boundFolder: uuid.New()}
		}
		return p
	}
	dispatch := func(placed types.Placement, bound bool) (*fakeRunner, *recRecorder, types.AgentRun) {
		fr := &fakeRunner{}
		srv, _, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
		run.Placement = placed
		srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
			RunToken: "run-token", Image: "wardyn/claude-code:latest", Policy: policy, UserMounts: posture(bound),
		})
		return fr, audit, run
	}
	for _, placed := range []types.Placement{types.PlacementRemote, ""} {
		fr, audit, run := dispatch(placed, true)
		if fr.createCalls != 0 {
			t.Errorf("placement %q: CreateSandbox calls = %d, want 0 for a runner-bound folder", placed, fr.createCalls)
		}
		if findAudit(audit.events, run.ID, "run.dispatch", "failure") == nil {
			t.Errorf("placement %q: no run.dispatch failure; events=%s", placed, auditDump(audit.events, run.ID))
		}
	}
	if fr, _, _ := dispatch(types.PlacementRemote, false); fr.createCalls != 1 {
		t.Errorf("the unbound mount: CreateSandbox calls = %d, want 1", fr.createCalls)
	}
}
