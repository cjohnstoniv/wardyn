// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/url"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func gitFactOrg(repo previewRepo) string {
	if repo.kind != "github" {
		return repo.org
	}
	u, err := url.Parse(repo.url)
	if err != nil {
		return ""
	}
	org, _, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	if repo.host != "github.com" {
		return repo.host + "/" + org
	}
	return org
}

// enrichGitFacts reads the same admitted snapshots and standing-capability
// meet the doors use. It never widens them, probes a forge or mints a token.
func enrichGitFacts(f componentFactInputs, facts []componentFact) {
	for _, repo := range previewRepos(*f.req, f.spec, f.scmSite) {
		org, lane := gitFactOrg(repo), gitProviderLane(repo, f.spec, f.scmSite)
		i := slices.IndexFunc(facts, func(fact componentFact) bool {
			return fact.Provider == repo.kind && fact.Org == org && fact.Lane == lane
		})
		if i < 0 {
			continue
		}
		fact := &facts[i]
		fact.PushRules = f.spec.PushRules // current global block; keyed overrides remain refused
		installURL := gitFactInstallURL(repo)
		if f.ceiling.Operator {
			fact.InstallURL = installURL
		}
		if a, on := adoEntraRunForRepo(f.scmSite, repo.locator, ""); on {
			canWrite := slices.Contains(a.ceiling, adoscope.CapCodeWrite) && (f.ceiling.Operator ||
				slices.Contains(a.caps, adoscope.CapCodeWrite) || slices.Contains(f.ceiling.Spec.AzureDevOpsCapabilities, adoscope.CapCodeWrite))
			if a, ok := a.withPolicyCapabilities(f.spec.AzureDevOpsCapabilities, adoStandingFor(f.ceiling)); ok {
				fact.Capabilities, fact.CapabilityCeiling = capabilityStrings(a.caps), capabilityStrings(a.ceiling)
				fact.TokenMode = a.tokenMode
				if a.tokenMode == types.ADOTokenModeOwnPAT {
					fact.TokenScopes = ownTokenScopeFacts(a.ceiling, a.serverHost != "")
				}
				write := slices.Contains(a.caps, adoscope.CapCodeWrite)
				if write || slices.Contains(a.caps, adoscope.CapCodeRead) {
					appendRepoAccess(fact, repo.url, write, canWrite)
				}
			}
			continue
		}
		if lane != gitLaneNone {
			access := gitRepoPolicyAccess(repo, lane, f.spec)
			if access != "" {
				canWrite := gitRepoPolicyAccess(repo, lane, f.ceiling.Spec) == types.PATAccessWrite
				appendRepoAccess(fact, repo.url, access == types.PATAccessWrite, canWrite)
			}
		}
		if installURL != "" {
			// Presence of a configured link says nothing about installation.
			// Members receive useful action text without the operator's URL.
			fact.Requirements = []SetupItem{{Kind: "repo_credential", ID: fact.ID, Label: "GitHub App installation", RequiredBy: "workspace", Status: "unverified", Detail: "Ask an operator to verify the GitHub App installation for this organisation."}}
		}
	}
}

// One provider row may claim both cloud and enterprise addresses. Its cloud
// App metadata applies only to a repository on the broker's supported forge.
func gitFactInstallURL(repo previewRepo) string {
	row := repo.verdict.Provider
	if repo.host == "github.com" && repo.verdict.Admitted && row.Kind == types.GitProviderGitHub &&
		validateGitHubAppInstallURL(0, row) == nil {
		return row.GitHubAppInstallURL
	}
	return ""
}

func capabilityStrings(caps []adoscope.Capability) []string {
	out := make([]string, len(caps))
	for i, cap := range caps {
		out[i] = string(cap)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func appendRepoAccess(f *componentFact, repo string, write, canWrite bool) {
	if slices.ContainsFunc(f.RepoAccess, func(a client.RepoAccessFact) bool { return a.Repo == repo }) {
		return
	}
	access := types.PATAccessRead
	if write {
		access = types.PATAccessWrite
	}
	f.RepoAccess = append(f.RepoAccess, client.RepoAccessFact{Repo: repo, Access: access, CanWrite: canWrite})
}

// Selection follows persistRunGrants: the last PAT/SSH grant for a host,
// the last explicit GitHub grant for a repo, or the first empty template.
// An empty PAT repo set grants no access; it must never read as a read grant.
func gitRepoPolicyAccess(repo previewRepo, lane string, spec types.RunPolicySpec) string {
	if lane == gitLaneDirect {
		return types.PATAccessRead
	}
	var selected, firstGitHub *types.GrantSpec
	for i := range spec.EligibleGrants {
		grant := &spec.EligibleGrants[i]
		if !gitGrantServes(*grant, repo) {
			continue
		}
		switch {
		case lane == string(types.GitLaneSSH) && grant.Kind == types.GrantSSHKey,
			lane == string(types.GitLanePAT) && grant.Kind == types.GrantGitPAT:
			selected = grant
		case lane == string(types.GitLaneApp) && grant.Kind == types.GrantGitHubToken:
			if firstGitHub == nil {
				firstGitHub = grant
			}
			if slices.ContainsFunc(githubScopeRepos(grant.Scope), func(key string) bool {
				return strings.EqualFold(key, gitBrokerKey(repo.url))
			}) {
				selected = grant
			}
		}
	}
	if selected == nil && firstGitHub != nil && len(githubScopeRepos(firstGitHub.Scope)) == 0 {
		selected = firstGitHub
	}
	if selected == nil {
		return ""
	}
	switch selected.Kind {
	case types.GrantSSHKey:
		// No read-only narrowing; policy access, not forge permission proof.
		return types.PATAccessWrite
	case types.GrantGitHubToken:
		var scope struct {
			Permissions map[string]string `json:"permissions"`
		}
		if json.Unmarshal(selected.Scope, &scope) == nil &&
			(scope.Permissions["contents"] == types.PATAccessRead || scope.Permissions["contents"] == types.PATAccessWrite) {
			return scope.Permissions["contents"]
		}
	case types.GrantGitPAT:
		scope, err := types.DecodeGitPATScope(selected.Scope)
		if err == nil && patScopeCoversRepo(scope, repo.url) {
			return scope.Normalize().Access
		}
	}
	return ""
}

func patScopeCoversRepo(scope types.GitPATScope, address string) bool {
	if scope.Repos == nil {
		return true
	}
	u, err := url.Parse(address)
	if err != nil {
		return false
	}
	path := strings.TrimPrefix(u.Path, "/")
	if scope.Normalize().Forge == types.PATForgeBitbucketServer {
		var valid bool
		path, valid = strings.CutPrefix(path, "scm/")
		if !valid {
			return false
		}
	}
	key, ok := types.PATRepoKey(scope.Forge, path)
	return ok && slices.ContainsFunc(*scope.Repos, func(entry string) bool {
		entryKey, valid := types.PATRepoKey(scope.Forge, entry)
		return valid && types.PATRepoCovers(entryKey, key)
	})
}

// The labels are the token page's actual widest-per-area scopes. Covers is
// derived from the canonical scope table, including read capabilities covered
// by an area's wider write scope. Auxiliary Graph identity binding is not a
// capability and therefore has an empty covers array.
func ownTokenScopeFacts(ceiling []adoscope.Capability, server bool) []client.TokenScopeFact {
	var out []client.TokenScopeFact
	labels := adoOwnPATTokenScopes(ceiling)
	if server {
		labels = adoServerTokenScopes
	}
	for _, label := range labels {
		fact := client.TokenScopeFact{Scope: label, Covers: []string{}}
		for _, cap := range ceiling {
			scopes, err := adoscope.ScopesFor([]adoscope.Capability{cap})
			if err != nil {
				continue
			}
			for _, scope := range scopes {
				page, ok := adoTokenPageScopes[strings.TrimPrefix(scope, adoscope.ResourceID+"/")]
				if ok && strings.HasPrefix(label, page.area+" (") && !slices.Contains(fact.Covers, string(cap)) {
					fact.Covers = append(fact.Covers, string(cap))
				}
			}
		}
		slices.Sort(fact.Covers)
		out = append(out, fact)
	}
	return out
}
