// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	branchTestRepo   = "https://dev.azure.com/contoso/proj/_git/app"
	branchTestGitHub = "acme/widgets"
)

// branchStore is capStore plus the workspaces the branch route reads.
type branchStore struct {
	*capStore
	ws map[uuid.UUID]types.Workspace
}

func (s *branchStore) GetWorkspace(_ context.Context, id uuid.UUID) (types.Workspace, error) {
	ws, ok := s.ws[id]
	if !ok {
		return types.Workspace{}, store.ErrNotFound
	}
	return ws, nil
}

func branchWorkspace(owner string, repos ...string) types.Workspace {
	ws := types.Workspace{ID: uuid.New(), Name: "shop", OwnedBy: owner}
	for _, r := range repos {
		ws.Sources = append(ws.Sources, types.WorkspaceSource{Type: types.WorkspaceSourceTypeRepo, Source: r})
	}
	return ws
}

func newBranchServer(t *testing.T, site types.SiteConfig, grants []types.CapabilityGrant, gh BranchLister, wss ...types.Workspace) (*Server, *recRecorder) {
	t.Helper()
	h := newHarness(t)
	st := &branchStore{capStore: &capStore{Store: r3IntegStore{}, site: site, grants: grants}, ws: map[uuid.UUID]types.Workspace{}}
	for _, ws := range wss {
		st.ws[ws.ID] = ws
	}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.Now = func() time.Time { return ownPATNow }
	cfg.GitHubBranches = gh
	return New(cfg), h.audit
}

// adoBranchFake answers the two Azure DevOps reads the arm makes and records
// every request that left the process.
type adoBranchFake struct {
	def  string
	refs []string

	mu    sync.Mutex
	urls  []*url.URL
	auths []string
}

func (f *adoBranchFake) install(t *testing.T) {
	t.Helper()
	prev := adoOwnPATTransport
	adoOwnPATTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		f.mu.Lock()
		f.urls = append(f.urls, r.URL)
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.mu.Unlock()
		body := `{"defaultBranch":"refs/heads/` + f.def + `"}`
		if strings.HasSuffix(r.URL.Path, "/refs") {
			var vals []string
			for _, n := range f.refs {
				vals = append(vals, `{"name":"refs/heads/`+n+`"}`)
			}
			body = `{"count":` + fmt.Sprint(len(vals)) + `,"value":[` + strings.Join(vals, ",") + `]}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	t.Cleanup(func() { adoOwnPATTransport = prev })
}

func (f *adoBranchFake) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.urls)
}

func storeOwnToken(t *testing.T, srv *Server, expires time.Time) {
	t.Helper()
	raw, _ := json.Marshal(adoOwnPATBlob{Token: ownPATToken, Org: "contoso", ExpiresOn: expires})
	if err := srv.cfg.Secrets.For(capSub).Put(context.Background(), adoOwnPATSecretName(ownPATRowID), raw); err != nil {
		t.Fatal(err)
	}
}

func getBranches(t *testing.T, srv *Server, cookie *http.Cookie, id uuid.UUID, repo string) (int, string) {
	t.Helper()
	w := doSSO(t, srv, http.MethodGet, "/api/v1/workspaces/"+id.String()+"/branches?repo="+url.QueryEscape(repo), cookie, "")
	return w.Code, w.Body.String()
}

func listBranches(t *testing.T, srv *Server, cookie *http.Cookie, id uuid.UUID, repo string) RepoBranches {
	t.Helper()
	code, body := getBranches(t, srv, cookie, id, repo)
	if code != http.StatusOK {
		t.Fatalf("GET branches = %d %s, want 200", code, body)
	}
	var out RepoBranches
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A workspace the caller may not launch answers exactly what a random id does:
// another member's, a security admin on another member's, and one the
// caller's capability grants refuse. The launchable one answers 200, so the
// comparison is never vacuous.
func TestWorkspaceBranches_UniformNotFound(t *testing.T) {
	open := branchWorkspace("", "https://example.invalid/x/_git/y")
	walled := branchWorkspace("", "https://example.invalid/x/_git/y")
	foreign := branchWorkspace("sub-carol", "https://example.invalid/x/_git/y")
	deny := []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capWorkspace, walled.ID.String(), types.CapabilityDeny)}
	srv, _ := newBranchServer(t, types.SiteConfig{}, deny, nil, open, walled, foreign)
	member := memberCookie(t)
	security := ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)
	const unknown = "https://other.invalid/q/_git/z"

	wantCode, want := getBranches(t, srv, member, uuid.New(), "https://example.invalid/x/_git/y")
	if wantCode != http.StatusNotFound {
		t.Fatalf("random id = %d %s, want 404", wantCode, want)
	}
	for name, got := range map[string]func() (int, string){
		"another member's workspace":             func() (int, string) { return getBranches(t, srv, member, foreign.ID, foreign.Sources[0].Source) },
		"security admin on a member's workspace": func() (int, string) { return getBranches(t, srv, security, foreign.ID, foreign.Sources[0].Source) },
		"a workspace the member is not granted":  func() (int, string) { return getBranches(t, srv, member, walled.ID, walled.Sources[0].Source) },
		// A repo the workspace does not carry: the launch door answers before
		// the repo check, so no 422 names the workspace to someone refused it.
		"another member's workspace, unknown repo": func() (int, string) { return getBranches(t, srv, member, foreign.ID, unknown) },
		"security admin, unknown repo":             func() (int, string) { return getBranches(t, srv, security, foreign.ID, unknown) },
		"not granted, unknown repo":                func() (int, string) { return getBranches(t, srv, member, walled.ID, unknown) },
	} {
		if code, body := got(); code != wantCode || body != want {
			t.Errorf("%s = %d %q, want the random id's %d %q", name, code, body, wantCode, want)
		}
	}
	if code, body := getBranches(t, srv, member, open.ID, open.Sources[0].Source); code != http.StatusOK {
		t.Fatalf("launchable workspace = %d %s, want 200", code, body)
	}
}

// Only the workspace's own repositories are listed.
func TestWorkspaceBranches_RepoNotOfWorkspace(t *testing.T) {
	ws := branchWorkspace("", branchTestRepo)
	srv, _ := newBranchServer(t, adoSite(ownPATTestRow()), nil, nil, ws)
	code, body := getBranches(t, srv, memberCookie(t), ws.ID, "https://dev.azure.com/contoso/proj/_git/other")
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, `"reason":"workspace_repo_unknown"`) ||
		!strings.Contains(body, "is not a repository of workspace shop.") {
		t.Fatalf("GET = %d %s, want 422 workspace_repo_unknown", code, body)
	}
}

// A repository on a host no provider row admits any more is never dialled,
// whatever its shape: github.com is the App's own host, so without the
// admission check the App would be asked.
func TestWorkspaceBranches_UnadmittedHostIsNeverDialled(t *testing.T) {
	f := &adoBranchFake{def: "main", refs: []string{"main"}}
	f.install(t)
	gh := &broker.FakeGitHubMinter{Branches: []string{"main"}}
	repos := []string{"https://github.com/acme/widgets", "https://evil.example.com/contoso/proj/_git/app"}
	ws := branchWorkspace("", repos...)
	srv, audit := newBranchServer(t, adoSite(ownPATTestRow()), nil, gh, ws)
	storeOwnToken(t, srv, ownPATNow.AddDate(0, 0, 10))
	for _, repo := range repos {
		out := listBranches(t, srv, memberCookie(t), ws.ID, repo)
		if out.Listing != branchListingUnavailable || out.Reason != branchReasonNotSupported || len(out.Branches) != 0 {
			t.Errorf("%s = %+v, want unavailable forge_not_supported", repo, out)
		}
	}
	if f.calls() != 0 || len(gh.LastListedRepos) != 0 {
		t.Fatalf("outbound: %d Azure DevOps requests, GitHub asked about %v; want none", f.calls(), gh.LastListedRepos)
	}
	if n := len(auditRowsOf(audit, "workspace.branches.list")); n != len(repos) {
		t.Fatalf("%d workspace.branches.list rows, want one per call (%d)", n, len(repos))
	}
}

// The person's own token lists the branches: default first, then by name.
// The request goes to the organisation on the Azure DevOps API, never to the
// repository URL's own host, and carries the person's token.
func TestWorkspaceBranches_ADOOwnTokenListed(t *testing.T) {
	f := &adoBranchFake{def: "main", refs: []string{"topic-alpha", "main", "release-q3"}}
	f.install(t)
	row := ownPATTestRow()
	row.BaseURLs = append(row.BaseURLs, "https://contoso.visualstudio.com")
	legacy := "https://contoso.visualstudio.com/DefaultCollection/proj/_git/app"
	ws := branchWorkspace("", branchTestRepo, legacy)
	srv, _ := newBranchServer(t, adoSite(row), nil, nil, ws)
	storeOwnToken(t, srv, ownPATNow.AddDate(0, 0, 10))

	for _, repo := range []string{branchTestRepo, legacy} {
		out := listBranches(t, srv, memberCookie(t), ws.ID, repo)
		if out.Listing != branchListingListed || out.Credential != branchCredADOOwnToken || out.Default != "main" ||
			strings.Join(out.Branches, ",") != "main,release-q3,topic-alpha" || out.Truncated {
			t.Fatalf("%s = %+v, want listed main,release-q3,topic-alpha through ado_own_token", repo, out)
		}
	}
	for i, u := range f.urls {
		if u.Host != "dev.azure.com" || !strings.HasPrefix(u.Path, "/contoso/proj/_apis/git/repositories/app") {
			t.Errorf("request %d went to %s, want dev.azure.com/contoso/proj/_apis/git/repositories/app", i, u)
		}
		if f.auths[i] != adoOwnPATHeaderValue(ownPATToken) {
			t.Errorf("request %d carried %q, want the person's own token", i, f.auths[i])
		}
		q := u.Query()
		if q.Get("api-version") != adoBranchAPIVersion || strings.HasSuffix(u.Path, "/refs") && q.Get("filter") != "heads/" {
			t.Errorf("request %d asked %q, want api-version=%s and, on refs, filter=heads/", i, u.RawQuery, adoBranchAPIVersion)
		}
	}
	if len(f.urls) != 4 {
		t.Fatalf("%d requests, want 4 (repository and refs, twice)", len(f.urls))
	}
}

// An own token past the expiry the person entered, or none at all, asks them
// to add one, and nothing is dialled.
func TestWorkspaceBranches_ADOExpiredTokenSignInNeeded(t *testing.T) {
	f := &adoBranchFake{def: "main", refs: []string{"main"}}
	f.install(t)
	ws := branchWorkspace("", branchTestRepo)
	srv, _ := newBranchServer(t, adoSite(ownPATTestRow()), nil, nil, ws)
	out := listBranches(t, srv, memberCookie(t), ws.ID, branchTestRepo)
	if out.Listing != branchListingUnavailable || out.Reason != branchReasonSignInNeeded {
		t.Fatalf("no token = %+v, want signin_needed", out)
	}
	storeOwnToken(t, srv, ownPATNow.Add(-time.Minute))
	out = listBranches(t, srv, memberCookie(t), ws.ID, branchTestRepo)
	if out.Listing != branchListingUnavailable || out.Reason != branchReasonSignInNeeded || out.Credential != branchCredADOOwnToken {
		t.Fatalf("expired token = %+v, want signin_needed through ado_own_token", out)
	}
	if f.calls() != 0 {
		t.Fatalf("%d requests went out with no live token", f.calls())
	}
}

// A sign-in row the person never connected asks them to sign in; a
// minted_pat row's sign-in only creates tokens, so it lists nothing. Neither
// dials.
func TestWorkspaceBranches_ADOSignInRows(t *testing.T) {
	f := &adoBranchFake{def: "main", refs: []string{"main"}}
	f.install(t)
	ws := branchWorkspace("", adoTestRepo)
	srv, _ := newBranchServer(t, adoSite(adoEntraTestRow()), nil, nil, ws)
	srv.cfg.ADOEntra = adoTestEntraSource
	out := listBranches(t, srv, memberCookie(t), ws.ID, adoTestRepo)
	if out.Listing != branchListingUnavailable || out.Reason != branchReasonSignInNeeded || out.Credential != branchCredADOSignIn {
		t.Fatalf("no sign-in = %+v, want signin_needed through ado_signin", out)
	}
	minted := adoEntraTestRow()
	minted.Entra.TokenMode = types.ADOTokenModeMintedPAT
	srv, _ = newBranchServer(t, adoSite(minted), nil, nil, ws)
	srv.cfg.ADOEntra = adoTestEntraSource
	out = listBranches(t, srv, memberCookie(t), ws.ID, adoTestRepo)
	if out.Listing != branchListingUnavailable || out.Reason != branchReasonNoCredential {
		t.Fatalf("minted_pat row = %+v, want no_credential", out)
	}
	if f.calls() != 0 {
		t.Fatalf("%d requests went out", f.calls())
	}
}

// 250 branches: 200 come back, the default first, and truncated is set. The
// arm asks for one more than it keeps.
func TestWorkspaceBranches_TruncatedAt200(t *testing.T) {
	var refs []string
	for i := range 250 {
		refs = append(refs, fmt.Sprintf("b%03d", i))
	}
	f := &adoBranchFake{def: "b249", refs: refs}
	f.install(t)
	ws := branchWorkspace("", branchTestRepo)
	srv, _ := newBranchServer(t, adoSite(ownPATTestRow()), nil, nil, ws)
	storeOwnToken(t, srv, ownPATNow.AddDate(0, 0, 10))
	out := listBranches(t, srv, memberCookie(t), ws.ID, branchTestRepo)
	if len(out.Branches) != branchListMax || !out.Truncated || out.Branches[0] != "b249" || out.Branches[1] != "b000" {
		t.Fatalf("got %d branches (first %v), truncated %v; want 200, default first, truncated", len(out.Branches), out.Branches[:2], out.Truncated)
	}
	if top := f.urls[len(f.urls)-1].Query().Get("$top"); top != "201" {
		t.Fatalf("refs $top = %q, want 201", top)
	}
}

// The limiter is per person: past the burst the next call is a 429 before
// anything is read.
func TestWorkspaceBranches_RateLimited(t *testing.T) {
	srv, _ := newBranchServer(t, types.SiteConfig{}, nil, nil)
	member := memberCookie(t)
	for i := range branchListBurst {
		if code, body := getBranches(t, srv, member, uuid.New(), "r"); code != http.StatusNotFound {
			t.Fatalf("call %d = %d %s, want 404 inside the burst", i, code, body)
		}
	}
	code, body := getBranches(t, srv, member, uuid.New(), "r")
	if code != http.StatusTooManyRequests || !strings.Contains(body, `"reason":"branch_list_rate_limited"`) {
		t.Fatalf("call past the burst = %d %s, want 429 branch_list_rate_limited", code, body)
	}
	other := ssoSession(t, "sub-dana", "dana@corp.example", oidc.RoleUser)
	if code, _ := getBranches(t, srv, other, uuid.New(), "r"); code != http.StatusNotFound {
		t.Fatalf("another person = %d, want their own bucket", code)
	}
}

// The audit row names the repository, the credential kind, the outcome and
// the count, and never a branch name or the token.
func TestWorkspaceBranches_AuditHasNoBranchNames(t *testing.T) {
	f := &adoBranchFake{def: "main", refs: []string{"topic-alpha", "main", "release-q3"}}
	f.install(t)
	ws := branchWorkspace("", branchTestRepo)
	srv, audit := newBranchServer(t, adoSite(ownPATTestRow()), nil, nil, ws)
	storeOwnToken(t, srv, ownPATNow.AddDate(0, 0, 10))
	listBranches(t, srv, memberCookie(t), ws.ID, branchTestRepo)
	rows := auditRowsOf(audit, "workspace.branches.list")
	if len(rows) != 1 || rows[0].Target != ws.ID.String() || rows[0].Actor != capSub || rows[0].Outcome != "success" {
		t.Fatalf("rows = %+v, want one success by the member on the workspace", rows)
	}
	data := string(rows[0].Data)
	for _, want := range []string{`"repo":"` + branchTestRepo + `"`, `"credential":"ado_own_token"`, `"listing":"listed"`, `"count":3`} {
		if !strings.Contains(data, want) {
			t.Errorf("data %s lacks %s", data, want)
		}
	}
	for _, leak := range []string{"main", "release-q3", "topic-alpha", ownPATToken} {
		if strings.Contains(data, leak) {
			t.Errorf("data %s carries %q", data, leak)
		}
	}
}

// The GitHub arm asks the organisation's App about the one repository, only
// where the admitting row permits the App lane.
func TestWorkspaceBranches_GitHubApp(t *testing.T) {
	app := types.GitProvider{ID: "gh-1", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://github.com/acme"}, Lanes: []types.GitLane{types.GitLaneApp}}
	gh := &broker.FakeGitHubMinter{Branches: []string{"dev", "main"}, DefaultBranch: "main"}
	ws := branchWorkspace("", branchTestGitHub)
	srv, _ := newBranchServer(t, adoSite(app), nil, gh, ws)
	out := listBranches(t, srv, memberCookie(t), ws.ID, branchTestGitHub)
	if out.Listing != branchListingListed || out.Credential != branchCredGitHubApp || strings.Join(out.Branches, ",") != "main,dev" {
		t.Fatalf("App lane = %+v, want listed main,dev through github_app", out)
	}
	if strings.Join(gh.LastListedRepos, ",") != branchTestGitHub {
		t.Fatalf("App asked about %v, want only %s", gh.LastListedRepos, branchTestGitHub)
	}

	patOnly := app
	patOnly.Lanes = []types.GitLane{types.GitLanePAT}
	gh = &broker.FakeGitHubMinter{Branches: []string{"main"}}
	srv, _ = newBranchServer(t, adoSite(patOnly), nil, gh, ws)
	out = listBranches(t, srv, memberCookie(t), ws.ID, branchTestGitHub)
	if out.Listing != branchListingUnavailable || out.Reason != branchReasonNoCredential || len(gh.LastListedRepos) != 0 {
		t.Fatalf("row without the App lane = %+v (App asked about %v), want no_credential and no ask", out, gh.LastListedRepos)
	}

	gh = &broker.FakeGitHubMinter{BranchesErr: fmt.Errorf("x: %w", broker.ErrGitHubRefused)}
	srv, _ = newBranchServer(t, adoSite(app), nil, gh, ws)
	out = listBranches(t, srv, memberCookie(t), ws.ID, branchTestGitHub)
	if out.Reason != branchReasonRefused {
		t.Fatalf("GitHub refusal = %+v, want forge_refused", out)
	}
}

func auditRowsOf(rec *recRecorder, action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range rec.snapshot() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

// adoBranchTarget reads only the path after the organisation or collection
// from the repository URL; the base is the lane's. A Server row's base is its
// own collection URL.
func TestADOBranchTarget(t *testing.T) {
	server := types.GitProvider{ID: "ado-srv-1", Kind: types.GitProviderAzureDevOps, BaseURLs: []string{"https://tfs.corp.example/tfs/Coll"},
		Lanes: []types.GitLane{types.GitLanePAT}, CredentialSource: types.CredentialSourcePerUser}
	sc := adoSite(ownPATTestRow(), server)
	services := adoEntraRun{rowID: ownPATRowID, org: "contoso"}
	srv := adoEntraRun{rowID: "ado-srv-1", org: "tfs/coll", serverHost: "tfs.corp.example"}
	for _, tc := range []struct {
		lane                adoEntraRun
		repo                string
		base, project, name string
		ok                  bool
	}{
		{services, "https://dev.azure.com/contoso/proj/_git/app", adoOwnPATAPIBase + "/contoso", "proj", "app", true},
		{services, "https://contoso@dev.azure.com/contoso/proj/_git/app", adoOwnPATAPIBase + "/contoso", "proj", "app", true},
		{services, "https://contoso.visualstudio.com/DefaultCollection/proj/_git/app", adoOwnPATAPIBase + "/contoso", "proj", "app", true},
		{services, "https://dev.azure.com/contoso/_git/app", adoOwnPATAPIBase + "/contoso", "app", "app", true},
		{services, "https://dev.azure.com/contoso/My%20Project/_git/app", adoOwnPATAPIBase + "/contoso", "My Project", "app", true},
		{services, "https://dev.azure.com/contoso/proj/app", "", "", "", false},
		{services, "https://dev.azure.com/contoso/proj/_git/..", "", "", "", false},
		{services, "https://dev.azure.com/contoso/proj/_git/.", "", "", "", false},
		{services, "https://dev.azure.com/contoso/../_git/app", "", "", "", false},
		{services, "https://dev.azure.com/contoso/_git/..", "", "", "", false},
		{services, "https://dev.azure.com/contoso/proj/_git/%2e%2e", "", "", "", false},
		{services, "https://dev.azure.com/contoso/%2E%2E/_git/app", "", "", "", false},
		{srv, "https://tfs.corp.example/tfs/Coll/proj/_git/app", "https://tfs.corp.example/tfs/Coll", "proj", "app", true},
	} {
		base, project, name, ok := adoBranchTarget(sc, tc.lane, tc.repo)
		if base != tc.base || project != tc.project || name != tc.name || ok != tc.ok {
			t.Errorf("%s = %q %q %q %v, want %q %q %q %v", tc.repo, base, project, name, ok, tc.base, tc.project, tc.name, tc.ok)
		}
	}
}

// adoBranchGet maps the forge's answer to a reason: a refusal (401, 403 or a redirect to its
// sign-in page, never followed) is forge_refused; a transient status, a body that is not JSON and an
// unreachable host are forge_unreachable; a 200 decodes.
func TestADOBranchGetStatusMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"ok", http.StatusOK, `{"defaultBranch":"refs/heads/main"}`, ""},
		{"unauthorised", http.StatusUnauthorized, ``, branchReasonRefused},
		{"forbidden", http.StatusForbidden, ``, branchReasonRefused},
		{"sign-in redirect", http.StatusFound, ``, branchReasonRefused},
		{"unavailable", http.StatusServiceUnavailable, ``, branchReasonUnreachable},
		{"not json", http.StatusOK, `<html>`, branchReasonUnreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := adoOwnPATTransport
			adoOwnPATTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				h := http.Header{}
				if tc.status == http.StatusFound {
					h.Set("Location", "https://app.vssps.visualstudio.com/_signin")
				}
				return &http.Response{StatusCode: tc.status, Header: h, Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
			})
			t.Cleanup(func() { adoOwnPATTransport = prev })
			var out struct{ DefaultBranch string }
			if got := adoBranchGet(context.Background(), "https://dev.azure.com/contoso/p/_apis/git/repositories/app", "Bearer x", &out); got != tc.want {
				t.Errorf("%d %q = %q, want %q", tc.status, tc.body, got, tc.want)
			}
		})
	}
	prev := adoOwnPATTransport
	t.Cleanup(func() { adoOwnPATTransport = prev })
	adoOwnPATTransport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("dial refused") })
	var out struct{}
	if got := adoBranchGet(context.Background(), "https://dev.azure.com/x", "Bearer x", &out); got != branchReasonUnreachable {
		t.Errorf("a failed dial = %q, want %q", got, branchReasonUnreachable)
	}
}
