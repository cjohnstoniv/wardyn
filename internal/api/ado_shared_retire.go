// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoWellKnownHosts are the Azure DevOps hosts a shared credential could be
// stored under whatever the provider rows say: the Services clone host, and the
// two SSH endpoints (dev.azure.com's and the legacy visualstudio.com one).
var adoWellKnownHosts = []string{"dev.azure.com", "ssh.dev.azure.com", "vs-ssh.visualstudio.com"}

// adoServicesHost reports whether host is an Azure DevOps Services address
// (dev.azure.com or <org>.visualstudio.com), which has no token lane.
func adoServicesHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "dev.azure.com" || strings.HasSuffix(host, ".visualstudio.com")
}

// adoSharedHosts is every host a shared Azure DevOps credential could sit
// under: the well-known ones, every host an azure_devops row names (a disabled
// row counts) and every visualstudio.com scm_hosts entry.
func adoSharedHosts(sc types.SiteConfig) []string {
	hosts := slices.Clone(adoWellKnownHosts)
	hosts = append(hosts, adoServerHosts(sc)...)
	for _, h := range sc.ScmHosts {
		if h = strings.ToLower(strings.TrimSpace(h)); strings.HasSuffix(h, ".visualstudio.com") {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// otherForgeSlugs is the slug of every host a NON-Azure-DevOps provider row
// names, plus github.com. A secret name carries only the slug, so a slug one of
// these shares (the same host, or a host that slugs alike) may be that forge's
// own credential, and no Azure DevOps retirement may touch it.
func otherForgeSlugs(sc types.SiteConfig) map[string]bool {
	out := map[string]bool{slugHost("github.com"): true}
	for _, row := range gitProviderRows(sc) {
		if row.Kind == types.GitProviderAzureDevOps {
			continue
		}
		for _, raw := range row.BaseURLs {
			if h := hostrules.HostOf(raw); h != "" {
				out[slugHost(h)] = true
			}
		}
	}
	return out
}

// RetiredADOSharedSecretNames is the list cmd/wardynd's boot sweep deletes from
// every namespace (#1429): for each Azure DevOps host, the stored shared token
// (git-pat-<host slug>), SSH key (ssh-key-<host slug>) and its known-hosts
// secret (known-hosts-<host slug>). A person's own token and the tokens Wardyn
// creates live under other, sealed names, so none of them is on this list.
//
// A host whose slug a non-Azure DevOps row (or github.com) also has is NOT on
// it: the name could be that forge's credential. Those hosts come back as
// skipped, for the sweep to log, so an admin can remove a shared Azure DevOps
// credential there by hand.
func RetiredADOSharedSecretNames(sc types.SiteConfig) (names, skipped []string) {
	other := otherForgeSlugs(sc)
	seen := map[string]bool{}
	for _, h := range adoSharedHosts(sc) {
		slug := slugHost(h)
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		if other[slug] {
			skipped = append(skipped, h)
			continue
		}
		for _, prefix := range []string{"git-pat-", "ssh-key-", "known-hosts-"} {
			names = append(names, prefix+slug)
		}
	}
	slices.Sort(names)
	slices.Sort(skipped)
	return names, skipped
}

// adoGrantHost reports whether a git grant's host is an Azure DevOps host — a
// Services address, a well-known SSH endpoint or an azure_devops row's host —
// and not one a non-Azure DevOps row also claims (an ambiguous host is not
// treated as Azure DevOps, the same rule as the boot sweep).
func adoGrantHost(sc types.SiteConfig, host string) bool {
	slug := slugHost(host)
	if slug == "" || otherForgeSlugs(sc)[slug] {
		return false
	}
	return slices.ContainsFunc(adoSharedHosts(sc), func(h string) bool { return slugHost(h) == slug })
}

// adoSSHGrantDropped is the warning on a run whose policy grants an SSH key for
// an Azure DevOps host.
const adoSSHGrantDropped = "ssh_key grant dropped: Azure DevOps has no SSH lane, its credentials are per person"

const retiredADOSharedNameRefusal = "%s is a retired shared Azure DevOps credential name: Azure DevOps credentials are per person now, so the operator can no longer store one"

// retiredADOSharedName reports whether name is one of the retired shared Azure
// DevOps credential names. A read error answers false: the boot sweep, not this
// door, is what removes them.
func (s *Server) retiredADOSharedName(ctx context.Context, name string) bool {
	if s.cfg.Store == nil {
		return false
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return false
	}
	names, _ := RetiredADOSharedSecretNames(sc)
	return slices.Contains(names, name)
}

// refuseRetiredADOSharedName writes the 400 for an operator's write of a
// retired shared Azure DevOps name and reports whether it did. A person's own
// namespace is not refused: their own token is theirs, and a grant for an Azure
// DevOps host reads only their own row (owner_only, forced in persistRunGrants).
func (s *Server) refuseRetiredADOSharedName(w http.ResponseWriter, r *http.Request, name, owner string) bool {
	if owner != "" || !s.retiredADOSharedName(r.Context(), name) {
		return false
	}
	writeErrorReason(w, http.StatusBadRequest, reasonSecretNameReserved, fmt.Sprintf(retiredADOSharedNameRefusal, name))
	return true
}
