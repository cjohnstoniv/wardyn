// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	neturl "net/url"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func groundGitHubGrants(spec *types.RunPolicySpec, detectedGitHub, detectedOther []string) []string {
	var warns []string
	kept := spec.EligibleGrants[:0]
	dropped := 0
	for _, g := range spec.EligibleGrants {
		if g.Kind != types.GrantGitHubToken {
			kept = append(kept, g)
			continue
		}
		if len(detectedGitHub) == 0 {
			dropped++
			continue // no remote -> drop the github token entirely
		}
		var sc struct {
			Repos       []string          `json:"repos"`
			Permissions map[string]string `json:"permissions"`
		}
		_ = json.Unmarshal(g.Scope, &sc)
		sc.Repos = detectedGitHub // override the guess with detected reality
		if b, err := json.Marshal(sc); err == nil {
			g.Scope = b
		}
		kept = append(kept, g)
	}
	spec.EligibleGrants = kept
	if dropped > 0 {
		warns = append(warns, "no GitHub git remote detected in the workspace; dropped the proposed github_token grant (nothing to scope it to)")
	} else if len(detectedGitHub) > 0 {
		warns = append(warns, "scoped github_token to the workspace's detected remote(s): "+strings.Join(detectedGitHub, ", "))
	}
	if len(detectedOther) > 0 {
		warns = append(warns, "non-GitHub remote host(s) detected ("+strings.Join(detectedOther, ", ")+"); add a git_pat grant with a stored PAT to broker credentials for these hosts")
	}
	return warns
}

// groundGitPATGrants makes the proposal's non-GitHub PAT access reflect the
// LOCAL workspace's ACTUAL non-github remotes (detectedOther): a git_pat grant
// is KEPT only when its host matches a detected remote host, and DROPPED with a
// warning otherwise (a stored PAT brokered for a host the workspace never uses
// is needless standing access). It never fabricates a grant the model didn't
// request (least privilege), mirroring groundGitHubGrants.
func groundGitPATGrants(spec *types.RunPolicySpec, detectedOther []string) []string {
	if len(spec.EligibleGrants) == 0 {
		return nil
	}
	detected := make(map[string]bool, len(detectedOther))
	for _, h := range detectedOther {
		detected[strings.ToLower(h)] = true
	}
	var warns []string
	kept := spec.EligibleGrants[:0]
	for _, g := range spec.EligibleGrants {
		if g.Kind != types.GrantGitPAT {
			kept = append(kept, g)
			continue
		}
		host, _, _, derr := gitPATScopeFields(g.Scope)
		if derr != nil || !detected[strings.ToLower(host)] {
			warns = append(warns, "dropped a git_pat grant (host "+host+"): no matching non-GitHub remote detected in the workspace")
			continue
		}
		warns = append(warns, "kept git_pat grant for detected non-GitHub remote host "+host)
		kept = append(kept, g)
	}
	spec.EligibleGrants = kept
	return warns
}

// composeWorkspaceGitRepos splits req.Workspaces' explicit WorkspaceGit-kind
// selections into GitHub-shaped ("owner/repo", case PRESERVED) and other
// entries — the same (github, other) shape gitremote.DetectGitHubRepos
// returns for a local directory's detected remotes — so groundGitHubGrants
// can ground a composed run's github_token grant on an EXPLICIT operator
// selection exactly like it already grounds one on a DETECTED local remote.
// See the "ground" stage call site (W15-b) for why this must feed the SAME
// grounding set.
//
// Deliberately does NOT reuse gitBrokerKeyFromSlug (workspace_run.go): that
// helper recognizes github.com the same way but LOWERCASES the result for its
// own purpose (a git-broker MAP KEY) — wrong here, where the value rides
// straight into a github_token grant's scope.repos.
func composeWorkspaceGitRepos(wss []composer.Workspace) (github, other []string) {
	for _, ws := range wss {
		if ws.Kind != composer.WorkspaceGit {
			continue
		}
		repo := strings.TrimSpace(ws.Repo)
		if repo == "" || !repoFieldSafe(repo) {
			continue
		}
		if cloneURL := repoCloneURL(repo); cloneURL != "" {
			if u, err := neturl.Parse(cloneURL); err == nil && strings.EqualFold(u.Hostname(), "github.com") {
				if slug := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"); strings.Count(slug, "/") == 1 {
					github = append(github, slug)
					continue
				}
			}
		}
		other = append(other, repo)
	}
	return github, other
}

// widenCeilingRepoAllowlist returns a ceiling copy whose github_token grant's
// repo scope is widened to repos when the ceiling's OWN grant sets none.
// composer.Clamp treats an empty ceiling repo list as DENY-ALL (W23-S1-3's
// RBAC floor for a hand-authored/ungrounded proposal) — but the compose and
// profile-synthesis pipelines (the two callers) have ALREADY grounded their
// own proposal's repos to something provably real (a detected git remote, an
// operator-selected git workspace, or a prior run's already-clamped grant)
// before calling Clamp, so widening the ceiling copy here keeps that
// legitimate access instead of silently dropping it. Never mutates the
// shared ceiling passed in (types.RunPolicySpec.Clone gives an independent
// copy first) — ceiling is commonly s.cfg.DefaultPolicy, the process-global
// default every run resolves against.
func widenCeilingRepoAllowlist(ceiling types.RunPolicySpec, repos []string) types.RunPolicySpec {
	if len(repos) == 0 {
		return ceiling
	}
	// Clone unconditionally (cheap, off the hot path — one call per compose/
	// profile request): ceiling.EligibleGrants[i].Scope is a shared backing
	// array whenever ceiling is s.cfg.DefaultPolicy itself, and writing into it
	// in place would corrupt the process-global default for every later run.
	out := ceiling.Clone()
	for i, g := range out.EligibleGrants {
		if g.Kind != types.GrantGitHubToken || len(ghScopeRepos(g.Scope)) > 0 {
			continue
		}
		var sc struct {
			Repos       []string          `json:"repos"`
			Permissions map[string]string `json:"permissions"`
		}
		_ = json.Unmarshal(g.Scope, &sc)
		sc.Repos = repos
		if b, err := json.Marshal(sc); err == nil {
			out.EligibleGrants[i].Scope = b
		}
	}
	return out
}

// ghScopeRepos decodes a github_token grant scope's repos list ("" fields
// tolerated — an undecodable/absent scope simply has no repos).
func ghScopeRepos(scope json.RawMessage) []string {
	var sc struct {
		Repos []string `json:"repos"`
	}
	_ = json.Unmarshal(scope, &sc)
	return sc.Repos
}

// synthGitHubRepos collects every repo a synthesized Recording-Mode profile's
// own github_token grant(s) already carry (internal/api/profile.go). They are
// provably real: recordmode.Synthesize derives them from grants the SOURCE
// run actually held, and that run's own grant was itself already clamped once
// at creation — never a raw, ungrounded guess the way a hand-authored
// inline_policy's would be. widenCeilingRepoAllowlist uses this to keep that
// access under an operator ceiling that places no repo allowlist, without
// reopening the deny-all floor Clamp now applies to a genuinely ungrounded
// proposal.
func synthGitHubRepos(spec types.RunPolicySpec) []string {
	var repos []string
	for _, g := range spec.EligibleGrants {
		if g.Kind == types.GrantGitHubToken {
			repos = append(repos, ghScopeRepos(g.Scope)...)
		}
	}
	return repos
}

// groundAPIKeySecretNames rewrites LLM-proposed api_key secret names that can
// never exist: the model may invent env-var-style names (e.g.
// "ANTHROPIC_API_KEY") that secretNameRE rejects, so the setup checklist's
// add-secret fix would dead-end in the dialog's name validation and the launch
// would 422 on a secret nobody can create. A name that fails secretNameRE gets
// the provider's canonical name when the host is a known LLM provider (same
// source of truth as agentLLMProvider), else a mechanical sanitize. Names the
// store accepts are left alone — the model's valid choice stands. Mirrors the
// deterministic-grounding rule: the model never invents unusable refs.
//
// W15-W15b-composer-pipeline-2: also stamps the provider's canonical
// header+format onto any api_key grant whose host is a known LLM provider
// and whose scope carries neither — mapProposedGrant (schema.go) builds an
// api_key grant's scope from ONLY {host, secret_name}, so a composer
// proposal's grant NEVER carries header/format at all. injectionRuleFromScope
// (runs_scm.go) then defaults an empty header/format to "Authorization" /
// "Bearer %s" — correct for OpenAI, WRONG for Anthropic, which requires
// "x-api-key: <key>" (a bare key, no Bearer prefix). The proxy would inject a
// header Anthropic's API rejects while the checklist reports model access
// "satisfied" — a run that provisions cleanly and then 401s on its first
// model call.
func groundAPIKeySecretNames(spec *types.RunPolicySpec) []string {
	var warns []string
	for i, g := range spec.EligibleGrants {
		if g.Kind != types.GrantAPIKey {
			continue
		}
		var scope map[string]any
		if err := json.Unmarshal(g.Scope, &scope); err != nil {
			continue // validation rejects undecodable scopes later
		}
		name, _ := scope["secret_name"].(string)
		host, _ := scope["host"].(string)
		changed := false

		if name != "" && !secretNameRE.MatchString(name) {
			fixed, ok := canonicalSecretForHost(host)
			if !ok {
				fixed = sanitizeSecretName(name)
			}
			if fixed != "" && fixed != name {
				scope["secret_name"] = fixed
				warns = append(warns, fmt.Sprintf("normalized api_key secret name %q to storable %q (host %s)", name, fixed, host))
				changed = true
			}
		}

		headerEmpty := scope["header"] == nil || scope["header"] == ""
		formatEmpty := scope["format"] == nil || scope["format"] == ""
		if headerEmpty && formatEmpty {
			if header, format, ok := canonicalHeaderForHost(host); ok {
				scope["header"] = header
				scope["format"] = format
				warns = append(warns, fmt.Sprintf("api_key grant for %s carried no header/format — stamped the provider's own (%s), not the injector's generic Bearer default", host, header))
				changed = true
			}
		}

		if !changed {
			continue
		}
		raw, err := json.Marshal(scope)
		if err != nil {
			continue
		}
		spec.EligibleGrants[i].Scope = raw
	}
	return warns
}

// canonicalHeaderForHost returns the header+format a known LLM provider host
// authenticates with (agentLLMProvider's own convention — the SAME table
// canonicalSecretForHost reads), ok=false for an unknown host (a proposal
// naming a third-party api_key host is left to injectionRuleFromScope's
// generic Bearer default, which is honest for a host this table knows
// nothing about).
func canonicalHeaderForHost(host string) (header, format string, ok bool) {
	for _, agent := range []string{"claude-code", "codex-cli"} {
		if p, pok := agentLLMProvider(agent); pok && p.host == host {
			return p.header, p.format, true
		}
	}
	return "", "", false
}

// canonicalSecretForHost maps a known LLM provider host to its canonical secret
// name via the agentLLMProvider table (single source of truth).
func canonicalSecretForHost(host string) (string, bool) {
	for _, agent := range []string{"claude-code", "codex-cli"} {
		if p, ok := agentLLMProvider(agent); ok && p.host == host {
			return p.secret, true
		}
	}
	return "", false
}

// sanitizeSecretName lowercases and maps a proposed name onto secretNameRE's
// alphabet ('_' and spaces become '-', other invalid runes drop, edge
// punctuation trims); returns "" when nothing storable remains.
func sanitizeSecretName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		case r == '_', r == ' ':
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), ".-_")
	if !secretNameRE.MatchString(out) {
		return ""
	}
	return out
}

// handleListComposerBackends returns the configured composer backends (no
// secrets) for the UI provider dropdown. 404 when the composer is disabled.
