// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The hosts the dry doors' fold adds are the hosts create dispatches, lane by
// lane: a workspace's registries and denies, its clone host, an ssh_key's
// endpoint and a git_pat's Azure DevOps bundle. The site-config SCM hosts are
// the one lane create adds that the dry doors withhold: no reader of theirs
// may see the operator's configured host list.
func TestDerivedEgress_PreviewEqualsDispatch(t *testing.T) {
	sshGrant := types.GrantSpec{Kind: types.GrantSSHKey, Scope: mustJSON(map[string]string{"host": "github.com", "key_secret_ref": "deploy-key"})}
	patGrant := gitPATGrant("dev.azure.com", "ado-pat")
	ws := types.Workspace{ID: uuid.New(), Name: "app", ApprovedEgress: []string{"registry.example"}, DeniedEgress: []string{"bad.example"},
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "https://gitlab.example/team/app"}}}
	sshEndpoint, _ := sshOver443Endpoint("github.com")
	for _, tc := range []struct {
		lane   string
		wsRefs []types.Workspace
		grants []types.GrantSpec
		repo   string
		want   string
	}{
		{"workspace", []types.Workspace{ws}, nil, "", "registry.example"},
		{"clone", []types.Workspace{ws}, nil, "", "gitlab.example"},
		{"site", nil, nil, "https://ghes.corp.example/team/app", "ghes.corp.example"},
		{"ssh", nil, []types.GrantSpec{sshGrant}, "", sshEndpoint},
		{"git_pat", nil, []types.GrantSpec{patGrant}, "", "dev.azure.com"},
	} {
		t.Run(tc.lane, func(t *testing.T) {
			var site types.SiteConfig
			if tc.lane == "site" {
				site.ScmHosts = []string{"ghes.corp.example"}
			}
			base := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, EligibleGrants: tc.grants}
			req := createRunRequest{Agent: "claude-code", Repo: tc.repo}
			// The dry doors: the fold's last step.
			f := runFold{spec: base.Clone(), wsRefs: tc.wsRefs, req: &req, scmSite: site}
			rec := &foldRecorder{}
			if !(&Server{}).stepDerivedEgress(&f, rec) {
				t.Fatal("stepDerivedEgress answered")
			}
			// Create: every lane the grants open is wired.
			gw := grantWiring{gitPATGrants: map[string]string{"dev.azure.com": "pat"}, sshGrants: map[string]string{"github.com": "ssh"}}
			dispatched := base.Clone()
			newComponentFixture(t).srv.unionRunEgress(context.Background(), uuid.New(), &dispatched, gw, tc.wsRefs, tc.repo, site, nil, runComponents{}, nil)
			if !slices.Contains(dispatched.AllowedDomains, tc.want) {
				t.Fatalf("dispatch lacks %s: %v", tc.want, dispatched.AllowedDomains)
			}
			if tc.lane == "site" {
				if slices.Contains(f.spec.AllowedDomains, tc.want) {
					t.Errorf("the dry doors disclose the configured SCM host: %v", f.spec.AllowedDomains)
				}
				return
			}
			if !slices.Equal(f.spec.AllowedDomains, dispatched.AllowedDomains) || !slices.Equal(f.spec.DeniedDomains, dispatched.DeniedDomains) {
				t.Errorf("preview allows %v denies %v; dispatch allows %v denies %v",
					f.spec.AllowedDomains, f.spec.DeniedDomains, dispatched.AllowedDomains, dispatched.DeniedDomains)
			}
			if !slices.ContainsFunc(rec.rows, func(r provenanceRow) bool { return r.Field == fieldAllowed && r.Value == tc.want }) &&
				tc.lane != "workspace" && tc.lane != "clone" {
				t.Errorf("no provenance row for %s: %+v", tc.want, rec.rows)
			}
		})
	}
}

// A lane create declined to wire adds no host at dispatch, and its grant no
// longer declares a repository for the site-config SCM hosts.
func TestDerivedEgress_VetoedLaneAddsNothingAtDispatch(t *testing.T) {
	spec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{gitPATGrant("dev.azure.com", "ado-pat")}}
	site := types.SiteConfig{ScmHosts: []string{"ghes.corp.example"}}
	newComponentFixture(t).srv.unionRunEgress(context.Background(), uuid.New(), &spec, grantWiring{}, nil, "", site, nil, runComponents{}, nil)
	if len(spec.AllowedDomains) != 0 {
		t.Errorf("a vetoed git_pat lane still dispatched %v", spec.AllowedDomains)
	}
}

// The dry doors add hosts only for the lanes create wires. Create here is its
// real grant wiring (persistRunGrants, then applySSHLaneWarnings): an ssh_key
// on an agent with no SSH lane, an ssh lane the admitting row turns off, and a
// git_pat on Azure DevOps Services each add nothing at either door.
func TestDerivedEgress_VetoedLanesMatchDispatch(t *testing.T) {
	sshGrant := types.GrantSpec{Kind: types.GrantSSHKey, Scope: mustJSON(map[string]string{"host": "github.com", "key_secret_ref": compOperatorSecret})}
	githubAppOnly := types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{
		{ID: "gh", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://github.com"}, Lanes: []types.GitLane{types.GitLaneApp}}}}}
	adoServices := types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{
		{ID: "ado", Kind: types.GitProviderAzureDevOps, BaseURLs: []string{"https://dev.azure.com/contoso"}, Lanes: []types.GitLane{types.GitLanePAT}}}}}
	sshEndpoint, _ := sshOver443Endpoint("github.com")
	for _, tc := range []struct {
		name, agent string
		site        types.SiteConfig
		grant       types.GrantSpec
		dropped     string
	}{
		{"codex-cli has no ssh lane", "codex-cli", types.SiteConfig{}, sshGrant, sshEndpoint},
		{"the row turns the ssh lane off", "claude-code", githubAppOnly, sshGrant, sshEndpoint},
		{"Azure DevOps Services has no pat lane", "claude-code", adoServices, gitPATGrant("dev.azure.com", "ado-pat"), "dev.azure.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, EligibleGrants: []types.GrantSpec{tc.grant}}
			req := createRunRequest{Agent: tc.agent}
			f := runFold{spec: base.Clone(), req: &req, scmSite: tc.site}
			if !(&Server{}).stepDerivedEgress(&f, &foldRecorder{}) {
				t.Fatal("stepDerivedEgress answered")
			}
			fx := newComponentFixture(t)
			fx.st.siteConfig = tc.site
			ctx, runID := context.Background(), uuid.New()
			w, r := httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
			dispatched := base.Clone()
			gw, ok := fx.srv.persistRunGrants(ctx, w, r, runID, time.Now(), dispatched, types.PlacementRemote, localJudgement{})
			if !ok {
				t.Fatalf("persistRunGrants answered %d %s", w.Code, w.Body.String())
			}
			fx.srv.applySSHLaneWarnings(ctx, req, runID, &gw)
			fx.srv.unionRunEgress(ctx, runID, &dispatched, gw, nil, "", tc.site, nil, runComponents{}, nil)
			if slices.Contains(dispatched.AllowedDomains, tc.dropped) {
				t.Fatalf("create wired the lane: %v", dispatched.AllowedDomains)
			}
			if !slices.Equal(f.spec.AllowedDomains, dispatched.AllowedDomains) {
				t.Errorf("preview allows %v; dispatch allows %v", f.spec.AllowedDomains, dispatched.AllowedDomains)
			}
		})
	}
}
