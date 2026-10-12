// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The GitHub CLI arm. An organisation's GitHub CLI row gives a run `gh`
// against the run's own GitHub repositories, as the organisation's GitHub App:
// the sandbox holds only an inert GH_TOKEN, and the proxy's api.github.com door
// (internal/egress/proxy/github_api.go) judges each request and adds the App
// installation token it mints from this arm's grants.

// githubCLITokenSentinel is the sandbox's GH_TOKEN: it authenticates nothing,
// and the door replaces it on every request it forwards.
const githubCLITokenSentinel = "wardyn-proxy-injects-github-token"

// githubCLICapabilities is the union of the attached GitHub CLI rows'
// capabilities, without any a CLI token never holds, and whether any row is
// attached at all.
func (c runComponents) githubCLICapabilities() (caps []ghscope.Capability, attached bool) {
	for _, a := range c.attached {
		if cli := a.snapshot.Definition.CLI; cli != nil && cli.Kind == types.CLIKindGitHub && cli.GitHub != nil {
			attached = true
			caps = append(caps, types.CLIGitHubCapabilities(cli.GitHub.Capabilities)...)
		}
	}
	slices.Sort(caps)
	return slices.Compact(caps), attached
}

// githubCLIRepos is the run's GitHub repositories the App lane serves, as the
// git broker keys them: the repositories the CLI may reach. The devcontainer
// repository is only read for its build and is not one of them.
func githubCLIRepos(req createRunRequest, spec types.RunPolicySpec, site types.SiteConfig) []string {
	req.DevcontainerRepo = ""
	var out []string
	for _, repo := range previewRepos(req, spec, site) {
		if repo.kind != "github" || gitProviderLane(repo, spec, site) != string(types.GitLaneApp) {
			continue
		}
		if key := gitBrokerKeyFromSlug(repo.locator); key != "" {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// githubCLIRepoRefusal refuses a run that attaches a GitHub CLI row but has no
// GitHub repository on the App lane for it to act on.
func githubCLIRepoRefusal(comps runComponents, req createRunRequest, spec types.RunPolicySpec, site types.SiteConfig) *runRefusal {
	if _, attached := comps.githubCLICapabilities(); !attached || len(githubCLIRepos(req, spec, site)) > 0 {
		return nil
	}
	return runError(http.StatusUnprocessableEntity, reasonCLINoGitHubRepo, accessSentences[reasonCLINoGitHubRepo])
}

// persistGitHubCLIGrants writes the GitHub CLI's grants once the git broker's
// repositories are known: one github_token grant per owner (an installation
// token covers one owner's repositories) whose scope sets api and carries the
// capabilities, and returns the door's grant for dispatch. Nil without a row.
func (s *Server) persistGitHubCLIGrants(ctx context.Context, runID uuid.UUID, now time.Time, comps runComponents, gitGrants map[string]uuid.UUID) (*proxy.GitHubAPIGrant, *runRefusal) {
	caps, attached := comps.githubCLICapabilities()
	switch {
	case !attached:
		return nil, nil
	case len(caps) == 0:
		return nil, runError(http.StatusUnprocessableEntity, reasonComponentDefinitionInvalid, "the GitHub CLI row grants no capability the GitHub CLI may hold")
	case len(gitGrants) == 0:
		return nil, runError(http.StatusUnprocessableEntity, reasonCLINoGitHubRepo, accessSentences[reasonCLINoGitHubRepo])
	}
	door := &proxy.GitHubAPIGrant{Capabilities: caps, Repos: slices.Sorted(maps.Keys(gitGrants)), Grants: map[string]uuid.UUID{}}
	byOwner := map[string][]string{}
	for _, repo := range door.Repos {
		owner, _, _ := strings.Cut(repo, "/")
		byOwner[owner] = append(byOwner[owner], repo)
	}
	for _, owner := range slices.Sorted(maps.Keys(byOwner)) {
		id := uuid.New()
		scope := mustJSON(map[string]any{"repos": byOwner[owner], "api": true, "capabilities": caps})
		if _, err := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{ID: id, RunID: runID, CreatedAt: now,
			Spec: types.GrantSpec{Kind: types.GrantGitHubToken, Scope: scope, TTLSeconds: 3600}}); err != nil {
			return nil, runServerError("create grant", err)
		}
		door.Grants[owner] = id
	}
	return door, nil
}

// confineBrokerEgress is confineGitBrokerEgress plus the GitHub CLI door, the
// one route left to api.github.com when the run has one: the confinement's own
// deny of the host is lifted (a deny the run already carried stays), its TLS
// port is allowed so the door can terminate it, and the sandbox gets the
// inert token gh needs to send a request at all.
func confineBrokerEgress(policy *types.RunPolicySpec, p dispatchParams, sandboxEnv map[string]string) []string {
	door := p.Components.GitHubAPI
	carried := slices.ContainsFunc(policy.DeniedDomains, func(d string) bool {
		return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".") == ghscope.APIHost
	})
	confined := confineGitBrokerEgress(policy, p.GitGrants)
	if door == nil {
		return confined
	}
	if !carried {
		policy.DeniedDomains = slices.DeleteFunc(slices.Clone(policy.DeniedDomains), func(d string) bool { return d == ghscope.APIHost })
	}
	policy.AllowedDomains = append(slices.Clip(policy.AllowedDomains), ghscope.APIHost+":443")
	sandboxEnv["GH_TOKEN"] = githubCLITokenSentinel
	sandboxEnv["GH_PROMPT_DISABLED"] = "1"
	return confined
}
