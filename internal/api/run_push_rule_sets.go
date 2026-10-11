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
func (s *Server) pushRuleSetsFor(spec types.RunPolicySpec, req *createRunRequest, sc types.SiteConfig) ([]types.PushRuleSet, error) {
	var adds []client.PushRuleOverride
	if req != nil && req.Overrides != nil {
		adds = req.Overrides.PushRules
	}
	if !spec.PushRules.IsSet() && len(adds) == 0 {
		return nil, nil
	}
	var sets []types.PushRuleSet
	for _, repo := range pushRuleRemotes(spec, req) {
		cloneURL := repoCloneURL(repo)
		if cloneURL == "" {
			continue // admission refuses an unreadable locator; nothing is pushed there
		}
		provider, basePath := pushRuleProvider(sc, cloneURL)
		host, org, err := types.PushRuleEntryOfServerRemote(provider, cloneURL, basePath)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", repo, err)
		}
		key, err := types.PushRuleKey(provider, host, org)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", repo, err)
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
			if add.PushRuleKey() == key {
				rules.DenyPaths = appendMissing(rules.DenyPaths, add.DenyPaths)
				rules.RequireReviewPaths = appendMissing(rules.RequireReviewPaths, add.RequireReviewPaths)
			}
		}
		if rules.IsSet() {
			sets = append(sets, types.PushRuleSet{Provider: provider, Host: host, Org: org, BasePath: basePath, Rules: rules})
		}
	}
	if err := types.ValidatePushRuleSets(sets); err != nil {
		return nil, err
	}
	return sets, nil
}

// pushRuleRemotes are the repositories a run brings: the request's own and every
// one its policy clones.
func pushRuleRemotes(spec types.RunPolicySpec, req *createRunRequest) []string {
	var out []string
	if req != nil && req.Repo != "" {
		out = append(out, req.Repo)
	}
	for _, r := range spec.WorkspaceRepos {
		if !slices.Contains(out, r.Repo) {
			out = append(out, r.Repo)
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
	return slices.ContainsFunc(sets, func(s types.PushRuleSet) bool { return len(s.Rules.RequireReviewPaths) > 0 })
}
