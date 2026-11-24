// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/assistant"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const buildFilesTestRepo = "acme/widgets"

// buildFilesStore is the assistant fake plus the workspaces a repository draft reads.
type buildFilesStore struct {
	*assistantFake
	ws map[uuid.UUID]types.Workspace
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
	site, _ := assistantTwoProviders()
	site.WorkspaceProviders = &types.WorkspaceProviders{Git: []types.GitProvider{{ID: "gh-1", Kind: types.GitProviderGitHub,
		BaseURLs: []string{"https://github.com/acme"}, Lanes: []types.GitLane{types.GitLaneApp}}}}
	f := newAssistantFake(site)
	f.templates = []types.RunTemplate{{ID: uuid.New(), Name: "deploy", Description: "ships the app"}}
	ws := branchWorkspace("", buildFilesTestRepo)
	srv, h := assistantSessionServer(t, f)
	srv.cfg.Store = &buildFilesStore{assistantFake: f, ws: map[uuid.UUID]types.Workspace{ws.ID: ws}}
	gh := &broker.FakeGitHubMinter{RootFiles: files}
	srv.cfg.GitHubBranches = gh
	person := "sub-bf-" + uuid.NewString()
	return &buildFilesEnv{f: f, audit: h.audit, srv: srv, gh: gh, ws: ws, person: person, cookie: ssoSession(t, person, person+"@corp.example", oidc.RoleUser)}
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
