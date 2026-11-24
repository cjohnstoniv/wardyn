// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/assistant"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const buildFilesTestRepo = "acme/widgets"

// buildFilesStore is the assistant fake plus the workspaces a repository draft reads.
type buildFilesStore struct {
	*assistantFake
	ws map[uuid.UUID]types.Workspace
	// draftErr fails a write of held files; replaceErr fails ReplaceAssistantSession.
	draftErr, replaceErr error
}

func (s *buildFilesStore) SetAssistantDraft(ctx context.Context, session uuid.UUID, draft []byte) error {
	if s.draftErr != nil && draft != nil {
		return s.draftErr
	}
	return s.assistantFake.SetAssistantDraft(ctx, session, draft)
}

func (s *buildFilesStore) ReplaceAssistantSession(ctx context.Context, old uuid.UUID, n types.AssistantSession, reset []byte) (types.AssistantSession, error) {
	if s.replaceErr != nil {
		return n, s.replaceErr
	}
	return s.assistantFake.ReplaceAssistantSession(ctx, old, n, reset)
}

// assistantDispatchSpy is the runner dispatch reaches. At each sandbox it makes, it records whether the assistant
// session's row already carried that run's id: the order a repository draft's confirm promises.
type assistantDispatchSpy struct {
	*fakeRunner
	f        *assistantFake
	mu       sync.Mutex
	attached []bool
}

func (s *assistantDispatchSpy) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	s.f.amu.Lock()
	found := false
	for _, ss := range s.f.sessions {
		found = found || (ss.RunID != nil && *ss.RunID == spec.RunID)
	}
	s.f.amu.Unlock()
	s.mu.Lock()
	s.attached = append(s.attached, found)
	s.mu.Unlock()
	return s.fakeRunner.CreateSandbox(ctx, spec)
}

func (s *buildFilesStore) GetWorkspace(_ context.Context, id uuid.UUID) (types.Workspace, error) {
	ws, ok := s.ws[id]
	if !ok {
		return types.Workspace{}, store.ErrNotFound
	}
	return ws, nil
}

// buildFilesEnv serves a compose assistant on a site whose GitHub App lane is allowed, one workspace that carries
// buildFilesTestRepo, and a minter that holds files for it.
type buildFilesEnv struct {
	st     *buildFilesStore
	spy    *assistantDispatchSpy
	f      *assistantFake
	audit  *recRecorder
	srv    *Server
	gh     *broker.FakeGitHubMinter
	ws     types.Workspace
	cookie *http.Cookie
	person string
}

func newBuildFilesEnv(t *testing.T, files ...broker.File) *buildFilesEnv {
	t.Helper()
	return newBuildFilesEnvSite(t, func(site *types.SiteConfig) {
		site.WorkspaceProviders = &types.WorkspaceProviders{Git: []types.GitProvider{{ID: "gh-1", Kind: types.GitProviderGitHub,
			BaseURLs: []string{"https://github.com/acme"}, Lanes: []types.GitLane{types.GitLaneApp}}}}
	}, files...)
}

// buildFilesGitLabRepo is a repository of the workspace on a host the repository draft cannot read.
const buildFilesGitLabRepo = "https://gitlab.example.com/x/y"

// newBuildFilesEnvSite is newBuildFilesEnv on a site the caller shapes.
func newBuildFilesEnvSite(t *testing.T, shape func(*types.SiteConfig), files ...broker.File) *buildFilesEnv {
	t.Helper()
	site, _ := assistantTwoProviders()
	shape(&site)
	f := newAssistantFake(site)
	f.templates = []types.RunTemplate{{ID: uuid.New(), Name: "deploy", Description: "ships the app"}}
	ws := branchWorkspace("", buildFilesTestRepo, buildFilesGitLabRepo)
	srv, h := assistantSessionServer(t, f)
	st := &buildFilesStore{assistantFake: f, ws: map[uuid.UUID]types.Workspace{ws.ID: ws}}
	srv.cfg.Store = st
	spy := &assistantDispatchSpy{fakeRunner: &fakeRunner{}, f: f}
	srv.cfg.Runner = spy
	gh := &broker.FakeGitHubMinter{RootFiles: files}
	srv.cfg.GitHubBranches = gh
	person := "sub-bf-" + uuid.NewString()
	return &buildFilesEnv{st: st, spy: spy, f: f, audit: h.audit, srv: srv, gh: gh, ws: ws, person: person, cookie: ssoSession(t, person, person+"@corp.example", oidc.RoleUser)}
}

const buildFilesTarget = `"purpose":"compose","target":{"kind":"template"}`

func (e *buildFilesEnv) digest(t *testing.T) string {
	t.Helper()
	w := doSSO(t, e.srv, http.MethodPost, "/api/v1/assistant/preview", e.cookie, `{`+buildFilesTarget+`}`)
	var p types.AssistantPreview
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &p) != nil {
		t.Fatalf("preview = %d %s", w.Code, w.Body)
	}
	return p.Digest
}

func (e *buildFilesEnv) create(t *testing.T, digest, extra string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.srv, http.MethodPost, "/api/v1/assistant/sessions", e.cookie,
		`{`+buildFilesTarget+`,"preview_digest":"`+digest+`","workspace_id":"`+e.ws.ID.String()+`","repo":"`+buildFilesTestRepo+`"`+extra+`}`)
}

func (e *buildFilesEnv) confirm(t *testing.T, cookie *http.Cookie, id uuid.UUID, digest string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.srv, http.MethodPost, "/api/v1/assistant/sessions/"+id.String()+"/build-files/confirm", cookie, `{"preview_digest":"`+digest+`"}`)
}

func buildFile(name, text string) broker.File {
	return broker.File{Name: name, Branch: "main", Size: len(text), Text: text}
}

// A repository draft reads and scans the files but sends nothing: the session fetches with no run, its events name
// the files without their text, and the audit row lists names only. The confirm sends them, on the same session
// when the binding is unchanged.
func TestAssistantRepoDraftHoldsFilesUntilConfirm(t *testing.T) {
	const dockerfile = "FROM node:20\nRUN npm ci\n"
	e := newBuildFilesEnv(t, buildFile("Dockerfile", dockerfile), buildFile("package.json", `{"name":"widgets"}`))
	digest := e.digest(t)
	w := e.create(t, digest, "")
	var created assistantSessionView
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &created) != nil || created.State != types.AssistantSessionFetching {
		t.Fatalf("create = %d %s, want 201 fetching", w.Code, w.Body)
	}
	got := e.f.session(created.ID)
	if got.RunID != nil || len(e.f.sessions) != 1 {
		t.Fatalf("a run %v or %d sessions exist before the confirm; want none and one", got.RunID, len(e.f.sessions))
	}
	var sawNames bool
	for _, ev := range e.f.eventsOf(created.ID) {
		if strings.Contains(string(ev.Payload), "npm ci") || strings.Contains(string(ev.Payload), `"name":"widgets"`) {
			t.Errorf("event %s carries a file's text: %s", ev.Kind, ev.Payload)
		}
		sawNames = sawNames || strings.Contains(string(ev.Payload), "Dockerfile")
	}
	if !sawNames {
		t.Error("no event names the files the person is asked to confirm")
	}
	if rows := auditRowsOf(e.audit, assistantBuildFilesFetchAction); len(rows) != 1 || strings.Contains(string(rows[0].Data), "npm ci") {
		t.Errorf("audit rows = %+v, want one row naming files and no text", rows)
	}

	wantReason(t, e.confirm(t, e.cookie, created.ID, "stale"), http.StatusConflict, reasonAssistantPreviewStale)
	if e.f.session(created.ID).State != types.AssistantSessionFetching {
		t.Error("a stale confirm ended or advanced the session")
	}
	other := ssoSession(t, "sub-other-"+uuid.NewString(), "o@corp.example", oidc.RoleUser)
	wantReason(t, e.confirm(t, other, created.ID, digest), http.StatusNotFound, reasonAssistantSessionNotFound)

	w = e.confirm(t, e.cookie, created.ID, digest)
	if w.Code != http.StatusOK {
		t.Fatalf("confirm = %d %s, want 200", w.Code, w.Body)
	}
	after := e.f.session(created.ID)
	if after.RunID == nil {
		t.Fatalf("session %+v has no run after the confirm", after)
	}
	files := assistant.FilesOf(mustStoredRecords(t, e.srv, e.f, created.ID))
	if len(files) != 2 || files[0].Text != dockerfile {
		t.Errorf("the records carry %+v, want the two confirmed files", files)
	}
	wantReason(t, e.confirm(t, e.cookie, created.ID, digest), http.StatusConflict, reasonAssistantBuildFilesNotPending)
}

func mustStoredRecords(t *testing.T, srv *Server, f *assistantFake, id uuid.UUID) []types.AssistantSection {
	t.Helper()
	recs, err := srv.assistantStoredRecords(context.Background(), f, id)
	if err != nil {
		t.Fatalf("stored records: %v", err)
	}
	return recs
}

// A file in a well-known secret format is held back and named, never sent; a clean one beside it is.
func TestAssistantRepoDraftHoldsBackASecretFile(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a", 36)
	e := newBuildFilesEnv(t, buildFile("Dockerfile", "ENV TOKEN="+secret+"\n"), buildFile("go.mod", "module widgets\n"))
	w := e.create(t, e.digest(t), "")
	var created assistantSessionView
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	var ev assistantBuildFilesEvent
	for _, row := range e.f.eventsOf(created.ID) {
		var p assistantBuildFilesEvent
		if json.Unmarshal(row.Payload, &p) == nil && len(p.Files)+len(p.HeldBack) > 0 {
			ev = p
		}
		if strings.Contains(string(row.Payload), secret) {
			t.Errorf("event %s carries the secret", row.Kind)
		}
	}
	if len(ev.HeldBack) != 1 || ev.HeldBack[0] != "Dockerfile" || len(ev.Files) != 1 || ev.Files[0].Name != "go.mod" {
		t.Errorf("event = %+v, want Dockerfile held back and go.mod sent", ev)
	}
	if strings.Contains(string(e.f.session(created.ID).Draft), secret) {
		t.Error("the held files include the secret")
	}
}

// The refusals happen before any read: a workspace that is another person's answers what a random id answers, a
// repository the workspace does not carry is 422, and a host no provider reads is 400.
func TestAssistantRepoDraftRefusesBeforeAnyRead(t *testing.T) {
	e := newBuildFilesEnv(t, buildFile("go.mod", "module widgets\n"))
	digest := e.digest(t)
	foreign := branchWorkspace("sub-carol", buildFilesTestRepo)
	e.srv.cfg.Store.(*buildFilesStore).ws[foreign.ID] = foreign
	body := func(ws uuid.UUID, repo string) string {
		return `{` + buildFilesTarget + `,"preview_digest":"` + digest + `","workspace_id":"` + ws.String() + `","repo":"` + repo + `"}`
	}
	post := func(b string) *httptest.ResponseRecorder {
		return doSSO(t, e.srv, http.MethodPost, "/api/v1/assistant/sessions", e.cookie, b)
	}
	want := post(body(uuid.New(), buildFilesTestRepo))
	if want.Code != http.StatusNotFound {
		t.Fatalf("random workspace = %d %s, want 404", want.Code, want.Body)
	}
	if got := post(body(foreign.ID, buildFilesTestRepo)); got.Code != want.Code || got.Body.String() != want.Body.String() {
		t.Errorf("another person's workspace = %d %s, want the random id's %d %s", got.Code, got.Body, want.Code, want.Body)
	}
	if got := post(body(e.ws.ID, "acme/other")); got.Code != http.StatusUnprocessableEntity {
		t.Errorf("a repository of another workspace = %d %s, want 422", got.Code, got.Body)
	}
	wantReason(t, post(body(e.ws.ID, "https://example.invalid/x/y")), http.StatusUnprocessableEntity, reasonWorkspaceRepoUnknown)
	wantReason(t, post(strings.Replace(body(e.ws.ID, buildFilesTestRepo), `"repo"`, `"repo_url":"x","repo"`, 1)), http.StatusBadRequest, reasonInvalidRequestBody)
	if len(e.gh.LastReadRepos) != 0 || len(e.f.sessions) != 0 {
		t.Errorf("a refused create read %v and made %d sessions", e.gh.LastReadRepos, len(e.f.sessions))
	}
}

// The assistant turned off between the create and the confirm: 409 with the reason, the session ends, and no
// file is sent.
func TestAssistantRepoDraftConfirmAfterTurnOff(t *testing.T) {
	e := newBuildFilesEnv(t, buildFile("go.mod", "module widgets\n"))
	digest := e.digest(t)
	w := e.create(t, digest, "")
	var created assistantSessionView
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	e.f.site.Assistant.Compose = false
	wantReason(t, e.confirm(t, e.cookie, created.ID, digest), http.StatusConflict, reasonAssistantOff)
	if got := e.f.session(created.ID); got.Live() || got.RunID != nil || len(got.Draft) != 0 {
		t.Errorf("session after a turned-off confirm = %+v, want ended, no run, no held files", got)
	}
}

// from_build_file is set only for a value found, as a whole token, in a confirmed file; the model's own claim is
// never read.
func TestAssistantValueInFiles(t *testing.T) {
	files := []assistant.File{{Name: "package-lock.json", Text: `{"resolved":"https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz"}`}}
	for value, want := range map[string]bool{
		`"registry.npmjs.org"`:                  true,
		`["registry.npmjs.org","left-pad"]`:     true,
		`{"host":"registry.npmjs.org"}`:         true,
		`["registry.npmjs.org","evil.example"]`: false,
		`"registry.npmjs.org.evil.example"`:     false,
		`[]`:                                    false,
		`7`:                                     false,
		`not json`:                              false,
	} {
		if got := assistantValueInFiles(json.RawMessage(value), files); got != want {
			t.Errorf("assistantValueInFiles(%s) = %v, want %v", value, got, want)
		}
	}
}

// created makes a repository draft that must succeed and returns its session.
func (e *buildFilesEnv) created(t *testing.T) (assistantSessionView, string) {
	t.Helper()
	digest := e.digest(t)
	w := e.create(t, digest, "")
	var out assistantSessionView
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("create = %d %s, want 201", w.Code, w.Body)
	}
	return out, digest
}

func (e *buildFilesEnv) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.srv, http.MethodPost, "/api/v1/assistant/sessions", e.cookie, body)
}

// A repository on a host no source reader serves is 400 assistant_repo_host_unsupported, and nothing is read.
func TestAssistantRepoDraftUnsupportedHost(t *testing.T) {
	e := newBuildFilesEnv(t, buildFile("go.mod", "module widgets\n"))
	w := e.post(t, `{`+buildFilesTarget+`,"preview_digest":"`+e.digest(t)+`","workspace_id":"`+e.ws.ID.String()+`","repo":"`+buildFilesGitLabRepo+`"}`)
	wantReason(t, w, http.StatusBadRequest, reasonAssistantRepoHostUnsupported)
	if len(e.gh.LastReadRepos) != 0 || len(e.f.sessions) != 0 {
		t.Errorf("an unsupported host read %v and made %d sessions", e.gh.LastReadRepos, len(e.f.sessions))
	}
	if rows := auditRowsOf(e.audit, assistantBuildFilesFetchAction); len(rows) != 1 || rows[0].Outcome != "failure" {
		t.Errorf("audit rows = %+v, want one failure row", rows)
	}
}

// With no git provider row at all the App arm still reads a github.com repository, as the branch list does.
func TestAssistantRepoDraftAppArmWithAnUnsetProvider(t *testing.T) {
	e := newBuildFilesEnvSite(t, func(site *types.SiteConfig) { site.WorkspaceProviders = nil }, buildFile("go.mod", "module widgets\n"))
	e.created(t)
	if strings.Join(e.gh.LastReadRepos, ",") != buildFilesTestRepo {
		t.Errorf("the App was asked about %v, want only %s", e.gh.LastReadRepos, buildFilesTestRepo)
	}
}

// A provider row that does not allow the App lane, an App with nothing to read through, and the host's refusals
// each answer their own reason, and none leaves a session.
func TestAssistantRepoDraftReadRefusals(t *testing.T) {
	for _, c := range []struct {
		name   string
		shape  func(*buildFilesEnv)
		status int
		reason string
	}{
		{"lane not allowed", func(e *buildFilesEnv) {
			e.f.site.WorkspaceProviders.Git[0].Lanes = []types.GitLane{types.GitLanePAT}
		}, http.StatusConflict, reasonAssistantRepoNoCredential},
		{"no App", func(e *buildFilesEnv) { e.srv.cfg.GitHubBranches = nil }, http.StatusConflict, reasonAssistantRepoNoCredential},
		{"refused", func(e *buildFilesEnv) { e.gh.RootFilesErr = fmt.Errorf("x: %w", broker.ErrGitHubRefused) }, http.StatusBadGateway, reasonAssistantRepoReadRefused},
		{"unreachable", func(e *buildFilesEnv) { e.gh.RootFilesErr = fmt.Errorf("dial") }, http.StatusBadGateway, reasonAssistantRepoUnreachable},
	} {
		e := newBuildFilesEnv(t, buildFile("go.mod", "module widgets\n"))
		c.shape(e)
		wantReason(t, e.create(t, e.digest(t), ""), c.status, c.reason)
		if len(e.f.sessions) != 0 {
			t.Errorf("%s: a refused read made %d sessions", c.name, len(e.f.sessions))
		}
	}
	if got := adoReadRefusal(branchReasonSignInNeeded); got.status != http.StatusConflict || got.reason != reasonAssistantRepoSignInNeeded {
		t.Errorf("the Azure DevOps sign-in refusal = %d %s, want 409 %s", got.status, got.reason, reasonAssistantRepoSignInNeeded)
	}
}

// A file with a NUL byte is held back and named, never a server error; a token in a JSON key, or in an earlier
// duplicate key, holds its file back too.
func TestAssistantRepoDraftHoldsBackBinaryAndKeyedSecrets(t *testing.T) {
	secret := "ghp_" + strings.Repeat("b", 36)
	e := newBuildFilesEnv(t, buildFile("Dockerfile", "FROM node\x00\n"), buildFile("package.json", `{"`+secret+`":"x"}`),
		buildFile("composer.json", `{"t":"`+secret+`","t":"clean"}`), buildFile("go.mod", "module widgets\n"))
	created, _ := e.created(t)
	var ev assistantBuildFilesEvent
	for _, row := range e.f.eventsOf(created.ID) {
		var p assistantBuildFilesEvent
		if json.Unmarshal(row.Payload, &p) == nil && len(p.Files)+len(p.HeldBack) > 0 {
			ev = p
		}
	}
	if strings.Join(ev.HeldBack, ",") != "Dockerfile,package.json,composer.json" || len(ev.Files) != 1 || ev.Files[0].Name != "go.mod" {
		t.Errorf("event = %+v, want three held back and go.mod sent", ev)
	}
}

// Dispatch sees the run's id on the session row: the id is written before anything is dispatched.
func TestAssistantRepoDraftConfirmAttachesTheRunBeforeDispatch(t *testing.T) {
	e := newBuildFilesEnv(t, buildFile("go.mod", "module widgets\n"))
	created, digest := e.created(t)
	if w := e.confirm(t, e.cookie, created.ID, digest); w.Code != http.StatusOK {
		t.Fatalf("confirm = %d %s", w.Code, w.Body)
	}
	e.spy.mu.Lock()
	defer e.spy.mu.Unlock()
	if len(e.spy.attached) != 1 || !e.spy.attached[0] {
		t.Errorf("dispatch made sandboxes with the run attached = %v, want exactly one, attached before it", e.spy.attached)
	}
}

// The confirm's digest covers the serving binding. Two providers: the person previewed on a, a is gone by
// the confirm and b serves with every record byte the same. The old digest is refused and the session stays
// fetching; b's digest confirms, on a new session that carries b and the files, and the first is ended as replaced.
func TestAssistantRepoDraftConfirmDigestCoversTheBinding(t *testing.T) {
	e := newBuildFilesEnv(t, buildFile("go.mod", "module widgets\n"))
	created, onA := e.created(t)
	e.f.site.Assistant.Providers = e.f.site.Assistant.Providers[1:]
	wantReason(t, e.confirm(t, e.cookie, created.ID, onA), http.StatusConflict, reasonAssistantPreviewStale)
	if got := e.f.session(created.ID); got.State != types.AssistantSessionFetching || got.RunID != nil {
		t.Fatalf("a stale confirm left %s with run %v, want fetching and none", got.State, got.RunID)
	}
	onB := e.digest(t)
	if onB == onA {
		t.Fatal("the digest did not change with the serving provider")
	}
	w := e.confirm(t, e.cookie, created.ID, onB)
	var out assistantBuildFilesConfirmView
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.ID == created.ID || out.RunID == nil {
		t.Fatalf("confirm = %d %s, want 200 on a new session with a run", w.Code, w.Body)
	}
	if got := e.f.session(created.ID); got.Live() || got.EndReason != types.AssistantEndReplaced || len(got.Draft) != 0 {
		t.Errorf("the first session = %s/%s with %d held bytes, want ended as replaced with none held", got.State, got.EndReason, len(got.Draft))
	}
	if got := e.f.session(out.ID); got.Binding.ProviderID != "b" || len(assistant.FilesOf(mustStoredRecords(t, e.srv, e.f, out.ID))) != 1 {
		t.Errorf("the new session = %+v, want provider b and the one confirmed file", got)
	}
}

// A replace that fails keeps the files the person was asked to confirm.
func TestAssistantRepoDraftFailedReplaceKeepsTheHeldFiles(t *testing.T) {
	e := newBuildFilesEnv(t, buildFile("go.mod", "module widgets\n"))
	created, _ := e.created(t)
	held := e.f.session(created.ID).Draft
	e.f.site.Assistant.Providers = e.f.site.Assistant.Providers[1:]
	e.st.replaceErr = fmt.Errorf("store down")
	if w := e.confirm(t, e.cookie, created.ID, e.digest(t)); w.Code != http.StatusInternalServerError {
		t.Fatalf("confirm = %d %s, want 500", w.Code, w.Body)
	}
	got := e.f.session(created.ID)
	if got.State != types.AssistantSessionFetching || string(got.Draft) != string(held) || len(held) == 0 {
		t.Errorf("after a failed replace the session is %s holding %q, want fetching holding %q", got.State, got.Draft, held)
	}
}

// A repository draft starts cold: a warm session of the person is ended as replaced and its run stopped, and a
// fetching row is ended as replaced by a plain create, its held files dropped.
func TestAssistantRepoDraftReplacesWarmAndFetchingSessions(t *testing.T) {
	e := newBuildFilesEnv(t, buildFile("go.mod", "module widgets\n"))
	_, bind := assistantTwoProviders()
	run := e.f.seedRun(t, e.person)
	warm := assistantLiveSession(e.person, &run, bind)
	e.f.seed(warm)
	created, digest := e.created(t)
	if got := e.f.session(warm.ID); got.Live() || got.EndReason != types.AssistantEndReplaced {
		t.Errorf("the warm session = %s/%s, want ended as replaced", got.State, got.EndReason)
	}
	if !e.f.runState(t, run).IsTerminal() {
		t.Errorf("the warm session's run is %s, want stopped", e.f.runState(t, run))
	}
	w := e.post(t, `{`+buildFilesTarget+`,"preview_digest":"`+digest+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("a plain create over a fetching row = %d %s, want 201", w.Code, w.Body)
	}
	if got := e.f.session(created.ID); got.Live() || got.EndReason != types.AssistantEndReplaced || len(got.Draft) != 0 {
		t.Errorf("the fetching row = %s/%s holding %d bytes, want ended as replaced and cleared", got.State, got.EndReason, len(got.Draft))
	}
}

// What changes between the create and the confirm ends the session with the reason and sends nothing: the purpose
// turned off, a provider that now defers inspection, and a workspace the person can no longer launch (answered as
// a missing one, with nothing read or launched). A template draft has no governance kind, so never_sent_kinds,
// which takes governance kinds only (governanceKindKnown), cannot name it.
func TestAssistantRepoDraftConfirmRefusals(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(*buildFilesEnv)
		status int
		reason string
	}{
		{"purpose off", func(e *buildFilesEnv) { e.f.site.Assistant.Compose = false }, http.StatusConflict, reasonAssistantOff},
		{"deferral", func(e *buildFilesEnv) {
			for i := range e.f.site.ModelProviders.Providers {
				e.f.site.ModelProviders.Providers[i].GatewayIdentity = &types.ModelProviderGatewayIdentity{Inspection: types.GatewayInspectionGateway}
			}
		}, http.StatusConflict, reasonAssistantNoProvider},
		{"workspace withdrawn", func(e *buildFilesEnv) { delete(e.srv.cfg.Store.(*buildFilesStore).ws, e.ws.ID) }, http.StatusNotFound, reasonWorkspaceNotFound},
	} {
		e := newBuildFilesEnv(t, buildFile("go.mod", "module widgets\n"))
		created, digest := e.created(t)
		reads := len(e.gh.LastReadRepos)
		c.change(e)
		wantReason(t, e.confirm(t, e.cookie, created.ID, digest), c.status, c.reason)
		got := e.f.session(created.ID)
		if got.Live() || got.EndReason != types.AssistantEndTurnedOff || got.RunID != nil || len(got.Draft) != 0 {
			t.Errorf("%s: session = %s/%s run %v holding %d bytes, want ended as turned off, no run, none held", c.name, got.State, got.EndReason, got.RunID, len(got.Draft))
		}
		if len(e.gh.LastReadRepos) != reads || e.spy.createCalls != 0 {
			t.Errorf("%s: the confirm read the repository again (%v) or dispatched (%d sandboxes)", c.name, e.gh.LastReadRepos, e.spy.createCalls)
		}
	}
}

// Ten minutes without a confirm: a confirm and an events read each end the session as idle with the files
// cleared, and the person's next create ends it as replaced.
func TestAssistantRepoDraftExpiry(t *testing.T) {
	age := func(e *buildFilesEnv, id uuid.UUID) {
		e.f.amu.Lock()
		defer e.f.amu.Unlock()
		s := e.f.sessions[id]
		s.CreatedAt = time.Now().Add(-assistantBuildFilesWindow - time.Minute)
		e.f.sessions[id] = s
	}
	e := newBuildFilesEnv(t, buildFile("go.mod", "module widgets\n"))
	created, digest := e.created(t)
	age(e, created.ID)
	wantReason(t, e.confirm(t, e.cookie, created.ID, digest), http.StatusConflict, reasonAssistantBuildFilesNotPending)
	if got := e.f.session(created.ID); got.Live() || got.EndReason != types.AssistantEndIdle || len(got.Draft) != 0 {
		t.Errorf("after the confirm: %s/%s holding %d bytes, want ended as idle and cleared", got.State, got.EndReason, len(got.Draft))
	}

	e = newBuildFilesEnv(t, buildFile("go.mod", "module widgets\n"))
	created, _ = e.created(t)
	age(e, created.ID)
	doSSO(t, e.srv, http.MethodGet, assistantPath(created.ID, "/events"), e.cookie, "")
	if got := e.f.session(created.ID); got.Live() || got.EndReason != types.AssistantEndIdle || len(got.Draft) != 0 {
		t.Errorf("after the events read: %s/%s holding %d bytes, want ended as idle and cleared", got.State, got.EndReason, len(got.Draft))
	}

	e = newBuildFilesEnv(t, buildFile("go.mod", "module widgets\n"))
	created, digest = e.created(t)
	age(e, created.ID)
	if w := e.create(t, digest, ""); w.Code != http.StatusCreated {
		t.Fatalf("the next create = %d %s", w.Code, w.Body)
	}
	if got := e.f.session(created.ID); got.Live() || got.EndReason != types.AssistantEndReplaced || len(got.Draft) != 0 {
		t.Errorf("after the next create: %s/%s holding %d bytes, want ended as replaced and cleared", got.State, got.EndReason, len(got.Draft))
	}
}

// A read that happened is audited as a failure even when the session could not be made, and the files are not held.
func TestAssistantRepoDraftAuditsAReadWhoseSessionFailed(t *testing.T) {
	e := newBuildFilesEnv(t, buildFile("go.mod", "module widgets\n"))
	e.st.draftErr = fmt.Errorf("store down")
	w := e.create(t, e.digest(t), "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("create = %d %s, want 500", w.Code, w.Body)
	}
	if rows := auditRowsOf(e.audit, assistantBuildFilesFetchAction); len(rows) != 1 || rows[0].Outcome != "failure" {
		t.Errorf("audit rows = %+v, want one failure row", rows)
	}
	for _, s := range e.f.sessions {
		if s.Live() {
			t.Errorf("session %s is still live after the failed create", s.ID)
		}
	}
}

// build_files answers exactly the confirmed files, as untrusted data; a session with none gets an empty list; and
// a draft part is marked from_build_file only when its every value is in a confirmed file.
func TestAssistantBuildFilesToolAndChip(t *testing.T) {
	f := newAssistantFake(types.SiteConfig{})
	ws := uuid.New()
	sess := assistantLiveSession("p", nil, types.AssistantBinding{})
	sess.WorkspaceID = &ws
	payload := assistant.AddBuildFiles(assistant.Payload{}, []assistant.File{{Name: "package-lock.json", Text: `{"resolved":"https://registry.npmjs.org/x/-/x-1.0.0.tgz"}`}})
	rec, _ := json.Marshal(map[string]any{"state": "records", "records": payload.Sections})
	f.seed(sess, types.AssistantEvent{Seq: 1, Kind: types.AssistantEventState, Payload: rec})
	srv := &Server{}
	env := &assistantToolEnv{r: httptest.NewRequest(http.MethodPost, "/", nil), ss: f, sess: sess, args: json.RawMessage(`{}`)}

	out, ref := srv.assistantToolBuildFiles(env)
	got, _ := out.(assistantBuildFilesResult)
	if ref != nil || !got.Untrusted || len(got.Files) != 1 || got.Files[0].Name != "package-lock.json" {
		t.Fatalf("build_files = %+v %+v, want the one confirmed file as untrusted", out, ref)
	}
	env.args = json.RawMessage(`{"x":1}`)
	if _, ref := srv.assistantToolBuildFiles(env); ref == nil || ref.status != http.StatusBadRequest {
		t.Errorf("build_files with an argument = %+v, want 400", ref)
	}
	env.args = json.RawMessage(`{}`)

	p := assistantProposal{Parts: []types.AssistantProposalPart{
		{Section: "a", Value: json.RawMessage(`["registry.npmjs.org"]`)},
		{Section: "b", Value: json.RawMessage(`["other.example"]`)},
		{Section: "c", Value: json.RawMessage(`["registry.npmjs.org","other.example"]`)},
	}}
	if ref := srv.assistantMarkBuildFiles(env, &p); ref != nil {
		t.Fatalf("mark = %+v", ref)
	}
	if !p.Parts[0].FromBuildFile || p.Parts[1].FromBuildFile || p.Parts[2].FromBuildFile {
		t.Errorf("chips = %v %v %v, want true false false: only a host found in a confirmed file is marked", p.Parts[0].FromBuildFile, p.Parts[1].FromBuildFile, p.Parts[2].FromBuildFile)
	}

	env.sess.WorkspaceID = nil
	if out, _ := srv.assistantToolBuildFiles(env); len(out.(assistantBuildFilesResult).Files) != 0 {
		t.Errorf("a session with no repository answered files: %+v", out)
	}
	if ref := srv.assistantMarkBuildFiles(env, &p); ref != nil || p.Parts[0].FromBuildFile {
		t.Errorf("a session with no repository still marks a part (%v, %+v)", p.Parts[0].FromBuildFile, ref)
	}
}
