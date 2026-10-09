// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// admissionCredentialHosts is what dispatch would hand the proxy, read at the
// door: each source it lists, and each thing it leaves out.
func TestAdmissionCredentialHosts(t *testing.T) {
	pat := func(host string, api bool) types.GrantSpec {
		scope := map[string]any{"host": host, "secret_name": "forge-pat"}
		if api {
			scope["api"], scope["forge"] = true, "gitlab"
		}
		return types.GrantSpec{Kind: types.GrantGitPAT, Scope: mustJSON(scope)}
	}
	gated := apiKeyOn("gated.example")
	gated.RequiresApproval = true
	mirror := func(to, token string) types.EgressRedirect {
		return types.EgressRedirect{From: "https://registry.npmjs.org/", To: to, Ecosystem: "npm", TokenSecretRef: token}
	}
	for _, tc := range []struct {
		name    string
		spec    types.RunPolicySpec
		site    types.SiteConfig
		subject string
		hosts   int
		collide bool
	}{
		{name: "nothing"},
		{name: "a bare ceiling entry names no host", spec: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey}, {Kind: types.GrantAPIKey}}}},
		{name: "two hosts", spec: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{apiKeyOn("a.example"), apiKeyOn("b.example")}}, hosts: 2},
		{name: "two grants, one host on another port", spec: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{apiKeyOn("a.example"), apiKeyOn("a.example:8443")}}, hosts: 2, collide: true},
		{name: "an approval-gated grant is never injected", spec: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{gated, apiKeyOn("gated.example")}}, hosts: 1},
		{name: "a forge API door on a credential's host", spec: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{apiKeyOn("gitlab.corp"), pat("gitlab.corp", true)}}, hosts: 2, collide: true},
		{name: "a PAT that stays on the git route", spec: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{apiKeyOn("gitlab.corp"), pat("gitlab.corp", false)}}, hosts: 1},
		{name: "a token redirect on a credential's host", spec: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{apiKeyOn("mirror.corp")}},
			site: types.SiteConfig{EgressRedirects: []types.EgressRedirect{mirror("https://mirror.corp/npm", "tok")}}, hosts: 2, collide: true},
		{name: "a token-less redirect authors no credential", spec: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{apiKeyOn("mirror.corp")}},
			site: types.SiteConfig{EgressRedirects: []types.EgressRedirect{mirror("https://mirror.corp/npm", "")}}, hosts: 1},
		{name: "one mirror behind several redirects is one credential",
			site: types.SiteConfig{EgressRedirects: []types.EgressRedirect{mirror("https://mirror.corp/npm", "tok"), mirror("https://Mirror.Corp:8443/pypi", "tok")}}, hosts: 1},
		{name: "the Azure DevOps lane's hosts", spec: types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: adoTestRepo}}},
			site: adoSite(adoEntraTestRow()), subject: adoTestOwner, hosts: len(adoContosoHosts)},
		{name: "a policy credential on an Azure DevOps lane host", spec: types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: adoTestRepo}},
			EligibleGrants: []types.GrantSpec{apiKeyOn("dev.azure.com")}},
			site: adoSite(adoEntraTestRow()), subject: adoTestOwner, hosts: len(adoContosoHosts) + 1, collide: true},
		// The lane takes the host: a token redirect onto a lane host is not a
		// second credential on a run the lane resolves for.
		{name: "a token redirect onto an Azure DevOps lane host, the lane resolves", spec: types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: adoTestRepo}}},
			site: feedSite(), subject: adoTestOwner, hosts: len(adoContosoHosts)},
		{name: "the same redirect on another port of the lane host", spec: types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: adoTestRepo}}},
			site: func() types.SiteConfig {
				sc := adoSite(adoEntraTestRow())
				sc.EgressRedirects = []types.EgressRedirect{mirror("https://PKGS.dev.azure.com:8443/contoso/_packaging/feed/npm/registry/", "tok")}
				return sc
			}(), subject: adoTestOwner, hosts: len(adoContosoHosts)},
		{name: "the same redirect on a run the lane does not resolve for keeps its token", spec: types.RunPolicySpec{},
			site: feedSite(), subject: adoTestOwner, hosts: 1},
		{name: "a policy credential on that redirect's host, no lane", spec: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{apiKeyOn("pkgs.dev.azure.com")}},
			site: feedSite(), subject: adoTestOwner, hosts: 2, collide: true},
		{name: "a policy credential on a lane host is still a second credential, redirect or not", spec: types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: adoTestRepo}},
			EligibleGrants: []types.GrantSpec{apiKeyOn("pkgs.dev.azure.com")}},
			site: feedSite(), subject: adoTestOwner, hosts: len(adoContosoHosts) + 1, collide: true},
		{name: "a token redirect onto a host the lane does not carry, beside the lane", spec: types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: adoTestRepo}}},
			site: func() types.SiteConfig {
				sc := adoSite(adoEntraTestRow())
				sc.EgressRedirects = []types.EgressRedirect{mirror("https://mirror.corp/npm", "tok")}
				return sc
			}(), subject: adoTestOwner, hosts: len(adoContosoHosts) + 1},
		{name: "the lane does not resolve for nobody", spec: types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: adoTestRepo}},
			EligibleGrants: []types.GrantSpec{apiKeyOn("dev.azure.com")}}, site: adoSite(adoEntraTestRow()), hosts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hosts := admissionCredentialHosts(tc.spec, tc.site, tc.subject)
			if len(hosts) != tc.hosts {
				t.Errorf("hosts = %+v, want %d", hosts, tc.hosts)
			}
			if _, _, found := firstCredentialCollision(hosts); found != tc.collide {
				t.Errorf("collision = %v, want %v (hosts %+v)", found, tc.collide, hosts)
			}
		})
	}
}
