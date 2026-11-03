// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"maps"
	"net"
	"net/url"
	"slices"
	"strings"
)

// PushDenyAllSecretsNote is what Review and the Read-only and Sealed presets
// say about a run that refuses every push and carries secrets of its own.
const PushDenyAllSecretsNote = "A push token you add as a secret could still push to a code host Wardyn doesn't recognize, if this run can reach it."

// githubForgeSubdomains are the hosts beside a GitHub host that take a push or
// an upload: the API, release asset uploads and gists.
var githubForgeSubdomains = []string{"api.", "uploads.", "gist."}

// DenyAllForgeHosts are the self-hosted code hosts the proxy's deny_all guard
// refuses beside the public ones it knows itself: the site's SCM hosts, each
// Git provider row's hosts (a GitHub row's API, upload and gist hosts with
// it), and the host of each of the run's git_pat and ssh_key grants. Lower
// case, no port, sorted; nil when there are none.
func DenyAllForgeHosts(site SiteConfig, grants []GrantSpec) []string {
	seen := map[string]bool{}
	add := func(h string) {
		h = strings.TrimSpace(h)
		if host, _, err := net.SplitHostPort(h); err == nil {
			h = host
		}
		if h = strings.TrimRight(strings.ToLower(h), "."); h != "" {
			seen[h] = true
		}
	}
	for _, h := range site.ScmHosts {
		add(h)
	}
	if wp := site.WorkspaceProviders; wp != nil {
		for _, row := range wp.Git {
			for _, raw := range row.BaseURLs {
				u, err := url.Parse(raw)
				if err != nil || u.Hostname() == "" {
					continue
				}
				add(u.Hostname())
				if row.Kind == GitProviderGitHub {
					for _, sub := range githubForgeSubdomains {
						add(sub + u.Hostname())
					}
				}
			}
		}
	}
	for _, g := range grants {
		if g.Kind != GrantGitPAT && g.Kind != GrantSSHKey {
			continue
		}
		var sc struct {
			Host string `json:"host"`
		}
		if json.Unmarshal(g.Scope, &sc) == nil {
			add(sc.Host)
		}
	}
	if len(seen) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(seen))
}
