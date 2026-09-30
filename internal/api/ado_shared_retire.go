// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoWellKnownHosts are the Azure DevOps hosts a shared credential could be
// stored under whatever the provider rows say: the Services clone host, and the
// two SSH endpoints (dev.azure.com's and the legacy visualstudio.com one).
var adoWellKnownHosts = []string{"dev.azure.com", "ssh.dev.azure.com", "vs-ssh.visualstudio.com"}

// RetiredADOSharedSecretNames is the list cmd/wardynd's boot sweep deletes from
// every namespace (#1429): for each Azure DevOps host, the stored shared token
// (git-pat-<host slug>), SSH key (ssh-key-<host slug>) and its known-hosts
// secret (known-hosts-<host slug>). The hosts are the well-known ones, every
// host an Azure DevOps provider row names (a disabled row counts), and every
// scm_hosts entry that is an Azure DevOps Services host. A person's own token
// and the tokens Wardyn creates live under other, sealed names, so none of
// them is on this list.
func RetiredADOSharedSecretNames(sc types.SiteConfig) []string {
	hosts := slices.Clone(adoWellKnownHosts)
	hosts = append(hosts, adoServerHosts(sc)...)
	for _, h := range sc.ScmHosts {
		if h = strings.ToLower(strings.TrimSpace(h)); strings.HasSuffix(h, ".visualstudio.com") {
			hosts = append(hosts, h)
		}
	}
	var names []string
	for _, h := range hosts {
		slug := slugHost(h)
		if slug == "" {
			continue
		}
		for _, prefix := range []string{"git-pat-", "ssh-key-", "known-hosts-"} {
			if n := prefix + slug; !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
	}
	slices.Sort(names)
	return names
}
