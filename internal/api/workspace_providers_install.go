// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var githubAppInstallPath = regexp.MustCompile(`^/apps/[a-z0-9]+(?:-[a-z0-9]+)*/installations/new$`)

// This is metadata, not an App lookup or installation readiness claim. The
// current App broker serves github.com alone; an enterprise row must never
// acquire a public-forge install link by inference.
func validateGitHubAppInstallURL(i int, row types.GitProvider) error {
	if row.GitHubAppInstallURL == "" {
		return nil
	}
	u, err := url.Parse(row.GitHubAppInstallURL)
	if err == nil && row.Kind == types.GitProviderGitHub && laneAllowed(row, types.GitLaneApp) &&
		validSiteURL(row.GitHubAppInstallURL) && u.Scheme == "https" && u.Host == "github.com" &&
		u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && !strings.Contains(row.GitHubAppInstallURL, "#") && u.RawPath == "" &&
		githubAppInstallPath.MatchString(u.Path) && slices.ContainsFunc(row.BaseURLs, func(base string) bool {
		return hostrules.HostOf(base) == u.Host
	}) {
		return nil
	}
	return fmt.Errorf("git[%d].github_app_install_url: use an https://github.com/apps/<slug>/installations/new URL on a matching GitHub App lane, with no credentials, port, query or fragment", i)
}
