// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
	"github.com/google/uuid"
)

func TestComponentFacts_GitOrgAndInstallIsolationAtDoors(t *testing.T) {
	const install = "https://github.com/apps/wardyn-test/installations/new"
	for _, operator := range []bool{false, true} {
		for _, door := range dryDoors {
			f := newComponentFixture(t)
			row := githubRow("private-github-row", false, "https://github.com")
			row.GitHubAppInstallURL = install
			f.st.siteConfig = types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{row}}}
			f.srv.cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, AllowAllEgress: true, PushRules: &types.PushRulesSpec{DenyPaths: []string{"private/**"}}}
			id := uuid.New()
			f.st.ws = &types.Workspace{ID: id, Name: "Approved workspace", Status: types.WorkspaceScanned, Sources: []types.WorkspaceSource{
				{Type: types.WorkspaceSourceTypeRepo, Source: "https://github.com/acme/one", Target: "/home/agent/one"},
				{Type: types.WorkspaceSourceTypeRepo, Source: "https://github.com/other/two", Target: "/home/agent/two"},
			}}
			f.st.workspaces = []types.Workspace{*f.st.ws}
			body := `{"agent":"claude-code","task":"facts","workspace_id":"` + id.String() + `"}`
			tier := kernelUserTier
			if operator {
				tier = kernelTiers[0]
			}
			w := kernelLaunch(t, f.srv, f.st.govEscapeStore, tier, []string{"eng"}, door, body)
			if w.Code != http.StatusOK {
				t.Fatalf("operator=%v %s = %d %s", operator, door, w.Code, w.Body.String())
			}
			var response struct {
				Components []client.ComponentFact `json:"components"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			var git []client.ComponentFact
			for _, fact := range response.Components {
				if fact.Kind == types.ComponentGitProvider {
					git = append(git, fact)
				}
			}
			if len(git) != 2 || git[0].Org != "acme" || git[1].Org != "other" || git[0].ID == git[1].ID {
				t.Fatalf("distinct organisations collapsed: %+v", git)
			}
			for _, fact := range git {
				componentFactGolden(t, "github_"+fact.Org+"_"+strconv.FormatBool(operator), fact)
				if fact.Status != componentUnknown || len(fact.RepoAccess) != 1 || fact.RepoAccess[0].Access != "read" || fact.RepoAccess[0].CanWrite || fact.PushRules == nil || !slices.Equal(fact.PushRules.DenyPaths, []string{"private/**"}) {
					t.Fatalf("fact must reflect the actual direct read/current push policy: %+v", fact)
				}
				if (fact.InstallURL == install) != operator || len(fact.Requirements) == 0 || fact.Requirements[0].Status != "unverified" {
					t.Fatalf("operator link/member guidance = %+v", fact)
				}
			}
			if strings.Contains(w.Body.String(), row.ID) || !operator && strings.Contains(w.Body.String(), install) {
				t.Fatalf("member/provider isolation failed: %s", w.Body.String())
			}
		}
	}
}

func TestComponentFacts_ADOResolvedStandingAndScopes(t *testing.T) {
	row := adoEntra(adoRow("private-ado-row", false, "https://dev.azure.com/acme"))
	row.Entra.CapabilityCeiling = []adoscope.Capability{adoscope.CapCodeRead, adoscope.CapCodeWrite, adoscope.CapProjectRead}
	row.Entra.DefaultProfile = []adoscope.Capability{adoscope.CapCodeRead, adoscope.CapProjectRead}
	req := createRunRequest{Agent: "claude-code", Repo: "https://dev.azure.com/acme/project/_git/one"}
	policy := types.RunPolicySpec{AzureDevOpsCapabilities: []adoscope.Capability{adoscope.CapCodeRead, adoscope.CapCodeWrite}}
	fold := runFold{mode: foldPreflight, req: &req, spec: policy, scmSite: types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{row}}}}
	facts := componentFacts(fold, &SCMAccess{Kind: "azure_devops", Org: "https://dev.azure.com/acme", State: modelAccessExpiring})
	if len(facts) != 2 {
		t.Fatalf("facts = %+v", facts)
	}
	git := facts[0]
	if git.Status != componentReady || !slices.Equal(git.Capabilities, []string{string(adoscope.CapCodeRead)}) || git.TokenMode != types.ADOTokenModeOwnPAT || len(git.TokenScopes) == 0 || git.RepoAccess[0].Access != "read" || git.RepoAccess[0].CanWrite {
		t.Fatalf("member standing meet was widened: %+v", git)
	}
	fold.ceiling.Operator = true
	git = componentFacts(fold, nil)[0]
	if git.Status != componentUnknown || git.RepoAccess[0].Access != "write" || !git.RepoAccess[0].CanWrite {
		t.Fatalf("operator policy standing access = %+v", git)
	}
	var codeScope *client.TokenScopeFact
	for i := range git.TokenScopes {
		if git.TokenScopes[i].Scope == "Code (Read & write)" {
			codeScope = &git.TokenScopes[i]
		}
	}
	if codeScope == nil || !slices.Equal(codeScope.Covers, []string{string(adoscope.CapCodeRead), string(adoscope.CapCodeWrite)}) {
		t.Fatalf("widest token scope must cover read and write: %+v", git.TokenScopes)
	}
}

func TestComponentFacts_GitPATPerRepositoryAccess(t *testing.T) {
	req := createRunRequest{Agent: "none", Repo: "https://git.corp.example/acme/one"}
	read := types.GrantSpec{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"host":"git.corp.example","secret_name":"never-publish","access":"read","repos":["acme/one"]}`)}
	write := types.GrantSpec{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"host":"git.corp.example","secret_name":"never-publish","access":"write","repos":["acme/one"]}`)}
	site := types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{{ID: "private-row", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://git.corp.example/acme"}, Lanes: []types.GitLane{types.GitLanePAT}}}}}
	for _, current := range []types.GrantSpec{read, write} {
		fold := runFold{mode: foldPreview, req: &req, scmSite: site, spec: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{current}}, ceiling: governanceCeiling{Spec: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{write}}}}
		git := componentFacts(fold, nil)[0]
		if git.Org != "git.corp.example/acme" || len(git.RepoAccess) != 1 || !git.RepoAccess[0].CanWrite || (git.RepoAccess[0].Access == "write") != (string(current.Scope) == string(write.Scope)) {
			t.Fatalf("PAT fact = %+v", git)
		}
		wire, err := json.Marshal(git)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(wire), "never-publish") || strings.Contains(string(wire), "private-row") {
			t.Fatalf("PAT fact carries credential metadata: %s", wire)
		}
	}
}

func TestComponentFacts_SelectedGitGrantCannotWidenRepositoryAccess(t *testing.T) {
	repo := previewRepo{kind: "github", host: "git.corp.example", url: "https://git.corp.example/acme/one"}
	pat := func(access, repos string) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"host":"git.corp.example","secret_name":"own","access":"` + access + `","repos":` + repos + `}`)}
	}
	write := pat("write", `["acme/one"]`)
	for _, tc := range []struct {
		name   string
		grants []types.GrantSpec
		want   string
	}{
		{"later read replaces earlier write", []types.GrantSpec{write, pat("read", `["acme/one"]`)}, "read"},
		{"later empty replaces earlier write", []types.GrantSpec{write, pat("write", `[]`)}, ""},
		{"later other repo replaces earlier write", []types.GrantSpec{write, pat("write", `["acme/two"]`)}, ""},
		{"explicit empty is not the omission default", []types.GrantSpec{pat("read", `[]`)}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gitRepoPolicyAccess(repo, "pat", types.RunPolicySpec{EligibleGrants: tc.grants}); got != tc.want {
				t.Fatalf("selected policy access = %q, want %q", got, tc.want)
			}
		})
	}
	repo.host, repo.url = "github.com", "https://github.com/acme/one"
	app := func(repos, access string) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":` + repos + `,"permissions":{"contents":"` + access + `"}}`)}
	}
	for _, tc := range []struct {
		grants []types.GrantSpec
		want   string
	}{
		{[]types.GrantSpec{app(`["acme/one"]`, "write"), app(`["acme/one"]`, "read")}, "read"},
		{[]types.GrantSpec{app(`[]`, "read")}, "read"},
		{[]types.GrantSpec{app(`["other/two"]`, "write")}, ""},
	} {
		if got := gitRepoPolicyAccess(repo, "app", types.RunPolicySpec{EligibleGrants: tc.grants}); got != tc.want {
			t.Fatalf("selected App access = %q, want %q", got, tc.want)
		}
	}
}

func TestComponentFacts_ADOServerTokenScopes(t *testing.T) {
	row := adoServerPAT(adoRow("server-private", false, "https://tfs.corp.example/collection"))
	req := createRunRequest{Agent: "none", Repo: "https://tfs.corp.example/collection/project/_git/one"}
	git := componentFacts(runFold{mode: foldPreview, req: &req, scmSite: types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{row}}}}, nil)[0]
	if git.TokenMode != types.ADOTokenModeOwnPAT || len(git.TokenScopes) != len(adoServerTokenScopes) || git.TokenScopes[0].Scope != adoServerTokenScopes[0] || git.RepoAccess[0].Access != "write" {
		t.Fatalf("Server's git-only token must not acquire Services Graph scope: %+v", git)
	}
}

func TestComponentFacts_GitHubMixedHostInstallBinding(t *testing.T) {
	const install = "https://github.com/apps/wardyn-test/installations/new"
	row := githubRow("private-mixed-row", false, "https://github.com/acme", "https://git.corp.example/acme")
	row.GitHubAppInstallURL = install
	if err := validateWorkspaceProviders(&types.WorkspaceProviders{Git: []types.GitProvider{row}}, true); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"github.com", "git.corp.example"} {
		for _, operator := range []bool{false, true} {
			req := createRunRequest{Agent: "none", Repo: "https://" + host + "/acme/one"}
			fold := runFold{mode: foldPreview, req: &req, spec: types.RunPolicySpec{AllowAllEgress: true},
				scmSite: types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{row}}},
				ceiling: governanceCeiling{Operator: operator}}
			git := componentFacts(fold, nil)[0]
			if (git.InstallURL == install) != (operator && host == "github.com") ||
				(len(git.Requirements) != 0) != (host == "github.com") {
				t.Fatalf("host=%s operator=%v: cloud App metadata crossed forge: %+v", host, operator, git)
			}
		}
	}
}
