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

// githubCLIRepos is the run's GitHub repositories the CLI may reach: the ones
// create keys the git broker by, derived the same way. Each wired github_token
// grant's own repositories, and with any such grant wired the run's declared
// repositories on github.com (persistRunGrants, then augmentGitBrokerGrants).
// The devcontainer repository is only read for its build and is not one.
func githubCLIRepos(req createRunRequest, spec types.RunPolicySpec, site types.SiteConfig) []string {
	var out []string
	wired := false
	for _, g := range lanesWiredFor(site, req.Agent, repoLocatorsOf(spec.WorkspaceRepos), spec.EligibleGrants) {
		if g.Kind != types.GrantGitHubToken {
			continue
		}
		// A built-in component's grant serves only its own repositories; create never maps the
		// run's declared ones onto it (firstGitHubGrantID skips it).
		wired = wired || !isBuiltinGitHubGrant(g)
		for _, repo := range githubScopeRepos(g.Scope) {
			out = append(out, strings.ToLower(repo))
		}
	}
	if wired {
		for _, slug := range append(repoLocatorsOf(spec.WorkspaceRepos), req.Repo) {
			if key := gitBrokerKeyFromSlug(slug); key != "" {
				out = append(out, key)
			}
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

// persistGitHubCLIGrants completes create's git wiring: the git broker's
// repositories (augmentGitBrokerGrants), then the GitHub CLI's grants over
// them, one github_token grant per owner (an installation token covers one
// owner's repositories) whose scope sets api and carries the capabilities. The
// door's grant lands on gw for dispatch. A refusal is written here; false
// means the caller aborts the run.
func (s *Server) persistGitHubCLIGrants(ctx context.Context, w http.ResponseWriter, r *http.Request, runID uuid.UUID, now time.Time,
	legacyRepo string, wsRepos []types.WorkspaceRepo, comps runComponents, gw *grantWiring,
) bool {
	gw.augmentGitBrokerGrants(legacyRepo, wsRepos)
	caps, attached := comps.githubCLICapabilities()
	switch {
	case !attached:
		return true
	case len(caps) == 0:
		return !runError(http.StatusUnprocessableEntity, reasonComponentDefinitionInvalid, "the GitHub CLI row grants no capability the GitHub CLI may hold").write(s, w, r)
	case len(gw.gitGrants) == 0:
		return !runError(http.StatusUnprocessableEntity, reasonCLINoGitHubRepo, accessSentences[reasonCLINoGitHubRepo]).write(s, w, r)
	}
	door := &proxy.GitHubAPIGrant{Capabilities: caps, Repos: slices.Sorted(maps.Keys(gw.gitGrants)), Grants: map[string]uuid.UUID{}}
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
			return !runServerError("create grant", err).write(s, w, r)
		}
		door.Grants[owner] = id
	}
	gw.githubAPI = door
	return true
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

// githubDoorLane is how the ceiling re-assertion names the GitHub CLI door in dropped_broker_lanes.
const githubDoorLane = "github_api"

// The GitHub CLI row's refusal under a ceiling that walls GitHub off (componentHostsBounded): the
// row the person defined, then an organisation's, which componentRefused words as "this component ...".
const (
	githubCLIBlockedProblem    = "cli: GitHub is blocked by your organisation."
	githubCLIBlockedOrgProblem = "uses the GitHub CLI, and GitHub is blocked by your organisation."
)

// githubDoorWithheld reports whether a ceiling's denies take the GitHub CLI door away. The door
// follows the git broker's family rule, not an exact-host one: a profile that denies any host
// the broker re-originates to (github.com among them) has walled GitHub off, and a credentialed
// lane on api.github.com must not outlive that. Plain egress to api.github.com stays exact-host.
func githubDoorWithheld(deny []string) bool { return ceilingDeniesAny(deny, gitBrokerManagedHosts) }

// closeGitHubDoor puts back what confineBrokerEgress lifted for the door: the host's :443 allow
// goes and its deny returns, so a run whose door was withheld meets the same wall as one that
// never had a door.
func closeGitHubDoor(policy *types.RunPolicySpec) {
	policy.AllowedDomains = slices.DeleteFunc(slices.Clone(policy.AllowedDomains), func(d string) bool { return d == ghscope.APIHost+":443" })
	if !ceilingDenies(policy.DeniedDomains, ghscope.APIHost) {
		policy.DeniedDomains = append(slices.Clip(policy.DeniedDomains), ghscope.APIHost)
	}
}

// dropGitHubDoor withholds the door at dispatch when the ceiling walls GitHub off: no grant, no
// inert token in the sandbox, and the host closed again. It returns the lane it dropped, if any.
func dropGitHubDoor(deny []string, policy *types.RunPolicySpec, p *dispatchParams, sandboxEnv map[string]string) []string {
	if p.Components.GitHubAPI == nil || !githubDoorWithheld(deny) {
		return nil
	}
	p.Components.GitHubAPI = nil
	closeGitHubDoor(policy)
	if sandboxEnv["GH_TOKEN"] == githubCLITokenSentinel {
		delete(sandboxEnv, "GH_TOKEN")
		delete(sandboxEnv, "GH_PROMPT_DISABLED")
	}
	return []string{githubDoorLane}
}
