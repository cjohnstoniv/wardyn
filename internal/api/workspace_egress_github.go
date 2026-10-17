// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Call after authorization and the requirement fold: a dropped grant leaves a
// direct clone, while any surviving github_token keeps the broker's single lane.
func (s *Server) unionDirectGitHubEgress(r *http.Request, req createRunRequest, spec *types.RunPolicySpec, ceiling governanceCeiling) ([]string, *runRefusal) {
	if req.InlinePolicy == nil || slices.ContainsFunc(spec.EligibleGrants, func(g types.GrantSpec) bool {
		return g.Kind == types.GrantGitHubToken
	}) {
		return nil, nil
	}
	if !directGitHubRepo(req.Repo) && !slices.ContainsFunc(spec.WorkspaceRepos, func(repo types.WorkspaceRepo) bool {
		return directGitHubRepo(repo.Repo)
	}) {
		return nil, nil
	}
	candidates := types.RunPolicySpec{AllowedDomains: []string{"github.com", "*.githubusercontent.com"}}
	if !s.runUngoverned(r.Context()) {
		// Clamp only the candidates against the domain ceiling. The complete
		// ceiling can inherit configured hosts; the resolved spec holds trusted
		// workspace additions that must not be clamped a second time.
		candidates, _ = composer.Clamp(candidates, types.RunPolicySpec{
			AllowedDomains: ceiling.Spec.AllowedDomains,
			AllowAllEgress: ceiling.Spec.AllowAllEgress,
		}, types.GovernanceLimits{})
		if _, _, err := s.narrowUserInlinePolicy(r.Context(), s.secretOwnerFromRequest(r), &candidates); err != nil {
			return nil, capabilityRunRefusal(err)
		}
	}
	return unionAllowedDomains(spec, candidates.AllowedDomains), nil
}

func directGitHubRepo(repo string) bool {
	u, err := url.Parse(repoCloneURL(repo))
	return err == nil && u.Scheme == "https" && strings.EqualFold(strings.TrimSuffix(u.Hostname(), "."), "github.com")
}
