// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
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
	// Every Services address is Azure DevOps, named by a row or not: an
	// organisation's <org>.visualstudio.com host is not known from any row.
	if adoServicesHost(host) {
		return true
	}
	return slices.ContainsFunc(adoSharedHosts(sc), func(h string) bool { return slugHost(h) == slug })
}

// adoGitGrants applies the Azure DevOps rules to a policy's needed git_pat
// secrets: a grant for an Azure DevOps host is owner_only whatever the policy
// said, unless every row deciding it takes each person's own token, in which
// case it is dropped. That is the 0.8.1 shared-token wiring: the pat lane never
// carries it (vetoed on Services, superseded by the ADO lane on Server), so the
// run reads the person's own credential instead. Where every deciding row is an
// own-token row, a launcher (subject) who has added no token on any of them is
// refused here, with the dispatch refusal's reason, rather than started into a
// proxy that cannot boot. Legacy open mode has no rows, so nothing is dropped
// there. A refusal comes back as a 422 status; any other error is a 500.
func (s *Server) adoGitGrants(ctx context.Context, subject string, needed []neededSecret, spec types.RunPolicySpec) ([]neededSecret, int, error) {
	var sc types.SiteConfig
	kept := needed[:0]
	for _, n := range needed {
		if n.kind == types.GrantGitPAT && n.host != "" {
			if s.cfg.Store != nil && sc.WorkspaceProviders == nil {
				var err error
				if sc, err = s.cfg.Store.GetSiteConfig(ctx); err != nil {
					return nil, http.StatusInternalServerError, fmt.Errorf("get site config: %w", err)
				}
			}
			if adoGrantHost(sc, n.host) {
				rows := laneRowsForGrantHost(sc, n.host, repoLocatorsOf(spec.WorkspaceRepos))
				if providersConfigured(sc) && adoRowsTakeOwnToken(rows, n.host) {
					if code, err := s.requireADOOwnToken(ctx, subject, rows); err != nil {
						return nil, code, err
					}
					continue
				}
				n.ownerOnly = true
			}
		}
		kept = append(kept, n)
	}
	return kept, 0, nil
}

// requireADOOwnToken refuses a launch whose Azure DevOps rows all take each
// person's own token when the launcher has added none on any of them. A row
// that signs in through Entra carries the run's credential itself, so its
// presence asks for nothing here.
func (s *Server) requireADOOwnToken(ctx context.Context, subject string, rows []types.GitProvider) (int, error) {
	if slices.ContainsFunc(rows, func(row types.GitProvider) bool { return !isADOOwnTokenRow(row) }) {
		return 0, nil
	}
	for _, row := range rows {
		// A presence check, like the Settings card's: marked, never a value read out.
		_, found, err := s.readADOOwnPAT(secretstore.WithPurpose(ctx, secretstore.PurposeStatus), subject, row.ID)
		if err != nil {
			return http.StatusInternalServerError, fmt.Errorf("read own Azure DevOps token: %w", err)
		}
		if found {
			return 0, nil
		}
	}
	//lint:ignore ST1005 the text is the sentence the refused person reads, as the dispatch refusal's is
	return http.StatusUnprocessableEntity, errors.New(adoOwnPATNotAddedRefusal)
}

// adoRowsTakeOwnToken reports whether rows are all Azure DevOps rows that carry
// each person's own token (on a Services host, any Azure DevOps row: it has no
// pat lane).
func adoRowsTakeOwnToken(rows []types.GitProvider, host string) bool {
	return len(rows) > 0 && !slices.ContainsFunc(rows, func(row types.GitProvider) bool {
		return row.Kind != types.GitProviderAzureDevOps || !(adoServicesHost(host) || isADOOwnTokenRow(row))
	})
}

// adoSSHGrantDropped is the warning on a run whose policy grants an SSH key for
// an Azure DevOps host.
const adoSSHGrantDropped = "ssh_key grant dropped: Azure DevOps has no SSH lane, its credentials are per person"

const retiredADOSharedNameRefusal = "%s is a retired shared Azure DevOps credential name: Azure DevOps credentials are per person now, so the operator can no longer store one"

// retiredADOSharedName reports whether name is one of the retired shared Azure
// DevOps credential names: one the boot sweep lists, or, for any organisation's
// <org>.visualstudio.com address no row names, the same three prefixes on a slug
// ending in -visualstudio-com. ok is false when the site config cannot be read.
func (s *Server) retiredADOSharedName(ctx context.Context, name string) (retired, ok bool) {
	var sc types.SiteConfig
	if s.cfg.Store != nil {
		var err error
		if sc, err = s.cfg.Store.GetSiteConfig(ctx); err != nil {
			return false, false
		}
	}
	names, _ := RetiredADOSharedSecretNames(sc)
	if slices.Contains(names, name) {
		return true, true
	}
	for _, prefix := range []string{"git-pat-", "ssh-key-", "known-hosts-"} {
		if slug, cut := strings.CutPrefix(name, prefix); cut && strings.HasSuffix(slug, "-visualstudio-com") &&
			!otherForgeSlugs(sc)[slug] {
			return true, true
		}
	}
	return false, true
}

// refuseRetiredADOSharedName writes the 400 for an operator's write of a
// retired shared Azure DevOps name and reports whether it did. A person's own
// namespace is not refused: their own token is theirs, and a grant for an Azure
// DevOps host reads only their own row (owner_only, forced in persistRunGrants).
func (s *Server) refuseRetiredADOSharedName(w http.ResponseWriter, r *http.Request, name, owner string) bool {
	if owner != "" {
		return false
	}
	retired, ok := s.retiredADOSharedName(r.Context(), name)
	if !ok {
		// Fail closed: with the provider rows unreadable the name cannot be
		// judged, and a value stored now is never swept (the sweep runs once).
		writeServerError(w, r, "get site config", errors.New("cannot tell whether the name is a retired shared Azure DevOps credential"))
		return true
	}
	if !retired {
		return false
	}
	writeErrorReason(w, http.StatusBadRequest, reasonSecretNameReserved, fmt.Sprintf(retiredADOSharedNameRefusal, name))
	return true
}
