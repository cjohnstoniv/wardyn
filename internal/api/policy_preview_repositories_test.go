// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPolicyPreviewRepositoryLocators(t *testing.T) {
	for _, tc := range []struct {
		name, repo, want, kind string
	}{
		{"slug", "acme/one", "https://github.com/acme/one.git", "github"},
		{"https", "https://PRIVATE-USER:PRIVATE-PASSWORD@GitHub.com/Acme/one.git?token=PRIVATE-QUERY#PRIVATE-FRAGMENT", "https://github.com/Acme/one.git", "github"},
		{"http port", "http://PRIVATE-USER:PRIVATE-PASSWORD@Forge.example:8080/Group/Sub/one.git/?token=PRIVATE-QUERY#PRIVATE-FRAGMENT", "http://forge.example:8080/Group/Sub/one.git", "other"},
		{"ipv6", "https://[2001:db8::1]:8443/Group/one.git", "https://[2001:db8::1]:8443/Group/one.git", "other"},
		{"scp", "PRIVATE-USER@GitHub.com:acme/one.git", "ssh://github.com/acme/one.git", "github"},
		{"scp alias", "PRIVATE-USER@ssh.github.com:acme/one.git?token=PRIVATE-QUERY#PRIVATE-FRAGMENT", "ssh://github.com/acme/one.git", "github"},
		{"ssh", "ssh://PRIVATE-USER:PRIVATE-PASSWORD@github.com/acme/one.git?token=PRIVATE-QUERY#PRIVATE-FRAGMENT", "ssh://github.com/acme/one.git", "github"},
		{"ssh alias port", "ssh://PRIVATE-USER@ssh.github.com:443/acme/one.git", "ssh://github.com/acme/one.git", "github"},
		{"ssh alias trailing dot", "ssh://PRIVATE-USER@SSH.GitHub.com.:443/acme/one.git", "ssh://github.com/acme/one.git", "github"},
		{"ado scp", "PRIVATE-USER@ssh.dev.azure.com:v3/contoso/project/one", "ssh://dev.azure.com/v3/contoso/project/one", "azure_devops"},
		{"ado ssh", "ssh://PRIVATE-USER@dev.azure.com:443/v3/contoso/project/one", "ssh://dev.azure.com/v3/contoso/project/one", "azure_devops"},
		{"ado escaped path", "https://dev.azure.com/contoso/Payments%20Platform/_git/Card%20Auth", "https://dev.azure.com/contoso/Payments%20Platform/_git/Card%20Auth", "azure_devops"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := providerRunFixture(t, types.SiteConfig{}, &capStore{}, nil)
			forbidPreviewSideEffects(t, srv)
			body := fmt.Sprintf(`{"agent":"claude-code","repo":%q}`, tc.repo)
			response := doSSO(t, srv, http.MethodPost, policyPreviewPath, ssoSession(t, "member", "m@example.com", oidc.RoleUser), body)
			access := previewResult(t, response).RepositoryAccess
			want := []policyPreviewRepository{{Kind: tc.kind, Repos: []string{tc.want}, DefaultProfile: []adoscope.Capability{}, CapabilityCeiling: []adoscope.Capability{}}}
			if !reflect.DeepEqual(access, want) {
				t.Fatalf("repository identity: got %+v, want %+v", access, want)
			}
			if strings.Contains(response.Body.String(), "PRIVATE-") {
				t.Fatalf("URL credentials leaked: %s", response.Body.String())
			}
		})
	}
}

func TestPolicyPreviewDistinctAndEquivalentSSHRepositories(t *testing.T) {
	for _, tc := range []struct {
		name, other string
		want        []string
	}{
		{"distinct", "ssh://git@ssh.github.com:443/acme/two.git", []string{"ssh://github.com/acme/one.git", "ssh://github.com/acme/two.git"}},
		{"equivalent", "ssh://git@ssh.github.com:443/acme/one.git", []string{"ssh://github.com/acme/one.git"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := providerRunFixture(t, types.SiteConfig{}, &capStore{}, nil)
			token := providerAdminToken(srv, "operator")
			forbidPreviewSideEffects(t, srv)
			body := fmt.Sprintf(`{"agent":"claude-code","repo":"git@github.com:acme/one.git","devcontainer_repo":%q}`, tc.other)
			response := do(t, srv, http.MethodPost, policyPreviewPath, token, body)
			access := previewResult(t, response).RepositoryAccess
			if len(access) != 1 || access[0].Kind != "github" || !slices.Equal(access[0].Repos, tc.want) {
				t.Fatalf("SSH repository identities: %+v, want %v", access, tc.want)
			}
		})
	}
}

func TestPolicyPreviewADORepositoryFactsStayTogether(t *testing.T) {
	read := []adoscope.Capability{adoscope.CapCodeRead, adoscope.CapProjectRead}
	write := []adoscope.Capability{adoscope.CapCodeRead, adoscope.CapCodeWrite, adoscope.CapProjectRead}
	all := []adoscope.Capability{adoscope.CapCodeRead, adoscope.CapCodeWrite, adoscope.CapPR, adoscope.CapProjectRead}
	for _, tc := range []struct {
		name          string
		secondProfile []adoscope.Capability
		secondCeiling []adoscope.Capability
		groups        int
	}{
		{"different profile", read, all, 2},
		{"different ceiling", write, write, 2},
		{"different profile and ceiling", read, read, 2},
		{"equivalent reordered duplicated sets", write, all, 1},
	} {
		for _, reverseRepos := range []bool{false, true} {
			for _, reverseProviders := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/repos_reversed=%t/providers_reversed=%t", tc.name, reverseRepos, reverseProviders), func(t *testing.T) {
					first, second := ownPATTestRow(), ownPATTestRow()
					first.ID, second.ID = "private-first-provider", "private-second-provider"
					first.BaseURLs = []string{"https://dev.azure.com/shared", "https://dev.azure.com/first", "https://dev.azure.com/private-unused-first"}
					second.BaseURLs = []string{"https://dev.azure.com/shared", "https://dev.azure.com/second", "https://dev.azure.com/private-unused-second"}
					first.Entra.DefaultProfile, first.Entra.CapabilityCeiling = slices.Clone(write), slices.Clone(all)
					second.Entra.DefaultProfile = append(slices.Clone(tc.secondProfile), tc.secondProfile[0])
					second.Entra.CapabilityCeiling = append(slices.Clone(tc.secondCeiling), tc.secondCeiling[0])
					slices.Reverse(second.Entra.DefaultProfile)
					slices.Reverse(second.Entra.CapabilityCeiling)
					rows := []types.GitProvider{first, second}
					if reverseProviders {
						slices.Reverse(rows)
					}
					site := types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: rows}}
					if err := validateWorkspaceProviders(site.WorkspaceProviders, true); err != nil {
						t.Fatalf("invalid provider fixture: %v", err)
					}
					before := string(mustJSON(site))
					firstRepo, secondRepo := "https://dev.azure.com/first/project/_git/repo", "https://dev.azure.com/second/project/_git/repo"
					ws := &types.Workspace{ID: uuid.New(), Name: "allowed", Status: types.WorkspaceScanned, Sources: []types.WorkspaceSource{
						{Type: types.WorkspaceSourceTypeRepo, Source: firstRepo, Target: "/home/agent/first"},
						{Type: types.WorkspaceSourceTypeRepo, Source: secondRepo, Target: "/home/agent/second"},
					}}
					if reverseRepos {
						slices.Reverse(ws.Sources)
					}
					srv := providerRunFixture(t, site, &capStore{}, ws)
					forbidPreviewSideEffects(t, srv)
					body := `{"agent":"claude-code","workspace_id":"` + ws.ID.String() + `"}`
					response := doSSO(t, srv, http.MethodPost, policyPreviewPath, ssoSession(t, "member", "m@example.com", oidc.RoleUser), body)
					access := previewResult(t, response).RepositoryAccess
					if len(access) != tc.groups {
						t.Fatalf("got %d groups, want %d: %+v", len(access), tc.groups, access)
					}
					seen := map[string]bool{}
					for _, group := range access {
						if group.Kind != "azure_devops" || group.Org != "https://dev.azure.com/shared" {
							t.Fatalf("wrong authorized group: %+v", group)
						}
						for _, repo := range group.Repos {
							profile, ceiling := write, all
							if repo == secondRepo {
								profile, ceiling = tc.secondProfile, tc.secondCeiling
							} else if repo != firstRepo {
								t.Fatalf("unexpected repository %q", repo)
							}
							if seen[repo] || !slices.Equal(group.DefaultProfile, profile) || !slices.Equal(group.CapabilityCeiling, ceiling) {
								t.Fatalf("wrong or duplicate facts for %s: %+v; want %v / %v", repo, group, profile, ceiling)
							}
							seen[repo] = true
						}
					}
					if len(seen) != 2 || strings.Contains(response.Body.String(), "private-") || string(mustJSON(site)) != before {
						t.Fatalf("lost facts, leaked provider details or mutated configuration: %s", response.Body.String())
					}
				})
			}
		}
	}
}
