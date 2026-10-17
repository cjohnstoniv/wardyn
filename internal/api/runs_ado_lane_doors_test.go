// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const adoFabrikamRepo = "https://dev.azure.com/fabrikam/proj/_git/app"

// adoDoorServer is a member-governed server whose site config is site and whose
// onboarded workspaces hold repos.
func adoDoorServer(t *testing.T, site types.SiteConfig, repos ...string) *Server {
	t.Helper()
	srv, st, _ := govEscapeFixture(t, &capStore{})
	for _, r := range repos {
		st.workspaces = append(st.workspaces, types.Workspace{ID: uuid.New(), Name: "ws-" + uuid.NewString()[:6],
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: r}}})
	}
	st.siteConfig = site
	return srv
}

// adoDoorBody is a draft with the run's own repo, workspace repositories, a
// devcontainer repository and an Azure DevOps list, each when non-empty.
func adoDoorBody(repo, devcontainer string, ws []string, caps []string) string {
	pol := map[string]any{"min_confinement_class": "CC2", "allowed_domains": []string{"api.anthropic.com"}}
	if len(ws) > 0 {
		var repos []map[string]string
		for _, r := range ws {
			repos = append(repos, map[string]string{"repo": r})
		}
		pol["workspace_repos"] = repos
	}
	if caps != nil {
		pol["azure_devops_capabilities"] = caps
	}
	req := map[string]any{"agent": "claude-code", "task": "t", "confinement_class": "CC2", "inline_policy": pol}
	if repo != "" {
		req["repo"] = repo
	}
	if devcontainer != "" {
		req["devcontainer_repo"] = devcontainer
	}
	b, _ := json.Marshal(req)
	return string(b)
}

func wideADOSite() types.SiteConfig {
	row := adoEntraTestRow()
	row.BaseURLs = []string{"https://dev.azure.com"}
	return adoSite(row)
}

// T11: every caller resolves the lane with the run's own repository. Each row
// fails when its call site leaves `repo` out.
func TestADOLaneCallSitesSeeTheRunsRepo(t *testing.T) {
	member := func() *http.Request {
		return httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(operatorCtx(adoTestOwner, "m@corp.example", oidc.RoleUser))
	}
	site := adoSite(adoEntraTestRow())

	t.Run("a standing", func(t *testing.T) {
		srv := adoDoorServer(t, site)
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", govSession(t, adoTestOwner, []string{"eng"}, false),
			adoDoorBody(adoTestRepo, "", nil, []string{"pr"}))
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), reasonADOCapabilitiesNonePermitted) {
			t.Fatalf("preflight = %d %s, want 422 %s", w.Code, w.Body.String(), reasonADOCapabilitiesNonePermitted)
		}
	})

	t.Run("b overrides", func(t *testing.T) {
		srv := &Server{cfg: Config{Store: &adoTestStore{site: site}}}
		spec := types.RunPolicySpec{}
		req := &createRunRequest{Repo: adoTestRepo}
		base, refusal := srv.adoOverrideBase(context.Background(), member(), spec, req)
		if refusal != nil || !slices.Equal(base, adoEntraTestRow().Entra.DefaultProfile) {
			t.Fatalf("member base = %v refusal %+v, want the row default", base, refusal)
		}
		admin := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
		if _, refusal := srv.adoOverrideBase(context.Background(), admin, spec, req); refusal == nil || refusal.body.Reason != reasonOverrideRefused {
			t.Fatalf("admin-token base refusal = %+v, want %s (no_person)", refusal, reasonOverrideRefused)
		}
	})

	vssps := types.GrantSpec{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"host":"vssps.dev.azure.com","secret_name":"pat","access":"read"}`)}
	t.Run("c PAT door", func(t *testing.T) {
		srv := &Server{cfg: Config{}}
		spec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{vssps}}
		if reason, _ := srv.patNarrowingAtDoor(member(), createRunRequest{}, spec, site); reason != "" {
			t.Fatalf("no repository: refused %q, want accepted (vssps is not a git host)", reason)
		}
		if reason, _ := srv.patNarrowingAtDoor(member(), createRunRequest{Repo: adoTestRepo}, spec, site); reason != reasonGitPATNarrowingUnsupportedHost {
			t.Fatalf("repo-only: reason %q, want %s", reason, reasonGitPATNarrowingUnsupportedHost)
		}
	})

	t.Run("d revive", func(t *testing.T) {
		srv := &Server{cfg: Config{Store: &adoTestStore{site: site}, Audit: &memAudit{}, Now: time.Now}}
		run := types.AgentRun{ID: uuid.New(), CreatedBy: adoTestOwner, Repo: adoTestRepo}
		grants := []types.CredentialGrant{patRowWith(uuid.New(), run.ID, "vssps.dev.azure.com", map[string]any{"access": "read"})}
		cfg := &proxy.Config{ADOGrant: &proxy.ADOGrantConfig{Organization: "contoso", Hosts: adoEntraHosts("contoso")}}
		if rerr := srv.revivePATNarrowing(context.Background(), run, cfg, grants, types.ActorHuman, adoTestOwner); rerr == nil {
			t.Fatal("revive with the stored lane on vssps was allowed, want refused")
		}
		cfg.ADOGrant = nil
		if rerr := srv.revivePATNarrowing(context.Background(), run, cfg, grants, types.ActorHuman, adoTestOwner); rerr != nil {
			t.Fatalf("revive with no stored lane refused: %+v", rerr)
		}
	})

	t.Run("e credentialed destinations and local identity lanes", func(t *testing.T) {
		dests := credentialedDestinations(site, types.RunPolicySpec{}, adoTestRepo, adoTestOwner)
		if !slices.ContainsFunc(dests, func(d types.Destination) bool { return d.Host == "pkgs.dev.azure.com" }) {
			t.Errorf("credentialedDestinations = %+v, want the lane's hosts", dests)
		}
		run := types.AgentRun{ID: uuid.New(), CreatedBy: adoTestOwner, Repo: adoTestRepo}
		if ado, _ := localIdentityLanes(context.Background(), run, site, runner.SandboxSpec{}, adoTestOwner); ado.org != "contoso" {
			t.Errorf("localIdentityLanes = %+v, want contoso", ado)
		}
	})

	t.Run("f api_key on a lane host", func(t *testing.T) {
		srv := &Server{cfg: Config{Store: &adoTestStore{site: site}}}
		spec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"pkgs.dev.azure.com","secret_name":"k"}`)}}}
		if refusal := srv.credentialHostRefusal(member(), spec, ""); refusal != nil {
			t.Fatalf("no repository: refused %+v", refusal)
		}
		if refusal := srv.credentialHostRefusal(member(), spec, adoTestRepo); refusal == nil || refusal.body.Reason != reasonCredentialHostCollision {
			t.Fatalf("repo-only: refusal %+v, want %s", refusal, reasonCredentialHostCollision)
		}
	})

	t.Run("g token redirect onto a lane host", func(t *testing.T) {
		d := dispatchADOLane(t, feedSite(), nil, dispatchParams{ADOLaneRepo: adoTestRepo,
			Policy: types.RunPolicySpec{AllowedDomains: []string{"registry.npmjs.org"}}})
		ev := findAudit(d.events, d.run.ID, "run.artifact.redirect", "denied")
		if ev == nil || !strings.Contains(string(ev.Data), "the per-person Azure DevOps lane carries this host's credential") {
			t.Fatalf("run.artifact.redirect denied = %v, want the lane row", ev)
		}
		for _, in := range d.spec.ProxyConfig.Injection {
			if in.Rule.SecretName == feedToken {
				t.Errorf("the redirect's token is on the run: %+v", in.Rule)
			}
		}
	})
}

// T15: create, preflight and preview carry the identical decline sentence.
func TestADODeclineSentenceAtEveryDoor(t *testing.T) {
	for name, tc := range map[string]struct {
		site   types.SiteConfig
		ws     []string
		admin  bool
		repo   string
		reason adoLaneDecline
	}{
		"two organisations": {wideADOSite(), []string{adoTestRepo}, false, adoFabrikamRepo, adoDeclineCrossRepoConflict},
		"admin token":       {adoSite(adoEntraTestRow()), nil, true, adoTestRepo, adoDeclineNoPerson},
	} {
		for door, want := range map[string]int{"/api/v1/runs": http.StatusCreated, "/api/v1/runs/preflight": http.StatusOK, "/api/v1/runs/policy-preview": http.StatusOK} {
			t.Run(name+door, func(t *testing.T) {
				srv := adoDoorServer(t, tc.site, tc.ws...)
				body := adoDoorBody(tc.repo, "", tc.ws, nil)
				var w *httptest.ResponseRecorder
				if tc.admin {
					w = do(t, srv, http.MethodPost, door, adminToken, body)
				} else {
					w = doSSO(t, srv, http.MethodPost, door, govSession(t, adoTestOwner, []string{"eng"}, false), body)
				}
				if w.Code != want || !strings.Contains(w.Body.String(), tc.reason.sentence()) {
					t.Fatalf("%s = %d %s, want %d with the %s sentence", door, w.Code, w.Body.String(), want, tc.reason)
				}
			})
		}
	}
}

// T16: preview's ado_lane_declined is a run-level answer from the resolver
// launch uses; groups are untouched and devcontainer_repo is never an input.
// devcontainer_repo is an operator's field, so those drafts use the admin
// token: an ADO devcontainer_repo alone reports nothing, where an ADO repo would
// report no_person.
func TestPolicyPreview_ADOLaneDeclinedIsRunLevel(t *testing.T) {
	for name, tc := range map[string]struct {
		site         types.SiteConfig
		ws           []string
		repo, devc   string
		token        bool
		want         string
		wantGroupFor string
	}{
		"two orgs":                    {wideADOSite(), []string{adoTestRepo}, adoFabrikamRepo, "", false, string(adoDeclineCrossRepoConflict), ""},
		"devcontainer-only ADO":       {wideADOSite(), nil, "", adoFabrikamRepo, true, "", adoFabrikamRepo},
		"partial success (https+ssh)": {adoSite(adoEntraTestRow()), []string{adoTestSSHRepo}, adoTestRepo, "", false, "", ""},
		"admin token":                 {adoSite(adoEntraTestRow()), nil, adoTestRepo, "", true, string(adoDeclineNoPerson), ""},
		"a resolving draft":           {adoSite(adoEntraTestRow()), nil, adoTestRepo, "", false, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			srv := adoDoorServer(t, tc.site, tc.ws...)
			body := adoDoorBody(tc.repo, tc.devc, tc.ws, nil)
			var w *httptest.ResponseRecorder
			if tc.token {
				w = do(t, srv, http.MethodPost, "/api/v1/runs/policy-preview", adminToken, body)
			} else {
				w = doSSO(t, srv, http.MethodPost, "/api/v1/runs/policy-preview", govSession(t, adoTestOwner, []string{"eng"}, false), body)
			}
			if w.Code != http.StatusOK {
				t.Fatalf("preview = %d %s", w.Code, w.Body.String())
			}
			var got policyPreviewResponse
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.ADOLaneDeclined != tc.want {
				t.Errorf("ado_lane_declined = %q, want %q", got.ADOLaneDeclined, tc.want)
			}
			if strings.Count(w.Body.String(), `"ado_lane_declined"`) > 1 {
				t.Errorf("ado_lane_declined appears below the run level: %s", w.Body.String())
			}
			if tc.wantGroupFor != "" && !slices.ContainsFunc(got.RepositoryAccess, func(g policyPreviewRepository) bool {
				return slices.Contains(g.Repos, tc.wantGroupFor)
			}) {
				t.Errorf("repository_access = %+v, want a group listing %s", got.RepositoryAccess, tc.wantGroupFor)
			}
		})
	}
}
