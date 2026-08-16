// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// widenCeilingRepoAllowlist returns a ceiling copy whose github_token grant's
// repo scope is widened to repos when the ceiling's OWN grant sets none.
// composer.Clamp treats an empty ceiling repo list as DENY-ALL (W23-S1-3's
// RBAC floor for a hand-authored/ungrounded proposal) — but profile.go
// (Record Mode's policy synthesis, the sole caller) has ALREADY grounded its
// own proposal's repos to something provably real (a prior run's already-
// clamped grant) before calling Clamp, so widening the ceiling copy here
// keeps that legitimate access instead of silently dropping it. Never
// mutates the shared ceiling passed in (types.RunPolicySpec.Clone gives an
// independent copy first) — ceiling is commonly s.cfg.DefaultPolicy, the
// process-global default every run resolves against.
func widenCeilingRepoAllowlist(ceiling types.RunPolicySpec, repos []string) types.RunPolicySpec {
	if len(repos) == 0 {
		return ceiling
	}
	// Clone unconditionally (cheap, off the hot path — one call per profile
	// request): ceiling.EligibleGrants[i].Scope is a shared backing array
	// whenever ceiling is s.cfg.DefaultPolicy itself, and writing into it in
	// place would corrupt the process-global default for every later run.
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
// at creation — never a raw, ungrounded guess. widenCeilingRepoAllowlist uses
// this to keep that access under an operator ceiling that places no repo
// allowlist, without reopening the deny-all floor Clamp applies to a
// genuinely ungrounded proposal.
func synthGitHubRepos(spec types.RunPolicySpec) []string {
	var repos []string
	for _, g := range spec.EligibleGrants {
		if g.Kind == types.GrantGitHubToken {
			repos = append(repos, ghScopeRepos(g.Scope)...)
		}
	}
	return repos
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
