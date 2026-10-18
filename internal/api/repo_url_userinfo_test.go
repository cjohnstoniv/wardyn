// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoCloneButtonURL is the one http(s) userinfo allowed: Azure DevOps' Clone
// button names the organisation that the path already names.
const adoCloneButtonURL = "https://contoso@dev.azure.com/contoso/proj/_git/app"

// credentialedRepoURLs are refused at every door. secret is a substring that must
// appear in no answer, log line or audit row: the credential, or for the
// near-misses of the Azure DevOps form, the userinfo with its host.
var credentialedRepoURLs = []struct{ url, secret string }{
	{"https://ci-user:s3cr3t-pass@github.com/acme/app.git", "s3cr3t-pass"},
	{"https://glpat-TOKEN123@github.com/acme/app.git", "glpat-TOKEN123"},
	{"http://uname-x@github.com/acme/app.git", "uname-x"},
	{"https://otheruser-x@dev.azure.com/contoso/proj/_git/app", "otheruser-x"},
	{"https://contoso:pw-x9@dev.azure.com/contoso/proj/_git/app", "pw-x9"},
	{"https://contoso:@dev.azure.com/contoso/proj/_git/app", "contoso:@"},
	{"https://contoso@evil.example/contoso/proj/_git/app", "contoso@evil"},
	{"https://contoso@dev.azure.com.evil/contoso/proj/_git/app", "contoso@dev.azure.com.evil"},
	{"ssh://git:sshpw-x7@ssh.github.com:443/acme/app.git", "sshpw-x7"}, // a password in an ssh:// URL
	// The token repeats as the first segment and a dot segment or escape climbs
	// out of it: git's curl squashes the path and sends the token.
	{"https://PAT-x7@dev.azure.com/PAT-x7/../contoso/proj/_git/app", "PAT-x7@"},
	{"https://PAT-x7@dev.azure.com/PAT-x7/./../contoso/proj/_git/app", "PAT-x7@"},
	{"https://PAT-x7@dev.azure.com/PAT-x7//../contoso/proj/_git/app", "PAT-x7@"},
	{"https://PAT-x7@dev.azure.com/PAT-x7/%2e%2e/contoso/proj/_git/app", "PAT-x7@"},
	{"https://PAT-x7@dev.azure.com/PAT-x7/..%2Fcontoso/proj/_git/app", "PAT-x7@"},
	{"https://PAT-x7@dev.azure.com/PAT-x7/..;/contoso/proj/_git/app", "PAT-x7@"},   // a dot segment with a ;param
	{"https://PAT-x7@dev.azure.com/PAT-x7/.../contoso/proj/_git/app", "PAT-x7@"},   // a trailing-dot segment
	{"https://PAT-x7@dev.azure.com/PAT-x7/..%20/contoso/proj/_git/app", "PAT-x7@"}, // a trailing-space segment
	{"\u200bhttps://zw-tok9@github.com/acme/app.git", "zw-tok9"},                   // a zero-width space before the scheme
}

// allowedUserRepoURLs carry a user name that is no secret: the SSH login user
// (the rule covers http(s) only) and the Azure DevOps Clone-button name.
var allowedUserRepoURLs = []string{"git@github.com:acme/app.git", "ssh://git@ssh.github.com:443/acme/app.git", adoCloneButtonURL}

func TestRepoURLHasUserinfo(t *testing.T) {
	for _, raw := range []string{
		"https://u:p@h/r", "https://tok@h/r", "http://u@h/r", "HTTPS://u@h/r", "https://@h/r",
		"https://u:p@h", " https://u@h/r",
		"https://other@dev.azure.com/contoso/p/_git/r", "https://contoso:x@dev.azure.com/contoso/p/_git/r",
		"https://contoso:@dev.azure.com/contoso/p/_git/r", "https://contoso@evil.com/contoso/p/_git/r",
		"https://contoso@dev.azure.com.evil/contoso/p/_git/r", "https://contoso@dev.azure.com:443/contoso/p/_git/r",
		"https://contoso@dev.azure.com", "https://contoso@dev.azure.com?x/contoso", "https://a@contoso@dev.azure.com/contoso/p",
		"ssh://u:pw@h/r", "ssh://git:@h/r", "SSH://git:pw@h:443/r",
	} {
		if !repoURLHasUserinfo(raw) {
			t.Errorf("repoURLHasUserinfo(%q) = false, want true", raw)
		}
	}
	for _, raw := range []string{
		"git@h:r", "ssh://git@h/r", "https://h/r", "https://h/r@x", "https://h/r?u=a@b", "https://h#a@b",
		"acme/app", "", "ftp://u@h/r",
		adoCloneButtonURL, "https://Contoso@DEV.azure.com/contoso/p/_git/r", "http://contoso@dev.azure.com/CONTOSO/p",
		"ssh://git@h/r:x", "ssh://h/r",
		"https://contoso@dev.azure.com/contoso/Payments%20Platform/_git/Card%20Auth%20(v2).Service",
		"https://contoso@dev.azure.com/contoso/proj/_git/app/",
	} {
		if repoURLHasUserinfo(raw) {
			t.Errorf("repoURLHasUserinfo(%q) = true, want false", raw)
		}
	}
}

func TestRedactRepoURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://u:p@h/r":            "https://h/r",
		"https://tok@h/r.git":        "https://h/r.git",
		"http://u@h:8080/a/b":        "http://h:8080/a/b",
		"https://u:p@h/r%zz":         "https://h/r%zz", // does not parse: cut at the last '@'
		"git@h:r":                    "git@h:r",
		"ssh://git@h/r":              "ssh://git@h/r",
		"https://h/a%2Fb":            "https://h/a%2Fb", // no userinfo: byte-identical
		"acme/app":                   "acme/app",
		"https://github.com/acme/ap": "https://github.com/acme/ap",
		adoCloneButtonURL:            adoCloneButtonURL, // the stored spelling is kept
		"ssh://git:pw@h:443/r":       "ssh://h:443/r",
	} {
		if got := redactRepoURL(raw); got != want {
			t.Errorf("redactRepoURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func assertRepoURLRefused(t *testing.T, srv *Server, w *httptest.ResponseRecorder, logged *lockedBuffer, secret string) {
	t.Helper()
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Reason != reasonRepoURLCredentials || !strings.Contains(body.Error, "git_pat") {
		t.Errorf("body = %s, want reason %s and a sentence naming git_pat", w.Body.String(), reasonRepoURLCredentials)
	}
	if strings.Contains(w.Body.String(), secret) {
		t.Errorf("body echoes the secret: %s", w.Body.String())
	}
	if strings.Contains(logged.String(), secret) {
		t.Errorf("a log line carries the secret: %s", logged.String())
	}
	if rec, ok := srv.cfg.Audit.(*recRecorder); ok {
		for _, ev := range rec.snapshot() {
			if strings.Contains(string(ev.Data), secret) {
				t.Errorf("audit row %s carries the secret: %s", ev.Action, ev.Data)
			}
		}
	}
}

func assertNotRepoURLRefused(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if strings.Contains(w.Body.String(), reasonRepoURLCredentials) {
		t.Fatalf("an SSH address was refused as carrying credentials: %d %s", w.Code, w.Body.String())
	}
}

// runPathDoor is admitRunDoor for the two review doors, which share the
// launch body but answer at their own path.
func runPathDoor(name, path string, memberReachable bool, body func(repo string) string) admitDoor {
	return admitDoor{
		name:            name,
		memberReachable: memberReachable,
		fire: func(t *testing.T, sc types.SiteConfig, repo string, operator bool) (*Server, *httptest.ResponseRecorder) {
			t.Helper()
			srv, st, _ := govEscapeFixture(t, &capStore{})
			st.siteConfig = sc
			if operator {
				return srv, do(t, srv, http.MethodPost, path, adminToken, body(repo))
			}
			return srv, doSSO(t, srv, http.MethodPost, path, govSession(t, govMemberSub, []string{"eng"}, false), body(repo))
		},
	}
}

func repoURLDoors() []admitDoor {
	doors := admitDoors()
	bodies := []struct {
		field  string
		member bool
		body   func(repo string) string
	}{
		{"repo", true, func(repo string) string { return `{"agent":"claude-code","task":"t","repo":` + quote(repo) + `}` }},
		{"devcontainer_repo", false, func(repo string) string {
			return `{"agent":"claude-code","task":"t","devcontainer_repo":` + quote(repo) + `}`
		}},
		{"inline workspace_repos", true, func(repo string) string {
			return `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2",` +
				`"workspace_repos":[{"repo":` + quote(repo) + `,"target":"/home/agent/work/app"}]}}`
		}},
	}
	for _, path := range []string{"/api/v1/runs/preflight", "/api/v1/runs/policy-preview"} {
		for _, b := range bodies {
			doors = append(doors, runPathDoor("POST "+strings.TrimPrefix(path, "/api/v1")+" ("+b.field+")", path, b.member, b.body))
		}
	}
	return doors
}

// TestRepoURLUserinfoRefusedAtEveryDoor: every door a repository URL enters by
// refuses userinfo with the one reason, in either provider mode, and neither the
// answer, a log line nor an audit row carries the secret.
func TestRepoURLUserinfoRefusedAtEveryDoor(t *testing.T) {
	sites := map[string]types.SiteConfig{"provider rows": admitSite(), "no provider rows": {}}
	for _, door := range repoURLDoors() {
		t.Run(door.name, func(t *testing.T) {
			for siteName, sc := range sites {
				for _, c := range credentialedRepoURLs {
					logged := captureSlog(t)
					srv, w := door.fire(t, sc, c.url, true)
					assertRepoURLRefused(t, srv, w, logged, c.secret)
					if door.memberReachable {
						srv, w = door.fire(t, sc, c.url, false)
						assertRepoURLRefused(t, srv, w, logged, c.secret)
					}
					if t.Failed() {
						t.Fatalf("site %s, url with %s", siteName, c.secret)
					}
				}
			}
			for _, ok := range allowedUserRepoURLs {
				_, w := door.fire(t, admitSite(), ok, true)
				assertNotRepoURLRefused(t, w)
				if door.memberReachable {
					_, w = door.fire(t, admitSite(), ok, false)
					assertNotRepoURLRefused(t, w)
				}
			}
		})
	}
}

// TestRepoURLUserinfoRefusedAtStoringDoors covers the doors that store a repo
// without admitting it: policy writes (and a preset's inline_policy) through
// validatePolicySpec, presets through the run contract, templates through the
// contract's shape checks.
func TestRepoURLUserinfoRefusedAtStoringDoors(t *testing.T) {
	for _, c := range credentialedRepoURLs {
		err := validatePolicySpec(types.RunPolicySpec{MinConfinementClass: types.CC2,
			WorkspaceRepos: []types.WorkspaceRepo{{Repo: c.url}}})
		if !errors.Is(err, errRepoURLUserinfo) || specRefusalReason(err, "bucket") != reasonRepoURLCredentials {
			t.Errorf("validatePolicySpec(%s) = %v, want the userinfo refusal under its own reason", c.secret, err)
		}
		if err != nil && strings.Contains(err.Error(), c.secret) {
			t.Errorf("policy refusal echoes the secret: %v", err)
		}

		srv := newHarness(t).srv
		for _, req := range []createRunRequest{
			{Repo: c.url},
			{DevcontainerRepo: c.url},
			{InlinePolicy: &types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: c.url}}}},
			{PolicyOverlay: json.RawMessage(`{"workspace_repos":[{"repo":` + quote(c.url) + `}]}`)},
		} {
			refusal := srv.runContractRefusal(req)
			if refusal == nil || refusal.body.Reason != reasonRepoURLCredentials {
				t.Errorf("runContractRefusal(%+v) = %+v, want %s", req, refusal, reasonRepoURLCredentials)
			}
		}

		// The template door's secret scan reads user:password@ first and refuses
		// it as a secret; a name or token alone reaches the contract's check.
		person := testTemplateOwner("person", "alice", "")
		for _, field := range []string{"repo", "devcontainer_repo"} {
			_, diags := validateTemplateDocument(structDoc(t, `{"`+field+`":`+quote(c.url)+`}`), person)
			got := diagReasons(diags)
			if !slices.Contains(got, reasonRepoURLCredentials+"@intent") && !slices.Contains(got, reasonTemplateSecretRefused+"@intent."+field) {
				t.Errorf("template %s with %s: diagnostics %v refuse neither as credentials nor as a secret", field, c.secret, got)
			}
		}
	}
	for _, ssh := range allowedUserRepoURLs { // ssh: an allowed user name
		if err := validatePolicySpec(types.RunPolicySpec{MinConfinementClass: types.CC2,
			WorkspaceRepos: []types.WorkspaceRepo{{Repo: ssh}}}); errors.Is(err, errRepoURLUserinfo) {
			t.Errorf("validatePolicySpec(%s) refused an SSH address: %v", ssh, err)
		}
		if refusal := repoURLUserinfoShapeRefusal(createRunRequest{Repo: ssh}); refusal != nil {
			t.Errorf("the shape check refused %s", ssh)
		}
	}
}

// TestStoredRepoURLRedactedInLogsAndAudit: a row stored before the doors refused
// userinfo still reaches the clone-record log lines, the 201's warnings and the
// branch-list audit row, all without the secret.
func TestStoredRepoURLRedactedInLogsAndAudit(t *testing.T) {
	for _, c := range credentialedRepoURLs {
		if repoCloneURL(c.url) == "" {
			continue // never cloned, so never in a clone record
		}
		hostPath := c.url[strings.LastIndex(c.url, "@")+1:]
		logged := captureSlog(t)
		_, warnings := buildRepoRecords("", []types.WorkspaceRepo{
			{Repo: c.url, Target: "/etc/app"},           // refused target: logged and warned
			{Repo: "acme/one", Target: "/home/agent/x"}, // takes the directory
			{Repo: c.url, Target: "/home/agent/x"},      // collides: logged and warned
			{Repo: c.url, Ref: "bad ref"},               // unsafe ref: logged
		})
		out := logged.String()
		if !strings.Contains(out, hostPath) || len(warnings) != 2 {
			t.Fatalf("fixture did not reach the log lines and warnings: log %q, warnings %q", out, warnings)
		}
		if strings.Contains(out, c.secret) {
			t.Errorf("clone-record log carries the secret: %s", out)
		}
		for _, w := range warnings {
			if strings.Contains(w, c.secret) {
				t.Errorf("warning carries the secret: %s", w)
			}
		}

		h := newHarness(t)
		h.srv.auditBranchList(httptest.NewRequest(http.MethodGet, "/", nil), uuid.NewString(), c.url, RepoBranches{})
		evs := h.audit.snapshot()
		if len(evs) != 1 || !strings.Contains(string(evs[0].Data), hostPath) {
			t.Fatalf("branch-list audit rows = %+v, want one naming the repository", evs)
		}
		if strings.Contains(string(evs[0].Data), c.secret) {
			t.Errorf("branch-list audit row carries the secret: %s", evs[0].Data)
		}
	}
}

// TestLauncherRefusesStoredRepoURLUserinfo: the server-side launchers (a source
// scan, a record session from a workspace) read stored rows, so admission refuses
// a row written before the doors did, with the same reason.
func TestLauncherRefusesStoredRepoURLUserinfo(t *testing.T) {
	srv := newHarness(t).srv
	for _, c := range credentialedRepoURLs {
		err := srv.admitLauncherRepo(t.Context(), c.url)
		if !errors.Is(err, errRepoURLUserinfo) {
			t.Fatalf("admitLauncherRepo(%s) = %v, want the userinfo refusal", c.secret, err)
		}
		w := httptest.NewRecorder()
		if !srv.writeAdmissionLaunchRefusal(w, httptest.NewRequest(http.MethodPost, "/", nil), err) {
			t.Fatal("writeAdmissionLaunchRefusal did not answer the userinfo refusal")
		}
		assertRepoURLRefused(t, srv, w, &lockedBuffer{}, c.secret)
	}
	for _, ssh := range allowedUserRepoURLs {
		if err := srv.admitLauncherRepo(t.Context(), ssh); errors.Is(err, errRepoURLUserinfo) {
			t.Errorf("admitLauncherRepo(%s) refused an SSH address", ssh)
		}
	}
}

// TestADOCloneButtonNameKeptInLogs: the Azure DevOps name is no secret, so the
// log line and the audit row keep the stored spelling.
func TestADOCloneButtonNameKeptInLogs(t *testing.T) {
	logged := captureSlog(t)
	if _, warnings := buildRepoRecords("", []types.WorkspaceRepo{{Repo: adoCloneButtonURL, Target: "/etc/app"}}); len(warnings) != 1 ||
		!strings.Contains(warnings[0], adoCloneButtonURL) {
		t.Errorf("warnings = %q, want the stored spelling", warnings)
	}
	if !strings.Contains(logged.String(), adoCloneButtonURL) {
		t.Errorf("log = %s, want the stored spelling %s", logged.String(), adoCloneButtonURL)
	}
	h := newHarness(t)
	h.srv.auditBranchList(httptest.NewRequest(http.MethodGet, "/", nil), uuid.NewString(), adoCloneButtonURL, RepoBranches{})
	if evs := h.audit.snapshot(); len(evs) != 1 || !strings.Contains(string(evs[0].Data), adoCloneButtonURL) {
		t.Errorf("audit = %+v, want the stored spelling", evs)
	}
}

// legacyRepoStore serves rows stored before the doors refused userinfo.
type legacyRepoStore struct {
	store.Store
	run    types.AgentRun
	ws     types.Workspace
	srcs   []types.Source
	policy types.RunPolicy
	intent json.RawMessage
}

func (s *legacyRepoStore) ListRuns(context.Context) ([]types.AgentRun, error) {
	return []types.AgentRun{s.run}, nil
}
func (s *legacyRepoStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return s.run, nil
}
func (s *legacyRepoStore) QueryAuditEvents(context.Context, uuid.UUID, int) ([]types.AuditEvent, error) {
	return nil, nil
}
func (s *legacyRepoStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, nil
}
func (s *legacyRepoStore) GetPolicy(context.Context, uuid.UUID) (types.RunPolicy, error) {
	return s.policy, nil
}
func (s *legacyRepoStore) GetLaunchIntent(context.Context, uuid.UUID, string) (store.LaunchIntentRecord, error) {
	return store.LaunchIntentRecord{RunID: s.run.ID, Document: s.intent}, nil
}
func (s *legacyRepoStore) ListLaunchIntents(context.Context, string, int) ([]store.LaunchIntentRecord, error) {
	return nil, nil
}
func (s *legacyRepoStore) DeleteLaunchIntentsCreatedBy(context.Context, string) (int, error) {
	return 0, nil
}
func (s *legacyRepoStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	return []types.Workspace{s.ws}, nil
}
func (s *legacyRepoStore) GetWorkspace(context.Context, uuid.UUID) (types.Workspace, error) {
	return s.ws, nil
}
func (s *legacyRepoStore) ListSources(context.Context) ([]types.Source, error) {
	return slices.Clone(s.srcs), nil
}
func (s *legacyRepoStore) GetSource(context.Context, uuid.UUID) (types.Source, error) {
	return s.srcs[0], nil
}

// TestLegacyRepoURLRedactedOnRead: GET /runs, /workspaces and /sources serve a
// legacy row's repository URL without its userinfo, and keep the Azure DevOps
// Clone-button spelling.
func TestLegacyRepoURLRedactedOnRead(t *testing.T) {
	for _, c := range credentialedRepoURLs {
		hostPath := c.url[strings.LastIndex(c.url, "@")+1:]
		id := uuid.New()
		st := &legacyRepoStore{
			run: types.AgentRun{ID: id, Agent: "claude-code", Repo: c.url, State: types.RunRunning},
			ws: types.Workspace{ID: id, Name: "app", Kind: types.WorkspaceKindRepo, Source: c.url,
				Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: c.url}}},
			srcs: []types.Source{{ID: id, Kind: types.SourceRepo, Name: "app", Locator: c.url}},
		}
		srv := New(baseTestConfig(newHarness(t), st))
		for _, path := range []string{"/runs", "/runs/" + id.String(), "/workspaces", "/workspaces/" + id.String(), "/sources", "/sources/" + id.String()} {
			w := do(t, srv, http.MethodGet, "/api/v1"+path, adminToken, "")
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), hostPath) {
				t.Fatalf("GET %s = %d %s, want 200 naming %s", path, w.Code, w.Body.String(), hostPath)
			}
			if strings.Contains(w.Body.String(), c.secret) {
				t.Errorf("GET %s serves the secret %s: %s", path, c.secret, w.Body.String())
			}
		}
		if st.ws.Sources[0].Source != c.url || st.srcs[0].Locator != c.url {
			t.Error("the read projection rewrote the stored row")
		}
	}
	st := &legacyRepoStore{
		run:  types.AgentRun{ID: uuid.New(), Agent: "claude-code", Repo: adoCloneButtonURL, State: types.RunRunning},
		srcs: []types.Source{{ID: uuid.New(), Kind: types.SourceRepo, Name: "app", Locator: adoCloneButtonURL}},
	}
	srv := New(baseTestConfig(newHarness(t), st))
	for _, path := range []string{"/runs", "/sources"} {
		if w := do(t, srv, http.MethodGet, "/api/v1"+path, adminToken, ""); !strings.Contains(w.Body.String(), adoCloneButtonURL) {
			t.Errorf("GET %s = %d %s, want the Azure DevOps spelling kept", path, w.Code, w.Body.String())
		}
	}
}

// TestGovernanceAndGradeAnswerTheUserinfoReason: the ceiling and grade doors map
// the policy validator's userinfo arm to the shared reason.
func TestGovernanceAndGradeAnswerTheUserinfoReason(t *testing.T) {
	err := validatePolicySpec(types.RunPolicySpec{MinConfinementClass: types.CC2,
		WorkspaceRepos: []types.WorkspaceRepo{{Repo: credentialedRepoURLs[0].url}}})
	if got := repoURLMsgReason("invalid ceiling: "+err.Error(), "bucket"); got != reasonRepoURLCredentials {
		t.Errorf("repoURLMsgReason = %q, want %s", got, reasonRepoURLCredentials)
	}
	if got := repoURLMsgReason("name is required", "bucket"); got != "bucket" {
		t.Errorf("repoURLMsgReason(other) = %q, want the bucket", got)
	}
	h := newHarness(t)
	w := do(t, h.srv, http.MethodPost, "/api/v1/policies/grade", adminToken,
		`{"spec":{"min_confinement_class":"CC2","workspace_repos":[{"repo":`+quote(credentialedRepoURLs[1].url)+`}]}}`)
	assertRepoURLRefused(t, h.srv, w, &lockedBuffer{}, credentialedRepoURLs[1].secret)
}

// legacyRunDocument is a stored run request naming url in every repository
// field, with a number a float round trip would round.
func legacyRunDocument(url string) string {
	return `{"api_version":"wardyn/v1","kind":"RunTemplate","coverage":"partial","intent":{"repo":` + quote(url) +
		`,"devcontainer_repo":` + quote(url) + `,"ends_after_sec":9007199254740993,` +
		`"inline_policy":{"min_confinement_class":"CC2","workspace_repos":[{"repo":` + quote(url) + `}]}}}`
}

// TestLegacyRepoURLRedactedOnDocumentReads (F1/F2): the launch-intent read, the
// policy read (and every spec read through redactSpecForRead), a preset read and
// a template read serve a legacy row's repository URLs without their credential,
// and keep the Azure DevOps spelling. A preset launch still reads the raw row,
// so the door refuses it.
func TestLegacyRepoURLRedactedOnDocumentReads(t *testing.T) {
	for _, c := range credentialedRepoURLs {
		hostPath := c.url[strings.LastIndex(c.url, "@")+1:]
		check := func(what, body string) {
			t.Helper()
			if !strings.Contains(body, hostPath) || strings.Contains(body, c.secret) {
				t.Errorf("%s with %s: %s, want %s without the credential", what, c.secret, body, hostPath)
			}
		}
		id := uuid.New()
		st := &legacyRepoStore{
			run:    types.AgentRun{ID: id, Agent: "claude-code", CreatedBy: "admin-token", State: types.RunRunning},
			intent: json.RawMessage(legacyRunDocument(c.url)),
			policy: types.RunPolicy{ID: id, Name: "p", Spec: types.RunPolicySpec{MinConfinementClass: types.CC2,
				WorkspaceRepos: []types.WorkspaceRepo{{Repo: "acme/one"}, {Repo: c.url}}}},
		}
		srv := New(baseTestConfig(newHarness(t), st))
		for _, path := range []string{"/runs/" + id.String() + "/launch-intent", "/policies/" + id.String()} {
			w := do(t, srv, http.MethodGet, "/api/v1"+path, adminToken, "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET %s = %d %s", path, w.Code, w.Body.String())
			}
			check("GET "+path, w.Body.String())
		}
		if !strings.Contains(do(t, srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/launch-intent", adminToken, "").Body.String(), "9007199254740993") {
			t.Error("the launch-intent read rounded a number")
		}
		if st.policy.Spec.WorkspaceRepos[1].Repo != c.url {
			t.Error("the policy read rewrote the stored row")
		}
		spec := redactSpecForRead(st.policy.Spec, false)
		if spec.WorkspaceRepos[1].Repo == c.url || spec.WorkspaceRepos[0].Repo != "acme/one" {
			t.Errorf("redactSpecForRead = %+v", spec.WorkspaceRepos)
		}

		req := `{"agent":"claude-code","repo":` + quote(c.url) + `,"inline_policy":{"min_confinement_class":"CC2","workspace_repos":[{"repo":` + quote(c.url) + `}]}}`
		preset := types.LaunchPreset{Name: "p", Version: 1, Request: json.RawMessage(req)}
		v, err := presetReadView(preset)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(v)
		check("preset read", string(b))
		if raw, err := presetView(preset); err != nil || raw.Request.Repo != c.url {
			t.Errorf("presetView (the launch read) = %q, %v; want the stored URL, which the door refuses", raw.Request.Repo, err)
		}

		tv, err := templateView(templateCaller{}, types.RunTemplate{ID: id, Name: "t", Document: json.RawMessage(legacyRunDocument(c.url))})
		if err != nil {
			t.Fatal(err)
		}
		b, _ = json.Marshal(tv)
		check("template read", string(b))
	}
	doc := []byte(legacyRunDocument(adoCloneButtonURL))
	if got := redactRepoURLsInJSON(doc); string(got) != string(doc) {
		t.Errorf("a document with nothing to redact was rewritten: %s", got)
	}
}

// TestProviderModeRefusalComesFirst (F7): with provider rows, a credentialed URL
// is refused for its credential before admission could answer with a sentence
// that names the URL.
func TestProviderModeRefusalComesFirst(t *testing.T) {
	const pat = "https://PAT-x7@dev.azure.com/PAT-x7/../contoso/proj/_git/app"
	for _, door := range admitDoors() {
		_, w := door.fire(t, admitSite(), pat, true)
		if body := w.Body.String(); !strings.Contains(body, reasonRepoURLCredentials) || strings.Contains(body, "PAT-x7@") ||
			strings.Contains(body, "outside every enabled provider") {
			t.Errorf("%s: %d %s, want the credentials refusal and no echo", door.name, w.Code, body)
		}
	}
}

// TestLegacyRepoURLRedactedOnSpecViews (F6): the governance profile view and the
// run effective-policy view's change list serve a legacy spec's repository URLs
// without their credential.
func TestLegacyRepoURLRedactedOnSpecViews(t *testing.T) {
	for _, c := range credentialedRepoURLs {
		spec := types.RunPolicySpec{MinConfinementClass: types.CC2, WorkspaceRepos: []types.WorkspaceRepo{{Repo: c.url, Ref: "main"}}}
		b, _ := json.Marshal(newProfileView(types.GovernanceProfile{Name: "g", Ceiling: spec}, nil, nil))
		if strings.Contains(string(b), c.secret) {
			t.Errorf("governance profile view serves %s: %s", c.secret, b)
		}
		got := strings.Join(policyFieldSets(spec)[fieldRepos], " ")
		if strings.Contains(got, c.secret) || !strings.HasSuffix(got, "@main") {
			t.Errorf("effective-policy change keys = %q, want the repository without %s", got, c.secret)
		}
		if spec.WorkspaceRepos[0].Repo != c.url {
			t.Error("a view rewrote the spec it read")
		}
	}
}
