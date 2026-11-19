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

// PublicForgeHosts are the public code hosts' git, SSH, API and upload hosts.
// The proxy's deny_all guard refuses them itself (with the rest of hosted Azure
// DevOps by suffix), and DenyAllForgeHosts always carries them, so a run that
// refuses every push always names forge_hosts and a runner whose proxy has no
// guard is refused it. codeload.github.com is left out on purpose: it serves
// repository archives for download and takes no write.
var PublicForgeHosts = []string{"github.com", "api.github.com", "uploads.github.com", "gist.github.com", "ssh.github.com",
	"dev.azure.com", "ssh.dev.azure.com", "vs-ssh.visualstudio.com"}

// githubForgeSubdomains are the hosts beside a GitHub host that take a push or
// an upload: the API, release asset uploads and gists.
var githubForgeSubdomains = []string{"api.", "uploads.", "gist."}

// DenyAllForgeHosts are the code hosts the proxy's deny_all guard refuses:
// PublicForgeHosts, the site's SCM hosts, each Git provider row's hosts (a
// GitHub row's API, upload and gist hosts with it), and the host of each of the
// run's git_pat and ssh_key grants. Lower case, no port, sorted; never empty.
func DenyAllForgeHosts(site SiteConfig, grants []GrantSpec) []string {
	seen := map[string]bool{}
	for _, h := range PublicForgeHosts {
		seen[h] = true
	}
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
	return slices.Sorted(maps.Keys(seen))
}
