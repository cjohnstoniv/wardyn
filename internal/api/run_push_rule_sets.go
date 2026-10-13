// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// pushRuleSetsFor is the dispatch plan's push rules: one set per SCM entry the
// run brings (a repository the policy clones or the request names), each the
// product after the ceiling. The policy's block (the ceiling's rules are already
// in it, composer.Clamp having met them) is laid into every entry's set first;
// the request's rules for that entry may only add a deny or a review path, so a
// person's rule never displaces an administrator's. A run with no entry carries
// no sets, and one with no rules anywhere needs no entry derived at all.
//
// The entry of a remote is derived from the remote itself, with the base path of
// the Azure DevOps Server provider row that claims its host (an unrecorded
// prefix makes the remote an error, never every collection under one entry).
func (s *Server) pushRuleSetsFor(spec types.RunPolicySpec, req *createRunRequest, sc types.SiteConfig) (sets []types.PushRuleSet, applied []client.PushRuleOverride, err error) {
	var adds []client.PushRuleOverride
	if req != nil && req.Overrides != nil {
		adds = req.Overrides.PushRules
	}
	if !spec.PushRules.IsSet() && len(adds) == 0 {
		return nil, nil, nil
	}
	for _, repo := range pushRuleRemotes(spec, req) {
		cloneURL := repoCloneURL(repo)
		if cloneURL == "" {
			continue // admission refuses an unreadable locator; nothing is pushed there
		}
		entry, key, err := pushRuleEntryOf(sc, cloneURL)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", repo, err)
		}
		if slices.ContainsFunc(sets, func(o types.PushRuleSet) bool { return o.Key() == key }) {
			continue
		}
		rules := types.PushRulesSpec{}
		if spec.PushRules != nil {
			rules = *spec.PushRules
			rules.DenyPaths = slices.Clone(rules.DenyPaths)
			rules.RequireReviewPaths = slices.Clone(rules.RequireReviewPaths)
		}
		for _, add := range adds {
			if add.PushRuleKey() != key {
				continue
			}
			// Only the paths the set did not already hold took effect.
			took := client.PushRuleOverride{Provider: add.Provider, Host: add.Host, Org: add.Org,
				DenyPaths:          slices.DeleteFunc(slices.Clone(add.DenyPaths), func(p string) bool { return slices.Contains(rules.DenyPaths, p) }),
				RequireReviewPaths: slices.DeleteFunc(slices.Clone(add.RequireReviewPaths), func(p string) bool { return slices.Contains(rules.RequireReviewPaths, p) })}
			rules.DenyPaths = appendMissing(rules.DenyPaths, add.DenyPaths)
			rules.RequireReviewPaths = appendMissing(rules.RequireReviewPaths, add.RequireReviewPaths)
			if len(took.DenyPaths)+len(took.RequireReviewPaths) > 0 {
				applied = append(applied, took)
			}
		}
		if rules.IsSet() {
			entry.Rules = rules
			sets = append(sets, entry)
		}
	}
	if err := types.ValidatePushRuleSets(sets); err != nil {
		return nil, nil, err
	}
	return sets, applied, nil
}

// pushRuleEntryOf is the SCM entry a remote's push rules are filed under: its
// identity (no rules yet) and canonical key, the one the dispatch plan and the
// component facts both derive.
func pushRuleEntryOf(sc types.SiteConfig, cloneURL string) (entry types.PushRuleSet, key string, err error) {
	provider, basePath := pushRuleProvider(sc, cloneURL)
	host, org, err := types.PushRuleEntryOfServerRemote(provider, cloneURL, basePath)
	if err != nil {
		return types.PushRuleSet{}, "", err
	}
	if key, err = types.PushRuleKey(provider, host, org); err != nil {
		return types.PushRuleSet{}, "", err
	}
	return types.PushRuleSet{Provider: provider, Host: host, Org: org, BasePath: basePath}, key, nil
}

// pushRuleRemotes are the repositories a run brings: the request's own, every
// one its policy clones and every one a built-in GitHub component reaches.
func pushRuleRemotes(spec types.RunPolicySpec, req *createRunRequest) []string {
	var out []string
	if req != nil && req.Repo != "" {
		out = append(out, req.Repo)
	}
	remotes := repoLocatorsOf(spec.WorkspaceRepos)
	if req != nil {
		remotes = append(remotes, builtinGitHubRemotes(req.Components)...)
	}
	for _, r := range remotes {
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

func appendMissing(dst, add []string) []string {
	for _, p := range add {
		if !slices.Contains(dst, p) {
			dst = append(dst, p)
		}
	}
	return dst
}

// pushRuleProvider is the provider kind of a clone URL and, for an Azure DevOps
// Server, the base path of the row that claims its host: the path of that row's
// base URL. The row that claims the host decides the kind; with no row, the host
// names it (dev.azure.com and *.visualstudio.com are Azure DevOps, the rest GitHub).
func pushRuleProvider(sc types.SiteConfig, cloneURL string) (provider, basePath string) {
	t, ok := parseCloneTarget(cloneURL, adoServerHosts(sc))
	if !ok {
		return string(types.GitProviderGitHub), ""
	}
	for _, row := range gitProviderRows(sc) {
		if !rowClaimsHost(row, t) {
			continue
		}
		if row.Kind == types.GitProviderAzureDevOps && adoEgressDomains(t.host) == nil {
			for _, raw := range row.BaseURLs {
				if u, err := url.Parse(raw); err == nil && hostrules.HostOf(raw) == t.host {
					return string(row.Kind), strings.TrimRight(u.Path, "/")
				}
			}
		}
		return string(row.Kind), ""
	}
	if adoEgressDomains(t.host) != nil {
		return string(types.GitProviderAzureDevOps), ""
	}
	return string(types.GitProviderGitHub), ""
}

// pushReviewRules reports whether the run's rules include a require-review
// path, wherever they are carried: the proxy then refuses an unattended push
// that would be held.
func pushReviewRules(policy types.RunPolicySpec, sets []types.PushRuleSet) bool {
	if len(sets) == 0 {
		return policy.PushRules != nil && len(policy.PushRules.RequireReviewPaths) > 0
	}
	if policy.PushRules != nil && len(policy.PushRules.RequireReviewPaths) > 0 {
		return true // an entry with no set is held to the base, which can hold a push
	}
	return slices.ContainsFunc(sets, func(s types.PushRuleSet) bool { return len(s.Rules.RequireReviewPaths) > 0 })
}

// pushRuleBase is the run's push rules as the ceiling left them, for the proxy to
// hold an entry with no set to (an App or a PAT reaches more than the entries the
// run brought). Carried only beside sets: without them the policy's own block is
// the run-wide regime.
func pushRuleBase(policy types.RunPolicySpec, sets []types.PushRuleSet) *types.PushRulesSpec {
	if len(sets) == 0 || !policy.PushRules.IsSet() {
		return nil
	}
	base := *policy.PushRules
	return &base
}
